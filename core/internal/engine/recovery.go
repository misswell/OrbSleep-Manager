package engine

import (
	"bytes"
	"context"
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
		if txn.Kind != "attach" {
			_ = e.store.FinishTransaction(txn.ID, "rollback_failed", "未知事务类型，需人工处理", "rollback_failed")
			continue
		}
		e.recoverAttachTxn(txn)
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
		e.finishTxn(txn.ID, "committed", nil, "committed")
		_ = e.updateOriginalOperation(payload.OperationID, "succeeded", "", "恢复：事务已完成，仅为补记状态")
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
		current, rerr := os.ReadFile(it.ComposeFile)
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s: 读取 Compose 失败: %v", it.ContainerName, rerr))
			continue
		}
		switch {
		case len(it.ManagedContent) > 0 && bytes.Equal(current, it.ManagedContent):
			if rerr := compose.RestoreFile(it.ComposeFile, it.BackupPath); rerr != nil {
				errs = append(errs, fmt.Sprintf("%s: 恢复备份失败: %v", it.ContainerName, rerr))
				continue
			}
			upctx, upcancel := context.WithTimeout(context.Background(), 5*time.Minute)
			uperr := cli.UpService(upctx, it.ComposeProject, it.ComposeFile, it.WorkingDir, it.ComposeService)
			upcancel()
			if uperr != nil {
				errs = append(errs, fmt.Sprintf("%s: 重建失败: %v", it.ContainerName, uperr))
				continue
			}
			ins, werr := e.waitContainerRunning(ctx, it.ContainerName, 2*time.Minute)
			if werr != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", it.ContainerName, werr))
				continue
			}
			for _, r := range it.Removals {
				if !containerPublishes(ins, r) {
					errs = append(errs, fmt.Sprintf("%s: 原端口 %d 未重新发布", it.ContainerName, r.HostPort))
					werr = fmt.Errorf("port not published")
					break
				}
			}
			if werr != nil {
				continue
			}
			if hasRecord {
				if derr := e.store.DeleteService(it.ServiceID); derr != nil {
					errs = append(errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
					continue
				}
				_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
			}
			restored = append(restored, it.ContainerName)
			logger.add("info", it.ContainerName+" 已恢复到接入前状态")
		case bytes.Equal(current, it.Content):
			// Compose was never modified (or was already restored).
			if hasRecord {
				errs = append(errs, fmt.Sprintf("%s: 托管记录存在但 Compose 已恢复，需人工确认", it.ContainerName))
				continue
			}
			restored = append(restored, it.ContainerName)
		default:
			errs = append(errs, fmt.Sprintf("%s: Compose 文件在事务中断后被外部修改，未覆盖", it.ContainerName))
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
		e.finishTxn(txn.ID, "rollback_failed", errors.New(msg), "rollback_failed")
		_ = e.updateOriginalOperation(payload.OperationID, "rollback_failed", msg, "启动恢复不完整")
		e.setLastError("检测到未完成的接入事务，恢复不完整: " + msg)
		return
	}
	logger.add("info", "未完成事务已回滚，原端口恢复")
	logger.finish("succeeded", "")
	e.finishTxn(txn.ID, "rolled_back", nil, "rolled_back")
	_ = e.updateOriginalOperation(payload.OperationID, "rolled_back", "", "启动时自动回滚未完成事务")
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
