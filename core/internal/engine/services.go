package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
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
	switch action {
	case "start", "stop":
		// Serialise lifecycle changes with attach/restore/proxy transactions.
		if !e.writeMu.TryLock() {
			return nil, errors.New("正在执行接入/恢复等操作，请稍后重试")
		}
		defer e.writeMu.Unlock()
		svc, ok, err := e.store.GetService(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("托管服务不存在: %s", id)
		}
		if err := e.containerAction(ctx, svc, action); err != nil {
			return nil, err
		}
		e.triggerReconcile()
		return map[string]any{"ok": true}, nil
	case "updatePolicy":
		return e.updatePolicy(id, params)
	case "restore", "remove":
		svc, ok, err := e.store.GetService(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("托管服务不存在: %s", id)
		}
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
	if !e.writeMu.TryLock() {
		return nil, errors.New("正在执行接入/恢复等操作，请稍后重试")
	}
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
	ctx, done, err := e.beginOperation(20 * time.Minute)
	if err != nil {
		return "", err
	}
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
		done()
		return "", err
	}
	go e.runDetach(ctx, svc, op, remove, done)
	return opID, nil
}

func (e *Engine) runDetach(ctx context.Context, svc model.Service, op model.Operation, remove bool, done func()) {
	defer done()
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	logger := e.newOpLogger(op)
	logger.add("info", fmt.Sprintf("开始恢复 %s 的原始端口映射", svc.ContainerName))
	if err := e.restoreService(ctx, logger, svc); err != nil {
		status := "failed"
		var ae *attachError
		if errors.As(err, &ae) && ae.RollbackFailed {
			status = "rollback_failed"
		}
		logger.finish(status, err.Error())
		e.setLastError("恢复失败: " + err.Error())
		return
	}
	if remove {
		if dir, ok := e.store.GetMeta("serviceBackup:" + svc.ID); ok {
			if err := os.RemoveAll(dir); err == nil {
				_ = e.store.DeleteMeta("serviceBackup:" + svc.ID)
			}
		}
	}
	logger.add("info", "原始端口映射已恢复；OrbSleep 已停止管理该服务")
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.add("warn", "同步代理配置失败: "+err.Error())
	}
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

// restoreService restores the original host port mapping. The proxy target is
// removed first and verified; the managed record is only deleted after the
// container is running and the original ports are published and reachable.
func (e *Engine) restoreService(ctx context.Context, logger *opLogger, svc model.Service) error {
	cli, cliErr := e.cliRunner()
	if cliErr != nil {
		return fmt.Errorf("docker CLI 不可用，无法重建服务: %w", cliErr)
	}
	admin, adminErr := e.adminClient()
	if adminErr != nil {
		return fmt.Errorf("代理管理接口不可用，无法安全恢复: %w", adminErr)
	}
	origCfg, err := admin.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("读取代理配置失败，已中止恢复（未做任何修改）: %w", err)
	}
	if err := validateProxyConfig(origCfg); err != nil {
		return fmt.Errorf("代理配置无效，已中止恢复（未做任何修改）: %w", err)
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

	// 1. Remove the proxy target first. If this cannot be verified, abort before
	// touching the gateway or Compose file.
	nextCfg := origCfg.Clone()
	nextCfg.Remove(svc.ContainerName)
	if err := e.putProxyConfigVerified(ctx, admin, nextCfg); err != nil {
		return fmt.Errorf("删除代理目标失败，已中止恢复（未做任何其他修改）: %w", err)
	}
	logger.add("info", "已从代理移除目标: "+svc.ContainerName)

	// 2. Release the host ports.
	e.gw.RemoveService(svc.ID)
	logger.add("info", "已停止端口网关监听")

	removals := bindingsFor(svc)

	// revertManaged puts the service back into the managed state. Every step is
	// verified; failures are returned so the caller can report rollback_failed.
	revertManaged := func() error {
		var errs []string
		current, readErr := os.ReadFile(composeFile)
		if readErr == nil {
			if modified, _, err := compose.RemoveBindings(current, svc.ComposeService, removals); err == nil {
				mode := os.FileMode(0o644)
				if st, statErr := os.Stat(composeFile); statErr == nil {
					mode = st.Mode()
				}
				if werr := compose.WriteAtomic(composeFile, modified, mode); werr != nil {
					errs = append(errs, "重写托管 Compose 失败: "+werr.Error())
				}
			}
		}
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if uerr := cli.UpService(rctx, svc.ComposeProject, composeFile, workDir, svc.ComposeService); uerr != nil {
			errs = append(errs, "重建容器失败: "+uerr.Error())
		}
		cancel()
		if gerr := e.ensureGatewayForService(ctx, svc.ID); gerr != nil {
			errs = append(errs, "重新监听端口失败: "+gerr.Error())
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 60*time.Second)
		if perr := e.putProxyConfigVerified(pctx, admin, origCfg); perr != nil {
			errs = append(errs, "恢复代理配置失败: "+perr.Error())
		}
		pcancel()
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	}
	failWithRevert := func(cause error) error {
		rerr := revertManaged()
		ae := &attachError{Err: cause}
		if rerr != nil {
			ae.RollbackFailed = true
			ae.Detail = "已尝试回退到托管状态，但失败: " + rerr.Error()
			logger.add("error", ae.Detail)
		} else {
			logger.add("warn", "已回退到托管状态")
		}
		return ae
	}

	// 3. Restore only this service's ports. Never overwrite the file with a full
	// backup while it exists: that could discard unrelated user edits.
	current, readErr := os.ReadFile(composeFile)
	if readErr != nil {
		if backupFile == "" {
			return failWithRevert(fmt.Errorf("Compose 文件不可读且没有备份: %w", readErr))
		}
		if err := compose.RestoreFile(composeFile, backupFile); err != nil {
			return failWithRevert(fmt.Errorf("从备份恢复 Compose 文件失败: %w", err))
		}
		logger.add("warn", "Compose 文件缺失，已从备份恢复")
	} else {
		out, added, err := compose.AddBindings(current, svc.ComposeService, removals)
		if err != nil {
			return failWithRevert(fmt.Errorf("无法安全地定点恢复端口（未覆盖原文件）: %w；请手动将 %s 加回 %s",
				err, portsText(removals), composeFile))
		}
		if len(added) > 0 {
			mode := os.FileMode(0o644)
			if st, statErr := os.Stat(composeFile); statErr == nil {
				mode = st.Mode()
			}
			if err := compose.WriteAtomic(composeFile, out, mode); err != nil {
				return failWithRevert(fmt.Errorf("写入 Compose 文件失败: %w", err))
			}
			logger.add("info", fmt.Sprintf("已恢复端口声明: %s", portsText(removals)))
		} else {
			logger.add("info", "Compose 中已存在原始端口声明，无需修改")
		}
	}

	// 4. Recreate, then verify both container state and published ports.
	if err := cli.UpService(ctx, svc.ComposeProject, composeFile, workDir, svc.ComposeService); err != nil {
		return failWithRevert(fmt.Errorf("重建服务失败: %w", err))
	}
	ins, err := e.waitContainerRunning(ctx, svc.ContainerName, time.Duration(maxInt(svc.StartTimeoutSecs, 30))*time.Second)
	if err != nil {
		return failWithRevert(fmt.Errorf("服务未恢复运行: %w", err))
	}
	for _, b := range svc.Bindings {
		if !containerPublishes(ins, model.PortBinding{
			HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol,
		}) {
			return failWithRevert(fmt.Errorf("容器未重新发布原始端口 %s:%d", hostLabel(b.HostIP), b.HostPort))
		}
	}
	if err := e.testBindings(ctx, svc.Bindings); err != nil {
		return failWithRevert(fmt.Errorf("原端口连通性测试失败: %w", err))
	}
	logger.add("info", "原端口连通性测试通过")

	if err := e.store.DeleteService(svc.ID); err != nil {
		// The record must not outlive the managed wiring. Put the service back
		// into the verified managed state, or report rollback_failed.
		return failWithRevert(fmt.Errorf("删除托管记录失败: %w", err))
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
