package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/runtime"
)

// envGroup is one environment's slice of managed services during reconcile.
type envGroup struct {
	envID    string
	services []*model.Service
}

// activitySnapshot copies the gateway activity timestamps.
func (e *Engine) activitySnapshot() map[string]time.Time {
	e.activityMu.Lock()
	activity := make(map[string]time.Time, len(e.activity))
	for k, v := range e.activity {
		activity[k] = v
	}
	e.activityMu.Unlock()
	return activity
}

// persistServiceIfChanged writes a service row only when a reconciled field
// actually moved, keeping write amplification low.
func (e *Engine) persistServiceIfChanged(svc model.Service) {
	if svc.LastActiveAt != "" {
		// Volatile fields ride along with the runtime update path.
		if err := e.store.UpsertService(svc); err != nil {
			e.logger.Printf("warn", "更新服务状态失败: %v", err)
		}
		return
	}
	if err := e.store.UpsertService(svc); err != nil {
		e.logger.Printf("warn", "更新服务状态失败: %v", err)
	}
}

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
	// Retry pending transaction recovery BEFORE taking writeMu. Recovery
	// acquires recoveryMu → writeMu itself; calling it while writeMu is held
	// would self-deadlock the reconcile loop against recoverAttachTxn.
	e.retryPendingRecovery()
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

	services, err := e.store.ListServices()
	if err != nil {
		e.logger.Printf("warn", "reconcile 读取服务失败: %v", err)
		return
	}
	if len(services) == 0 {
		e.gw.Prune(map[string]bool{})
		return
	}

	// Group services by their pinned environment. Legacy records (no
	// environment_id) map onto the environment of the socket they were
	// attached on; that is the current primary for records without a pin.
	primary := e.currentEnvID()
	groups := map[string]*envGroup{}
	order := []string{}
	for i := range services {
		svc := &services[i]
		envID := svc.EnvironmentID
		if envID == "" {
			if svc.DockerSocket == "" || svc.DockerSocket == strings.TrimPrefix(e.currentEnvEndpoint(), "unix://") {
				envID = primary
			} else {
				envID = runtime.EnvID("unix://" + svc.DockerSocket)
			}
		}
		g, ok := groups[envID]
		if !ok {
			g = &envGroup{envID: envID}
			groups[envID] = g
			order = append(order, envID)
		}
		g.services = append(g.services, svc)
	}

	// Which environments are enabled right now?
	enabledEnvs := map[string]bool{primary: true}
	envs, _ := e.store.ListEnvironments()
	for _, env := range envs {
		if env.Enabled {
			enabledEnvs[env.ID] = true
		}
	}
	e.logger.Printf("info", "reconcile：检查 %d 个托管服务（%d 个环境）", len(services), len(order))

	s := e.settings.Get()
	keep := map[string]bool{}
	activity := e.activitySnapshot()

	for _, envID := range order {
		group := groups[envID]
		if !enabledEnvs[envID] {
			// The environment was disabled: its proxy mappings live on that
			// env's proxy, so routing through anything local would only
			// create dead listeners. Prune the routes and say so.
			for _, svc := range group.services {
				for _, b := range svc.Bindings {
					e.gw.Remove(fmt.Sprintf("%s|%s|%d", svc.ID, b.HostIP, b.HostPort))
				}
				svc.Status = "missing"
				svc.LastError = "所属容器环境已停用"
				e.persistServiceIfChanged(*svc)
			}
			continue
		}
		rt, err := e.runtimeForEnv(ctx, envID)
		if err != nil {
			e.logger.Printf("warn", "reconcile 跳过环境 %s：%v", envID, err)
			// Keep the listeners: an offline engine (e.g. a stopped Colima
			// VM) must not tear down wiring that serves again when it returns.
			for _, svc := range group.services {
				for _, b := range svc.Bindings {
					keep[fmt.Sprintf("%s|%s|%d", svc.ID, b.HostIP, b.HostPort)] = true
				}
				svc.Status = "missing"
				svc.LastError = "容器环境不可达: " + err.Error()
				e.persistServiceIfChanged(*svc)
			}
			continue
		}
		spec := e.proxySpecForEnv(envID)
		e.reconcileEnvGroup(ctx, rt, spec, group, s, activity, keep)
	}
	e.gw.Prune(keep)
}

// reconcileEnvGroup reconciles one environment's services against that env's
// own Docker client and proxy container.
func (e *Engine) reconcileEnvGroup(ctx context.Context, rt runtime.Runtime, spec proxyEnvSpec, group *envGroup, s model.Settings, activity map[string]time.Time, keep map[string]bool) {
	summaries, lerr := rt.ListContainers(ctx)
	if lerr != nil {
		e.logger.Printf("warn", "reconcile 列出容器失败（环境 %s）: %v", spec.EnvID, lerr)
		for _, svc := range group.services {
			for _, b := range svc.Bindings {
				keep[fmt.Sprintf("%s|%s|%d", svc.ID, b.HostIP, b.HostPort)] = true
			}
			svc.Status = "missing"
			svc.LastError = "读取容器列表失败: " + lerr.Error()
			e.persistServiceIfChanged(*svc)
		}
		return
	}
	byID := make(map[string]dockerx.ContainerSummary, len(summaries))
	byName := make(map[string]dockerx.ContainerSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
		for _, name := range s.Names {
			byName[strings.TrimPrefix(name, "/")] = s
		}
	}

	proxyIns, proxyErr := rt.InspectContainer(ctx, spec.ContainerName)
	proxyRunning := proxyErr == nil && proxyIns.State.Running
	proxyIP := ""
	if proxyErr == nil {
		proxyIP = containerIP(proxyIns)
	}

	for _, svc := range group.services {
		var summary *dockerx.ContainerSummary
		var lastErr string
		if svc.ContainerID != "" {
			if s, ok := byID[svc.ContainerID]; ok {
				summary = &s
			}
		}
		if summary == nil {
			// Rebind by name only within the service's own environment: two
			// environments can run same-named containers; adopting one from
			// the wrong environment would sleep/restore the wrong workload.
			if s, ok := byName[svc.ContainerName]; ok {
				summary = &s
				svc.ContainerID = s.ID
			}
		}
		status := "missing"
		if summary == nil {
			lastErr = "容器不存在（可能已被删除）"
		} else {
			svc.ContainerID = summary.ID
			switch {
			case strings.EqualFold(summary.State, "running"):
				if e.gw.ActiveConns(svc.ID) > 0 {
					status = "managed_running"
				} else {
					status = "managed_idle"
				}
			case strings.EqualFold(summary.State, "paused"):
				status = "warning"
				lastErr = "容器处于暂停状态"
			case strings.EqualFold(summary.State, "restarting") || strings.EqualFold(summary.State, "dead"):
				status = "warning"
				lastErr = "容器状态异常: " + summary.State
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
				// If a healthy route is already correct, keep it. Otherwise
				// remove the stale route so it cannot forward to the wrong IP.
				if e.gw.Matches(route) {
					keep[key] = true
				} else {
					e.gw.Remove(key)
				}
			} else {
				keep[key] = true
			}
			if summary != nil && containerSummaryPublishes(*summary, model.PortBinding{
				HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol,
			}) {
				status = "warning"
				lastErr = fmt.Sprintf("容器又重新发布了端口 %d，与端口网关冲突（可能有人手动恢复了配置）", b.HostPort)
			}
		}
		svc.ActiveConnections = e.gw.ActiveConns(svc.ID)
		if at, ok := activity[svc.ID]; ok && at.After(parseTime(svc.LastActiveAt)) {
			svc.LastActiveAt = at.UTC().Format(time.RFC3339)
		}
		svc.Status = status
		svc.LastError = lastErr
		e.persistServiceIfChanged(*svc)
	}

	if proxyRunning {
		force := false
		e.mu.Lock()
		if e.lastProxyIDs[spec.EnvID] != proxyIns.ID {
			e.lastProxyIDs[spec.EnvID] = proxyIns.ID
			force = true
		}
		e.mu.Unlock()
		if err := e.syncProxyConfigForEnv(ctx, spec.EnvID, force); err != nil {
			e.logger.Printf("warn", "同步代理配置失败（环境 %s）: %v", spec.EnvID, err)
		}
	} else if proxyErr != nil {
		e.logger.Printf("warn", "环境 %s 的代理容器未运行，其托管服务的新连接将无法唤醒", spec.EnvID)
	}
}

// retryPendingRecovery re-runs batch recovery for stuck transactions at most
// once every two minutes, and only when the proxy admin endpoint answers.
// It must be called WITHOUT writeMu held: recovery takes recoveryMu → writeMu.
func (e *Engine) retryPendingRecovery() {
	running, err := e.store.ListRunningTransactions()
	if err != nil || len(running) == 0 {
		return
	}
	s := e.settings.Get()
	e.recoveryMu.Lock()
	due := time.Since(e.lastRecovery) > 2*time.Minute
	e.recoveryMu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	reachable := e.proxyAdminReachable(ctx, s.AdminPort)
	cancel()
	if reachable {
		e.logger.Printf("warn", "仍存在 %d 个未完成事务，重试恢复", len(running))
		e.RecoverTransactions()
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
		// Watch every enabled environment, one connection each. A flapping
		// engine only affects its own watcher.
		envs, _ := e.store.ListEnvironments()
		primary := e.currentEnvID()
		var wg sync.WaitGroup
		watch := func(envID string) {
			defer wg.Done()
			e.watchEnvEvents(envID)
		}
		wg.Add(1)
		go watch(primary)
		for _, env := range envs {
			if !env.Enabled || env.ID == primary {
				continue
			}
			wg.Add(1)
			go watch(env.ID)
		}
		wg.Wait()
		if !sleepOrStop(e.stopCh, 2*time.Second) {
			return
		}
	}
}

// watchEnvEvents streams one environment's container events until an error
// or shutdown. Every relevant event just nudges the reconcile loop.
func (e *Engine) watchEnvEvents(envID string) {
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
	defer cancel()
	rt, err := e.runtimeForEnv(ctx, envID)
	if err != nil {
		<-done
		sleepOrStop(e.stopCh, 5*time.Second)
		return
	}
	body, err := rt.Events(ctx)
	if err != nil {
		<-done
		sleepOrStop(e.stopCh, 3*time.Second)
		return
	}
	defer body.Close()
	e.logger.Printf("info", "已订阅 Docker 事件（环境 %s）", envID)
	dec := json.NewDecoder(body)
	for {
		var ev dockerEvent
		if err := dec.Decode(&ev); err != nil {
			return
		}
		if e.relevantEvent(&ev) {
			e.triggerReconcile()
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
	if name == proxy.ContainerName || strings.HasPrefix(name, proxy.ContainerName+"-") {
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

func serviceRuntimeChanged(a, b model.Service) bool {
	return a.ContainerID != b.ContainerID || a.Status != b.Status || a.LastError != b.LastError ||
		a.ActiveConnections != b.ActiveConnections || a.LastActiveAt != b.LastActiveAt
}

func containerSummaryPublishes(s dockerx.ContainerSummary, b model.PortBinding) bool {
	for _, p := range s.Ports {
		if int(p.PublicPort) == 0 || int(p.PublicPort) != b.HostPort {
			continue
		}
		if int(p.PrivatePort) != b.ContainerPort {
			continue
		}
		proto := p.Type
		if proto == "" {
			proto = "tcp"
		}
		want := b.Protocol
		if want == "" {
			want = "tcp"
		}
		if proto != want {
			continue
		}
		if b.HostIP == "" || b.HostIP == "0.0.0.0" || p.IP == "" || p.IP == b.HostIP {
			return true
		}
	}
	return false
}
