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
	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/state"
)

// RecoverTransactions rolls back attach transactions that were interrupted by
// an abrupt process termination. It runs synchronously before the API starts.
// RecoverTransaction retries exactly one interrupted transaction.
func (e *Engine) RecoverTransaction(id string) error {
	// One recovery coordinator lock for single and batch recovery: two
	// concurrent recoveries could otherwise edit the same Compose file, proxy
	// config and transaction rows at once.
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	txn, ok, err := e.store.GetTransaction(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("事务 %s 不存在", id)
	}
	if txn.Status != "running" && txn.Status != "rollback_failed" && txn.Status != "needs_repair" {
		return fmt.Errorf("事务 %s 已是终态 %s", id, txn.Status)
	}
	e.lastRecovery = time.Now()
	switch txn.Kind {
	case "attach":
		e.recoverAttachTxn(txn)
	case "detach":
		e.recoverDetachTxn(txn)
	default:
		return fmt.Errorf("未知事务类型 %s", txn.Kind)
	}
	return nil
}

func (e *Engine) RecoverTransactions() {
	// Hold the coordinator lock for the whole batch so startup recovery, the
	// reconcile loop and a GUI-triggered resolve can never run concurrently.
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	e.lastRecovery = time.Now()
	txns, err := e.store.ListRunningTransactions()
	if err != nil {
		e.logger.Printf("warn", "读取未完成事务失败: %v", err)
		return
	}
	if len(txns) == 0 {
		return
	}
	e.logger.Printf("warn", "发现 %d 个未完成事务，尝试恢复", len(txns))
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := e.dockerClient(dctx); err != nil {
		dcancel()
		e.logger.Printf("error", "无法恢复未完成事务：Docker 不可用: %v（事务保留，稍后重试）", err)
		return
	}
	dcancel()
	if _, err := e.cliRunner(); err != nil {
		e.logger.Printf("error", "无法恢复未完成事务：docker CLI 不可用: %v（事务保留，稍后重试）", err)
		return
	}
	for _, txn := range txns {
		switch txn.Kind {
		case "attach":
			e.recoverAttachTxn(txn)
		case "detach":
			e.recoverDetachTxn(txn)
		default:
			_ = e.store.FinishTransaction(txn.ID, "rollback_failed", "未知事务类型，需人工处理", "rollback_failed")
		}
	}
}

func (e *Engine) recoverAttachTxn(txn state.Transaction) {
	_, payload, err := e.loadTxn(txn.ID)
	if err != nil {
		_ = e.store.FinishTransaction(txn.ID, "rollback_failed", "事务数据损坏: "+err.Error(), "rollback_failed")
		e.logger.Printf("error", "事务 %s 数据损坏: %v", txn.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Serialize against attach/detach, which hold writeMu for their whole run.
	// Without this a recovery could edit a Compose file an in-flight attach is
	// also editing. Lock order: recoveryMu → writeMu → per-file Compose lock.
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	// If every item reached committed_item before the crash, the transaction is
	// effectively committed; only the terminal marker was missing.
	if e.txnAllCommitted(ctx, payload) {
		e.finishTxnAtomic(txn.ID, payload.OperationID, "committed", nil, "committed")
		e.logger.Printf("info", "事务 %s 已完成，补记为 committed", txn.ID)
		return
	}

	op := model.Operation{
		ID:        newID("op"),
		Kind:      "recovery",
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: txnBackupDir(payload),
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		e.logger.Printf("error", "无法创建恢复操作记录: %v", err)
		return
	}
	logger := e.newOpLogger(op)
	logger.add("warn", fmt.Sprintf("恢复未完成事务 %s（%s）", txn.ID, txn.Kind))

	admin, err := e.adminClient()
	if err != nil {
		// Keep the transaction running so a later reconcile can retry once the
		// proxy is available again.
		logger.finish("failed", "代理管理接口不可用，暂不自动恢复（事务保留）: "+err.Error())
		e.setLastError("存在未完成事务；代理不可用，暂未自动恢复")
		return
	}
	if _, err := admin.GetConfig(ctx); err != nil {
		logger.finish("failed", "读取代理配置失败，暂不自动恢复（事务保留）: "+err.Error())
		e.setLastError("存在未完成事务；代理不可读，暂未自动恢复")
		return
	}
	cli, err := e.cliRunner()
	if err != nil {
		logger.finish("failed", "docker CLI 不可用，暂不自动恢复（事务保留）: "+err.Error())
		return
	}

	var errs []string
	var restored []string
	for i := len(payload.Items) - 1; i >= 0; i-- {
		it := payload.Items[i]
		if it.BackupPath == "" {
			continue
		}
		if e.recoverAttachItem(ctx, cli, txn.ID, it, logger, &errs) {
			restored = append(restored, it.ContainerName)
		}
	}

	if len(restored) > 0 {
		cur, cerr := admin.GetConfig(ctx)
		if cerr != nil {
			errs = append(errs, "读取代理配置失败: "+cerr.Error())
		} else {
			for _, name := range restored {
				cur.Remove(name)
				if original, ok := payload.Baseline.Service(name); ok {
					cur.Upsert(original)
				}
			}
			pctx, pcancel := context.WithTimeout(context.Background(), 60*time.Second)
			perr := e.putProxyConfigVerified(pctx, admin, cur)
			pcancel()
			if perr != nil {
				errs = append(errs, "恢复代理配置失败: "+perr.Error())
			}
		}
	}

	if len(errs) > 0 {
		msg := fmt.Sprintf("恢复不完整：%v；备份保留在 %s", errs, txnBackupDir(payload))
		logger.finish("rollback_failed", msg)
		e.finishTxnAtomic(txn.ID, payload.OperationID, "rollback_failed", errors.New(msg), "rollback_failed")
		e.setLastError("检测到未完成的接入事务，恢复不完整: " + msg)
		return
	}
	logger.add("info", "未完成事务已回滚，原端口恢复")
	logger.finish("succeeded", "")
	e.finishTxnAtomic(txn.ID, payload.OperationID, "rolled_back", nil, "rolled_back")
}

// recoverAttachItem rolls back one attach-transaction item. It returns true
// when the item is back at its pre-attach state. The per-file Compose lock is
// held for the whole item so a concurrent attach/detach on the same file
// serializes against this recovery.
func (e *Engine) recoverAttachItem(ctx context.Context, cli composeCLI, txnID string, it txnItem, logger *opLogger, errs *[]string) bool {
	lockCtx, lockCancel := context.WithTimeout(ctx, 60*time.Second)
	lock, lerr := e.composeLocker.Acquire(lockCtx, it.ComposeFile)
	lockCancel()
	if lerr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 获取 Compose 文件锁失败: %v", it.ContainerName, lerr))
		return false
	}
	defer lock.Release()

	hasRecord := false
	if it.ServiceID != "" {
		if _, ok, _ := e.store.GetService(it.ServiceID); ok {
			hasRecord = true
		}
	}
	// A previous failed recovery attempt may have left reconcile-created
	// gateway listeners on the ports we are about to restore.
	if it.ServiceID != "" {
		e.gw.RemoveService(it.ServiceID)
	}
	current, rerr := os.ReadFile(it.ComposeFile)
	if rerr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 读取 Compose 失败: %v", it.ContainerName, rerr))
		return false
	}
	expectedManaged := it.ManagedContent
	if len(expectedManaged) == 0 {
		// Crash after the atomic rename but before ManagedContent was
		// persisted: the pending bytes are the new managed content.
		expectedManaged = it.PendingContent
	}
	switch {
	case len(expectedManaged) > 0 && bytes.Equal(current, expectedManaged):
		if rerr := compose.RestoreFile(it.ComposeFile, it.BackupPath); rerr != nil {
			*errs = append(*errs, fmt.Sprintf("%s: 恢复备份失败: %v", it.ContainerName, rerr))
			if hasRecord {
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
			}
			return false
		}
		if !e.recoverVerifyOriginal(ctx, cli, it, logger, errs) {
			if hasRecord {
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
			}
			return false
		}
		if hasRecord {
			if derr := e.store.DeleteService(it.ServiceID); derr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
				return false
			}
			_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
		}
		logger.add("info", it.ContainerName+" 已恢复到接入前状态")
		return true
	case bytes.Equal(current, it.Content):
		// Compose was never modified (or a previous rollback already
		// restored it). If a record survived, finish the original-state
		// restore and verify the ports.
		if hasRecord {
			if !e.recoverVerifyOriginal(ctx, cli, it, logger, errs) {
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
				return false
			}
			if derr := e.store.DeleteService(it.ServiceID); derr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
				return false
			}
			_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
		}
		return true
	case len(expectedManaged) == 0:
		// Crash before this item wrote the Compose file: no managed or
		// pending snapshot was ever persisted. Items sharing one Compose
		// file must not be judged against plan-time Content — earlier
		// items may already have committed their edits. Only this item's
		// proxy registration needs undoing.
		if hasRecord {
			if !e.recoverVerifyOriginal(ctx, cli, it, logger, errs) {
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
				return false
			}
			if derr := e.store.DeleteService(it.ServiceID); derr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
				e.recoverRevertToManaged(ctx, cli, it, logger, errs)
				return false
			}
			_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
		}
		logger.add("info", it.ContainerName+" 未修改过 Compose 文件，仅撤销其代理注册")
		return true
	default:
		*errs = append(*errs, fmt.Sprintf("%s: Compose 文件在事务中断后被外部修改，未覆盖", it.ContainerName))
		if hasRecord {
			if gerr := e.ensureGatewayForService(ctx, it.ServiceID); gerr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 重新监听失败: %v", it.ContainerName, gerr))
			}
			return false
		}
		// No service record survived the crash, but the interrupted attach may
		// already have removed the container's published ports. Without a
		// repair route the workload is unreachable even though the proxy
		// mapping is still in place. Rebuild availability from the payload.
		if gerr := e.ensureGatewayRoutesForTxnItem(ctx, txnID, it); gerr != nil {
			*errs = append(*errs, fmt.Sprintf("%s: 临时恢复监听失败: %v", it.ContainerName, gerr))
		} else {
			logger.add("warn", it.ContainerName+" 已按事务记录重建临时端口监听（服务记录缺失，接入已撤销）")
		}
		return false
	}
}

// ensureGatewayRoutesForTxnItem installs host gateway listeners for an attach
// item whose service record is gone: for every original binding the container
// no longer publishes itself, route 127.0.0.1:host → proxy.
func (e *Engine) ensureGatewayRoutesForTxnItem(ctx context.Context, txnID string, it txnItem) error {
	s := e.settings.Get()
	ins, err := e.inspectContainerBestEffort(ctx, it.ContainerName)
	if err != nil {
		return err
	}
	proxyIP := ""
	if s.ProxyDialMode == "container_ip" {
		proxyIP = e.proxyContainerIP(ctx)
	}
	routeID := it.ServiceID
	if routeID == "" {
		routeID = "recover-" + txnID
	}
	var lastErr error
	for _, b := range it.Bindings {
		pb := model.PortBinding{HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol}
		if ins != nil && containerPublishes(ins, pb) {
			// The container serves the host port itself again; adding a
			// gateway listener would conflict.
			continue
		}
		route, rerr := gatewayRouteFor(routeID, it.ContainerName, b, s, proxyIP)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if err := e.gw.Ensure(route); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// inspectContainerBestEffort inspects a container by name; a missing container
// returns (nil, nil) so callers can distinguish "gone" from "query failed".
func (e *Engine) inspectContainerBestEffort(ctx context.Context, name string) (*dockerx.ContainerInspect, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	ins, err := c.InspectContainer(ctx, name)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such") {
			return nil, nil
		}
		return nil, err
	}
	return ins, nil
}

// recoverDetachTxn restores the managed state for an interrupted restore/remove
// operation, because the managed record still exists.
func (e *Engine) recoverDetachTxn(txn state.Transaction) {
	var p detachPayload
	if err := json.Unmarshal([]byte(txn.Data), &p); err != nil {
		_ = e.store.FinishTransaction(txn.ID, "rollback_failed", "事务数据损坏: "+err.Error(), "rollback_failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Same coordination as attach recovery: recoveryMu is already held by the
	// caller; writeMu serializes against attach/detach, the per-file lock
	// against anything else touching this Compose file.
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	lockCtx, lockCancel := context.WithTimeout(ctx, 60*time.Second)
	lock, lerr := e.composeLocker.Acquire(lockCtx, p.Service.ComposeFile)
	lockCancel()
	if lerr != nil {
		_ = e.store.FinishTransaction(txn.ID, "rollback_failed", "获取 Compose 文件锁失败: "+lerr.Error(), "rollback_failed")
		e.setLastError("恢复操作未完成：获取 Compose 文件锁失败（事务保留，可重试）")
		return
	}
	defer lock.Release()
	e.logger.Printf("warn", "恢复未完成的恢复操作: %s", p.Service.ContainerName)
	_, svcExists, _ := e.store.GetService(p.Service.ID)
	if !svcExists {
		// The record was already deleted: the detach completed.
		_ = e.syncProxyConfig(ctx, true)
		e.finishTxnAtomic(txn.ID, p.OperationID, "committed", nil, "committed")
		return
	}
	c, err := e.dockerClient(ctx)
	if err != nil {
		e.logger.Printf("error", "未完成恢复操作：Docker 不可用: %v（事务保留）", err)
		return
	}
	cli, err := e.cliRunner()
	if err != nil {
		e.logger.Printf("error", "未完成恢复操作：docker CLI 不可用: %v（事务保留）", err)
		return
	}
	fail := func(msg string) {
		e.logger.Printf("error", "恢复操作回滚不完整: %s", msg)
		// Best effort: keep the managed service reachable if its listener was
		// already released. A port conflict is reported and left for the user.
		if gerr := e.ensureGatewayForService(ctx, p.Service.ID); gerr != nil {
			e.logger.Printf("warn", "重新建立托管监听失败: %v", gerr)
		}
		e.finishTxnAtomic(txn.ID, p.OperationID, "rollback_failed", errors.New(msg), "rollback_failed")
		e.setLastError("恢复操作回滚不完整: " + msg)
	}
	current, err := os.ReadFile(p.Service.ComposeFile)
	rebuilt := false
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fail("读取 Compose 失败: " + err.Error())
			return
		}
		// The Compose file is gone (deleted during the interrupted detach, or
		// already missing when it started). Rebuild the managed content: the
		// managed record still exists and the service must stay managed.
		managed, merr := e.rebuildManagedCompose(p.Service, p.ManagedContent)
		if merr != nil {
			fail("Compose 文件缺失且无法安全重建托管内容: " + merr.Error())
			return
		}
		if werr := compose.WriteAtomic(p.Service.ComposeFile, managed, 0o644); werr != nil {
			fail("恢复托管 Compose 失败: " + werr.Error())
			return
		}
		e.logger.Printf("warn", "Compose 文件缺失，已重建托管内容: %s", p.Service.ComposeFile)
		current = managed
		rebuilt = true
	}
	_ = rebuilt
	e.gw.RemoveService(p.Service.ID)
	// External-modification protection applies only to a file that already
	// exists: a rebuilt file is, by construction, the managed content.
	if !rebuilt && !bytes.Equal(current, p.ManagedContent) {
		if len(p.ManagedContent) == 0 {
			fail("缺少托管 Compose 快照")
			return
		}
		if len(p.ExpectedRestored) == 0 {
			fail("缺少预期恢复快照，无法区分中断状态与外部修改，未覆盖")
			return
		}
		if !bytes.Equal(current, p.ExpectedRestored) {
			fail("Compose 文件在恢复操作中断后被外部修改，未覆盖")
			return
		}
		mode := os.FileMode(0o644)
		if st, serr := os.Stat(p.Service.ComposeFile); serr == nil {
			mode = st.Mode()
		}
		if werr := compose.WriteAtomic(p.Service.ComposeFile, p.ManagedContent, mode); werr != nil {
			fail("恢复托管 Compose 失败: " + werr.Error())
			return
		}
	}
	// The Compose file now carries the managed content, but the container may
	// still be in its pre-manage state (ports published) or gone entirely when
	// the crash happened before UpService. Decide from the real container
	// state, not from whether the file was rewritten.
	needUp, derr := e.detachRecoveryNeedsUp(ctx, c, p.Service)
	if derr != nil {
		fail("检查容器状态失败: " + derr.Error())
		return
	}
	if needUp {
		workDir := p.WorkingDir
		if workDir == "" {
			workDir = filepath.Dir(p.Service.ComposeFile)
		}
		upctx, upcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		uperr := cli.UpService(upctx, p.Service.ComposeProject, p.Service.ComposeFile, workDir, p.Service.ComposeService)
		upcancel()
		if uperr != nil {
			fail("重建托管容器失败: " + uperr.Error())
			return
		}
		if _, werr := e.waitContainerRunning(ctx, p.Service.ContainerName, 2*time.Minute); werr != nil {
			fail("托管容器未就绪: " + werr.Error())
			return
		}
	}
	if err := e.ensureGatewayForService(ctx, p.Service.ID); err != nil {
		fail("恢复端口监听失败: " + err.Error())
		return
	}
	if err := e.syncProxyConfig(ctx, true); err != nil {
		e.logger.Printf("warn", "恢复未完成恢复操作后同步代理失败（reconcile 会重试）: %v", err)
	}
	e.finishTxnAtomic(txn.ID, p.OperationID, "rolled_back", nil, "rolled_back")
	e.logger.Printf("info", "未完成的恢复操作已回退到托管状态: %s", p.Service.ContainerName)
}

// detachRecoveryNeedsUp reports whether the container must be (re)created from
// the managed Compose content: it is missing, or it still publishes one of the
// service's original host ports (pre-manage state left by the interruption).
func (e *Engine) detachRecoveryNeedsUp(ctx context.Context, c *dockerx.Client, svc model.Service) (bool, error) {
	ins, err := c.InspectContainer(ctx, svc.ContainerName)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such") {
			return true, nil // container gone: recreate it managed
		}
		return false, err
	}
	for _, b := range svc.Bindings {
		if containerPublishes(ins, model.PortBinding{
			HostPort:      b.HostPort,
			ContainerPort: b.ContainerPort,
			Protocol:      b.Protocol,
		}) {
			return true, nil
		}
	}
	return false, nil
}

// rebuildManagedCompose reconstructs managed Compose content for a service
// whose Compose file is gone. The stored managed snapshot wins; otherwise the
// pre-attach backup is rebuilt (with other managed services' ports cleared)
// and this service's own ports removed again.
func (e *Engine) rebuildManagedCompose(svc model.Service, managedContent []byte) ([]byte, error) {
	if len(managedContent) > 0 {
		return managedContent, nil
	}
	if good, ok := e.composeLastKnownGood(svc.ComposeFile); ok {
		return good, nil
	}
	backupDir, ok := e.store.GetMeta("serviceBackup:" + svc.ID)
	if !ok {
		return nil, errors.New("没有服务备份，无法重建")
	}
	composeFile := svc.ComposeFile
	if raw, merr := os.ReadFile(filepath.Join(backupDir, "metadata.json")); merr == nil {
		var meta struct {
			ComposeFile string `json:"composeFile"`
		}
		if json.Unmarshal(raw, &meta) == nil && meta.ComposeFile != "" {
			composeFile = meta.ComposeFile
		}
	}
	restored, err := e.rebuildMissingCompose(svc, filepath.Join(backupDir, filepath.Base(composeFile)))
	if err != nil {
		return nil, err
	}
	managed, _, err := compose.RemoveBindings(restored, svc.ComposeService, bindingsFor(svc))
	if err != nil {
		return nil, fmt.Errorf("从重建内容推导托管内容失败: %w", err)
	}
	return managed, nil
}

// recoverVerifyOriginal starts the service from its original Compose content
// and verifies that every removed port is published again.
func (e *Engine) recoverVerifyOriginal(ctx context.Context, cli composeCLI, it txnItem, logger *opLogger, errs *[]string) bool {
	upctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	uperr := cli.UpService(upctx, it.ComposeProject, it.ComposeFile, it.WorkingDir, it.ComposeService)
	cancel()
	if uperr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 重建失败: %v", it.ContainerName, uperr))
		return false
	}
	ins, werr := e.waitContainerRunning(ctx, it.ContainerName, 2*time.Minute)
	if werr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", it.ContainerName, werr))
		return false
	}
	for _, r := range it.Removals {
		if !containerPublishes(ins, r) {
			*errs = append(*errs, fmt.Sprintf("%s: 原端口 %d 未重新发布", it.ContainerName, r.HostPort))
			return false
		}
	}
	return true
}

// recoverRevertToManaged restores the managed Compose content, recreates the
// container and re-installs the gateway so a failed recovery leaves a
// consistent managed service instead of an unreachable one.
func (e *Engine) recoverRevertToManaged(ctx context.Context, cli composeCLI, it txnItem, logger *opLogger, errs *[]string) {
	managed := it.ManagedContent
	if len(managed) == 0 {
		managed = it.PendingContent
	}
	if len(managed) == 0 || it.ServiceID == "" {
		return
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(it.ComposeFile); err == nil {
		mode = st.Mode()
	}
	if err := compose.WriteAtomic(it.ComposeFile, managed, mode); err != nil {
		*errs = append(*errs, it.ContainerName+": 回退托管 Compose 失败: "+err.Error())
		return
	}
	upctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	uperr := cli.UpService(upctx, it.ComposeProject, it.ComposeFile, it.WorkingDir, it.ComposeService)
	cancel()
	if uperr != nil {
		*errs = append(*errs, it.ContainerName+": 回退托管重建失败: "+uperr.Error())
		return
	}
	if _, err := e.waitContainerRunning(ctx, it.ContainerName, 2*time.Minute); err != nil {
		*errs = append(*errs, it.ContainerName+": 回退托管容器未就绪: "+err.Error())
		return
	}
	if err := e.ensureGatewayForService(ctx, it.ServiceID); err != nil {
		*errs = append(*errs, it.ContainerName+": 回退托管端口监听失败: "+err.Error())
		return
	}
	logger.add("warn", it.ContainerName+" 已回退到托管状态")
}

// ReconcileOrphanOperations repairs operation records left "running" by an
// abrupt exit when their transaction already reached a terminal state (or when
// no transaction was ever created, e.g. proxy setup).
func (e *Engine) ReconcileOrphanOperations() {
	ops, err := e.store.ListRunningOperations()
	if err != nil || len(ops) == 0 {
		return
	}
	txns, err := e.store.ListTransactions()
	if err != nil {
		return
	}
	byOp := map[string]state.Transaction{}
	for _, t := range txns {
		if t.OperationID != "" {
			byOp[t.OperationID] = t
		}
	}
	for _, op := range ops {
		status := ""
		msg := ""
		if t, ok := byOp[op.ID]; ok {
			switch t.Status {
			case "committed":
				status, msg = "succeeded", ""
			case "rolled_back":
				status, msg = "rolled_back", t.Error
			case "rollback_failed":
				status, msg = "rollback_failed", t.Error
			default:
				continue
			}
		} else {
			status = "failed"
			msg = "后台助手异常退出，操作未完成"
		}
		op.Status = status
		op.Error = msg
		op.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		op.Log = append(op.Log, model.LogEntry{At: op.FinishedAt, Level: "warn", Message: "启动时修正未完成的操作记录: " + msg})
		if err := e.store.UpdateOperation(op); err != nil {
			e.logger.Printf("warn", "修正操作记录失败 %s: %v", op.ID, err)
		}
	}
}

func (e *Engine) txnAllCommitted(ctx context.Context, payload txnPayload) bool {
	if len(payload.Items) == 0 {
		return false
	}
	_ = ctx
	// Items may share one Compose file. Each item stores the file content at its
	// own commit point, so only the last item per file is the final expectation;
	// comparing every intermediate snapshot would force a false rollback.
	expectedByFile := map[string][]byte{}
	for _, it := range payload.Items {
		if it.ServiceID == "" || it.Phase != "committed_item" {
			return false
		}
		if _, ok, err := e.store.GetService(it.ServiceID); err != nil || !ok {
			return false
		}
		expected := it.ManagedContent
		if len(expected) == 0 {
			expected = it.PendingContent
		}
		if len(expected) > 0 {
			expectedByFile[it.ComposeFile] = expected
		}
	}
	for file, expected := range expectedByFile {
		raw, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(raw, expected) {
			return false
		}
	}
	return true
}

func (e *Engine) updateOriginalOperation(opID, status, cause, logMsg string) error {
	if opID == "" {
		return nil
	}
	op, ok, err := e.store.GetOperation(opID)
	if err != nil || !ok {
		return err
	}
	if op.Status == "running" || status == "rollback_failed" {
		op.Status = status
		op.Error = cause
		op.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if logMsg != "" {
		op.Log = append(op.Log, model.LogEntry{At: time.Now().UTC().Format(time.RFC3339), Level: "warn", Message: logMsg})
	}
	return e.store.UpdateOperation(op)
}

func txnBackupDir(payload txnPayload) string {
	for _, it := range payload.Items {
		if it.BackupPath != "" {
			return filepath.Dir(it.BackupPath)
		}
	}
	return ""
}
