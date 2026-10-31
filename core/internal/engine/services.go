package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/failpoint"
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
		if err := e.ensureNoUnresolvedWork(); err != nil {
			return nil, err
		}
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
		if err := e.ensureNoUnresolvedWork(); err != nil {
			return nil, err
		}
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
	if err := e.ensureNoUnresolvedWork(); err != nil {
		return nil, err
	}
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
	// done() (and therefore txnWG.Done) must run on every exit path, including
	// the early lock-failure return: beginOperation already incremented the
	// counter before this goroutine started.
	defer done()

	// Same lock order as runAttach: writeMu first, then the per-file Compose
	// lock. Acquiring them the other way around deadlocks against a concurrent
	// attach on the same file.
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	lockCtx, lockCancel := context.WithTimeout(ctx, 60*time.Second)
	lock, lerr := e.composeLocker.Acquire(lockCtx, svc.ComposeFile)
	lockCancel()
	if lerr != nil {
		logger := e.newOpLogger(op)
		logger.finish("failed", "获取 Compose 文件锁失败，已中止（未做修改）: "+lerr.Error())
		return
	}
	defer lock.Release()
	logger := e.newOpLogger(op)
	logger.add("info", fmt.Sprintf("开始恢复 %s 的原始端口映射", svc.ContainerName))
	composeFile := svc.ComposeFile
	workDir := filepath.Dir(composeFile)
	backupFile := ""
	if backupDir, ok := e.store.GetMeta("serviceBackup:" + svc.ID); ok {
		var meta struct {
			ComposeFile string `json:"composeFile"`
			WorkingDir  string `json:"workingDir"`
		}
		if raw, merr := os.ReadFile(filepath.Join(backupDir, "metadata.json")); merr == nil {
			if json.Unmarshal(raw, &meta) == nil {
				if meta.ComposeFile != "" {
					composeFile = meta.ComposeFile
				}
				if meta.WorkingDir != "" {
					workDir = meta.WorkingDir
				}
			}
		}
		backupFile = filepath.Join(backupDir, filepath.Base(composeFile))
	}
	managedContent, readErr := os.ReadFile(composeFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		logger.finish("failed", "读取 Compose 文件失败，已中止: "+readErr.Error())
		return
	}
	rebuildSvc := svc
	rebuildSvc.ComposeFile = composeFile
	detachSvc := svc
	detachSvc.ComposeFile = composeFile
	payload := detachPayload{OperationID: op.ID, Service: detachSvc, WorkingDir: workDir, Phase: "prepared"}
	if readErr == nil {
		payload.ManagedContent = managedContent
		expected, cerrs := compose.RestoreManagedBindings(managedContent, svc.ComposeService, bindingsFor(svc))
		if len(cerrs) > 0 {
			logger.finish("failed", "恢复端口冲突，已中止（未做修改）: "+joinErrors(cerrs))
			return
		}
		payload.ExpectedRestored = expected
	} else {
		if backupFile == "" {
			logger.finish("failed", "Compose 文件缺失且没有可用备份，已中止")
			return
		}
		rebuilt, rerr := e.rebuildMissingCompose(rebuildSvc, backupFile)
		if rerr != nil {
			logger.finish("failed", "Compose 文件缺失且无法安全重建，已中止: "+rerr.Error())
			return
		}
		payload.ExpectedRestored = rebuilt
		managed, _, rerr := compose.RemoveBindings(rebuilt, svc.ComposeService, bindingsFor(svc))
		if rerr != nil {
			logger.finish("failed", "无法从重建文件推导托管状态，已中止: "+rerr.Error())
			return
		}
		payload.ManagedContent = managed
		payload.Phase = "prepared_missing"
		logger.add("warn", "Compose 文件缺失，已先持久化重建与托管快照")
	}
	txnID, txnErr := e.startTxn(op.ID, "detach", payload)
	if txnErr != nil {
		logger.finish("failed", "无法写入事务日志，已中止（未做修改）: "+txnErr.Error())
		return
	}
	if err := e.restoreService(ctx, logger, svc); err != nil {
		status := "failed"
		var ae *attachError
		if errors.As(err, &ae) && ae.RollbackFailed {
			status = "rollback_failed"
			e.finishTxnAtomic(txnID, op.ID, "rollback_failed", err, "rollback_failed")
		} else {
			e.finishTxnAtomic(txnID, op.ID, "rolled_back", err, "rolled_back")
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
	if raw, rerr := os.ReadFile(composeFile); rerr == nil {
		if merr := e.markComposeLastKnownGood(composeFile, raw); merr != nil {
			logger.add("warn", "保存 Compose 最后可信快照失败: "+merr.Error())
		}
	}
	logger.add("info", "原始端口映射已恢复；OrbSleep 已停止管理该服务")
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.add("warn", "同步代理配置失败: "+err.Error())
	}
	e.finishTxnAtomic(txnID, op.ID, "committed", nil, "committed")
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

// rebuildMissingCompose reconstructs a deleted Compose file from this service's
// pre-attach backup while removing ports of other services that are still
// managed. Restoring the raw backup would otherwise re-publish their old ports
// and conflict with their gateway listeners.
func (e *Engine) rebuildMissingCompose(svc model.Service, backupFile string) ([]byte, error) {
	content, err := os.ReadFile(backupFile)
	if err != nil {
		if good, ok := e.composeLastKnownGood(svc.ComposeFile); ok {
			content = good
		} else {
			return nil, err
		}
	}
	services, err := e.store.ListServices()
	if err != nil {
		return nil, err
	}
	for _, other := range services {
		if other.ID == svc.ID || other.ComposeFile != svc.ComposeFile {
			continue
		}
		// Managed services must not republish TCP ports. Clear every TCP
		// declaration — including stale ones an old backup may contain — but
		// keep UDP mappings, which Docker still publishes directly.
		out, rerr := compose.ClearServiceTCPPorts(content, other.ComposeService)
		if rerr != nil {
			return nil, fmt.Errorf("移除共享服务 %s 的旧 TCP 端口失败: %w", other.ContainerName, rerr)
		}
		content = out
	}
	// Ensure this service's own ports are present (the backup may have been
	// taken before the container was last edited).
	out, cerrs := compose.RestoreManagedBindings(content, svc.ComposeService, bindingsFor(svc))
	if len(cerrs) > 0 {
		return nil, fmt.Errorf("补回本服务端口失败: %s", joinErrors(cerrs))
	}
	return out, nil
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
	failpoint.Hit("detach_gateway_removed")
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
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if gerr := e.ensureGatewayForService(rctx, svc.ID); gerr != nil {
			errs = append(errs, "重新监听端口失败: "+gerr.Error())
		}
		rcancel()
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
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		// A read failure is not proof the file is gone (permissions, I/O
		// errors). WriteAtomic would replace a file we could not even read,
		// so refuse and keep the managed state.
		return failWithRevert(fmt.Errorf("读取 Compose 文件失败，为避免覆盖用户文件已中止: %w", readErr))
	}
	if readErr != nil {
		if backupFile == "" {
			return failWithRevert(fmt.Errorf("Compose 文件不存在且没有备份，无法恢复: %w", readErr))
		}
		rebuildSvc := svc
		rebuildSvc.ComposeFile = composeFile
		rebuilt, rerr := e.rebuildMissingCompose(rebuildSvc, backupFile)
		if rerr != nil {
			return failWithRevert(fmt.Errorf("Compose 文件缺失且无法安全重建: %w", rerr))
		}
		mode := os.FileMode(0o644)
		if st, statErr := os.Stat(backupFile); statErr == nil {
			mode = st.Mode()
		}
		if err := compose.WriteAtomic(composeFile, rebuilt, mode); err != nil {
			return failWithRevert(fmt.Errorf("重建 Compose 文件失败: %w", err))
		}
		failpoint.Hit("detach_compose_written")
		logger.add("warn", "Compose 文件缺失，已按共享服务当前状态重建")
	} else {
		out, cerrs := compose.RestoreManagedBindings(current, svc.ComposeService, removals)
		if len(cerrs) > 0 {
			return failWithRevert(fmt.Errorf("无法安全地定点恢复端口（未覆盖原文件）: %s；请手动将 %s 加回 %s",
				joinErrors(cerrs), portsText(removals), composeFile))
		}
		if !bytes.Equal(out, current) {
			mode := os.FileMode(0o644)
			if st, statErr := os.Stat(composeFile); statErr == nil {
				mode = st.Mode()
			}
			if err := compose.WriteAtomic(composeFile, out, mode); err != nil {
				return failWithRevert(fmt.Errorf("写入 Compose 文件失败: %w", err))
			}
			failpoint.Hit("detach_compose_written")
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
	failpoint.Hit("detach_container_ready")
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
	failpoint.Hit("detach_before_record_delete")

	if err := deleteServiceRecord(e, svc.ID); err != nil {
		// The record must not outlive the managed wiring. Put the service back
		// into the verified managed state, or report rollback_failed.
		return failWithRevert(fmt.Errorf("删除托管记录失败: %w", err))
	}
	return nil
}

// deleteServiceRecord is a seam for fault-injection tests.
var deleteServiceRecord = func(e *Engine, id string) error {
	return e.store.DeleteService(id)
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
