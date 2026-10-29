// Package scan converts Docker inspect data into OrbSleep containers and decides
// whether a container is safe to put behind the lazy TCP proxy.
package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
)

// Options tunes classification.
type Options struct {
	ExcludeImagePatterns []string
	ProxyContainerName   string
}

const (
	verdictEligible    = "eligible"
	verdictNeedsReview = "needs_review"
	verdictExcluded    = "excluded"
)

// Convert builds a model.Container from inspect data.
func Convert(ins *dockerx.ContainerInspect, opts Options) model.Container {
	labels := ins.Config.Labels
	c := model.Container{
		ID:                     ins.ID,
		Name:                   strings.TrimPrefix(ins.Name, "/"),
		Image:                  ins.Config.Image,
		State:                  ins.State.Status,
		CreatedAt:              ins.Created,
		StartedAt:              ins.State.StartedAt,
		ComposeProject:         labels["com.docker.compose.project"],
		ComposeService:         labels["com.docker.compose.service"],
		ComposeWorkingDir:      labels["com.docker.compose.project.working_dir"],
		ComposeContainerNumber: parseInt(labels["com.docker.compose.container-number"]),
		Oneoff:                 strings.EqualFold(labels["com.docker.compose.oneoff"], "true"),
		NetworkMode:            ins.HostConfig.NetworkMode,
		RestartPolicy:          ins.HostConfig.RestartPolicy.Name,
		AutoRemove:             ins.HostConfig.AutoRemove,
		Privileged:             ins.HostConfig.Privileged,
		ReadonlyRootfs:         ins.HostConfig.ReadonlyRootfs,
		Mounts:                 []model.Mount{},
		Ports:                  []model.PortBinding{},
		Networks:               []model.Network{},
	}
	c.ComposeConfigFiles = splitConfigFiles(labels["com.docker.compose.project.config_files"])

	if ins.Config.Healthcheck != nil {
		c.Healthcheck.Configured = true
		c.Healthcheck.Test = strings.Join(ins.Config.Healthcheck.Test, " ")
	}
	if ins.State.Health != nil {
		c.Healthcheck.Status = ins.State.Health.Status
	}

	for name, ep := range ins.NetworkSettings.Networks {
		c.Networks = append(c.Networks, model.Network{Name: name, IP: ep.IPAddress, Gateway: ep.Gateway})
	}
	sort.Slice(c.Networks, func(i, j int) bool { return c.Networks[i].Name < c.Networks[j].Name })

	for _, m := range ins.Mounts {
		c.Mounts = append(c.Mounts, model.Mount{
			Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, ReadOnly: !m.RW,
		})
	}

	c.Ports = extractPorts(ins)
	c.Classification = Classify(&c, opts)
	return c
}

// extractPorts reads HostConfig.PortBindings (and falls back to the runtime map).
func extractPorts(ins *dockerx.ContainerInspect) []model.PortBinding {
	seen := map[string]bool{}
	var out []model.PortBinding
	add := func(key string, b dockerx.PortBinding) {
		proto := "tcp"
		portStr := key
		if i := strings.Index(key, "/"); i >= 0 {
			portStr, proto = key[:i], key[i+1:]
		}
		containerPort, err := strconv.Atoi(portStr)
		if err != nil || containerPort <= 0 {
			return
		}
		hostPort, err := strconv.Atoi(b.HostPort)
		if err != nil {
			return
		}
		hostIP := normalizeHostIP(b.HostIP)
		k := fmt.Sprintf("%s|%d|%d|%s", hostIP, hostPort, containerPort, proto)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, model.PortBinding{HostIP: hostIP, HostPort: hostPort, ContainerPort: containerPort, Protocol: proto})
	}
	for key, bindings := range ins.HostConfig.PortBindings {
		for _, b := range bindings {
			add(key, b)
		}
	}
	// NetworkSettings.Ports is authoritative for the ports the container is
	// actually publishing right now (e.g. after a restart with -P).
	for key, bindings := range ins.NetworkSettings.Ports {
		for _, b := range bindings {
			add(key, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].ContainerPort < out[j].ContainerPort
	})
	return out
}

// Classify evaluates a container and fills c.Classification.
func Classify(c *model.Container, opts Options) model.Classification {
	cl := model.Classification{Verdict: verdictEligible, Reasons: []model.Reason{}, Warnings: []model.Reason{}, ProxyCandidates: []model.PortBinding{}}
	if opts.ProxyContainerName != "" && (c.Name == opts.ProxyContainerName || c.ComposeProject == "orbsleep-proxy") {
		cl.Reasons = append(cl.Reasons, model.Reason{
			Code: "orbsleep_proxy", Message: "这是 OrbSleep 自己的 Lazy TCP Proxy 容器，不能被再次接入", Severity: "blocker",
		})
		cl.Verdict = verdictExcluded
		return cl
	}
	blockers := 0
	reviews := 0
	block := func(code, msg string) {
		cl.Reasons = append(cl.Reasons, model.Reason{Code: code, Message: msg, Severity: "blocker"})
		blockers++
	}
	review := func(code, msg string) {
		cl.Warnings = append(cl.Warnings, model.Reason{Code: code, Message: msg, Severity: "warning"})
		reviews++
	}
	info := func(code, msg string) {
		cl.Reasons = append(cl.Reasons, model.Reason{Code: code, Message: msg, Severity: "info"})
	}

	switch {
	case c.NetworkMode == "host":
		block("network_mode_host", "容器使用 host 网络，无法安全接管宿主机端口")
	case strings.HasPrefix(c.NetworkMode, "container:"):
		block("network_mode_container", "容器复用其他容器的网络命名空间，无法独立接管端口")
	case c.NetworkMode == "none":
		block("network_mode_none", "容器没有网络，无法提供 TCP 服务")
	}
	if c.AutoRemove {
		block("auto_remove", "容器设置了 --rm，停止后会被自动删除，无法按需唤醒")
	}
	if c.Oneoff {
		block("compose_oneoff", "这是一次性（run）容器，没有可持续的 Compose 服务定义")
	}
	if c.ComposeProject == "" || c.ComposeService == "" {
		block("no_compose", "不是由 Docker Compose 创建（或缺少 Compose 标签），第一版不支持自动迁移")
	}
	if c.Privileged {
		review("privileged", "容器以特权模式运行，重建前请确认其安全影响")
	}

	if len(c.ComposeConfigFiles) == 0 {
		review("compose_file_unknown", "无法从标签定位 Compose 配置文件")
	} else if len(c.ComposeConfigFiles) > 1 {
		review("multiple_compose_files", "项目由多个 Compose 文件合并，无法可靠定位端口声明来源")
	} else {
		f := c.ComposeConfigFiles[0]
		if _, err := os.Stat(f); err != nil {
			review("compose_file_missing", "Compose 配置文件不存在: "+f)
		}
		if ext := strings.ToLower(filepath.Ext(f)); ext != ".yml" && ext != ".yaml" {
			review("compose_file_format", "Compose 配置文件不是 YAML: "+f)
		}
	}
	if c.ComposeContainerNumber > 1 {
		review("multiple_instances", fmt.Sprintf("Compose 服务当前有多个实例（本容器是第 %d 个），无法建立稳定的端口对应关系", c.ComposeContainerNumber))
	}

	var tcpCount, udpCount, staticCount int
	for _, p := range c.Ports {
		switch p.Protocol {
		case "udp":
			udpCount++
			continue
		case "tcp":
			tcpCount++
		}
		if p.HostPort <= 0 {
			block("dynamic_host_port", fmt.Sprintf("端口 %d/tcp 使用动态宿主机端口，无法稳定接管", p.ContainerPort))
			continue
		}
		if p.ContainerPort <= 0 {
			block("unknown_container_port", "无法确定容器内部端口")
			continue
		}
		if p.HostPort < 1024 {
			block("low_port", fmt.Sprintf("宿主机端口 %d 低于 1024；特权端口助手尚未实现", p.HostPort))
			continue
		}
		if p.HostIP == "::1" || p.HostIP == "::" || strings.Contains(p.HostIP, ":") {
			block("ipv6_binding", fmt.Sprintf("端口 %d 使用 IPv6 绑定（%s），当前端口网关仅支持 IPv4", p.HostPort, p.HostIP))
			continue
		}
		if p.HostIP == "" {
			review("all_interfaces", fmt.Sprintf("端口 %d 绑定所有网卡，接入后仍会保持；请确认 OrbStack 的 LAN 暴露策略", p.HostPort))
		}
		staticCount++
	}
	if tcpCount == 0 {
		if udpCount > 0 {
			block("udp_only", "只存在 UDP 端口映射，第一版不支持 UDP 转发")
		} else {
			block("no_published_tcp", "没有宿主机 TCP 端口映射，无需也无法接入按需休眠")
		}
	} else if staticCount == 0 {
		block("no_static_tcp", "没有可安全接管的静态 TCP 端口映射")
	}
	if udpCount > 0 && tcpCount > 0 {
		review("udp_ports_preserved", "存在 UDP 端口映射，接入后会原样保留，不由代理管理")
	}
	if c.Healthcheck.Configured {
		info("healthcheck", "容器配置了健康检查，代理会在冷启动时等待其就绪")
	}
	for _, p := range c.Ports {
		if p.Protocol != "tcp" || p.HostPort <= 0 || p.HostPort < 1024 {
			continue
		}
		if strings.Contains(p.HostIP, ":") {
			continue
		}
		if p.ContainerPort <= 0 {
			continue
		}
		cl.ProxyCandidates = append(cl.ProxyCandidates, p)
	}
	if isStatefulImage(c.Image, opts.ExcludeImagePatterns) {
		review("stateful_image", "镜像看起来是数据库/消息队列等有状态服务，按需休眠可能影响连接与数据一致性")
	}

	switch {
	case blockers > 0:
		cl.Verdict = verdictExcluded
	case reviews > 0:
		cl.Verdict = verdictNeedsReview
	}
	if cl.Verdict != verdictExcluded && len(cl.Reasons) == 0 {
		cl.Reasons = append(cl.Reasons, model.Reason{Code: "static_tcp", Message: "静态 TCP 端口映射，可由 Compose 安全重建", Severity: "info"})
	}
	return cl
}

func isStatefulImage(image string, patterns []string) bool {
	base := strings.ToLower(ImageBase(image))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.Contains(base, p) {
			return true
		}
	}
	return false
}

// ImageBase strips registry, tag and digest.
func ImageBase(image string) string {
	s := image
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

func splitConfigFiles(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizeHostIP(ip string) string {
	switch ip {
	case "", "0.0.0.0":
		return ""
	}
	return ip
}

func parseInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
