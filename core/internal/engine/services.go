package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

// Services returns managed services with live connection counts.
func (e *Engine) Services() ([]model.Service, error) {
	list, err := e.store.ListServices()
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []model.Service{}
	}
	e.activityMu.Lock()
	activity := make(map[string]time.Time, len(e.activity))
	for k, v := range e.activity {
		activity[k] = v
	}
	e.activityMu.Unlock()
	for i := range list {
		list[i].ActiveConnections = e.gw.ActiveConns(list[i].ID)
		if at, ok := activity[list[i].ID]; ok && at.After(parseTime(list[i].LastActiveAt)) {
			list[i].LastActiveAt = at.UTC().Format(time.RFC3339)
		}
	}
	return list, nil
}

// ServiceAction performs a lifecycle action. start/stop/updatePolicy are
// synchronous; restore/remove are transactional and return an operation id.
func (e *Engine) ServiceAction(ctx context.Context, id, action string, params map[string]any) (map[string]any, error) {
	svc, ok, err := e.store.GetService(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("托管服务不存在: %s", id)
	}
	switch action {
	case "start":
		if err := e.containerAction(ctx, svc, "start"); err != nil {
			return nil, err
		}
		e.triggerReconcile()
		return map[string]any{"ok": true}, nil
	case "stop":
		if err := e.containerAction(ctx, svc, "stop"); err != nil {
			return nil, err
		}
		e.triggerReconcile()
		return map[string]any{"ok": true}, nil
	case "updatePolicy":
		return e.updatePolicy(id, params)
	case "restore", "remove":
		opID, err := e.startDetach(svc, action == "remove")
		if err != nil {
			return nil, err
		}
		return map[string]any{"operationId": opID}, nil
	default:
		return nil, fmt.Errorf("未知操作: %s", action)
	}
}

func (e *Engine) containerAction(ctx context.Context, svc model.Service, action string) error {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return err
	}
	target := svc.ContainerID
	if target == "" {
		target = svc.ContainerName
	}
	ins, err := c.InspectContainer(ctx, target)
	if err != nil {
		return fmt.Errorf("容器不可用: %w", err)
	}
	if action == "start" {
		if ins.State.Running {
			return nil
		}
		return c.Post(ctx, "/containers/"+ins.ID+"/start", nil, nil)
	}
	if !ins.State.Running {
		return nil
	}
	return c.Post(ctx, "/containers/"+ins.ID+"/stop?t=10", nil, nil)
}

func (e *Engine) updatePolicy(id string, params map[string]any) (map[string]any, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	svc, ok, err := e.store.GetService(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("托管服务不存在: %s", id)
	}
	if v, ok := params["idleTimeoutSecs"]; ok {
		n := toInt(v)
		if n < 30 || n > 604800 {
			return nil, errors.New("休眠时间必须在 30 到 604800 秒之间")
		}
		svc.IdleTimeoutSecs = n
	}
	if v, ok := params["excludedFromSleep"]; ok {
		if b, ok := v.(bool); ok {
			svc.ExcludedFromSleep = b
		}
	}
	if err := e.store.UpsertService(svc); err != nil {
		return nil, err
	}
	if err := e.syncProxyConfig(context.Background(), true); err != nil {
		return nil, fmt.Errorf("更新代理配置失败: %w", err)
	}
	e.logger.Printf("info", "更新休眠策略: %s idle=%ds excluded=%v", svc.ContainerName, svc.IdleTimeoutSecs, svc.ExcludedFromSleep)
	return map[string]any{"ok": true}, nil
}

func (e *Engine) startDetach(svc model.Service, remove bool) (string, error) {
	opID := newID("op")
	kind := "restore"
	if remove {
		kind = "remove"
	}
	op := model.Operation{
		ID:        opID,
		Kind:      kind,
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: filepath.Join(e.settings.Get().BackupDir, opID),
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		return "", err
	}
	go e.runDetach(svc, op, remove)
	return opID, nil
}

func (e *Engine) runDetach(svc model.Service, op model.Operation, remove bool) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	ctx, cancel := contextWithOpTimeout()
	defer cancel()
	logger := e.newOpLogger(op)
	logger.add("info", fmt.Sprintf("开始恢复 %s 的原始端口映射", svc.ContainerName))
	if err := e.restoreService(ctx, logger, svc); err != nil {
		logger.finish("failed", err.Error())
		e.setLastError("恢复失败: " + err.Error())
		return
	}
	logger.add("info", "原始端口映射已恢复；OrbSleep 已停止管理该服务")
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.add("warn", "同步代理配置失败: "+err.Error())
	}
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

func (e *Engine) restoreService(ctx context.Context, logger *opLogger, svc model.Service) error {
	cli, cliErr := e.cliRunner()
	if cliErr != nil {
		return fmt.Errorf("docker CLI 不可用，无法重建服务: %w", cliErr)
	}
	composeFile := svc.ComposeFile
	workDir := filepath.Dir(composeFile)
	backupFile := ""
	if backupDir, ok := e.store.GetMeta("serviceBackup:" + svc.ID); ok {
		metaRaw, _ := os.ReadFile(filepath.Join(backupDir, "metadata.json"))
		var meta struct {
			ComposeFile string `json:"composeFile"`
			WorkingDir  string `json:"workingDir"`
		}
		_ = json.Unmarshal(metaRaw, &meta)
		if meta.ComposeFile != "" {
			composeFile = meta.ComposeFile
		}
		if meta.WorkingDir != "" {
			workDir = meta.WorkingDir
		}
		backupFile = filepath.Join(backupDir, filepath.Base(composeFile))
	}

	// 1. Release the host ports from the gateway.
	e.gw.RemoveService(svc.ID)
	restoreGateway := func() {
		for _, b := range svc.Bindings {
			if route, err := e.gatewayRoute(ctx, svc.ID, svc.ContainerName, b, e.settings.Get()); err == nil {
				_ = e.gw.Ensure(route)
			}
		}
	}
	logger.add("info", "已停止端口网关监听")

	// 2. Remove the proxy target (keep the original config for rollback).
	admin, adminErr := e.adminClient()
	var origConfig, nextConfig proxy.Config
	proxyRemoved := false
	if adminErr == nil {
		if cfg, err := admin.GetConfig(ctx); err == nil {
			origConfig = cfg
			nextConfig = cfg
			nextConfig.Remove(svc.ContainerName)
			if err := admin.UpdateConfig(ctx, nextConfig); err != nil {
				logger.add("warn", "移除代理目标失败: "+err.Error())
			} else {
				proxyRemoved = true
			}
		}
	}

	removals := bindingsFor(svc)
	// revertManaged re-establishes the managed (proxied) state if restoring the
	// original config fails halfway, so we never leave the service unreachable.
	revertManaged := func() {
		if current, err := os.ReadFile(composeFile); err == nil {
			if modified, _, err := compose.RemoveBindings(current, svc.ComposeService, removals); err == nil {
				mode := os.FileMode(0o644)
				if st, statErr := os.Stat(composeFile); statErr == nil {
					mode = st.Mode()
				}
				if compose.WriteAtomic(composeFile, modified, mode) == nil {
					rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					_ = cli.UpService(rctx, svc.ComposeProject, composeFile, workDir, svc.ComposeService)
					cancel()
				}
			}
		}
		restoreGateway()
		if proxyRemoved {
			_ = admin.UpdateConfig(ctx, origConfig)
		}
	}

	// 3. Re-add only this service's ports. Restoring the whole file would also
	// revert ports of other managed services sharing the same Compose file.
	current, readErr := os.ReadFile(composeFile)
	if readErr != nil {
		if backupFile == "" {
			restoreGateway()
			if proxyRemoved {
				_ = admin.UpdateConfig(ctx, origConfig)
			}
			return fmt.Errorf("读取 Compose 文件失败且没有可用备份: %w", readErr)
		}
		if err := compose.RestoreFile(composeFile, backupFile); err != nil {
			restoreGateway()
			if proxyRemoved {
				_ = admin.UpdateConfig(ctx, origConfig)
			}
			return fmt.Errorf("恢复 Compose 文件失败: %w", err)
		}
		logger.add("warn", "Compose 文件缺失，已从备份完整恢复")
	} else {
		out, added, err := compose.AddBindings(current, svc.ComposeService, removals)
		if err != nil {
			if backupFile == "" {
				restoreGateway()
				if proxyRemoved {
					_ = admin.UpdateConfig(ctx, origConfig)
				}
				return fmt.Errorf("无法安全地恢复端口声明: %w", err)
			}
			logger.add("warn", "无法定点恢复端口（"+err.Error()+"），改用完整备份恢复；若该 Compose 文件还管理着其他服务，请检查端口")
			if err := compose.RestoreFile(composeFile, backupFile); err != nil {
				revertManaged()
				return fmt.Errorf("完整备份恢复失败: %w", err)
			}
		} else if len(added) > 0 {
			mode := os.FileMode(0o644)
			if st, statErr := os.Stat(composeFile); statErr == nil {
				mode = st.Mode()
			}
			if err := compose.WriteAtomic(composeFile, out, mode); err != nil {
				revertManaged()
				return fmt.Errorf("写入 Compose 文件失败: %w", err)
			}
			logger.add("info", fmt.Sprintf("已恢复端口声明: %s", portsText(removals)))
		} else {
			logger.add("info", "Compose 中已存在原始端口声明，无需修改")
		}
	}

	// 4. Recreate the service and verify.
	if err := cli.UpService(ctx, svc.ComposeProject, composeFile, workDir, svc.ComposeService); err != nil {
		revertManaged()
		return fmt.Errorf("重建服务失败，已回退到托管状态: %w", err)
	}
	if _, err := e.waitContainerRunning(ctx, svc.ContainerName, time.Duration(maxInt(svc.StartTimeoutSecs, 30))*time.Second); err != nil {
		revertManaged()
		return fmt.Errorf("服务未恢复运行，已回退到托管状态: %w", err)
	}
	if err := e.testBindings(ctx, svc.Bindings); err != nil {
		logger.add("warn", "原端口连通性测试未通过: "+err.Error())
	}
	if err := e.store.DeleteService(svc.ID); err != nil {
		return fmt.Errorf("删除托管记录失败: %w", err)
	}
	return nil
}

func bindingsFor(svc model.Service) []model.PortBinding {
	out := make([]model.PortBinding, 0, len(svc.Bindings))
	for _, b := range svc.Bindings {
		out = append(out, model.PortBinding{
			HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol,
		})
	}
	return out
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
