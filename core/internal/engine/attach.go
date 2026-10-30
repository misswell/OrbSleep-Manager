package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/dockerx"
	"orbsleep/internal/gateway"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

type attachError struct {
	Err            error
	RollbackFailed bool
	Detail         string
}

func (a *attachError) Error() string {
	if a.Detail != "" {
		return a.Err.Error() + "（" + a.Detail + "）"
	}
	return a.Err.Error()
}
func (a *attachError) Unwrap() error { return a.Err }

type appliedItem struct {
	snap           *planSnapshot
	serviceID      string
	backupPath     string
	managedContent []byte
}

type undoStep struct {
	name string
	fn   func() error
}

// ApplyPlan starts executing a plan asynchronously and returns an operation id.
func (e *Engine) ApplyPlan(planID string) (string, error) {
	e.plansMu.Lock()
	entry, ok := e.plans[planID]
	if !ok {
		e.plansMu.Unlock()
		return "", errors.New("接入计划不存在或已过期，请重新生成")
	}
	if entry.applying {
		e.plansMu.Unlock()
		return "", errors.New("该计划正在执行")
	}
	if time.Since(entry.createdAt) > 10*time.Minute {
		delete(e.plans, planID)
		e.plansMu.Unlock()
		return "", errors.New("接入计划已过期，请重新生成")
	}
	if entry.plan.Status != "ready" || len(entry.items) == 0 {
		e.plansMu.Unlock()
		return "", errors.New("接入计划包含阻塞项，无法执行")
	}
	if running, _ := e.store.ListRunningTransactions(); len(running) > 0 {
		e.plansMu.Unlock()
		return "", errors.New("存在未完成的接入事务，请先查看操作记录并完成恢复")
	}
	entry.applying = true
	plan := entry.plan
	items := append([]planSnapshot(nil), entry.items...)
	e.plansMu.Unlock()

	ctx, done, err := e.beginOperation(20 * time.Minute)
	if err != nil {
		return "", err
	}
	opID := newID("op")
	backupDir := filepath.Join(e.settings.Get().BackupDir, opID)
	op := model.Operation{
		ID:        opID,
		Kind:      "attach",
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: backupDir,
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		done()
		return "", err
	}
	go e.runAttach(ctx, plan, items, op, done)
	return opID, nil
}

func (e *Engine) runAttach(ctx context.Context, plan model.Plan, items []planSnapshot, op model.Operation, done func()) {
	defer done()
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	defer func() {
		e.plansMu.Lock()
		delete(e.plans, plan.ID)
		e.plansMu.Unlock()
	}()
	logger := e.newOpLogger(op)
	logger.add("info", fmt.Sprintf("接入计划 %s 开始执行（%d 个容器）", plan.ID, len(items)))

	// A complete, validated baseline snapshot is mandatory. Never start
	// modifying anything if it cannot be read: rollback must be able to
	// restore exactly what existed before.
	admin, err := e.adminClient()
	if err != nil {
		logger.finish("failed", "无法连接代理管理接口，已终止（未做任何修改）: "+err.Error())
		e.setLastError("接入未执行: " + err.Error())
		return
	}
	baseCfg, err := admin.GetConfig(ctx)
	if err != nil {
		logger.finish("failed", "读取代理基线配置失败，已终止（未做任何修改）: "+err.Error())
		e.setLastError("接入未执行: 无法读取代理基线配置")
		return
	}
	if err := validateProxyConfig(baseCfg); err != nil {
		logger.finish("failed", "代理基线配置无效，已终止（未做任何修改）: "+err.Error())
		e.setLastError("接入未执行: 代理基线配置无效")
		return
	}
	logger.add("info", fmt.Sprintf("代理基线已校验（%d 个既有服务）", len(baseCfg.Services)))

	// Persist the transaction before any modification. If the process dies
	// mid-way, startup recovery uses this record and the per-item backups.
	payload := txnFromPlan(op.ID, baseCfg, items)
	txnID, txnErr := e.startTxn(op.ID, "attach", payload)
	if txnErr != nil {
		logger.finish("failed", "无法写入事务日志，已终止（未做任何修改）: "+txnErr.Error())
		e.setLastError("接入未执行: 无法写入事务日志")
		return
	}

	fileStates := map[string][]byte{}
	var applied []appliedItem
	for i, snap := range items {
		logger.add("info", fmt.Sprintf("[%d/%d] 接入 %s", i+1, len(items), snap.containerName))
		ai, err := e.attachOne(ctx, logger, i, &items[i], fileStates, admin, txnID)
		if err != nil {
			rbFailed := false
			var ae *attachError
			if errors.As(err, &ae) {
				rbFailed = ae.RollbackFailed
			}
			logger.add("error", err.Error())
			if len(applied) > 0 {
				logger.add("warn", "回滚此前已完成的接入")
				if rbErr := e.rollbackApplied(logger, applied, baseCfg); rbErr != nil {
					var deferred *rollbackDeferredError
					if errors.As(rbErr, &deferred) {
						logger.add("warn", "回滚暂未执行，事务保留以便后续自动恢复: "+deferred.Error())
						logger.finish("failed", "接入失败且回滚未执行（事务保留，等待自动恢复）: "+err.Error())
						e.setLastError("接入失败，回滚待自动恢复: " + err.Error())
						return
					}
					logger.add("error", rbErr.Error())
					rbFailed = true
				}
			}
			if rbFailed {
				e.finishTxnAtomic(txnID, op.ID, "rollback_failed", err, "rollback_failed")
				logger.finish("rollback_failed", err.Error())
				e.setLastError("接入失败且回滚不完整: " + err.Error())
			} else {
				e.finishTxnAtomic(txnID, op.ID, "rolled_back", err, "rolled_back")
				logger.finish("rolled_back", err.Error())
				e.setLastError("接入失败，已回滚: " + err.Error())
			}
			return
		}
		applied = append(applied, *ai)
	}
	ids := make([]string, 0, len(applied))
	for _, ai := range applied {
		ids = append(ids, ai.serviceID)
	}
	logger.setAffected(ids)
	logger.add("info", "接入完成；此后原端口由 macOS 端口网关监听，GUI 关闭后仍然有效")
	e.finishTxnAtomic(txnID, op.ID, "committed", nil, "committed")
	logger.finish("succeeded", "")
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.add("warn", "同步代理配置失败: "+err.Error())
	}
	e.triggerReconcile()
}

func (e *Engine) attachOne(ctx context.Context, logger *opLogger, idx int, snap *planSnapshot, fileStates map[string][]byte, admin *proxy.AdminClient, txnID string) (*appliedItem, error) {
	s := e.settings.Get()
	cli, err := e.cliRunner()
	if err != nil {
		return nil, &attachError{Err: err}
	}
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, &attachError{Err: err}
	}

	ins, err := c.InspectContainer(ctx, snap.containerID)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("目标容器在接入前已消失: %w", err)}
	}
	if name := strings.TrimPrefix(ins.Name, "/"); name != snap.containerName {
		return nil, &attachError{Err: fmt.Errorf("容器名称已变化（%s → %s），请重新扫描", snap.containerName, name)}
	}
	// Guard against edits made after the plan was generated: never overwrite
	// a Compose file the user changed in the meantime. When several plan items
	// share one Compose file, compare against the state left by previous items.
	expected, ok := fileStates[snap.composeFile]
	if !ok {
		expected = snap.content
	}
	current, err := os.ReadFile(snap.composeFile)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("读取 Compose 文件失败: %w", err)}
	}
	if !bytes.Equal(current, expected) {
		return nil, &attachError{Err: errors.New("Compose 文件在计划生成后被修改过，请重新扫描并生成计划")}
	}
	for _, r := range snap.removals {
		if !containerPublishes(ins, r) {
			return nil, &attachError{Err: fmt.Errorf("容器当前不再发布 %d/tcp，请重新扫描", r.HostPort)}
		}
	}

	beforeCfg, err := cli.ComposeConfig(ctx, snap.composeProject, snap.composeFile, snap.workingDir)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("读取 Compose 有效配置失败: %w", err)}
	}
	prevCfg, err := admin.GetConfig(ctx)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("读取代理配置失败: %w", err)}
	}
	if err := validateProxyConfig(prevCfg); err != nil {
		return nil, &attachError{Err: fmt.Errorf("代理配置无效，无法安全接入: %w", err)}
	}
	// Re-check proxy port ownership: another plan may have been applied since
	// this plan was generated.
	existing, _ := e.store.ListServices()
	for _, other := range existing {
		for _, ob := range other.Bindings {
			for _, nb := range snap.bindings {
				if ob.ProxyPort == nb.ProxyPort {
					return nil, &attachError{Err: fmt.Errorf("代理端口 %d 已被服务 %s 占用，请重新生成计划", nb.ProxyPort, other.ContainerName)}
				}
			}
		}
	}

	svcID := newID("svc")
	itemDir := filepath.Join(logger.snapshot().BackupDir, fmt.Sprintf("item-%d", idx+1))
	backupPath, err := compose.BackupFile(snap.composeFile, itemDir)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("备份 Compose 文件失败: %w", err)}
	}
	if err := os.WriteFile(filepath.Join(itemDir, "proxy-config.yaml"), prevCfg.Marshal(), 0o600); err != nil {
		return nil, &attachError{Err: fmt.Errorf("备份代理配置失败: %w", err)}
	}
	meta := map[string]any{
		"containerId":    snap.containerID,
		"containerName":  snap.containerName,
		"composeProject": snap.composeProject,
		"composeService": snap.composeService,
		"composeFile":    snap.composeFile,
		"workingDir":     snap.workingDir,
		"removals":       snap.removals,
		"bindings":       snap.bindings,
		"createdAt":      time.Now().UTC().Format(time.RFC3339),
	}
	metaRaw, merr := json.MarshalIndent(meta, "", "  ")
	if merr != nil {
		return nil, &attachError{Err: fmt.Errorf("序列化备份元数据失败: %w", merr)}
	}
	if werr := os.WriteFile(filepath.Join(itemDir, "metadata.json"), metaRaw, 0o600); werr != nil {
		return nil, &attachError{Err: fmt.Errorf("写入备份元数据失败: %w", werr)}
	}
	if err := e.updateTxn(txnID, "prepared", func(p *txnPayload) {
		p.Items[idx].BackupPath = backupPath
		p.Items[idx].ServiceID = svcID
		p.Items[idx].Phase = "prepared"
	}); err != nil {
		return nil, &attachError{Err: fmt.Errorf("写入事务日志失败: %w", err)}
	}
	logger.add("info", fmt.Sprintf("已备份 %s 到 %s", snap.composeFile, backupPath))
	var undos []undoStep
	// out is the exact content this item wrote to the Compose file (once
	// composeWritten is set); fail uses it to detect external edits.
	var out []byte
	composeWritten := false
	// persistManagedState creates the stable backup and the managed record.
	// It is used on the success path and, when the Compose file was externally
	// modified mid-attach, on the failure path to keep a coherent managed
	// state instead of orphaned listeners.
	persistManagedState := func(runningIns *dockerx.ContainerInspect, status, lastErr string) error {
		containerID := snap.containerID
		if runningIns != nil {
			containerID = runningIns.ID
		}
		stableDir := filepath.Join(s.BackupDir, "services", svcID)
		if err := os.MkdirAll(stableDir, 0o700); err != nil {
			return err
		}
		if err := copyFileSimple(backupPath, filepath.Join(stableDir, filepath.Base(backupPath))); err != nil {
			return err
		}
		// Keep the pre-attach content too (for audit and emergency recovery).
		if werr := os.WriteFile(filepath.Join(stableDir, filepath.Base(backupPath)+".original"), snap.content, 0o600); werr != nil {
			return fmt.Errorf("写入原始 Compose 备份失败: %w", werr)
		}
		meta["serviceId"] = svcID
		metaRaw, merr := json.MarshalIndent(meta, "", "  ")
		if merr != nil {
			return fmt.Errorf("序列化稳定备份元数据失败: %w", merr)
		}
		if werr := os.WriteFile(filepath.Join(stableDir, "metadata.json"), metaRaw, 0o600); werr != nil {
			return fmt.Errorf("写入稳定备份元数据失败: %w", werr)
		}
		if err := e.store.SetMeta("serviceBackup:"+svcID, stableDir); err != nil {
			return fmt.Errorf("写入备份索引失败: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		return e.store.UpsertService(model.Service{
			ID:               svcID,
			ContainerID:      containerID,
			ContainerName:    snap.containerName,
			ComposeProject:   snap.composeProject,
			ComposeService:   snap.composeService,
			ComposeFile:      snap.composeFile,
			Status:           status,
			LastError:        lastErr,
			Bindings:         snap.bindings,
			IdleTimeoutSecs:  s.IdleTimeoutSecs,
			StartTimeoutSecs: s.StartTimeoutSecs,
			LastActiveAt:     now,
			CreatedAt:        now,
		})
	}
	fail := func(cause error) (*appliedItem, error) {
		// If the Compose file was externally modified after this item wrote
		// it, running the undo stack would either clobber the user's edits
		// (restoring the backup) or revoke the managed wiring (record, gateway
		// listeners, proxy entry) while the file no longer matches it. Keep
		// the managed state and require manual attention instead.
		if composeWritten {
			// Treat a missing/unreadable file exactly like an external edit:
			// the on-disk state is unknown, so neither restoring the backup nor
			// running the undo stack is safe. Keep the managed wiring and let
			// the user resolve the Compose file.
			current, rerr := os.ReadFile(snap.composeFile)
			if rerr != nil || !bytes.Equal(current, out) {
				reason := "Compose 文件在接入过程中被外部修改"
				if rerr != nil {
					reason = "Compose 文件在接入过程中缺失或无法读取（" + rerr.Error() + "）"
				}
				logger.add("error", reason+"，已跳过自动回滚并保留托管状态")
				detail := reason + "，为避免覆盖或重建用户文件未自动回滚；已保留托管状态，备份位于 " + backupPath
				freshIns := ins
				if c, cerr := e.dockerClient(context.Background()); cerr == nil {
					if ii, ierr := c.InspectContainer(context.Background(), snap.containerName); ierr == nil {
						freshIns = ii
					}
				}
				if _, ok, gerr := e.store.GetService(svcID); gerr == nil && !ok {
					if perr := persistManagedState(freshIns, "warning", "接入中断："+reason+"，自动回滚已跳过"); perr != nil {
						detail += "；创建托管记录失败: " + perr.Error()
					} else {
						detail += "；已创建托管记录以保留网关监听与代理配置"
					}
				}
				// The failure may have happened before the gateway install
				// step; make the kept managed state immediately coherent.
				gctx, gcancel := context.WithTimeout(context.Background(), 30*time.Second)
				if gerr := e.ensureGatewayForService(gctx, svcID); gerr != nil {
					detail += "；重建网关监听失败: " + gerr.Error()
				}
				gcancel()
				return nil, &attachError{Err: cause, RollbackFailed: true, Detail: detail}
			}
		}
		logger.add("warn", "回滚本容器的修改")
		var undoErrs []string
		for i := len(undos) - 1; i >= 0; i-- {
			if uerr := undos[i].fn(); uerr != nil {
				logger.add("error", fmt.Sprintf("回滚步骤「%s」失败: %v", undos[i].name, uerr))
				undoErrs = append(undoErrs, undos[i].name+": "+uerr.Error())
			}
		}
		ae := &attachError{Err: cause}
		if len(undoErrs) > 0 {
			ae.RollbackFailed = true
			ae.Detail = strings.Join(undoErrs, "; ")
		}
		return nil, ae
	}

	// 1. Register the service in the proxy. The undo step is registered before
	// the write so an uncertain result (write applied but verification failed)
	// is always followed by a restore attempt.
	nextCfg := prevCfg.Clone()
	sc := proxy.ServiceConfig{
		Name:             snap.containerName,
		IdleTimeoutSecs:  s.IdleTimeoutSecs,
		StartTimeoutSecs: s.StartTimeoutSecs,
	}
	for _, m := range snap.proxyMappings {
		sc.Ports = append(sc.Ports, fmt.Sprintf("%d:%d", m.ListenPort, m.TargetPort))
	}
	sort.Strings(sc.Ports)
	nextCfg.Upsert(sc)
	undos = append(undos, undoStep{name: "恢复代理配置", fn: func() error {
		rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return e.putProxyConfigVerified(rctx, admin, prevCfg)
	}})
	if err := admin.UpdateConfig(ctx, nextCfg); err != nil {
		return fail(fmt.Errorf("更新代理配置失败: %w", err))
	}
	if err := e.verifyProxyService(ctx, admin, sc); err != nil {
		return fail(fmt.Errorf("确认代理配置失败: %w", err))
	}
	if err := e.updateTxn(txnID, "proxy_updated", func(p *txnPayload) { p.Items[idx].Phase = "proxy_updated" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("代理已注册 %s（%s）", snap.containerName, strings.Join(sc.Ports, ", ")))

	// 2. Edit the Compose file.
	out, _, err = compose.RemoveBindings(current, snap.composeService, snap.removals)
	if err != nil {
		return fail(fmt.Errorf("修改 Compose 文件失败: %w", err))
	}
	info, err := os.Stat(snap.composeFile)
	if err != nil {
		return fail(fmt.Errorf("读取 Compose 文件属性失败: %w", err))
	}
	// Write-ahead: persist the exact bytes about to be written before touching
	// the file. If the process dies after the atomic rename but before the
	// post-write transaction update, recovery can still recognise the file.
	if err := e.updateTxn(txnID, "compose_write_pending", func(p *txnPayload) {
		p.Items[idx].PendingContent = append([]byte(nil), out...)
		p.Items[idx].Phase = "compose_write_pending"
	}); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	// Re-check immediately before writing: the plan-time equality check above
	// ran before the proxy update, and the file must not have been changed in
	// between either.
	if fresh, ferr := os.ReadFile(snap.composeFile); ferr != nil {
		return fail(fmt.Errorf("写入前读取 Compose 文件失败: %w", ferr))
	} else if !bytes.Equal(fresh, current) {
		return fail(errors.New("Compose 文件在接入过程中被外部修改，已中止写入"))
	}
	if err := compose.WriteAtomic(snap.composeFile, out, info.Mode()); err != nil {
		return fail(fmt.Errorf("写入 Compose 文件失败: %w", err))
	}
	composeWritten = true
	fileStates[snap.composeFile] = out
	undos = append(undos, undoStep{name: "恢复 Compose 并重建", fn: func() error {
		if err := compose.RestoreFile(snap.composeFile, backupPath); err != nil {
			return fmt.Errorf("恢复 Compose 文件失败: %w", err)
		}
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := cli.UpService(rctx, snap.composeProject, snap.composeFile, snap.workingDir, snap.composeService); err != nil {
			return fmt.Errorf("回滚后重建服务失败: %w", err)
		}
		return nil
	}})
	if err := e.updateTxn(txnID, "compose_updated", func(p *txnPayload) {
		p.Items[idx].ManagedContent = out
		p.Items[idx].Phase = "compose_updated"
	}); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("已移除端口映射: %s", portsText(snap.removals)))

	// 3. Validate the effective config only lost those ports.
	afterCfg, err := cli.ComposeConfig(ctx, snap.composeProject, snap.composeFile, snap.workingDir)
	if err != nil {
		return fail(fmt.Errorf("修改后 Compose 配置无效: %w", err))
	}
	if err := compose.ValidateEffective(beforeCfg, afterCfg, snap.composeService, snap.removals); err != nil {
		return fail(err)
	}
	logger.add("info", "Compose 有效配置校验通过")

	// 4. Recreate only the target service.
	logger.add("info", "重建目标服务（仅该服务）…")
	if err := cli.UpService(ctx, snap.composeProject, snap.composeFile, snap.workingDir, snap.composeService); err != nil {
		return fail(fmt.Errorf("重建容器失败: %w", err))
	}
	newIns, err := e.waitContainerRunning(ctx, snap.containerName, time.Duration(maxInt(s.StartTimeoutSecs, 30))*time.Second)
	if err != nil {
		return fail(err)
	}
	for _, r := range snap.removals {
		if containerPublishes(newIns, r) {
			return fail(fmt.Errorf("容器重建后仍然发布 %d/tcp（代理无法接管）", r.HostPort))
		}
	}
	if err := e.updateTxn(txnID, "container_recreated", func(p *txnPayload) { p.Items[idx].Phase = "container_recreated" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", "容器已重建，原端口已释放")

	// 5. Install the host port gateway. Register the cleanup before creating
	// any listener so a partial failure on a later port cannot leave one behind.
	undos = append(undos, undoStep{name: "移除端口网关监听", fn: func() error {
		e.gw.RemoveService(svcID)
		return nil
	}})
	for _, b := range snap.bindings {
		route, err := e.gatewayRoute(ctx, svcID, snap.containerName, b, s)
		if err != nil {
			return fail(err)
		}
		if err := e.gw.Ensure(route); err != nil {
			return fail(fmt.Errorf("启动端口网关失败（%s:%d）: %w", hostLabel(b.HostIP), b.HostPort, err))
		}
	}
	if err := e.updateTxn(txnID, "gateway_ready", func(p *txnPayload) { p.Items[idx].Phase = "gateway_ready" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("端口网关已监听: %s", bindingsText(snap.bindings)))

	// 6. Verify the original address works (this also wakes the container).
	if err := e.testBindings(ctx, snap.bindings); err != nil {
		return fail(fmt.Errorf("原端口连通性测试失败: %w", err))
	}
	if err := e.updateTxn(txnID, "verified", func(p *txnPayload) { p.Items[idx].Phase = "verified" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", "原端口连通性测试通过")

	if err := persistManagedState(newIns, "managed_running", ""); err != nil {
		return fail(fmt.Errorf("保存托管状态失败: %w", err))
	}
	undos = append(undos, undoStep{name: "删除托管记录", fn: func() error {
		_ = e.store.DeleteMeta("serviceBackup:" + svcID)
		return e.store.DeleteService(svcID)
	}})
	if err := e.updateTxn(txnID, "committed_item", func(p *txnPayload) {
		p.Items[idx].ServiceID = svcID
		p.Items[idx].Phase = "committed_item"
	}); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("%s 接入成功：%s → 代理 → 容器", snap.containerName, bindingsText(snap.bindings)))
	return &appliedItem{snap: snap, serviceID: svcID, backupPath: backupPath, managedContent: out}, nil
}

// rollbackApplied reverts previously successful items of a failed plan. It only
// deletes a managed record after the original Compose port mapping has been
// restored, the container is running and the original ports are published.
// rollbackDeferredError means a rollback could not even start because a
// dependency was unavailable. The transaction must stay "running" so startup
// recovery can retry it later.
type rollbackDeferredError struct{ Cause error }

func (d *rollbackDeferredError) Error() string {
	return "回滚依赖不可用: " + d.Cause.Error()
}
func (d *rollbackDeferredError) Unwrap() error { return d.Cause }

func (e *Engine) rollbackApplied(logger *opLogger, applied []appliedItem, base proxy.Config) error {
	// Rollback must never inherit a cancelled business context (e.g. shutdown).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cli, err := e.cliRunner()
	if err != nil {
		return &rollbackDeferredError{Cause: fmt.Errorf("docker CLI 不可用: %w", err)}
	}
	admin, err := e.adminClient()
	if err != nil {
		return &rollbackDeferredError{Cause: fmt.Errorf("代理管理接口不可用: %w", err)}
	}
	if _, err := admin.GetConfig(ctx); err != nil {
		return &rollbackDeferredError{Cause: fmt.Errorf("无法读取代理配置: %w", err)}
	}
	var errs []string
	rolledBack := map[string]bool{}
	for i := len(applied) - 1; i >= 0; i-- {
		ai := applied[i]
		name := ai.snap.containerName
		logger.add("warn", "回滚 "+name)
		e.gw.RemoveService(ai.serviceID)

		current, err := os.ReadFile(ai.snap.composeFile)
		if err != nil || !bytes.Equal(current, ai.managedContent) {
			errs = append(errs, name+": Compose 文件在回滚前被外部修改，已跳过自动恢复（保留托管记录）")
			if gerr := e.ensureGatewayForService(ctx, ai.serviceID); gerr != nil {
				errs = append(errs, name+": 重新监听失败: "+gerr.Error())
			}
			continue
		}
		if err := compose.RestoreFile(ai.snap.composeFile, ai.backupPath); err != nil {
			errs = append(errs, name+": 恢复 Compose 失败: "+err.Error())
			if gerr := e.ensureGatewayForService(ctx, ai.serviceID); gerr != nil {
				errs = append(errs, name+": 重新监听失败: "+gerr.Error())
			}
			continue
		}
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		upErr := cli.UpService(rctx, ai.snap.composeProject, ai.snap.composeFile, ai.snap.workingDir, ai.snap.composeService)
		cancel()
		if upErr == nil {
			var ins *dockerx.ContainerInspect
			ins, upErr = e.waitContainerRunning(ctx, name, 2*time.Minute)
			if upErr == nil {
				for _, r := range ai.snap.removals {
					if !containerPublishes(ins, r) {
						upErr = fmt.Errorf("原始端口 %d/tcp 未重新发布", r.HostPort)
						break
					}
				}
			}
		}
		if upErr != nil {
			errs = append(errs, name+": 恢复原容器失败: "+upErr.Error())
			if rerr := e.revertToManaged(ai); rerr != nil {
				errs = append(errs, name+": 回退到托管状态失败: "+rerr.Error())
			}
			continue
		}
		if err := deleteServiceRecord(e, ai.serviceID); err != nil {
			errs = append(errs, name+": 删除托管记录失败: "+err.Error())
			if rerr := e.revertToManaged(ai); rerr != nil {
				errs = append(errs, name+": 回退到托管状态失败: "+rerr.Error())
			}
			continue
		}
		_ = e.store.DeleteMeta("serviceBackup:" + ai.serviceID)
		rolledBack[name] = true
		logger.add("info", name+" 已恢复到接入前状态")
	}

	if len(rolledBack) > 0 {
		cur, err := admin.GetConfig(ctx)
		if err != nil {
			errs = append(errs, "读取代理配置失败: "+err.Error())
		} else {
			for name := range rolledBack {
				cur.Remove(name)
				if original, ok := base.Service(name); ok {
					cur.Upsert(original)
				}
			}
			rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			err := e.putProxyConfigVerified(rctx, admin, cur)
			cancel()
			if err != nil {
				errs = append(errs, "恢复代理配置失败: "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("回滚不完整：%s；备份位于 %s，请按日志手动恢复", strings.Join(errs, "; "), logger.snapshot().BackupDir)
	}
	logger.add("info", "回滚完成，服务已恢复到接入前状态")
	return nil
}

// revertToManaged rewrites the managed Compose content, recreates the container
// and re-installs gateway listeners. Used when restoring the original port
// mapping failed midway.
func (e *Engine) revertToManaged(ai appliedItem) error {
	cli, err := e.cliRunner()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(ai.snap.composeFile); err == nil {
		mode = st.Mode()
	}
	if err := compose.WriteAtomic(ai.snap.composeFile, ai.managedContent, mode); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := cli.UpService(ctx, ai.snap.composeProject, ai.snap.composeFile, ai.snap.workingDir, ai.snap.composeService); err != nil {
		return err
	}
	if _, err := e.waitContainerRunning(ctx, ai.snap.containerName, 2*time.Minute); err != nil {
		return err
	}
	return e.ensureGatewayForService(ctx, ai.serviceID)
}

func (e *Engine) ensureGatewayForService(ctx context.Context, serviceID string) error {
	svc, ok, err := e.store.GetService(serviceID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	s := e.settings.Get()
	for _, b := range svc.Bindings {
		route, err := e.gatewayRoute(ctx, serviceID, svc.ContainerName, b, s)
		if err != nil {
			return err
		}
		if err := e.gw.Ensure(route); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) waitContainerRunning(ctx context.Context, name string, timeout time.Duration) (*dockerx.ContainerInspect, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ins, err := c.InspectContainer(ctx, name)
		if err == nil && ins.State.Running {
			return ins, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("容器状态为 %s", ins.State.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("容器 %s 未在 %s 内就绪: %v", name, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func containerPublishes(ins *dockerx.ContainerInspect, b model.PortBinding) bool {
	for key, bindings := range ins.HostConfig.PortBindings {
		port, proto := splitPortKey(key)
		if port != b.ContainerPort || proto != b.Protocol {
			continue
		}
		for _, hb := range bindings {
			if hb.HostPort == fmt.Sprintf("%d", b.HostPort) {
				return true
			}
		}
	}
	return false
}

func splitPortKey(key string) (int, string) {
	proto := "tcp"
	portStr := key
	if i := strings.Index(key, "/"); i >= 0 {
		portStr, proto = key[:i], key[i+1:]
	}
	n, _ := strconv.Atoi(portStr)
	return n, proto
}

// bindingProbeTimeout is how long a freshly recreated container gets to start
// accepting TCP connections on its original port.
var bindingProbeTimeout = 10 * time.Second

func (e *Engine) testBindings(ctx context.Context, bindings []model.Binding) error {
	for _, b := range bindings {
		host := b.HostIP
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		addr := fmt.Sprintf("%s:%d", host, b.HostPort)
		deadline := time.Now().Add(bindingProbeTimeout)
		var lastErr error
		for {
			d := netDialer(3 * time.Second)
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err == nil {
				conn.Close()
				lastErr = nil
				break
			}
			lastErr = err
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if lastErr != nil {
			return fmt.Errorf("%s 无法连接: %w", addr, lastErr)
		}
	}
	return nil
}

// gatewayRouteFor builds a gateway route from an already-resolved proxy IP.
// In container_ip mode proxyIP must be non-empty: the caller gets an error
// instead of a silent fallback to 127.0.0.1, which could forward connections
// to an unrelated local process.
func gatewayRouteFor(svcID, name string, b model.Binding, s model.Settings, proxyIP string) (gateway.Route, error) {
	targetHost := "127.0.0.1"
	if s.ProxyDialMode == "container_ip" {
		if proxyIP == "" {
			return gateway.Route{}, fmt.Errorf("container_ip 模式无法解析代理容器 IP（代理不可用）")
		}
		targetHost = proxyIP
	}
	return gateway.Route{
		Key:        fmt.Sprintf("%s|%s|%d", svcID, b.HostIP, b.HostPort),
		ServiceID:  svcID,
		BindHost:   b.HostIP,
		BindPort:   b.HostPort,
		TargetHost: targetHost,
		TargetPort: b.ProxyPort,
		Label:      name,
	}, nil
}

func (e *Engine) gatewayRoute(ctx context.Context, svcID, name string, b model.Binding, s model.Settings) (gateway.Route, error) {
	ip := ""
	if s.ProxyDialMode == "container_ip" {
		ip = e.proxyContainerIP(ctx)
	}
	return gatewayRouteFor(svcID, name, b, s, ip)
}

func portsText(list []model.PortBinding) string {
	var parts []string
	for _, p := range list {
		parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", hostLabel(p.HostIP), p.HostPort, p.ContainerPort, p.Protocol))
	}
	return strings.Join(parts, ", ")
}

func bindingsText(list []model.Binding) string {
	var parts []string
	for _, b := range list {
		parts = append(parts, fmt.Sprintf("%s:%d → 代理 %d", hostLabel(b.HostIP), b.HostPort, b.ProxyPort))
	}
	return strings.Join(parts, ", ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
