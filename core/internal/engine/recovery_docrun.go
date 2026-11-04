package engine

import (
	"context"
	"fmt"
	"time"

	"orbsleep/internal/deployment/docrun"
	"orbsleep/internal/dockerx"
)

// isContainerNotFound accepts ONLY a structured 404 from the engine: any
// other failure (timeout, 5xx, socket down — even one whose body mentions
// "no such") must keep the transaction alive instead of entering a
// destructive branch.
func isContainerNotFound(err error) bool {
	return dockerx.IsErrNotFound(err)
}

// recoverDocrunAttachItem rolls back one docrun attach item after a crash.
// The durable state lives in the transaction payload (full recreation specs),
// and the actual container state is re-derived by inspection: the strategy
// sequence is rename/create based, so every intermediate state is recognisable.
// Returns true when the item is back at its pre-attach state.
func (e *Engine) recoverDocrunAttachItem(ctx context.Context, txnID string, it txnItem, logger *opLogger, errs *[]string) bool {
	if it.Docrun == nil {
		// Very old interrupted txn (or payload corruption): nothing safe to do
		// beyond removing a possibly orphaned record.
		if it.ServiceID != "" {
			_ = e.store.DeleteService(it.ServiceID)
		}
		*errs = append(*errs, it.ContainerName+": 事务缺少重建快照，已移除托管记录（容器未被修改过则无影响）")
		return false
	}
	prep := it.Docrun
	eng, err := e.docrunEngineForServiceByID(it.EnvID)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 容器运行时不可用: %v", it.ContainerName, err))
		return false
	}
	hasRecord := false
	if it.ServiceID != "" {
		if _, ok, _ := e.store.GetService(it.ServiceID); ok {
			hasRecord = true
		}
	}
	// Gateway listeners from a partially committed attach must not survive a
	// rollback: remove them up front (recovery re-ensures them if the managed
	// state turns out to be the right outcome below).
	if it.ServiceID != "" {
		e.gw.RemoveService(it.ServiceID)
	}
	bctx, bcancel := context.WithTimeout(ctx, 10*time.Second)
	_, backupErr := eng.InspectContainer(bctx, prep.BackupName)
	bcancel()
	// Triage: only a definitive NotFound means "no anchor". An API outage
	// (socket down, timeout, 5xx) leaves the state UNKNOWN — destructive
	// recovery must not run on an unreadable board; keep the txn for a retry.
	switch {
	case backupErr == nil:
		// anchor present
	case isContainerNotFound(backupErr):
		// anchor absent
	default:
		*errs = append(*errs, fmt.Sprintf("%s: 无法读取回滚锚点状态: %v（事务保留，等待重试）", it.ContainerName, backupErr))
		return false
	}
	backupExists := backupErr == nil

	switch {
	case backupExists:
		// Crash between rename and verified replacement: roll back to the
		// original. The replacement (if create already happened) sits under
		// the ORIGINAL name; its id may never have been persisted, so locate
		// it by our management label before removing it — otherwise the
		// rename-back would collide with the living replacement.
		replacementID := it.ReplacementID
		if ins, ierr := eng.InspectContainer(ctx, prep.OriginalName); ierr == nil {
			// Strict identity: BOTH labels must match exactly. The service id
			// is a random per-attach value an external container cannot guess,
			// so labels alone prove ownership here. The config fingerprint is
			// deliberately NOT compared: a created-but-not-started container's
			// inspect can differ from the create spec in image-default fields,
			// which would make recovery refuse valid replacements.
			if it.ServiceID != "" &&
				ins.Config.Labels[docrun.LabelServiceID] == it.ServiceID &&
				ins.Config.Labels[docrun.LabelManaged] == "true" {
				replacementID = ins.ID
			} else if replacementID == "" {
				// Not ours: refuse to touch it; keep the txn for manual care.
				*errs = append(*errs, fmt.Sprintf("%s: 名称下的容器身份不符，拒绝回滚删除（需人工核对）", it.ContainerName))
				return false
			}
		}
		if rerr := docrun.RollbackManaged(ctx, eng, prep, replacementID); rerr != nil {
			*errs = append(*errs, fmt.Sprintf("%s: 回滚容器重建失败: %v", it.ContainerName, rerr))
			return false
		}
		if hasRecord {
			if derr := deleteServiceRecord(e, it.ServiceID); derr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
				return false
			}
			_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
		}
		logger.add("info", it.ContainerName+" 已恢复到接入前状态（原容器已还原）")
		return true

	default:
		// No parked backup: either nothing happened yet, or the replacement is
		// already in place.
		ins, err := eng.InspectContainer(ctx, prep.OriginalName)
		if err != nil {
			if !isContainerNotFound(err) {
				// Unknown container state: never run destructive recovery on
				// an unreadable board.
				*errs = append(*errs, fmt.Sprintf("%s: 无法读取容器状态: %v（事务保留，等待重试）", it.ContainerName, err))
				return false
			}
			// Container gone entirely. If a record survived, the managed state
			// can be rebuilt from the snapshot; otherwise nothing to do.
			if hasRecord && it.Phase == "committed_item" {
				if merr := e.recoverDocrunRebuildManaged(ctx, eng, it, errs); merr == nil {
					logger.add("warn", it.ContainerName+" 容器缺失，已按快照重建托管容器")
					return false
				}
			}
			if it.ServiceID != "" {
				_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
				_ = e.store.DeleteService(it.ServiceID)
			}
			logger.add("info", it.ContainerName+" 容器不存在，未做容器级修改")
			return true
		}
		if ins.Config.Labels[docrun.LabelServiceID] == it.ServiceID || ins.Config.Labels[docrun.LabelManaged] == "true" {
			// The replacement is running under the original name.
			if it.Phase == "committed_item" && hasRecord {
				// Effectively committed; finish the bookkeeping: ensure the
				// gateway listens, drop a leftover anchor by its recorded ID
				// (post-commit cleanup may not have run before the crash),
				// and mark the txn committed contribution.
				if gerr := e.ensureGatewayForService(ctx, it.ServiceID); gerr != nil {
					*errs = append(*errs, fmt.Sprintf("%s: 恢复端口监听失败: %v", it.ContainerName, gerr))
					return false
				}
				if rerr := eng.RemoveContainer(ctx, prep.OriginalID, true); rerr != nil && !isContainerNotFound(rerr) {
					logger.add("warn", it.ContainerName+" 清理残留锚点失败（可手动删除）: "+rerr.Error())
				}
				logger.add("info", it.ContainerName+" 接入已完成，补记提交")
				return true
			}
			// Uncommitted replacement: restore the original from the snapshot
			// (park the replacement, create the original, verify). The
			// replacement's verified ID guards against a swap mid-recovery.
			snap := &docrun.ManagedSnapshot{
				ServiceID:    it.ServiceID,
				OriginalName: prep.OriginalName,
				OriginalID:   it.ReplacementID,
				OriginalSpec: prep.OriginalSpec,
				ManagedSpec:  prep.ManagedSpec,
				Secondary:    prep.Secondary,
			}
			if _, rollback, rerr := docrun.RestoreOriginal(ctx, eng, snap, ins.ID); rerr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 恢复原容器失败: %v", it.ContainerName, rerr))
				return false
			} else {
				_ = rollback
			}
			if ins2, werr := docrun.InspectRunning(ctx, eng, prep.OriginalName, 2*time.Minute); werr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 原容器未就绪: %v", it.ContainerName, werr))
				return false
			} else {
				for _, r := range it.Removals {
					if !containerPublishes(ins2, r) {
						*errs = append(*errs, fmt.Sprintf("%s: 原端口 %d 未重新发布", it.ContainerName, r.HostPort))
						return false
					}
				}
			}
			// Drop the parked replacement BY ID (captured before the restore);
			// a name-based removal here could hit an unrelated container.
			if rerr := eng.RemoveContainer(ctx, ins.ID, true); rerr != nil && !isContainerNotFound(rerr) {
				logger.add("warn", it.ContainerName+" 清理停用的替换容器失败（可手动删除）: "+rerr.Error())
			}
			if hasRecord {
				if derr := deleteServiceRecord(e, it.ServiceID); derr != nil {
					*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
					return false
				}
				_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
			}
			logger.add("info", it.ContainerName+" 已恢复到接入前状态（替换容器已撤销）")
			return true
		}
		// The container under the original name is the ORIGINAL (untouched or
		// already rolled back). Only the proxy registration may need undoing
		// (handled by the caller via the baseline) and a stale record removed.
		if hasRecord {
			// The original serves its own ports again: the record is stale.
			if derr := deleteServiceRecord(e, it.ServiceID); derr != nil {
				*errs = append(*errs, fmt.Sprintf("%s: 删除托管记录失败: %v", it.ContainerName, derr))
				return false
			}
			_ = e.store.DeleteMeta("serviceBackup:" + it.ServiceID)
		}
		logger.add("info", it.ContainerName+" 未被替换（或已还原），仅撤销其代理注册")
		return true
	}
}

// recoverDocrunRebuildManaged recreates the managed replacement for a committed
// docrun item whose container disappeared.
func (e *Engine) recoverDocrunRebuildManaged(ctx context.Context, eng docrun.Engine, it txnItem, errs *[]string) error {
	if it.Docrun == nil {
		return fmt.Errorf("缺少快照")
	}
	id, err := docrun.CreateAndStart(ctx, eng, it.Docrun.ManagedSpec)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 重建托管容器失败: %v", it.ContainerName, err))
		return err
	}
	_ = e.store.SetServiceStatus(it.ServiceID, "managed_running", "", id)
	if gerr := e.ensureGatewayForService(ctx, it.ServiceID); gerr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: 恢复端口监听失败: %v", it.ContainerName, gerr))
		return gerr
	}
	return nil
}

// docrunEngineForServiceByID resolves a strategy engine for an env id. The
// unified runtime.Runtime satisfies docrun.Engine.
func (e *Engine) docrunEngineForServiceByID(envID string) (docrun.Engine, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.runtimeForEnv(ctx, envID)
}
