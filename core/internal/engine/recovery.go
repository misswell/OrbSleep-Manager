package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
	"orbsleep/internal/state"
)

// RecoverTransactions rolls back attach transactions that were interrupted by
// an abrupt process termination. It runs synchronously before the API starts.
func (e *Engine) RecoverTransactions() {
	e.recoveryMu.Lock()
	e.lastRecovery = time.Now()
	e.recoveryMu.Unlock()
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
			errs = append(errs, fmt.Sprintf("%s: 读取 Compose 失败: %v", it.ContainerName, rerr))
			continue
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
				errs = append(errs, fmt.Sprintf("%s: 恢复备份失败: %v", it.ContainerName, rerr))
				if hasRecord {
					e.recoverRevertToManaged(ctx, cli, it, logger, &errs)
				}
				continue
			}
			if !e.recoverVerifyOriginal(ctx, cli, it, logger, &errs) {
				if hasRecord {
					e.recoverRevertToManaged(ctx, cli, it, logger, &errs)
				}
				continue
			}
			if hasRecord {
				if derr := e.store.DeleteService(it.ServiceID); derr != nil {
					errs = append(errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
					e.recoverRevertToManaged(ctx, cli, it, logger, &errs)
					continue
				}
				_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
			}
			restored = append(restored, it.ContainerName)
			logger.add("info", it.ContainerName+" 已恢复到接入前状态")
		case bytes.Equal(current, it.Content):
			// Compose was never modified (or a previous rollback already
			// restored it). If a record survived, finish the original-state
			// restore and verify the ports.
			if hasRecord {
				if !e.recoverVerifyOriginal(ctx, cli, it, logger, &errs) {
					e.recoverRevertToManaged(ctx, cli, it, logger, &errs)
					continue
				}
				if derr := e.store.DeleteService(it.ServiceID); derr != nil {
					errs = append(errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
					e.recoverRevertToManaged(ctx, cli, it, logger, &errs)
					continue
				}
				_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
			}
			restored = append(restored, it.ContainerName)
		default:
			errs = append(errs, fmt.Sprintf("%s: Compose 文件在事务中断后被外部修改，未覆盖", it.ContainerName))
			if hasRecord {
				if gerr := e.ensureGatewayForService(ctx, it.ServiceID); gerr != nil {
					errs = append(errs, fmt.Sprintf("%s: 重新监听失败: %v", it.ContainerName, gerr))
				}
			}
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
	e.logger.Printf("warn", "恢复未完成的恢复操作: %s", p.Service.ContainerName)
	_, svcExists, _ := e.store.GetService(p.Service.ID)
	if !svcExists {
		// The record was already deleted: the detach completed.
		_ = e.syncProxyConfig(ctx, true)
		e.finishTxnAtomic(txn.ID, p.OperationID, "committed", nil, "committed")
		return
	}
	if _, err := e.dockerClient(ctx); err != nil {
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
	if err != nil {
		fail("读取 Compose 失败: " + err.Error())
		return
	}
	e.gw.RemoveService(p.Service.ID)
	if !bytes.Equal(current, p.ManagedContent) {
		if len(p.ManagedContent) == 0 {
			fail("缺少托管 Compose 快照")
			return
		}
		if len(p.ExpectedRestored) > 0 && !bytes.Equal(current, p.ExpectedRestored) {
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
		upctx, upcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		uperr := cli.UpService(upctx, p.Service.ComposeProject, p.Service.ComposeFile, filepath.Dir(p.Service.ComposeFile), p.Service.ComposeService)
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
	for _, it := range payload.Items {
		if it.ServiceID == "" || it.Phase != "committed_item" {
			return false
		}
		if _, ok, err := e.store.GetService(it.ServiceID); err != nil || !ok {
			return false
		}
		if len(it.ManagedContent) > 0 {
			raw, err := os.ReadFile(it.ComposeFile)
			if err != nil || !bytes.Equal(raw, it.ManagedContent) {
				return false
			}
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
