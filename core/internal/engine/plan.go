package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/scan"
)

type planEntry struct {
	plan      model.Plan
	createdAt time.Time
	allowLan  bool
	applying  bool
	items     []planSnapshot
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
}

// CreatePlan validates the selected containers and produces a dry-run plan.
func (e *Engine) CreatePlan(ctx context.Context, ids []string, allowLan bool) (model.Plan, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

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
	c, err := e.dockerClient(ctx)
	if err != nil {
		return plan, err
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

	proxyOK := false
	if admin, err := e.adminClient(); err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := admin.Ping(pingCtx); err == nil {
			proxyOK = true
		}
		cancel()
	}
	if !proxyOK {
		plan.Blockers = append(plan.Blockers, model.Reason{
			Code:     "proxy_unavailable",
			Message:  "Lazy TCP Proxy 未运行或管理接口不可达；请先在「设置」中安装并启动代理",
			Severity: "blocker",
		})
	}

	cli, cliErr := e.cliRunner()
	if cliErr != nil {
		plan.Blockers = append(plan.Blockers, model.Reason{
			Code:     "compose_cli_unavailable",
			Message:  "Docker Compose CLI 不可用，无法安全校验和重建服务: " + cliErr.Error(),
			Severity: "blocker",
		})
	}
	published, _ := e.proxyPublishedPorts(ctx)
	seen := map[string]bool{}
	entry := &planEntry{createdAt: time.Now(), allowLan: allowLan}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		ins, err := c.InspectContainer(ctx, id)
		if err != nil {
			plan.Blockers = append(plan.Blockers, model.Reason{
				Code: "container_gone", Message: fmt.Sprintf("容器 %s 无法读取: %v", shortID(id), err), Severity: "blocker",
			})
			continue
		}
		cont := scan.Convert(ins, scan.Options{
			ExcludeImagePatterns: s.ExcludeImagePatterns,
			ProxyContainerName:   proxy.ContainerName,
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
		item, snap, blockers := e.buildPlanItem(ctx, cont, &plan, &used, s.ProxyPortStart, s.ProxyPortEnd, cli, published)
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

	e.plansMu.Lock()
	for id, p := range e.plans {
		if time.Since(p.createdAt) > 10*time.Minute {
			delete(e.plans, id)
		}
	}
	e.plans[plan.ID] = entry
	e.plansMu.Unlock()
	return plan, nil
}

func (e *Engine) buildPlanItem(ctx context.Context, cont model.Container, plan *model.Plan, used *map[int]bool, rangeStart, rangeEnd int, cli composeCLI, published map[int]bool) (*model.PlanItem, *planSnapshot, []model.Reason) {
	var blockers []model.Reason
	block := func(code, msg string) {
		blockers = append(blockers, model.Reason{Code: code, Message: msg, Severity: "blocker"})
		plan.Blockers = append(plan.Blockers, model.Reason{Code: code, Message: cont.Name + "：" + msg, Severity: "blocker"})
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
		if len(published) > 0 && !published[proxyPort] {
			block("proxy_range_mismatch", fmt.Sprintf("代理容器没有发布端口 %d；修改端口范围后需要在设置中重新安装/启动代理", proxyPort))
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
	}
	return &item, snap, nil
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
