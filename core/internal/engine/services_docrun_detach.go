package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	"orbsleep/internal/model"
)

// runDetachDocrun is the docrun branch of runDetach: restore/remove a service
// that was attached by recreating it from a full container snapshot. The
// writeMu is already held by runDetach; there is no Compose file to lock.
func (e *Engine) runDetachDocrun(ctx context.Context, svc model.Service, op model.Operation, remove bool, done func()) {
	logger := e.newOpLogger(op)
	logger.add("info", fmt.Sprintf("开始恢复 %s 的原始端口映射（快照重建路径）", svc.ContainerName))

	payload, _, err := e.runDetachDocrunPayload(op.ID, svc)
	if err != nil {
		logger.finish("failed", "读取容器快照失败，已中止（未做修改）: "+err.Error())
		return
	}
	txnID, txnErr := e.startTxn(op.ID, "detach", payload)
	if txnErr != nil {
		logger.finish("failed", "无法写入事务日志，已中止（未做修改）: "+txnErr.Error())
		return
	}
	if err := e.restoreDocrunService(ctx, logger, svc); err != nil {
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
	logger.add("info", "原始端口映射已恢复；OrbSleep 已停止管理该服务")
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.add("warn", "同步代理配置失败: "+err.Error())
	}
	e.finishTxnAtomic(txnID, op.ID, "committed", nil, "committed")
	logger.finish("succeeded", "")
	e.triggerReconcile()
}
