package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orbsleep/internal/deployment/docrun"
	"orbsleep/internal/dockerx"
	"orbsleep/internal/failpoint"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/runtime"
)

// docrunEngine adapts the engine's Docker client to the strategy interface.
// It shares the create-body mapping with the dockerapi adapter via
// runtime.CreateRequestBody.
type docrunEngine struct{ c *dockerx.Client }

func (a docrunEngine) InspectContainer(ctx context.Context, id string) (*dockerx.ContainerInspect, error) {
	return a.c.InspectContainer(ctx, id)
}
func (a docrunEngine) StartContainer(ctx context.Context, id string) error {
	return a.c.StartContainer(ctx, id)
}
func (a docrunEngine) StopContainer(ctx context.Context, id string, timeoutSecs int) error {
	return a.c.StopContainer(ctx, id, timeoutSecs)
}
func (a docrunEngine) RemoveContainer(ctx context.Context, id string, force bool) error {
	return a.c.RemoveContainer(ctx, id, force)
}
func (a docrunEngine) RenameContainer(ctx context.Context, id, name string) error {
	return a.c.RenameContainer(ctx, id, name)
}
func (a docrunEngine) CreateContainer(ctx context.Context, spec *runtime.ContainerSpec) (string, error) {
	return a.c.CreateContainer(ctx, spec.Name, runtime.CreateRequestBody(spec))
}
func (a docrunEngine) ConnectNetwork(ctx context.Context, network, containerID string, endpoint *runtime.EndpointConfig) error {
	return a.c.ConnectNetwork(ctx, network, containerID, endpoint)
}

// attachOneDocrun attaches a container created outside Compose. The sequence
// mirrors attachOne but replaces the Compose edit/recreate with the snapshot
// based replacement; every rule of the transaction system applies unchanged:
// snapshot before mutation, undo registered before the write, managed record
// only after end-to-end verification.
func (e *Engine) attachOneDocrun(ctx context.Context, logger *opLogger, idx int, snap *planSnapshot, admin *proxy.AdminClient, txnID string) (*appliedItem, error) {
	s := e.settings.Get()
	c, err := e.clientForEnv(ctx, snap.envID)
	if err != nil {
		return nil, &attachError{Err: err}
	}
	eng := docrunEngine{c: c}

	ins, err := c.InspectContainer(ctx, snap.containerID)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("目标容器在接入前已消失: %w", err)}
	}
	if name := strings.TrimPrefix(ins.Name, "/"); name != snap.containerName {
		return nil, &attachError{Err: fmt.Errorf("容器名称已变化（%s → %s），请重新扫描", snap.containerName, name)}
	}
	// Re-map the current config and compare fingerprints: the container must
	// be exactly what the plan saw.
	current, err := docrun.SpecFromInspect(ins)
	if err != nil {
		return nil, &attachError{Err: err}
	}
	if got := docrun.Fingerprint(current); got != snap.docrunFingerprint {
		return nil, &attachError{Err: errors.New("容器配置在计划生成后被修改过，请重新扫描并生成计划")}
	}

	prevCfg, err := admin.GetConfig(ctx)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("读取代理配置失败: %w", err)}
	}
	if err := validateProxyConfig(prevCfg); err != nil {
		return nil, &attachError{Err: fmt.Errorf("代理配置无效，无法安全接入: %w", err)}
	}
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
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		return nil, &attachError{Err: fmt.Errorf("创建备份目录失败: %w", err)}
	}
	if err := os.WriteFile(filepath.Join(itemDir, "proxy-config.yaml"), prevCfg.Marshal(), 0o600); err != nil {
		return nil, &attachError{Err: fmt.Errorf("备份代理配置失败: %w", err)}
	}

	// Build the durable recreation snapshot with the real service id and
	// persist it in the transaction payload BEFORE any container mutation.
	prep, err := docrun.PrepareAttach(ins, svcID, snap.takeoverKeys)
	if err != nil {
		return nil, &attachError{Err: fmt.Errorf("生成重建快照失败: %w", err)}
	}
	if got := docrun.Fingerprint(prep.OriginalSpec); got != snap.docrunFingerprint {
		return nil, &attachError{Err: errors.New("容器配置在计划生成后被修改过，请重新扫描并生成计划")}
	}
	meta := map[string]any{
		"containerId":   snap.containerID,
		"containerName": snap.containerName,
		"strategy":      "docrun",
		"image":         ins.Config.Image,
		"removals":      snap.removals,
		"bindings":      snap.bindings,
		"createdAt":     time.Now().UTC().Format(time.RFC3339),
	}
	metaRaw, merr := json.MarshalIndent(meta, "", "  ")
	if merr != nil {
		return nil, &attachError{Err: fmt.Errorf("序列化备份元数据失败: %w", merr)}
	}
	if werr := os.WriteFile(filepath.Join(itemDir, "metadata.json"), metaRaw, 0o600); werr != nil {
		return nil, &attachError{Err: fmt.Errorf("写入备份元数据失败: %w", werr)}
	}
	if err := e.updateTxn(txnID, "prepared", func(p *txnPayload) {
		p.Items[idx].BackupPath = itemDir
		p.Items[idx].ServiceID = svcID
		p.Items[idx].Phase = "prepared"
		p.Items[idx].Docrun = prep
		p.Items[idx].Strategy = "docrun"
	}); err != nil {
		return nil, &attachError{Err: fmt.Errorf("写入事务日志失败: %w", err)}
	}
	backupPath := itemDir
	logger.add("info", fmt.Sprintf("已保存完整容器快照到 %s", itemDir))

	var undos []undoStep
	persistManagedState := func(runningIns *dockerx.ContainerInspect, status, lastErr string) error {
		containerID := snap.containerID
		if runningIns != nil {
			containerID = runningIns.ID
		}
		stableDir := filepath.Join(s.BackupDir, "services", svcID)
		if err := os.MkdirAll(stableDir, 0o700); err != nil {
			return err
		}
		snapRaw, err := json.MarshalIndent(docrun.ManagedSnapshot{
			Version:      1,
			ServiceID:    svcID,
			OriginalName: snap.containerName,
			OriginalID:   prep.OriginalID,
			Fingerprint:  prep.Fingerprint,
			OriginalSpec: prep.OriginalSpec,
			ManagedSpec:  prep.ManagedSpec,
			Secondary:    prep.Secondary,
			AttachedAt:   time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		if err != nil {
			return fmt.Errorf("序列化容器快照失败: %w", err)
		}
		if err := os.WriteFile(filepath.Join(stableDir, "container-snapshot.json"), snapRaw, 0o600); err != nil {
			return fmt.Errorf("写入容器快照失败: %w", err)
		}
		if err := os.WriteFile(filepath.Join(stableDir, "metadata.json"), metaRaw, 0o600); err != nil {
			return fmt.Errorf("写入稳定备份元数据失败: %w", err)
		}
		if err := e.store.SetMeta("serviceBackup:"+svcID, stableDir); err != nil {
			return fmt.Errorf("写入备份索引失败: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		return e.store.UpsertService(model.Service{
			ID:               svcID,
			ContainerID:      containerID,
			ContainerName:    snap.containerName,
			Status:           status,
			LastError:        lastErr,
			Bindings:         snap.bindings,
			IdleTimeoutSecs:  s.IdleTimeoutSecs,
			StartTimeoutSecs: s.StartTimeoutSecs,
			LastActiveAt:     now,
			CreatedAt:        now,
			DockerSocket:     c.Socket(),
			EnvironmentID:    snap.envID,
			Strategy:         "docrun",
		})
	}
	fail := func(cause error) (*appliedItem, error) {
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

	// 1. Pre-join the proxy container to the target's networks. The proxy
	// joins target networks itself when a service registers, with its own
	// short timeout — on slower engines (Colima) that join can cancel and
	// tear down the admin response. Doing it here (idempotent) removes the
	// slow path entirely.
	e.prejoinProxyNetworks(ctx, snap.envID, ins)

	// 1a. Register the service in the proxy (identical semantics to compose).
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
	failpoint.Hit("proxy_updated")
	if err := e.updateTxn(txnID, "proxy_updated", func(p *txnPayload) { p.Items[idx].Phase = "proxy_updated" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("代理已注册 %s（%s）", snap.containerName, strings.Join(sc.Ports, ", ")))

	// 2. Snapshot already persisted; recreate the container. RecreateManaged
	// keeps its own step-level rollback; the undo below covers everything
	// after RecreateManaged returned successfully.
	createdID, cleanup, err := docrun.RecreateManaged(ctx, eng, prep, func(phase string) error {
		perr := e.updateTxn(txnID, phase, func(p *txnPayload) {
			p.Items[idx].Phase = phase
		})
		if perr != nil {
			return fmt.Errorf("写入事务日志失败: %w", perr)
		}
		// Crash-injection hooks mirror the compose path: recovery must be
		// able to roll the sequence back from any of these states.
		failpoint.Hit("docrun_" + phase)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	undos = append(undos, undoStep{name: "回滚容器重建", fn: func() error {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return docrun.RollbackManaged(rctx, eng, prep, createdID)
	}})
	if err := e.updateTxn(txnID, "container_recreated", func(p *txnPayload) {
		p.Items[idx].Phase = "container_recreated"
		p.Items[idx].ReplacementID = createdID
	}); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", "容器已按快照重建，原端口已释放")

	// 3. Verify the replacement really stopped publishing the takeover ports.
	freshIns, ferr := c.InspectContainer(ctx, createdID)
	if ferr != nil {
		return fail(fmt.Errorf("重建容器不可读: %w", ferr))
	}
	for _, r := range snap.removals {
		if containerPublishes(freshIns, r) {
			return fail(fmt.Errorf("容器重建后仍然发布 %d/tcp（代理无法接管）", r.HostPort))
		}
	}

	// 4. Install the host port gateway.
	undos = append(undos, undoStep{name: "移除端口网关监听", fn: func() error {
		e.gw.RemoveService(svcID)
		return nil
	}})
	for _, b := range snap.bindings {
		route, err := e.gatewayRouteForEnv(ctx, snap.envID, svcID, snap.containerName, b, s)
		if err != nil {
			return fail(err)
		}
		if err := e.gwEnsureWithRetry(route, 20*time.Second); err != nil {
			return fail(fmt.Errorf("启动端口网关失败（%s:%d）: %w", hostLabel(b.HostIP), b.HostPort, err))
		}
	}
	failpoint.Hit("gateway_ready")
	if err := e.updateTxn(txnID, "gateway_ready", func(p *txnPayload) { p.Items[idx].Phase = "gateway_ready" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("端口网关已监听: %s", bindingsText(snap.bindings)))

	// 5. Verify the original address works (also wakes the container).
	if err := e.testBindings(ctx, snap.bindings); err != nil {
		return fail(fmt.Errorf("原端口连通性测试失败: %w", err))
	}
	if err := e.updateTxn(txnID, "verified", func(p *txnPayload) { p.Items[idx].Phase = "verified" }); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", "原端口连通性测试通过")

	failpoint.Hit("before_database_commit")
	if err := persistManagedState(freshIns, "managed_running", ""); err != nil {
		return fail(fmt.Errorf("保存托管状态失败: %w", err))
	}
	undos = append(undos, undoStep{name: "删除托管记录", fn: func() error {
		_ = e.store.DeleteMeta("serviceBackup:" + svcID)
		return e.store.DeleteService(svcID)
	}})
	// 6. Only now drop the rollback anchor. A failure here is not fatal: the
	// stale anchor keeps running stopped and is cleaned up on the next attach
	// of the same name or by `docker rm` — never worth failing the attach for.
	if err := cleanup(true); err != nil {
		logger.add("warn", "移除原容器备份失败（不影响接入，可稍后手动清理）: "+err.Error())
	} else {
		logger.add("info", "已移除原容器备份（数据卷与绑定目录均保留）")
	}
	if err := e.updateTxn(txnID, "committed_item", func(p *txnPayload) {
		p.Items[idx].ServiceID = svcID
		p.Items[idx].Phase = "committed_item"
	}); err != nil {
		return fail(fmt.Errorf("写入事务日志失败: %w", err))
	}
	logger.add("info", fmt.Sprintf("%s 接入成功：%s → 代理 → 容器", snap.containerName, bindingsText(snap.bindings)))
	return &appliedItem{
		snap: snap, serviceID: svcID, backupPath: backupPath,
		strategy: "docrun", prep: prep, replacementID: createdID,
	}, nil
}

// rollbackDocrunApplied reverts one successfully attached docrun item: remove
// the replacement, restore the original container from the snapshot, delete
// the managed record. The proxy config is restored by the caller.
func (e *Engine) rollbackDocrunApplied(ctx context.Context, logger *opLogger, ai appliedItem) error {
	eng, err := e.docrunEngineForService(ai.serviceID)
	if err != nil {
		return err
	}
	e.gw.RemoveService(ai.serviceID)
	if ai.prep == nil {
		return fmt.Errorf("缺少容器重建快照，无法回滚")
	}
	if err := docrun.RollbackManaged(ctx, eng, ai.prep, ai.replacementID); err != nil {
		return err
	}
	// The original container is running with its ports again; verify quickly.
	ins, werr := docrun.InspectRunning(ctx, eng, ai.snap.containerName, 2*time.Minute)
	if werr != nil {
		return werr
	}
	for _, r := range ai.snap.removals {
		if !containerPublishes(ins, r) {
			return fmt.Errorf("原始端口 %d/tcp 未重新发布", r.HostPort)
		}
	}
	if err := deleteServiceRecord(e, ai.serviceID); err != nil {
		return err
	}
	_ = e.store.DeleteMeta("serviceBackup:" + ai.serviceID)
	return nil
}

// docrunEngineForService resolves the strategy engine for a service's env.
func (e *Engine) docrunEngineForService(serviceID string) (docrun.Engine, error) {
	svc, ok, err := e.store.GetService(serviceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("托管服务不存在: %s", serviceID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := e.clientForEnv(ctx, svc.EnvironmentID)
	if err != nil {
		return nil, err
	}
	return docrunEngine{c: c}, nil
}
