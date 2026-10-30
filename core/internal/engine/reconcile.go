package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

func (e *Engine) reconcileLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.reconcile()
		case <-e.eventsCh:
			e.reconcile()
		}
	}
}

func (e *Engine) reconcile() {
	// Never touch gateway routes or proxy config while a transactional
	// operation (attach/restore/proxy-setup) is mutating them.
	if !e.writeMu.TryLock() {
		e.logger.Printf("info", "跳过本轮 reconcile：正在执行写操作")
		return
	}
	defer e.writeMu.Unlock()
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if running, _ := e.store.ListRunningTransactions(); len(running) > 0 {
		s := e.settings.Get()
		e.recoveryMu.Lock()
		due := time.Since(e.lastRecovery) > 2*time.Minute
		e.recoveryMu.Unlock()
		if due && e.proxyAdminReachable(ctx, s.AdminPort) {
			e.logger.Printf("warn", "仍存在 %d 个未完成事务，重试恢复", len(running))
			e.RecoverTransactions()
		}
	}

	services, err := e.store.ListServices()
	if err != nil {
		e.logger.Printf("warn", "reconcile 读取服务失败: %v", err)
		return
	}
	if len(services) == 0 {
		e.gw.Prune(map[string]bool{})
		return
	}
	e.logger.Printf("info", "reconcile：检查 %d 个托管服务", len(services))
	c, err := e.dockerClient(ctx)
	if err != nil {
		e.logger.Printf("warn", "reconcile 跳过：Docker 不可用: %v", err)
		return
	}
	s := e.settings.Get()

	proxyIns, proxyErr := c.InspectContainer(ctx, proxy.ContainerName)
	proxyRunning := proxyErr == nil && proxyIns.State.Running
	proxyIP := ""
	if proxyErr == nil {
		proxyIP = containerIP(proxyIns)
	}

	e.activityMu.Lock()
	activity := make(map[string]time.Time, len(e.activity))
	for k, v := range e.activity {
		activity[k] = v
	}
	e.activityMu.Unlock()

	keep := map[string]bool{}
	for i := range services {
		svc := &services[i]
		var ins *dockerx.ContainerInspect
		var lastErr string
		if svc.ContainerID != "" {
			ii, err := c.InspectContainer(ctx, svc.ContainerID)
			if err == nil {
				ins = ii
			}
		}
		if ins == nil {
			if ii, err := c.InspectContainer(ctx, svc.ContainerName); err == nil {
				ins = ii
				svc.ContainerID = ii.ID
			}
		}
		status := "missing"
		if ins == nil {
			lastErr = "容器不存在（可能已被删除）"
		} else {
			svc.ContainerID = ins.ID
			switch {
			case ins.State.Running && ins.State.Paused:
				status = "warning"
				lastErr = "容器处于暂停状态"
			case ins.State.Running:
				if e.gw.ActiveConns(svc.ID) > 0 {
					status = "managed_running"
				} else {
					status = "managed_idle"
				}
			default:
				status = "managed_stopped"
			}
		}

		// Keep the host gateway listeners alive for every binding, even while
		// the container sleeps: the first connection wakes it through the proxy.
		for _, b := range svc.Bindings {
			key := fmt.Sprintf("%s|%s|%d", svc.ID, b.HostIP, b.HostPort)
			// Build routes through the shared helper: in container_ip mode an
			// unresolvable proxy IP must surface as an error and skip the
			// listener, never fall back to 127.0.0.1.
			route, rerr := gatewayRouteFor(svc.ID, svc.ContainerName, b, s, proxyIP)
			if rerr == nil {
				rerr = e.gw.Ensure(route)
			}
			if rerr != nil {
				status = "warning"
				lastErr = rerr.Error()
				// A stale route may still point at the old proxy container IP.
				// Remove it instead of keeping it via keep[key].
				e.gw.Remove(key)
			} else {
				keep[key] = true
			}
			if ins != nil && containerPublishes(ins, model.PortBinding{
				HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol,
			}) {
				status = "warning"
				lastErr = fmt.Sprintf("容器又重新发布了端口 %d，与端口网关冲突（可能有人手动恢复了 Compose 文件）", b.HostPort)
			}
		}
		svc.ActiveConnections = e.gw.ActiveConns(svc.ID)
		if at, ok := activity[svc.ID]; ok && at.After(parseTime(svc.LastActiveAt)) {
			svc.LastActiveAt = at.UTC().Format(time.RFC3339)
		}
		svc.Status = status
		svc.LastError = lastErr
		if err := e.store.UpsertService(*svc); err != nil {
			e.logger.Printf("warn", "更新服务状态失败: %v", err)
		}
	}
	e.gw.Prune(keep)

	if proxyRunning {
		force := false
		e.mu.Lock()
		if e.lastProxyID != proxyIns.ID {
			e.lastProxyID = proxyIns.ID
			force = true
		}
		e.mu.Unlock()
		if err := e.syncProxyConfig(ctx, force); err != nil {
			e.logger.Printf("warn", "同步代理配置失败: %v", err)
		}
	} else if proxyErr != nil || proxyIns != nil {
		e.logger.Printf("warn", "代理容器未运行，已托管服务的新连接将无法唤醒")
	}
}

func (e *Engine) eventsLoop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.stopCh:
			return
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			select {
			case <-e.stopCh:
				cancel()
			case <-ctx.Done():
			}
		}()
		c, err := e.dockerClient(ctx)
		if err != nil {
			cancel()
			<-done
			if !sleepOrStop(e.stopCh, 5*time.Second) {
				return
			}
			continue
		}
		body, err := c.Events(ctx)
		if err != nil {
			cancel()
			<-done
			if !sleepOrStop(e.stopCh, 3*time.Second) {
				return
			}
			continue
		}
		e.logger.Printf("info", "已订阅 Docker 事件")
		dec := json.NewDecoder(body)
		for {
			var ev dockerEvent
			if err := dec.Decode(&ev); err != nil {
				break
			}
			if e.relevantEvent(&ev) {
				e.triggerReconcile()
			}
		}
		body.Close()
		cancel()
		<-done
		if !sleepOrStop(e.stopCh, 2*time.Second) {
			return
		}
	}
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (e *Engine) relevantEvent(ev *dockerEvent) bool {
	if ev.Type != "container" {
		return false
	}
	name := ev.Actor.Attributes["name"]
	if name == proxy.ContainerName {
		return true
	}
	services, err := e.store.ListServices()
	if err != nil {
		return false
	}
	for _, svc := range services {
		if svc.ContainerName == name || svc.ContainerID == ev.Actor.ID {
			return true
		}
	}
	return false
}

func sleepOrStop(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}
