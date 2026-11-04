package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"orbsleep/internal/deployment/docrun"
	"orbsleep/internal/failpoint"
	"orbsleep/internal/model"
)

// loadDocrunSnapshot reads the managed snapshot for a docrun service from its
// stable backup directory.
func (e *Engine) loadDocrunSnapshot(svcID string) (*docrun.ManagedSnapshot, string, error) {
	backupDir, ok := e.store.GetMeta("serviceBackup:" + svcID)
	if !ok {
		return nil, "", fmt.Errorf("找不到服务 %s 的备份目录", svcID)
	}
	raw, err := os.ReadFile(filepath.Join(backupDir, "container-snapshot.json"))
	if err != nil {
		return nil, backupDir, fmt.Errorf("读取容器快照失败: %w", err)
	}
	snap, err := docrun.LoadSnapshot(raw)
	if err != nil {
		return nil, backupDir, err
	}
	return snap, backupDir, nil
}

// restoreDocrunService puts the original container back from the durable
// snapshot. Sequence mirrors the compose restore: proxy target out first,
// gateway released, then stop managed → park it → recreate original → start →
// verify ports → delete record → remove parked container.
func (e *Engine) restoreDocrunService(ctx context.Context, logger *opLogger, svc model.Service) error {
	admin, adminErr := e.adminClientForEnv(svc.EnvironmentID)
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
	snap, _, err := e.loadDocrunSnapshot(svc.ID)
	if err != nil {
		return fmt.Errorf("无法恢复：容器快照缺失（未做任何修改）: %w", err)
	}
	if snap.ServiceID == "" {
		snap.ServiceID = svc.ID
	}
	eng, err := e.docrunEngineForService(svc.ID)
	if err != nil {
		return fmt.Errorf("容器运行时不可用: %w", err)
	}

	// 1. Remove the proxy target first (verified write).
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

	// revertManaged restores the managed state, guided by the REAL container
	// state: if a completed rollback() already put the managed container back
	// under the original name, only proxy + gateway need re-wiring — blindly
	// creating another one would conflict with the just-restored container
	// and report a successful rollback as failed.
	revertManaged := func() error {
		var errs []string
		ins, ierr := eng.InspectContainer(ctx, snap.OriginalName)
		alreadyBack := ierr == nil &&
			ins.Config.Labels[docrun.LabelServiceID] == svc.ID &&
			ins.Config.Labels[docrun.LabelManaged] == "true"
		if !alreadyBack {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			id, cerr := docrun.CreateAndStart(rctx, eng, snap.ManagedSpec)
			cancel()
			if cerr != nil {
				errs = append(errs, "重建托管容器失败: "+cerr.Error())
			} else {
				_ = e.store.SetServiceStatus(svc.ID, "managed_running", "", id)
			}
		}
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
			return errors.New(joinErrList(errs))
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

	// 3. Verify the managed container still matches the snapshot before
	// touching it: external recreation must never be silently overwritten.
	// The verified ID is carried into RestoreOriginal so stop/rename/park can
	// never act on a container swapped in between verify and act.
	verified, verr := docrun.VerifyManaged(ctx, eng, snap)
	if verr != nil {
		// External modification: keep the managed wiring, ask for manual care.
		if gerr := e.ensureGatewayForService(ctx, svc.ID); gerr != nil {
			logger.add("error", "重建网关监听失败: "+gerr.Error())
		}
		if perr := e.putProxyConfigVerified(ctx, admin, origCfg); perr != nil {
			logger.add("error", "恢复代理配置失败: "+perr.Error())
		}
		return &attachError{Err: verr, RollbackFailed: true, Detail: "容器已被外部修改，恢复已跳过；托管状态保留"}
	}

	// 4. Restore the original container from the snapshot.
	createdID, rollback, err := docrun.RestoreOriginal(ctx, eng, snap, verified.ID)
	if err != nil {
		return failWithRevert(err)
	}
	logger.add("info", "已按快照恢复原容器: "+snap.OriginalName)

	ins, err := docrun.InspectRunning(ctx, eng, snap.OriginalName, time.Duration(maxInt(svc.StartTimeoutSecs, 30))*time.Second)
	if err != nil {
		if rollback != nil {
			if rbErr := rollback(); rbErr != nil {
				logger.add("error", "回退到托管状态失败: "+rbErr.Error())
			}
		}
		return failWithRevert(fmt.Errorf("恢复容器未就绪: %w", err))
	}
	failpoint.Hit("detach_container_ready")
	for _, b := range svc.Bindings {
		if !containerPublishes(ins, model.PortBinding{
			HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol,
		}) {
			if rollback != nil {
				_ = rollback()
			}
			return failWithRevert(fmt.Errorf("容器未重新发布原始端口 %s:%d", hostLabel(b.HostIP), b.HostPort))
		}
	}
	if err := e.testBindings(ctx, svc.Bindings); err != nil {
		if rollback != nil {
			_ = rollback()
		}
		return failWithRevert(fmt.Errorf("原端口连通性测试失败: %w", err))
	}
	logger.add("info", "原端口连通性测试通过")
	failpoint.Hit("detach_before_record_delete")

	if err := deleteServiceRecord(e, svc.ID); err != nil {
		if rollback != nil {
			_ = rollback()
		}
		return failWithRevert(fmt.Errorf("删除托管记录失败: %w", err))
	}
	// 5. Drop the parked managed container. Failure here is cosmetic: it is a
	// stopped, renamed leftover.
	if err := docrun.RemoveManagedContainer(ctx, eng, snap, verified.ID); err != nil {
		logger.add("warn", "清理停用的托管容器失败（可手动删除）: "+err.Error())
	}
	_ = createdID
	return nil
}

// runDetachDocrunPayload builds the detach transaction payload for a docrun
// service (no Compose content involved).
func (e *Engine) runDetachDocrunPayload(opID string, svc model.Service) (detachPayload, *docrun.ManagedSnapshot, error) {
	snap, _, err := e.loadDocrunSnapshot(svc.ID)
	if err != nil {
		return detachPayload{}, nil, err
	}
	if snap.ServiceID == "" {
		snap.ServiceID = svc.ID
	}
	svcCopy := svc
	svcCopy.Strategy = "docrun"
	return detachPayload{
		OperationID: opID,
		Service:     svcCopy,
		Phase:       "prepared",
		Strategy:    "docrun",
		Docrun:      snap,
	}, snap, nil
}

// recoverDocrunDetachTxn re-establishes the managed state for an interrupted
// docrun restore/remove. It inspects real container state instead of trusting
// the phase, because the restore sequence is inspect-driven.
func (e *Engine) recoverDocrunDetachTxn(txnID string, p detachPayload) {
	svc := p.Service
	snap := p.Docrun
	if snap == nil {
		// No snapshot in the payload: fall back to the stable backup.
		loaded, _, err := e.loadDocrunSnapshot(svc.ID)
		if err != nil {
			_ = e.store.FinishTransaction(txnID, "rollback_failed", "缺少容器快照: "+err.Error(), "rollback_failed")
			e.setLastError("恢复操作回滚不完整: " + err.Error())
			return
		}
		snap = loaded
		if snap.ServiceID == "" {
			snap.ServiceID = svc.ID
		}
	}
	eng, err := e.docrunEngineForService(svc.ID)
	if err != nil {
		e.logger.Printf("error", "未完成恢复操作：容器运行时不可用: %v（事务保留）", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Managed container present and untouched → the detach never reached the
	// container steps; the managed state is intact. Identity triage: only a
	// definitive 404 means "absent"; anything else keeps the txn for retry.
	ins, ierr := eng.InspectContainer(ctx, snap.OriginalName)
	switch {
	case ierr == nil:
		// identity triage below
	case isContainerNotFound(ierr):
		ins = nil // absent: fall through to the rebuild path
	default:
		// Unknown state (API down, timeout, …): never run destructive
		// recovery on an unreadable board.
		e.logger.Printf("error", "未完成恢复操作：无法读取容器状态: %v（事务保留，等待重试）", ierr)
		return
	}
	if ins != nil {
		if ins.Config.Labels[docrun.LabelServiceID] == svc.ID && ins.Config.Labels[docrun.LabelManaged] == "true" {
			if gerr := e.ensureGatewayForService(ctx, svc.ID); gerr != nil {
				_ = e.store.FinishTransaction(txnID, "rollback_failed", "恢复端口监听失败: "+gerr.Error(), "rollback_failed")
				e.setLastError("恢复操作回滚不完整: " + gerr.Error())
				return
			}
			if serr := e.syncProxyConfig(ctx, true); serr != nil {
				e.logger.Printf("warn", "恢复后同步代理失败（reconcile 会重试）: %v", serr)
			}
			e.finishTxnAtomic(txnID, p.OperationID, "rolled_back", nil, "rolled_back")
			e.logger.Printf("info", "未完成的恢复操作已回退到托管状态: %s", svc.ContainerName)
			return
		}
		// The original container is back (restore reached the create step).
		// Identity: ports alone are not proof — the config fingerprint must
		// match the snapshot's original spec before we treat it as ours.
		publishesAll := true
		for _, b := range svc.Bindings {
			if !containerPublishes(ins, model.PortBinding{HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol}) {
				publishesAll = false
				break
			}
		}
		fpMatch := false
		if current, cerr := docrun.SpecFromInspect(ins); cerr == nil {
			fpMatch = docrun.Fingerprint(current) == docrun.Fingerprint(snap.OriginalSpec)
		}
		if publishesAll && fpMatch {
			if derr := deleteServiceRecord(e, svc.ID); derr != nil {
				_ = e.store.FinishTransaction(txnID, "rollback_failed", "删除托管记录失败: "+derr.Error(), "rollback_failed")
				e.setLastError("恢复操作回滚不完整: " + derr.Error())
				return
			}
			if rerr := docrun.RemoveManagedContainer(ctx, eng, snap, svc.ContainerID); rerr != nil {
				e.logger.Printf("warn", "清理停用的托管容器失败（可手动删除）: %v", rerr)
			}
			e.finishTxnAtomic(txnID, p.OperationID, "committed", nil, "committed")
			e.logger.Printf("info", "未完成的恢复操作已完成: %s", svc.ContainerName)
			return
		}
		if !fpMatch || !publishesAll {
			// A container under our name that is neither the healthy managed
			// replacement nor the verified original: fail closed and keep the
			// transaction for manual attention. Never stop/remove it.
			msg := fmt.Sprintf("名称 %s 下的容器与快照不一致（指纹匹配=%v 端口发布=%v），禁止自动删除；请人工核对该容器后处理事务",
				snap.OriginalName, fpMatch, publishesAll)
			_ = e.store.FinishTransaction(txnID, "needs_repair", msg, "needs_repair")
			e.setLastError(msg)
			return
		}
	}
	// Neither a healthy managed container nor a healthy original: park the
	// stale managed container if present, then rebuild the original. The
	// stale container is cleaned BY ID after identity verification.
	staleIns, staleErr := eng.InspectContainer(ctx, docrun.StaleName(snap.OriginalName))
	switch {
	case staleErr == nil:
		if staleIns.Config.Labels[docrun.LabelServiceID] != svc.ID ||
			staleIns.Config.Labels[docrun.LabelManaged] != "true" {
			_ = e.store.FinishTransaction(txnID, "needs_repair",
				fmt.Sprintf("停用名称 %s 被无身份容器占用，禁止自动删除", docrun.StaleName(snap.OriginalName)), "needs_repair")
			e.setLastError("停用容器名称被外部占用，需要人工处理")
			return
		}
		if rerr := eng.RemoveContainer(ctx, staleIns.ID, true); rerr != nil && !isContainerNotFound(rerr) {
			_ = e.store.FinishTransaction(txnID, "rollback_failed", "清理停用容器失败: "+rerr.Error(), "rollback_failed")
			e.setLastError("恢复操作回滚不完整: " + rerr.Error())
			return
		}
	case isContainerNotFound(staleErr):
		// no stale container: fine
	default:
		_ = e.store.FinishTransaction(txnID, "rollback_failed", "无法读取停用容器状态: "+staleErr.Error(), "rollback_failed")
		e.setLastError("恢复操作回滚不完整: " + staleErr.Error())
		return
	}
	if _, err := docrun.CreateAndStart(ctx, eng, snap.OriginalSpec); err != nil {
		// Fall back to the managed state instead of leaving nothing running.
		if _, merr := docrun.CreateAndStart(ctx, eng, snap.ManagedSpec); merr == nil {
			if gerr := e.ensureGatewayForService(ctx, svc.ID); gerr == nil {
				e.finishTxnAtomic(txnID, p.OperationID, "rolled_back", nil, "rolled_back")
				e.logger.Printf("warn", "恢复原容器失败，已回退到托管状态: %s", svc.ContainerName)
				return
			}
		}
		_ = e.store.FinishTransaction(txnID, "rollback_failed", "恢复原容器失败: "+err.Error(), "rollback_failed")
		e.setLastError("恢复操作回滚不完整: " + err.Error())
		return
	}
	ins, werr := docrun.InspectRunning(ctx, eng, snap.OriginalName, 2*time.Minute)
	if werr != nil {
		_ = e.store.FinishTransaction(txnID, "rollback_failed", "恢复容器未就绪: "+werr.Error(), "rollback_failed")
		e.setLastError("恢复操作回滚不完整: " + werr.Error())
		return
	}
	for _, b := range svc.Bindings {
		if !containerPublishes(ins, model.PortBinding{HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Protocol: b.Protocol}) {
			_ = e.store.FinishTransaction(txnID, "rollback_failed",
				fmt.Sprintf("恢复容器未发布原端口 %d", b.HostPort), "rollback_failed")
			e.setLastError("恢复操作回滚不完整: 原端口未恢复")
			return
		}
	}
	if derr := deleteServiceRecord(e, svc.ID); derr != nil {
		_ = e.store.FinishTransaction(txnID, "rollback_failed", "删除托管记录失败: "+derr.Error(), "rollback_failed")
		e.setLastError("恢复操作回滚不完整: " + derr.Error())
		return
	}
	e.finishTxnAtomic(txnID, p.OperationID, "committed", nil, "committed")
	e.logger.Printf("info", "未完成的恢复操作已完成（按快照恢复）: %s", svc.ContainerName)
}

func joinErrList(errs []string) string {
	out := ""
	for i, s := range errs {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
