package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/deployment/docrun"
	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/scan"
)

type planEntry struct {
	plan             model.Plan
	createdAt        time.Time
	allowLan         bool
	applying         bool
	dockerGeneration int // plan snapshot was taken on this environment
	items            []planSnapshot
}

type planSnapshot struct {
	containerID    string
	containerName  string
	composeProject string
	composeService string
	composeFile    string
	workingDir     string
	content        []byte
	removals       []model.PortBinding
	removedDecls   []compose.PortDecl
	proxyMappings  []model.ProxyMapping
	bindings       []model.Binding
	requiresLan    bool
	// strategy: "compose" | "docrun". envID pins the item to the environment
	// the plan was generated against.
	strategy string
	envID    string
	// takeoverKeys lists the container port keys whose TCP bindings the
	// gateway takes over (docrun only).
	takeoverKeys []string
	// docrunFingerprint is the plan-time fingerprint of the recreation spec;
	// attach re-verifies it before mutating anything.
	docrunFingerprint string
	// docrun carries the full attach snapshot once rebuilt at attach time
	// (plan-time items carry only the fingerprint).
	docrun *docrun.PreparedAttach
}

// CreatePlan validates the selected containers and produces a dry-run plan.
// It is deliberately read-only: no writeMu is held. An attach that races the
// plan is caught at apply time — attachOne re-reads the Compose file and
// refuses when it no longer matches the plan's snapshot.
func (e *Engine) CreatePlan(ctx context.Context, ids []string, allowLan bool) (model.Plan, error) {
	e.mu.Lock()
	planGeneration := e.dockerGeneration
	e.mu.Unlock()

	plan := model.Plan{
		ID:        newID("plan"),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "ready",
		Blockers:  []model.Reason{},
		Warnings:  []model.Reason{},
		Items:     []model.PlanItem{},
	}
	if len(ids) == 0 {
		return plan, errors.New("没有选择容器")
	}
	// Containers are inspected per resolved environment; the primary client
	// is only a fallback candidate inside the loop.
	if _, derr := e.dockerClient(ctx); derr != nil {
		// Primary unreachable is not fatal for a plan that may target other
		// enabled environments; the per-item resolution reports accurately.
		e.logger.Printf("info", "主环境不可达，将尝试其他已启用环境: %v", derr)
	}
	s := e.settings.Get()
	services, err := e.store.ListServices()
	if err != nil {
		return plan, err
	}
	managed := map[string]string{}
	used := map[int]bool{}
	for _, svc := range services {
		managed[svc.ContainerID] = svc.ContainerName
		for _, b := range svc.Bindings {
			used[b.ProxyPort] = true
		}
	}
	// Also reserve ports of other live (not yet applied) plans.
	e.plansMu.Lock()
	for id, p := range e.plans {
		if time.Since(p.createdAt) > 10*time.Minute {
			delete(e.plans, id)
			continue
		}
		for _, it := range p.items {
			for _, b := range it.bindings {
				used[b.ProxyPort] = true
			}
		}
	}
	e.plansMu.Unlock()

	// Enabled environments: every one may contribute containers to the plan;
	// they must still all resolve to a single environment for the operation.
	envs, _ := e.store.ListEnvironments()
	var enabledEnvIDs []string
	for _, env := range envs {
		if env.Enabled {
			enabledEnvIDs = append(enabledEnvIDs, env.ID)
		}
	}

	// checkPlanEnvProxy validates the environment's proxy BEFORE any item is
	// planned: an operation drives exactly one proxy baseline, so the checks
	// must run against the environment the containers actually live on.
	checkPlanEnvProxy := func(envID string) bool {
		envName := "默认环境"
		for _, env := range envs {
			if env.ID == envID && env.Name != "" {
				envName = env.Name
			}
		}
		ok := true
		block := func(code, msg string) {
			plan.Blockers = append(plan.Blockers, model.Reason{Code: code, Message: msg, Severity: "blocker"})
			ok = false
		}
		proxyOK := false
		if admin, err := e.adminClientForEnv(envID); err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := admin.Ping(pingCtx); err == nil {
				proxyOK = true
			}
			cancel()
		}
		if !proxyOK {
			block("proxy_unavailable", fmt.Sprintf("环境 %s 的 Lazy TCP Proxy 未运行或管理接口不可达；请先安装并启动代理", envName))
			return false
		}
		envClient, cerr := e.clientForEnv(ctx, envID)
		if cerr != nil {
			block("environment_unreachable", fmt.Sprintf("环境 %s 不可达: %v", envName, cerr))
			return false
		}
		proxyIns, ierr := envClient.InspectContainer(ctx, e.proxySpecForEnv(envID).ContainerName)
		switch {
		case ierr != nil:
			// The admin endpoint answers on 127.0.0.1 regardless of which
			// Docker instance hosts the proxy. The plan must not wire targets
			// into a proxy that cannot reach this environment's containers.
			block("proxy_not_on_current_instance", fmt.Sprintf("Lazy TCP Proxy 未运行在环境 %s 上；请先对该环境执行 proxy-setup 再生成接入计划", envName))
		case !proxyIns.State.Running:
			block("proxy_not_running", fmt.Sprintf("环境 %s 的 Lazy TCP Proxy 容器存在但未运行；请执行 proxy-setup", envName))
		}
		if _, cliErr := e.cliForEnv(ctx, envID); cliErr != nil {
			block("compose_cli_unavailable", "Docker Compose CLI 不可用，无法安全校验和重建服务: "+cliErr.Error())
		}
		if _, pubErr := e.proxyPublishedPortsForEnv(ctx, envID); pubErr != nil {
			block("proxy_ports_unknown", "无法确认代理端口发布状态（"+pubErr.Error()+"）；请检查代理容器后再试")
		}
		return ok
	}
	seen := map[string]bool{}
	entry := &planEntry{createdAt: time.Now(), allowLan: allowLan, dockerGeneration: planGeneration}
	e.mu.Lock()
	if e.dockerGeneration != planGeneration {
		// The environment switched while the plan was being generated.
		e.mu.Unlock()
		return plan, errors.New("生成计划期间 Docker 环境已切换，请重新扫描")
	}
	e.mu.Unlock()
	planEnvID := ""
	rangeStart, rangeEnd := 0, 0
	var published map[int]PublishedPort
	var pubErr error
	// Preflight the PRIMARY environment before any item: proxy availability
	// is a plan-level condition that must surface even when every item fails
	// classification (zero-item plans still report the reason).
	checkedEnvID := e.currentEnvID()
	checkPlanEnvProxy(checkedEnvID)

	// perEnvCLI memoises the compose CLI per resolved environment so every
	// item's CLI traffic goes to the environment that actually holds the
	// container (a merged multi-env scan yields ids from several).
	perEnvCLI := map[string]composeCLI{}
	resolveCLI := func(envID string) composeCLI {
		if got, ok := perEnvCLI[envID]; ok {
			return got
		}
		got, err := e.cliForEnv(ctx, envID)
		if err != nil {
			got = nil
		}
		perEnvCLI[envID] = got
		return got
	}

	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		// Resolve which environment actually owns this container: try the
		// primary first, then the other enabled ones. An id exists on exactly
		// one engine, so the first inspect hit is authoritative.
		envID := ""
		var ins *dockerx.ContainerInspect
		var insErr error
		for _, candID := range append([]string{e.currentEnvID()}, enabledEnvIDs...) {
			if candID == "" {
				continue
			}
			cand, cerr := e.clientForEnv(ctx, candID)
			if cerr != nil {
				continue
			}
			probe, perr := cand.InspectContainer(ctx, id)
			if perr == nil {
				envID, ins = candID, probe
				break
			}
			if !dockerx.IsErrNotFound(perr) {
				insErr = perr
			}
		}
		if ins == nil {
			msg := fmt.Sprintf("容器 %s 无法读取", shortID(id))
			if insErr != nil {
				msg += ": " + insErr.Error()
			} else {
				msg += ": 不存在于任何已启用环境"
			}
			plan.Blockers = append(plan.Blockers, model.Reason{
				Code: "container_gone", Message: msg, Severity: "blocker",
			})
			continue
		}
		cont := scan.Convert(ins, scan.Options{
			ExcludeImagePatterns: s.ExcludeImagePatterns,
			ProxyContainerName:   e.proxySpecForEnv(envID).ContainerName,
		})
		if name, ok := managed[cont.ID]; ok {
			plan.Blockers = append(plan.Blockers, model.Reason{
				Code: "already_managed", Message: fmt.Sprintf("%s 已由 OrbSleep 托管（%s）", cont.Name, name), Severity: "blocker",
			})
			continue
		}
		if cont.Classification.Verdict == "excluded" {
			for _, r := range cont.Classification.Reasons {
				if r.Severity == "blocker" {
					plan.Blockers = append(plan.Blockers, model.Reason{
						Code: r.Code, Message: fmt.Sprintf("%s：%s", cont.Name, r.Message), Severity: "blocker",
					})
				}
			}
			continue
		}
		if planEnvID == "" {
			// First resolved item fixes the plan's environment: its proxy is
			// the only baseline this operation may drive.
			planEnvID = envID
			if planEnvID != checkedEnvID {
				if !checkPlanEnvProxy(planEnvID) {
					continue
				}
				checkedEnvID = planEnvID
			}
			spec := e.proxySpecForEnv(planEnvID)
			rangeStart, rangeEnd = spec.Start, spec.End
			if rangeStart <= 0 || rangeEnd <= rangeStart {
				// The primary environment keeps the global settings range.
				rangeStart, rangeEnd = s.ProxyPortStart, s.ProxyPortEnd
			}
			published, pubErr = e.proxyPublishedPortsForEnv(ctx, planEnvID)
			if pubErr != nil {
				plan.Blockers = append(plan.Blockers, model.Reason{
					Code:     "proxy_ports_unknown",
					Message:  "无法确认代理端口发布状态（" + pubErr.Error() + "）；请检查代理容器后再试",
					Severity: "blocker",
				})
				continue
			}
		} else if envID != planEnvID {
			plan.Blockers = append(plan.Blockers, model.Reason{
				Code:     "mixed_environments",
				Message:  "一次接入只能操作同一个容器环境；请分别选择每个环境的容器生成计划",
				Severity: "blocker",
			})
			continue
		}
		item, snap, blockers := e.buildPlanItem(ctx, ins, cont, &plan, &used, rangeStart, rangeEnd, resolveCLI(envID), published, envID)
		if len(blockers) > 0 {
			continue
		}
		if !allowLan && item.RequiresLanConfirmation {
			plan.Blockers = append(plan.Blockers, model.Reason{
				Code:     "lan_confirmation_required",
				Message:  fmt.Sprintf("%s 的端口绑定所有网卡，需要显式确认后才会执行", cont.Name),
				Severity: "blocker",
			})
			continue
		}
		plan.Items = append(plan.Items, *item)
		entry.items = append(entry.items, *snap)
	}
	if len(plan.Items) > 0 {
		plan.Warnings = append(plan.Warnings, model.Reason{
			Code:     "interruption",
			Message:  "重建容器期间原端口会短暂不可用（通常 1-5 秒）；已接入服务之后由代理按需唤醒",
			Severity: "warning",
		})
	}
	if len(plan.Blockers) > 0 || len(plan.Items) == 0 {
		plan.Status = "blocked"
	}
	entry.plan = plan

	// Publish under the SAME environment lock that guards the generation: a
	// switch during the container walk must not publish a plan the UI can show
	// and select, whose items describe containers of a Docker instance that is
	// already gone. Checking the generation and then inserting into the map as
	// two steps left exactly that window open.
	if !e.publishPlan(entry, planGeneration) {
		return plan, errors.New("生成计划期间 Docker 环境已切换，请重新扫描")
	}
	return plan, nil
}

// publishPlan stores a plan only while its Docker generation is current, and
// evicts expired entries in the same critical section. Lock order is
// e.mu → e.plansMu; no path may acquire them in the opposite order.
func (e *Engine) publishPlan(entry *planEntry, planGeneration int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dockerGeneration != planGeneration {
		return false
	}
	e.plansMu.Lock()
	defer e.plansMu.Unlock()
	for id, p := range e.plans {
		if time.Since(p.createdAt) > 10*time.Minute {
			delete(e.plans, id)
		}
	}
	e.plans[entry.plan.ID] = entry
	return true
}

// checkProxyPublishedPort validates that the proxy port the gateway is about to
// use is genuinely published on the local IPv4 loopback interface. An empty
// binding set means "nothing published" and must block, never be skipped. Every
// binding of the port is checked: the gateway dials 127.0.0.1, so anything else
// — the wildcard address, a routable interface, or an IPv6-only loopback — is
// either an exposure or an unreachable route, and both have to stop the plan.
func checkProxyPublishedPort(published map[int]PublishedPort, proxyPort int, block func(code, msg string)) {
	checkProxyPublishedPortMode(published, proxyPort, false, block)
}

// checkProxyPublishedPortMode validates the proxy port's publication. For
// VM-forwarded engines (acceptWildcard) the in-VM binding is 0.0.0.0 by
// design — the runtime's host forwarder lands it on the macOS loopback — so
// the exposure check is replaced by an explicit macOS-side loopback probe.
func checkProxyPublishedPortMode(published map[int]PublishedPort, proxyPort int, acceptWildcard bool, block func(code, msg string)) {
	pb, ok := published[proxyPort]
	if !ok {
		block("proxy_port_unpublished", fmt.Sprintf("代理容器没有发布端口 %d；修改端口范围后需要在设置中重新安装/启动代理", proxyPort))
		return
	}
	if !acceptWildcard {
		// The gateway forwards to 127.0.0.1:<proxyPort>; a binding on a different
		// host port or a non-loopback interface would make the service
		// unreachable even though the container looks healthy.
		if routable := pb.routableBindings(); len(routable) > 0 {
			block("proxy_port_exposed", fmt.Sprintf("代理端口 %d 发布在 %s，并非仅本机 IPv4 回环，拒绝接管（存在对外暴露风险）",
				proxyPort, strings.Join(routable, "、")))
			return
		}
	}
	hostPort := pb.loopbackV4Port()
	if acceptWildcard && hostPort == 0 {
		hostPort = pb.wildcardV4Port()
		if hostPort != 0 {
			// Verify the macOS loopback actually accepts connections: the VM
			// forwarder binds (0.0.0.0) on the host and relays into the VM.
			// A completed dial proves the forwarder exists; the in-VM
			// listener may legitimately not exist until the service is
			// registered on that port.
			if vmForwarded("127.0.0.1", proxyPort) {
				hostPort = proxyPort
			} else {
				block("proxy_port_not_forwarded", fmt.Sprintf("代理端口 %d 未出现在本机回环（VM 转发未生效）；请确认该环境已启动并重新执行 proxy-setup", proxyPort))
				return
			}
		}
	}
	switch {
	case hostPort == 0:
		block("proxy_port_not_loopback", fmt.Sprintf("代理端口 %d 没有发布在 127.0.0.1（当前: %s）；网关只连接本机 IPv4 回环，请重新安装代理",
			proxyPort, strings.Join(bindingList(pb), "、")))
	case hostPort != proxyPort:
		block("proxy_port_mismatch", fmt.Sprintf("代理端口 %d 发布在 127.0.0.1:%d，与网关目标不一致；请重新安装代理", proxyPort, hostPort))
	}
}

func bindingList(p PublishedPort) []string {
	out := make([]string, 0, len(p.Bindings))
	for _, b := range p.Bindings {
		addr := b.HostIP
		if addr == "" {
			addr = "0.0.0.0"
		}
		out = append(out, fmt.Sprintf("%s:%d", addr, b.HostPort))
	}
	return out
}

func (e *Engine) buildPlanItem(ctx context.Context, ins *dockerx.ContainerInspect, cont model.Container, plan *model.Plan, used *map[int]bool, rangeStart, rangeEnd int, cli composeCLI, published map[int]PublishedPort, envID string) (*model.PlanItem, *planSnapshot, []model.Reason) {
	var blockers []model.Reason
	block := func(code, msg string) {
		blockers = append(blockers, model.Reason{Code: code, Message: msg, Severity: "blocker"})
		plan.Blockers = append(plan.Blockers, model.Reason{Code: code, Message: cont.Name + "：" + msg, Severity: "blocker"})
	}
	if cont.Strategy == "docrun" {
		return e.buildDocrunPlanItem(ctx, ins, cont, plan, used, rangeStart, rangeEnd, published, envID)
	}
	if len(cont.ComposeConfigFiles) != 1 {
		block("compose_file_unresolved", "需要唯一的 Compose 配置文件，当前无法定位")
		return nil, nil, blockers
	}
	file := cont.ComposeConfigFiles[0]
	if _, err := os.Stat(file); err != nil {
		block("compose_file_missing", "Compose 配置文件不存在: "+file)
		return nil, nil, blockers
	}
	content, err := os.ReadFile(file)
	if err != nil {
		block("compose_file_unreadable", "无法读取 Compose 文件: "+err.Error())
		return nil, nil, blockers
	}
	if cli == nil {
		block("compose_cli_unavailable", "Docker Compose CLI 不可用，无法校验 Compose 配置")
		return nil, nil, blockers
	}
	if _, err := cli.ComposeConfig(ctx, cont.ComposeProject, file, cont.ComposeWorkingDir); err != nil {
		block("compose_invalid", "Compose 有效配置校验失败（可能缺少 .env 变量或存在语法错误）: "+err.Error())
		return nil, nil, blockers
	}
	decls, err := compose.ReadDecls(content, cont.ComposeService)
	if err != nil {
		block("compose_unsupported", fmt.Sprintf("无法解析服务 %q 的端口声明: %v", cont.ComposeService, err))
		return nil, nil, blockers
	}
	candidates := cont.Classification.ProxyCandidates
	if len(candidates) == 0 {
		block("no_candidates", "没有可接管的 TCP 端口")
		return nil, nil, blockers
	}
	acceptWildcard := !e.proxySpecForEnv(envID).LoopbackPublish
	var removals []model.PortBinding
	var removedDecls []compose.PortDecl
	for _, cand := range candidates {
		found := false
		for _, d := range decls {
			if compose.BindingEqual(d.Binding, cand) {
				removals = append(removals, cand)
				removedDecls = append(removedDecls, d)
				found = true
				break
			}
		}
		if !found {
			block("compose_port_mismatch", fmt.Sprintf("Compose 文件中找不到 %s:%d->%d/tcp 的声明（可能使用了变量、端口范围或多文件合并）",
				hostLabel(cand.HostIP), cand.HostPort, cand.ContainerPort))
		}
	}
	if len(blockers) > 0 {
		return nil, nil, blockers
	}

	var mappings []model.ProxyMapping
	var bindings []model.Binding
	requiresLan := false
	for _, cand := range removals {
		proxyPort, ok := allocateProxyPort(*used, rangeStart, rangeEnd)
		if !ok {
			block("proxy_ports_exhausted", fmt.Sprintf("代理端口范围 %d-%d 已用尽，请在设置中扩大范围", rangeStart, rangeEnd))
			return nil, nil, blockers
		}
		// The gateway forwards to 127.0.0.1:<proxyPort>; every binding published
		// for the allocated proxy port must be exactly that address and port
		// (VM-forwarded engines accept the in-VM wildcard with a loopback probe).
		before := len(blockers)
		checkProxyPublishedPortMode(published, proxyPort, acceptWildcard, block)
		if len(blockers) > before {
			return nil, nil, blockers
		}
		*used = withPort(*used, proxyPort)
		mappings = append(mappings, model.ProxyMapping{ListenPort: proxyPort, TargetPort: cand.ContainerPort})
		bindings = append(bindings, model.Binding{
			HostIP: cand.HostIP, HostPort: cand.HostPort, ContainerPort: cand.ContainerPort,
			ProxyPort: proxyPort, Protocol: "tcp",
		})
		if cand.HostIP == "" {
			requiresLan = true
		}
	}

	risks := []model.Reason{
		{Code: "recreate", Message: fmt.Sprintf("将执行 docker compose up -d --no-deps --force-recreate %s", cont.ComposeService), Severity: "info"},
		{Code: "data_preserved", Message: "命名卷、Bind Mount、网络、环境变量与健康检查均保持不变", Severity: "info"},
	}
	for _, w := range cont.Classification.Warnings {
		risks = append(risks, w)
	}
	if requiresLan {
		risks = append(risks, model.Reason{
			Code: "lan_exposure", Message: "原始端口绑定所有网卡，接入后仍保持原绑定范围（不会自动扩大或收窄）", Severity: "warning",
		})
	}
	item := model.PlanItem{
		ContainerID:             cont.ID,
		ContainerName:           cont.Name,
		ComposeProject:          cont.ComposeProject,
		ComposeService:          cont.ComposeService,
		ComposeFile:             file,
		Removals:                removals,
		ProxyMappings:           mappings,
		Bindings:                bindings,
		RequiresLanConfirmation: requiresLan,
		Risks:                   risks,
		Diff:                    compose.DiffForRemovals(string(content), removedDecls, file),
		Steps: []string{
			"备份 Compose 文件与代理配置",
			"写入 Lazy TCP Proxy 服务配置并验证",
			"从 Compose 文件移除目标端口映射",
			"校验 Compose 有效配置（仅端口变化）",
			"仅重建目标服务",
			"启动 macOS 端口网关监听",
			"通过原端口完成连通性测试",
		},
	}
	snap := &planSnapshot{
		containerID:    cont.ID,
		containerName:  cont.Name,
		composeProject: cont.ComposeProject,
		composeService: cont.ComposeService,
		composeFile:    file,
		workingDir:     cont.ComposeWorkingDir,
		content:        content,
		removals:       removals,
		removedDecls:   removedDecls,
		proxyMappings:  mappings,
		bindings:       bindings,
		requiresLan:    requiresLan,
		strategy:       "compose",
		envID:          envID,
	}
	return &item, snap, nil
}

// buildDocrunPlanItem plans the attach of a container that has no Compose
// identity. There is no file to validate — the safety comes from the full
// inspect snapshot that attach persists before the first mutation.
func (e *Engine) buildDocrunPlanItem(ctx context.Context, ins *dockerx.ContainerInspect, cont model.Container, plan *model.Plan, used *map[int]bool, rangeStart, rangeEnd int, published map[int]PublishedPort, envID string) (*model.PlanItem, *planSnapshot, []model.Reason) {
	var blockers []model.Reason
	block := func(code, msg string) {
		blockers = append(blockers, model.Reason{Code: code, Message: msg, Severity: "blocker"})
		plan.Blockers = append(plan.Blockers, model.Reason{Code: code, Message: cont.Name + "：" + msg, Severity: "blocker"})
	}
	candidates := cont.Classification.ProxyCandidates
	if len(candidates) == 0 {
		block("no_candidates", "没有可接管的 TCP 端口")
		return nil, nil, blockers
	}
	var removals []model.PortBinding
	var mappings []model.ProxyMapping
	var bindings []model.Binding
	var keys []string
	requiresLan := false
	acceptWildcard := !e.proxySpecForEnv(envID).LoopbackPublish
	for _, cand := range candidates {
		proxyPort, ok := allocateProxyPort(*used, rangeStart, rangeEnd)
		if !ok {
			block("proxy_ports_exhausted", fmt.Sprintf("代理端口范围 %d-%d 已用尽，请在设置中扩大范围", rangeStart, rangeEnd))
			return nil, nil, blockers
		}
		before := len(blockers)
		checkProxyPublishedPortMode(published, proxyPort, acceptWildcard, block)
		if len(blockers) > before {
			return nil, nil, blockers
		}
		*used = withPort(*used, proxyPort)
		removals = append(removals, cand)
		mappings = append(mappings, model.ProxyMapping{ListenPort: proxyPort, TargetPort: cand.ContainerPort})
		bindings = append(bindings, model.Binding{
			HostIP: cand.HostIP, HostPort: cand.HostPort, ContainerPort: cand.ContainerPort,
			ProxyPort: proxyPort, Protocol: "tcp",
		})
		keys = append(keys, fmt.Sprintf("%d/%s", cand.ContainerPort, cand.Protocol))
		if cand.HostIP == "" {
			requiresLan = true
		}
	}

	// Plan-time fingerprint: attach refuses to run when the container config
	// drifted after this plan was generated.
	prep, err := docrun.PrepareAttach(ins, "", keys)
	if err != nil {
		block("docrun_snapshot_failed", "无法生成容器重建快照: "+err.Error())
		return nil, nil, blockers
	}

	risks := []model.Reason{
		{Code: "recreate", Message: "将通过 Docker Engine API 按完整配置快照重建容器（保留镜像/环境变量/挂载/网络/资源限制）", Severity: "info"},
		{Code: "data_preserved", Message: "具名卷、Bind Mount、匿名卷按原卷名重新挂载；重建期间服务短暂不可用", Severity: "info"},
	}
	for _, w := range cont.Classification.Warnings {
		risks = append(risks, w)
	}
	if requiresLan {
		risks = append(risks, model.Reason{
			Code: "lan_exposure", Message: "原始端口绑定所有网卡，接入后仍保持原绑定范围（不会自动扩大或收窄）", Severity: "warning",
		})
	}
	item := model.PlanItem{
		ContainerID:             cont.ID,
		ContainerName:           cont.Name,
		Removals:                removals,
		ProxyMappings:           mappings,
		Bindings:                bindings,
		RequiresLanConfirmation: requiresLan,
		Risks:                   risks,
		Strategy:                "docrun",
		EnvironmentID:           envID,
		Diff:                    "",
		Steps: []string{
			"保存完整容器配置快照（含挂载、网络、环境变量、资源限制）",
			"写入 Lazy TCP Proxy 服务配置并验证",
			"停止原容器并重命名为回滚锚点",
			"按快照重建容器（移除被接管的 TCP 端口）",
			"启动新容器并恢复附属网络",
			"启动 macOS 端口网关监听",
			"通过原端口完成连通性测试",
			"验证通过后移除原容器备份",
		},
	}
	snap := &planSnapshot{
		containerID:       cont.ID,
		containerName:     cont.Name,
		removals:          removals,
		proxyMappings:     mappings,
		bindings:          bindings,
		requiresLan:       requiresLan,
		strategy:          "docrun",
		envID:             envID,
		takeoverKeys:      keys,
		docrunFingerprint: prep.Fingerprint,
	}
	return &item, snap, nil
}

// vmForwarded reports whether a TCP dial to the loopback port completes —
// the VM forwarder accepts (then relays), so a completed handshake proves
// forwarding exists regardless of what listens inside the VM.
func vmForwarded(host string, port int) bool {
	d := netDialer(1500 * time.Millisecond)
	conn, err := d.DialContext(context.Background(), "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func allocateProxyPort(used map[int]bool, start, end int) (int, bool) {
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, true
		}
	}
	return 0, false
}

func withPort(m map[int]bool, p int) map[int]bool {
	m[p] = true
	return m
}

func hostLabel(ip string) string {
	if ip == "" {
		return "0.0.0.0"
	}
	return ip
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
