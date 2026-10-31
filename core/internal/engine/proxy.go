package engine

import (
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
// PublishedPort is one host-side binding of the proxy container.
type PublishedPort struct {
	ContainerPort int
	HostPort      int
	HostIP        string // "" means all interfaces
}

// proxyPublishedPorts returns the proxy's actual TCP host bindings keyed by
// container port. Callers must verify the specific port they are about to
// hand to the gateway — an empty result means nothing is published and must
// block the plan, never be treated as "no need to check".
func (e *Engine) proxyPublishedPorts(ctx context.Context) (map[int]PublishedPort, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	ins, err := c.InspectContainer(ctx, proxy.ContainerName)
	if err != nil {
		return nil, err
	}
	out := map[int]PublishedPort{}
	for key, bindings := range ins.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		port, proto := splitPortKey(key)
		if proto != "tcp" || port <= 0 {
			continue
		}
		for _, b := range bindings {
			hostPort, perr := strconv.Atoi(b.HostPort)
			if perr != nil || hostPort <= 0 {
				continue
			}
			out[port] = PublishedPort{ContainerPort: port, HostPort: hostPort, HostIP: b.HostIP}
			break
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
	if err := e.ensureNoUnresolvedWork(); err != nil {
		return "", err
	}
	ctx, done, err := e.beginOperation(10 * time.Minute)
	if err != nil {
		return "", err
	}
	op := model.Operation{
		ID:        newID("op"),
		Kind:      "proxy_setup",
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: filepath.Join(e.settings.Get().BackupDir, "proxy"),
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		done()
		return "", err
	}
	go e.runProxySetup(ctx, op, done)
	return op.ID, nil
}

// ProxyStop stops and removes the proxy container but keeps its configuration,
// settings and backups so it can be started again.
func (e *Engine) ProxyStop() (string, error) {
	if err := e.ensureNoUnresolvedWork(); err != nil {
		return "", err
	}
	ctx, done, err := e.beginOperation(5 * time.Minute)
	if err != nil {
		return "", err
	}
	op := model.Operation{
		ID:        newID("op"),
		Kind:      "proxy_stop",
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BackupDir: filepath.Join(e.settings.Get().BackupDir, "proxy"),
		Log:       []model.LogEntry{},
	}
	if err := e.store.InsertOperation(op); err != nil {
		done()
		return "", err
	}
	go e.runProxyStop(ctx, op, done)
	return op.ID, nil
}

func (e *Engine) runProxyStop(ctx context.Context, op model.Operation, done func()) {
	defer done()
	logger := e.newOpLogger(op)
	unlock, err := e.beginSafeWrite()
	if err != nil {
		logger.finish("failed", "安全检查失败，未执行任何修改: "+err.Error())
		e.setLastError("代理停止未执行: " + err.Error())
		return
	}
	defer unlock()
	cli, err := e.cliRunner()
	if err != nil {
		logger.finish("failed", err.Error())
		return
	}
	composePath := e.proxyComposePath()
	if _, err := os.Stat(composePath); err != nil {
		logger.finish("failed", "代理未安装: "+err.Error())
		return
	}
	logger.add("info", "停止并移除代理容器（配置与设置保留）")
	if err := cli.ProxyDown(ctx, composePath); err != nil {
		logger.finish("failed", "停止代理失败: "+err.Error())
		return
	}
	if services, _ := e.store.ListServices(); len(services) > 0 {
		logger.add("warn", "仍有托管服务：代理停止后，新连接将无法唤醒这些容器")
	}
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

func (e *Engine) runProxySetup(ctx context.Context, op model.Operation, done func()) {
	defer done()
	logger := e.newOpLogger(op)
	unlock, err := e.beginSafeWrite()
	if err != nil {
		logger.finish("failed", "安全检查失败，未执行任何修改: "+err.Error())
		e.setLastError("代理安装未执行: " + err.Error())
		return
	}
	defer unlock()
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
	old := e.settings.Get()
	var ins *dockerx.ContainerInspect
	if i, err := c.InspectContainer(ctx, proxy.ContainerName); err == nil {
		if proj := i.Config.Labels["com.docker.compose.project"]; proj != "orbsleep-proxy" {
			logger.finish("failed", fmt.Sprintf("已存在同名容器 %s（Compose 项目 %q），为避免覆盖用户配置已停止；请先改名或移除该容器", proxy.ContainerName, proj))
			return
		}
		ins = i
	}
	owned := proxyOwnedHostPorts(ins)
	next := old

	// Select the published range, treating ports already published by our own
	// proxy as reusable (a running proxy legitimately owns them).
	size := next.ProxyPortEnd - next.ProxyPortStart + 1
	if size <= 0 {
		logger.finish("failed", "代理端口范围无效")
		return
	}
	if !rangeUsable(next.ProxyPortStart, next.ProxyPortEnd, owned) {
		base, end, ok := findFreeRangeExcluding(size, owned, map[int]bool{next.AdminPort: true, next.WebPort: true})
		if !ok {
			logger.finish("failed", "找不到可用的代理端口范围，请在设置中手动指定")
			return
		}
		logger.add("warn", fmt.Sprintf("端口范围 %d-%d 不可用，改用 %d-%d", next.ProxyPortStart, next.ProxyPortEnd, base, end))
		next.ProxyPortStart, next.ProxyPortEnd = base, end
	}
	rangeSet := portRangeSet(next.ProxyPortStart, next.ProxyPortEnd)

	// Allocate management ports jointly so they can never collide.
	adminPort, err := chooseManagedPort(next.AdminPort, owned, mergeSets(rangeSet, next.WebPort))
	if err != nil {
		logger.finish("failed", "分配代理管理端口失败: "+err.Error())
		return
	}
	webPort, err := chooseManagedPort(next.WebPort, owned, mergeSets(rangeSet, adminPort))
	if err != nil {
		logger.finish("failed", "分配代理状态端口失败: "+err.Error())
		return
	}
	next.AdminPort, next.WebPort = adminPort, webPort

	// Never silently change the range while services still use ports from it.
	existing, err := e.store.ListServices()
	if err != nil {
		logger.finish("failed", "读取托管服务失败: "+err.Error())
		return
	}
	for _, svc := range existing {
		for _, b := range svc.Bindings {
			if b.ProxyPort < next.ProxyPortStart || b.ProxyPort > next.ProxyPortEnd {
				logger.finish("failed", fmt.Sprintf("端口范围 %d-%d 不包含已托管服务 %s 的代理端口 %d；请先恢复该服务",
					next.ProxyPortStart, next.ProxyPortEnd, svc.ContainerName, b.ProxyPort))
				return
			}
		}
	}

	samePorts := old.ProxyPortStart == next.ProxyPortStart && old.ProxyPortEnd == next.ProxyPortEnd &&
		old.AdminPort == next.AdminPort && old.WebPort == next.WebPort
	if ins != nil && ins.State.Running && samePorts && e.proxyAdminReachable(ctx, next.AdminPort) {
		if err := e.syncProxyConfig(ctx, true); err != nil {
			logger.finish("failed", "代理正在运行，但同步服务配置失败: "+err.Error())
			return
		}
		logger.add("info", "代理已就绪且端口未变化，无需重建（重复执行安全）")
		logger.finish("succeeded", "")
		e.triggerReconcile()
		return
	}
	if ins != nil && ins.State.Running && samePorts {
		logger.add("warn", "代理容器在运行但管理接口不可达，将重建代理容器")
	}

	key, err := e.proxyAPIKey()
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
		if err := compose.WriteAtomic(cfgPath, proxy.Config{}.Marshal(), 0o600); err != nil {
			logger.finish("failed", "写入代理配置失败: "+err.Error())
			return
		}
	}
	composePath := e.proxyComposePath()
	oldCompose, composeReadErr := os.ReadFile(composePath)
	hadCompose := false
	switch {
	case composeReadErr == nil:
		hadCompose = true
	case os.IsNotExist(composeReadErr):
		hadCompose = false
	default:
		logger.finish("failed", "读取旧代理 Compose 文件失败，已中止（未做任何修改）: "+composeReadErr.Error())
		return
	}
	// Persist a complete snapshot before any write so a failed install can be
	// diagnosed and manually recovered even if automatic restore fails.
	snapDir := filepath.Join(logger.snapshot().BackupDir, op.ID)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		logger.finish("failed", "创建代理备份目录失败: "+err.Error())
		return
	}
	if hadCompose {
		if err := compose.WriteAtomic(filepath.Join(snapDir, "docker-compose.yml"), oldCompose, 0o600); err != nil {
			logger.finish("failed", "备份旧代理 Compose 失败，已中止（未做任何修改）: "+err.Error())
			return
		}
	}
	settingsJSON, jerr := json.MarshalIndent(old, "", "  ")
	if jerr != nil {
		logger.finish("failed", "序列化旧设置失败，已中止: "+jerr.Error())
		return
	}
	if err := compose.WriteAtomic(filepath.Join(snapDir, "settings.json"), settingsJSON, 0o600); err != nil {
		logger.finish("failed", "备份旧设置失败，已中止（未做任何修改）: "+err.Error())
		return
	}
	raw, jerr := json.MarshalIndent(next, "", "  ")
	if jerr != nil {
		logger.finish("failed", "序列化新设置失败，已中止: "+jerr.Error())
		return
	}
	if err := compose.WriteAtomic(filepath.Join(snapDir, "new-settings.json"), raw, 0o600); err != nil {
		logger.finish("failed", "备份新设置失败，已中止（未做任何修改）: "+err.Error())
		return
	}

	restore := func(reason error) {
		logger.add("warn", "恢复代理原有设置: "+reason.Error())
		var errs []string
		if err := e.settings.Save(old); err != nil {
			errs = append(errs, "恢复设置失败: "+err.Error())
		}
		if hadCompose {
			if err := compose.WriteAtomic(composePath, oldCompose, 0o600); err != nil {
				errs = append(errs, "恢复旧 Compose 失败: "+err.Error())
			}
		} else if err := os.Remove(composePath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, "删除新 Compose 失败: "+err.Error())
		}
		if hadCompose {
			rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if uerr := cli.ProxyUp(rctx, composePath); uerr != nil {
				errs = append(errs, "恢复旧代理容器失败: "+uerr.Error())
			} else if werr := e.waitProxyAdminAt(rctx, old.AdminPort, 90*time.Second); werr != nil {
				errs = append(errs, "旧代理管理接口未恢复: "+werr.Error())
			}
			rcancel()
		} else {
			// A fresh install failed before settings were committed; remove any
			// half-configured container so the next attempt starts clean.
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if c != nil {
				if rerr := c.RemoveContainer(rctx, proxy.ContainerName, true); rerr != nil && !dockerx.IsErrNotFound(rerr) {
					errs = append(errs, "清理新代理容器失败: "+rerr.Error())
				}
			}
			rcancel()
		}
		status := "failed"
		msg := reason.Error()
		if len(errs) > 0 {
			status = "rollback_failed"
			msg += "；回滚不完整: " + strings.Join(errs, "; ")
			logger.add("error", msg)
		}
		logger.finish(status, msg+"；备份位于 "+snapDir)
	}

	if err := compose.WriteAtomic(composePath, proxy.ComposeFile(next, key, cfgDir), 0o600); err != nil {
		restore(fmt.Errorf("写入代理 Compose 文件失败: %w", err))
		return
	}

	logger.add("info", fmt.Sprintf("代理端口范围 %d-%d，管理 API 127.0.0.1:%d，状态页 127.0.0.1:%d",
		next.ProxyPortStart, next.ProxyPortEnd, next.AdminPort, next.WebPort))
	logger.add("info", "拉取并启动代理容器…")
	if err := cli.ProxyUp(ctx, composePath); err != nil {
		restore(fmt.Errorf("启动代理失败: %w", err))
		return
	}
	logger.add("info", "等待代理管理接口就绪…")
	if err := e.waitProxyAdminAt(ctx, next.AdminPort, 90*time.Second); err != nil {
		restore(err)
		return
	}
	if err := e.settings.Save(next); err != nil {
		restore(fmt.Errorf("保存设置失败: %w", err))
		return
	}
	if err := e.syncProxyConfig(ctx, true); err != nil {
		restore(fmt.Errorf("写入代理服务配置失败: %w", err))
		return
	}
	logger.add("info", "Lazy TCP Proxy 已就绪")
	logger.finish("succeeded", "")
	e.triggerReconcile()
}

// proxyAPIKey returns the cached admin key, generating one on first use.
func (e *Engine) proxyAPIKey() (string, error) {
	e.mu.Lock()
	key := e.proxyKey
	e.mu.Unlock()
	if key != "" {
		return key, nil
	}
	k, err := proxy.APIKey(e.dataDir)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.proxyKey = k
	e.mu.Unlock()
	return k, nil
}

// proxyAdminReachable checks the admin API on a specific local port.
func (e *Engine) proxyAdminReachable(ctx context.Context, port int) bool {
	admin, err := e.proxyAdminAt(port)
	if err != nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return admin.Ping(pingCtx) == nil
}

func (e *Engine) proxyAdminAt(port int) (*proxy.AdminClient, error) {
	e.mu.Lock()
	key := e.proxyKey
	e.mu.Unlock()
	if key == "" {
		k, err := proxy.APIKey(e.dataDir)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		e.proxyKey = k
		e.mu.Unlock()
		key = k
	}
	return proxy.NewAdminClient(fmt.Sprintf("127.0.0.1:%d", port), key), nil
}

func (e *Engine) waitProxyAdminAt(ctx context.Context, port int, timeout time.Duration) error {
	admin, err := e.proxyAdminAt(port)
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
			return fmt.Errorf("代理管理接口 127.0.0.1:%d 在 %s 内不可达: %v", port, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// proxyOwnedHostPorts returns host ports currently published by the proxy
// container (including ranges), so setup can treat them as its own.
func proxyOwnedHostPorts(ins *dockerx.ContainerInspect) map[int]bool {
	out := map[int]bool{}
	if ins == nil {
		return out
	}
	add := func(hp string) {
		hp = strings.TrimSpace(hp)
		if hp == "" {
			return
		}
		if i := strings.Index(hp, "-"); i > 0 {
			start, err1 := strconv.Atoi(hp[:i])
			end, err2 := strconv.Atoi(hp[i+1:])
			if err1 == nil && err2 == nil && start > 0 && end >= start && end <= 65535 {
				for p := start; p <= end; p++ {
					out[p] = true
				}
			}
			return
		}
		if p, err := strconv.Atoi(hp); err == nil && p > 0 {
			out[p] = true
		}
	}
	for _, bindings := range ins.NetworkSettings.Ports {
		for _, b := range bindings {
			add(b.HostPort)
		}
	}
	for _, bindings := range ins.HostConfig.PortBindings {
		for _, b := range bindings {
			add(b.HostPort)
		}
	}
	return out
}

func rangeUsable(start, end int, owned map[int]bool) bool {
	if start <= 0 || end < start {
		return false
	}
	for p := start; p <= end; p++ {
		if owned[p] {
			continue
		}
		if !proxy.PortFree("127.0.0.1", p) {
			return false
		}
	}
	return true
}

func findFreeRangeExcluding(size int, owned, reserved map[int]bool) (int, int, bool) {
	if size <= 0 {
		size = 100
	}
	for base := 20000; base+size-1 <= 40000; base += size {
		ok := true
		for p := base; p < base+size; p++ {
			if reserved[p] || owned[p] || !proxy.PortFree("127.0.0.1", p) {
				ok = false
				break
			}
		}
		if ok {
			return base, base + size - 1, true
		}
	}
	return 0, 0, false
}

// chooseManagedPort keeps preferred when it is free or already owned by our
// proxy, otherwise picks the next free port not reserved for other purposes.
func chooseManagedPort(preferred int, owned, reserved map[int]bool) (int, error) {
	if preferred > 0 && !reserved[preferred] && (owned[preferred] || proxy.PortFree("127.0.0.1", preferred)) {
		return preferred, nil
	}
	for p := preferred; p < preferred+200 && p <= 65535; p++ {
		if reserved[p] || owned[p] {
			continue
		}
		if proxy.PortFree("127.0.0.1", p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("从 %d 起找不到空闲端口", preferred)
}

func portRangeSet(start, end int) map[int]bool {
	out := map[int]bool{}
	for p := start; p <= end; p++ {
		out[p] = true
	}
	return out
}

func mergeSets(base map[int]bool, ports ...int) map[int]bool {
	out := map[int]bool{}
	for p := range base {
		out[p] = true
	}
	for _, p := range ports {
		if p > 0 {
			out[p] = true
		}
	}
	return out
}

func (e *Engine) syncProxyConfig(ctx context.Context, force bool) error {
	services, err := e.store.ListServices()
	if err != nil {
		return err
	}
	// The proxy only runs on the currently selected Docker instance; services
	// pinned to another instance are managed by that instance's proxy and must
	// not be pushed into this one's config.
	clientSocket := strings.TrimPrefix(strings.TrimSpace(e.settings.Get().DockerSocket), "unix://")
	if c, derr := e.dockerClient(ctx); derr == nil {
		clientSocket = strings.TrimPrefix(c.Socket(), "unix://")
	}
	scoped := make([]model.Service, 0, len(services))
	for _, svc := range services {
		if svc.DockerSocket == "" || svc.DockerSocket == clientSocket {
			scoped = append(scoped, svc)
		}
	}
	desired := e.desiredProxyConfig(scoped)
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
			s.Ports = append([]string(nil), s.Ports...)
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

// validateProxyConfig rejects a baseline that cannot be safely restored.
func validateProxyConfig(cfg proxy.Config) error {
	seen := map[string]bool{}
	for _, svc := range cfg.Services {
		if strings.TrimSpace(svc.Name) == "" {
			return errors.New("代理配置中存在空服务名")
		}
		if seen[svc.Name] {
			return fmt.Errorf("代理配置中存在重复服务 %q", svc.Name)
		}
		seen[svc.Name] = true
		if len(svc.Ports) == 0 {
			return fmt.Errorf("代理配置中服务 %q 没有监听端口", svc.Name)
		}
		for _, p := range svc.Ports {
			parts := strings.Split(p, ":")
			if len(parts) != 2 {
				return fmt.Errorf("服务 %q 的端口声明无效: %q", svc.Name, p)
			}
			listen, err1 := strconv.Atoi(parts[0])
			target, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil || listen < 1 || listen > 65535 || target < 1 || target > 65535 {
				return fmt.Errorf("服务 %q 的端口声明无效: %q", svc.Name, p)
			}
		}
	}
	return nil
}

// putProxyConfigVerified writes a config and re-reads it. A failure to read
// back does not mean the write did not happen; callers must treat this as an
// uncertain state and attempt a restore.
func (e *Engine) putProxyConfigVerified(ctx context.Context, admin *proxy.AdminClient, cfg proxy.Config) error {
	if err := admin.UpdateConfig(ctx, cfg); err != nil {
		return err
	}
	got, err := admin.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("写入后读取代理配置失败: %w", err)
	}
	if !proxyConfigEqual(got, cfg) {
		return errors.New("代理配置写入后校验不一致")
	}
	return nil
}

// verifyProxyService confirms one service entry exists with the expected ports.
func (e *Engine) verifyProxyService(ctx context.Context, admin *proxy.AdminClient, want proxy.ServiceConfig) error {
	got, err := admin.GetConfig(ctx)
	if err != nil {
		return err
	}
	svc, ok := got.Service(want.Name)
	if !ok {
		return fmt.Errorf("代理中没有服务 %q", want.Name)
	}
	sort.Strings(svc.Ports)
	sort.Strings(want.Ports)
	if strings.Join(svc.Ports, ",") != strings.Join(want.Ports, ",") {
		return fmt.Errorf("代理中服务 %q 的端口为 %v，期望 %v", want.Name, svc.Ports, want.Ports)
	}
	return nil
}
