package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

func (e *Engine) proxyComposePath() string {
	return filepath.Join(e.dataDir, "proxy", "docker-compose.yml")
}

func (e *Engine) proxyConfigDir() string {
	return filepath.Join(e.dataDir, "proxy")
}

func (e *Engine) proxyStatus(ctx context.Context) model.ProxyStatus {
	s := e.settings.Get()
	ps := model.ProxyStatus{
		ContainerName:  proxy.ContainerName,
		PublishedRange: model.Range{Start: s.ProxyPortStart, End: s.ProxyPortEnd},
	}
	if _, err := os.Stat(e.proxyComposePath()); err == nil {
		ps.Installed = true
	}
	c, err := e.dockerClient(ctx)
	if err != nil {
		ps.Error = err.Error()
		return ps
	}
	ins, err := c.InspectContainer(ctx, proxy.ContainerName)
	if err != nil {
		if !dockerx.IsErrNotFound(err) {
			ps.Error = err.Error()
		}
		return ps
	}
	ps.Installed = true
	ps.Running = ins.State.Running
	ps.ContainerIP = containerIP(ins)
	if admin, err := e.adminClient(); err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := admin.Ping(pingCtx); err == nil {
			ps.AdminReachable = true
		}
		cancel()
	}
	if ps.Running {
		mctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		if _, err := proxy.FetchMetrics(mctx, fmt.Sprintf("127.0.0.1:%d", s.WebPort)); err == nil {
			ps.MetricsReachable = true
		}
		cancel()
	}
	switch {
	case !ps.Running:
		ps.Error = "代理容器未运行"
	case !ps.AdminReachable:
		ps.Error = "代理管理接口不可达（请检查端口 " + fmt.Sprintf("%d", s.AdminPort) + "）"
	}
	return ps
}

func containerIP(ins *dockerx.ContainerInspect) string {
	names := make([]string, 0, len(ins.NetworkSettings.Networks))
	for name := range ins.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ip := ins.NetworkSettings.Networks[name].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// proxyPublishedPorts returns the proxy's container ports that are published to
// the macOS host. The gateway can only reach those ports.
func (e *Engine) proxyPublishedPorts(ctx context.Context) (map[int]bool, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	ins, err := c.InspectContainer(ctx, proxy.ContainerName)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for key, bindings := range ins.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		port, proto := splitPortKey(key)
		if proto == "tcp" && port > 0 {
			out[port] = true
		}
	}
	return out, nil
}

func (e *Engine) proxyContainerIP(ctx context.Context) string {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return ""
	}
	ins, err := c.InspectContainer(ctx, proxy.ContainerName)
	if err != nil {
		return ""
	}
	return containerIP(ins)
}

// ProxySetup installs or restarts the proxy and returns an operation id.
func (e *Engine) ProxySetup() (string, error) {
	op := model.Operation{
		ID:        newID("op"),
		Kind:      "proxy_setup",
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: filepath.Join(e.settings.Get().BackupDir, "proxy"),
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		return "", err
	}
	go e.runProxySetup(op)
	return op.ID, nil
}

func (e *Engine) runProxySetup(op model.Operation) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	logger := e.newOpLogger(op)
	logger.add("info", "开始安装/启动 Lazy TCP Proxy")

	c, err := e.dockerClient(ctx)
	if err != nil {
		logger.finish("failed", "Docker 不可用: "+err.Error())
		return
	}
	cli, err := e.cliRunner()
	if err != nil {
		logger.finish("failed", err.Error())
		return
	}
	if ins, err := c.InspectContainer(ctx, proxy.ContainerName); err == nil {
		if proj := ins.Config.Labels["com.docker.compose.project"]; proj != "orbsleep-proxy" {
			logger.finish("failed", fmt.Sprintf("已存在同名容器 %s（Compose 项目 %q），为避免覆盖用户配置已停止；请先改名或移除该容器", proxy.ContainerName, proj))
			return
		}
	}
	s := e.settings.Get()

	// Pick free management ports.
	if !proxy.PortFree("127.0.0.1", s.AdminPort) || s.AdminPort == s.WebPort {
		if p, err := proxy.PickFreePort(47811); err == nil {
			s.AdminPort = p
		}
	}
	if !proxy.PortFree("127.0.0.1", s.WebPort) || s.WebPort == s.AdminPort {
		if p, err := proxy.PickFreePort(47810); err == nil {
			s.WebPort = p
		}
	}
	// Ensure the whole published range is free.
	size := s.ProxyPortEnd - s.ProxyPortStart + 1
	if !rangeFree(s.ProxyPortStart, s.ProxyPortEnd) {
		base, end, ok := findFreeRange(size)
		if !ok {
			logger.finish("failed", "找不到可用的代理端口范围，请在设置中手动指定")
			return
		}
		logger.add("warn", fmt.Sprintf("端口范围 %d-%d 已被占用，改用 %d-%d", s.ProxyPortStart, s.ProxyPortEnd, base, end))
		s.ProxyPortStart, s.ProxyPortEnd = base, end
	}
	// Never silently change the range while services still use ports from it.
	existing, _ := e.store.ListServices()
	for _, svc := range existing {
		for _, b := range svc.Bindings {
			if b.ProxyPort < s.ProxyPortStart || b.ProxyPort > s.ProxyPortEnd {
				logger.finish("failed", fmt.Sprintf("端口范围 %d-%d 不包含已托管服务 %s 的代理端口 %d；请先恢复该服务",
					s.ProxyPortStart, s.ProxyPortEnd, svc.ContainerName, b.ProxyPort))
				return
			}
		}
	}
	if err := e.settings.Save(s); err != nil {
		logger.finish("failed", "保存设置失败: "+err.Error())
		return
	}
	logger.add("info", fmt.Sprintf("代理端口范围 %d-%d，管理 API 127.0.0.1:%d，状态页 127.0.0.1:%d",
		s.ProxyPortStart, s.ProxyPortEnd, s.AdminPort, s.WebPort))

	key, err := proxy.APIKey(e.dataDir)
	if err != nil {
		logger.finish("failed", "生成代理密钥失败: "+err.Error())
		return
	}
	cfgDir := e.proxyConfigDir()
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		logger.finish("failed", err.Error())
		return
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, proxy.Config{}.Marshal(), 0o600); err != nil {
			logger.finish("failed", "写入代理配置失败: "+err.Error())
			return
		}
	}
	if err := os.WriteFile(e.proxyComposePath(), proxy.ComposeFile(s, key, cfgDir), 0o600); err != nil {
		logger.finish("failed", "写入代理 Compose 文件失败: "+err.Error())
		return
	}
	logger.add("info", "拉取并启动代理容器…")
	if err := cli.ProxyUp(ctx, e.proxyComposePath()); err != nil {
		logger.finish("failed", "启动代理失败: "+err.Error())
		return
	}
	logger.add("info", "等待代理管理接口就绪…")
	if err := e.waitProxyAdmin(ctx, 90*time.Second); err != nil {
		logger.finish("failed", err.Error())
		return
	}
	if err := e.syncProxyConfig(ctx, true); err != nil {
		logger.finish("failed", "写入代理服务配置失败: "+err.Error())
		return
	}
	logger.add("info", "Lazy TCP Proxy 已就绪")
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

func (e *Engine) waitProxyAdmin(ctx context.Context, timeout time.Duration) error {
	admin, err := e.adminClient()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		last = admin.Ping(pingCtx)
		cancel()
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("代理管理接口在 %s 内不可达: %v", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (e *Engine) syncProxyConfig(ctx context.Context, force bool) error {
	services, err := e.store.ListServices()
	if err != nil {
		return err
	}
	desired := e.desiredProxyConfig(services)
	admin, err := e.adminClient()
	if err != nil {
		return err
	}
	cur, err := admin.GetConfig(ctx)
	if err != nil {
		e.setLastError("读取代理配置失败: " + err.Error())
		return err
	}
	if !force && proxyConfigEqual(cur, desired) {
		return nil
	}
	if err := admin.UpdateConfig(ctx, desired); err != nil {
		e.setLastError("同步代理配置失败: " + err.Error())
		return err
	}
	verify, err := admin.GetConfig(ctx)
	if err != nil {
		return err
	}
	if !proxyConfigEqual(verify, desired) {
		return errors.New("代理配置写入后校验不一致")
	}
	e.logger.Printf("info", "代理配置已同步（%d 个服务）", len(desired.Services))
	return nil
}

func (e *Engine) desiredProxyConfig(services []model.Service) proxy.Config {
	var cfg proxy.Config
	for _, svc := range services {
		sc := proxy.ServiceConfig{
			Name:             svc.ContainerName,
			IdleTimeoutSecs:  svc.IdleTimeoutSecs,
			StartTimeoutSecs: svc.StartTimeoutSecs,
		}
		if svc.ExcludedFromSleep {
			// The proxy has no "never stop" mode; a very large idle timeout is
			// the supported way to temporarily opt out of sleeping.
			sc.IdleTimeoutSecs = 604800
		}
		for _, b := range svc.Bindings {
			sc.Ports = append(sc.Ports, fmt.Sprintf("%d:%d", b.ProxyPort, b.ContainerPort))
		}
		sort.Strings(sc.Ports)
		if len(sc.Ports) == 0 {
			continue
		}
		cfg.Upsert(sc)
	}
	sort.Slice(cfg.Services, func(i, j int) bool { return cfg.Services[i].Name < cfg.Services[j].Name })
	return cfg
}

func proxyConfigEqual(a, b proxy.Config) bool {
	if len(a.Services) != len(b.Services) {
		return false
	}
	index := func(c proxy.Config) map[string]proxy.ServiceConfig {
		m := map[string]proxy.ServiceConfig{}
		for _, s := range c.Services {
			sort.Strings(s.Ports)
			m[s.Name] = s
		}
		return m
	}
	am, bm := index(a), index(b)
	for name, as := range am {
		bs, ok := bm[name]
		if !ok || strings.Join(as.Ports, ",") != strings.Join(bs.Ports, ",") ||
			as.IdleTimeoutSecs != bs.IdleTimeoutSecs || as.StartTimeoutSecs != bs.StartTimeoutSecs {
			return false
		}
	}
	return true
}

func rangeFree(start, end int) bool {
	for p := start; p <= end; p++ {
		if !proxy.PortFree("127.0.0.1", p) {
			return false
		}
	}
	return true
}

func findFreeRange(size int) (int, int, bool) {
	if size <= 0 {
		size = 100
	}
	for base := 20000; base+size-1 <= 40000; base += size {
		if rangeFree(base, base+size-1) {
			return base, base + size - 1, true
		}
	}
	return 0, 0, false
}
