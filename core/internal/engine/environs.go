package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orbsleep/internal/dockercli"
	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/runtime"
)

// currentEnvID returns the environment id the engine's primary Docker client
// is bound to. Legacy records (and every pre-multi-env code path) implicitly
// belong to this environment.
func (e *Engine) currentEnvID() string {
	s := e.settings.Get()
	if sock := strings.TrimPrefix(strings.TrimSpace(s.DockerSocket), "unix://"); sock != "" {
		return runtime.EnvID("unix://" + sock)
	}
	e.mu.Lock()
	c := e.docker
	e.mu.Unlock()
	if c != nil {
		return runtime.EnvID("unix://" + c.Socket())
	}
	return ""
}

// currentEnvEndpoint returns the primary endpoint (unix socket path) without
// contacting Docker. Empty when nothing is configured or detected yet.
func (e *Engine) currentEnvEndpoint() string {
	s := e.settings.Get()
	if sock := strings.TrimPrefix(strings.TrimSpace(s.DockerSocket), "unix://"); sock != "" {
		return sock
	}
	e.mu.Lock()
	c := e.docker
	e.mu.Unlock()
	if c != nil {
		return c.Socket()
	}
	return ""
}

// ensurePrimaryEnvironment registers the primary environment row on first run
// after the upgrade so the GUI always has a concrete environment to show, and
// legacy services (environment_id=”) map onto it deterministically.
func (e *Engine) ensurePrimaryEnvironment() {
	envs, err := e.store.ListEnvironments()
	if err != nil {
		e.logger.Printf("warn", "读取环境注册表失败: %v", err)
		return
	}
	endpoint := e.currentEnvEndpoint()
	if endpoint == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		if c, derr := e.dockerClient(ctx); derr == nil {
			endpoint = c.Socket()
		}
		cancel()
	}
	if endpoint == "" {
		// No Docker reachable at all; retry on the next reconcile.
		return
	}
	for _, env := range envs {
		if strings.TrimPrefix(env.Endpoint, "unix://") == endpoint {
			if !env.Enabled {
				env.Enabled = true
				_ = e.store.UpsertEnvironment(env)
			}
			return
		}
	}
	kind := runtime.KindFromPath(endpoint)
	env := model.Environment{
		ID:       runtime.EnvID("unix://" + endpoint),
		Name:     runtime.DisplayNameForKind(kind),
		Kind:     string(kind),
		Endpoint: "unix://" + endpoint,
		Enabled:  true,
		Status:   "online",
		AddedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := e.store.UpsertEnvironment(env); err != nil {
		e.logger.Printf("warn", "登记主环境失败: %v", err)
	} else {
		e.logger.Printf("info", "已登记容器环境 %s（%s）", env.Name, env.Endpoint)
	}
}

// envClients caches one client per secondary environment. The primary client
// keeps its own singleflight machinery (dockerClient).
func (e *Engine) clientForEnv(ctx context.Context, envID string) (*dockerx.Client, error) {
	if envID == "" || envID == e.currentEnvID() {
		return e.dockerClient(ctx)
	}
	e.mu.Lock()
	cached := e.envClients[envID]
	e.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	env, ok, err := e.store.GetEnvironment(envID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("环境 %s 不存在", envID)
	}
	socket := strings.TrimPrefix(env.Endpoint, "unix://")
	if !strings.HasPrefix(env.Endpoint, "unix://") {
		return nil, fmt.Errorf("环境 %s 不是本机 socket 端点，暂不支持该操作", env.Name)
	}
	c, err := dockerx.New(ctx, socket)
	if err != nil {
		return nil, fmt.Errorf("环境 %s 不可达: %w", env.Name, err)
	}
	e.mu.Lock()
	e.envClients[envID] = c
	e.mu.Unlock()
	return c, nil
}

// dropEnvClient forgets a cached secondary client (disable/remove/offline).
func (e *Engine) dropEnvClient(envID string) {
	e.mu.Lock()
	delete(e.envClients, envID)
	e.mu.Unlock()
}

// ListEnvironments merges the registered rows with a fresh discovery pass so
// the GUI sees both managed and merely discovered environments. Reachability
// is probed live; the registered rows are updated with what the probe saw.
func (e *Engine) ListEnvironments(ctx context.Context) model.EnvironmentList {
	e.ensurePrimaryEnvironment()
	registered, err := e.store.ListEnvironments()
	if err != nil {
		e.logger.Printf("warn", "读取环境注册表失败: %v", err)
		registered = []model.Environment{}
	}
	byEndpoint := map[string]bool{}
	primary := e.currentEnvID()
	for i := range registered {
		byEndpoint[strings.TrimPrefix(registered[i].Endpoint, "unix://")] = true
	}
	for _, cand := range runtime.Discover() {
		ep := strings.TrimPrefix(cand.Endpoint, "unix://")
		if byEndpoint[ep] {
			continue
		}
		registered = append(registered, model.Environment{
			ID:             runtime.EnvID(cand.Endpoint),
			Name:           cand.Name,
			Kind:           string(cand.Kind),
			Endpoint:       cand.Endpoint,
			Status:         "unknown",
			DiscoveredOnly: true,
		})
		byEndpoint[ep] = true
	}
	// Probe every row, in parallel with a global cap: serial probing of N
	// offline environments would stall one GUI refresh for N×4s. Each row's
	// writes happen in its own goroutine; the slice slot is goroutine-local
	// so no mutex is needed for the row itself.
	probeWG := sync.WaitGroup{}
	probeSem := make(chan struct{}, 8)
	for i := range registered {
		env := &registered[i]
		if env.DiscoveredOnly && !strings.HasPrefix(env.Endpoint, "unix://") {
			// tcp/ssh endpoints: shown, honestly marked unsupported.
			env.Remote = true
		}
		probeWG.Add(1)
		go func(env *model.Environment) {
			defer probeWG.Done()
			probeSem <- struct{}{}
			defer func() { <-probeSem }()
			probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			res := runtime.Probe(probeCtx, env.Endpoint)
			cancel()
			if env.DiscoveredOnly {
				if res.Reachable {
					env.Status = "online"
					env.EngineVersion, env.APIVersion = res.EngineVersion, res.APIVersion
				} else if proxySocketAlive(strings.TrimPrefix(env.Endpoint, "unix://")) {
					env.Status = "unknown"
				} else {
					env.Status = "offline"
				}
				return
			}
			if res.Reachable {
				env.Status = "online"
				env.EngineVersion, env.APIVersion = res.EngineVersion, res.APIVersion
				env.LastError = ""
			} else {
				env.Status = "offline"
				env.LastError = res.Error
			}
			// A failed persistence must not be silently equated with the probe
			// result: the row keeps its previous status and the mismatch is
			// logged for the operator.
			status := env.Status
			version, api := env.EngineVersion, env.APIVersion
			lastErr := env.LastError
			if err := e.store.SetEnvironmentStatus(env.ID, status, version, api, lastErr); err != nil {
				env.LastError = "状态持久化失败: " + err.Error()
				e.logger.Printf("warn", "环境 %s 状态持久化失败: %v", env.Name, err)
			}
			if env.Kind == "" {
				env.Kind = string(res.Kind)
			}
			if env.Kind == "podman" || env.Kind == string(runtime.KindPodman) {
				env.Capabilities = e.storeCapsFor(env)
			}
		}(env)
	}
	probeWG.Wait()
	return model.EnvironmentList{Environments: registered, PrimaryID: primary}
}

// storeCapsFor exposes the persisted capability view for a registered env.
func (e *Engine) storeCapsFor(env *model.Environment) model.Caps {
	return model.Caps{
		ListContainers: true, InspectContainer: true, StartStop: true,
		Recreate:       env.Kind != "podman", // podman attach stays unverified
		Events:         true,
		PortTakeover:   env.Kind != "podman" && !env.Remote,
		ComposeSupport: false,
	}
}

func proxySocketAlive(socket string) bool {
	if socket == "" {
		return false
	}
	st, err := os.Stat(socket)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// AddEnvironment registers a custom endpoint after probing it.
func (e *Engine) AddEnvironment(ctx context.Context, endpoint, name string) (model.EnvironmentList, error) {
	norm := runtime.NormalizeEndpoint(endpoint)
	if norm == "" {
		return model.EnvironmentList{}, fmt.Errorf("端点不能为空")
	}
	res, err := runtime.ValidateCustomEndpoint(ctx, norm)
	if err != nil {
		return model.EnvironmentList{}, err
	}
	if res.Kind == runtime.KindRemote || strings.HasPrefix(norm, "tcp://") {
		return model.EnvironmentList{}, fmt.Errorf("远程 Docker 端点的端口发布在远程机器上，本机网关无法接管，暂不支持接入（已在支持矩阵中说明）")
	}
	envID := runtime.EnvID(norm)
	if _, ok, _ := e.store.GetEnvironment(envID); ok {
		return model.EnvironmentList{}, fmt.Errorf("该环境已注册")
	}
	if name == "" {
		name = runtime.DisplayNameForKind(res.Kind)
	}
	env := model.Environment{
		ID: envID, Name: name, Kind: string(res.Kind), Endpoint: norm,
		Enabled: false, Status: "online",
		EngineVersion: res.EngineVersion, APIVersion: res.APIVersion,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := e.store.UpsertEnvironment(env); err != nil {
		return model.EnvironmentList{}, err
	}
	e.logger.Printf("info", "已添加容器环境 %s（%s）", name, norm)
	return e.ListEnvironments(ctx), nil
}

// EnvironmentAction performs enable/disable/test/remove on a registered env.
// enable provisions the environment's proxy; disable keeps services but marks
// the env inactive (their reconcile routes are pruned like foreign sockets).
func (e *Engine) EnvironmentAction(ctx context.Context, id, action string) (model.EnvironmentList, error) {
	env, ok, err := e.store.GetEnvironment(id)
	if err != nil {
		return model.EnvironmentList{}, err
	}
	if !ok {
		// A discovered (not yet registered) environment is materialised on
		// first action — enabling it IS the act of registering it.
		if action != "enable" && action != "test" {
			return model.EnvironmentList{}, fmt.Errorf("环境 %s 不存在", id)
		}
		var found *runtime.Candidate
		for _, cand := range runtime.Discover() {
			if runtime.EnvID(cand.Endpoint) == id {
				c := cand
				found = &c
				break
			}
		}
		if found == nil {
			return model.EnvironmentList{}, fmt.Errorf("环境 %s 不存在", id)
		}
		env = model.Environment{
			ID:       id,
			Name:     found.Name,
			Kind:     string(found.Kind),
			Endpoint: found.Endpoint,
			AddedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		if err := e.store.UpsertEnvironment(env); err != nil {
			return model.EnvironmentList{}, err
		}
	}
	// Mutating actions serialise with attach/reconcile so two concurrent
	// enables can never allocate overlapping proxy ranges.
	if action == "enable" || action == "disable" || action == "remove" {
		if err := e.lockWriteBounded(10 * time.Second); err != nil {
			return model.EnvironmentList{}, err
		}
		defer e.writeMu.Unlock()
	}
	switch action {
	case "test":
		// list refresh re-probes; nothing else to do.
	case "enable":
		if err := e.enableEnvironment(ctx, &env); err != nil {
			return model.EnvironmentList{}, err
		}
	case "disable":
		env.Enabled = false
		if err := e.store.UpsertEnvironment(env); err != nil {
			return model.EnvironmentList{}, err
		}
		e.dropEnvClient(id)
		e.triggerReconcile()
	case "remove":
		n, serr := e.store.CountServicesInEnvironment(id)
		if serr != nil {
			return model.EnvironmentList{}, serr
		}
		if n > 0 {
			return model.EnvironmentList{}, fmt.Errorf("环境 %s 上仍有 %d 个托管服务，请先恢复它们", env.Name, n)
		}
		if env.Enabled {
			return model.EnvironmentList{}, fmt.Errorf("请先停用环境 %s 再删除", env.Name)
		}
		if err := e.store.DeleteEnvironment(id); err != nil {
			return model.EnvironmentList{}, err
		}
		e.dropEnvClient(id)
	default:
		return model.EnvironmentList{}, fmt.Errorf("未知环境操作: %s", action)
	}
	return e.ListEnvironments(ctx), nil
}

// enableEnvironment marks the env active and provisions its proxy with a
// disjoint port range. The primary env keeps the global settings range.
func (e *Engine) enableEnvironment(ctx context.Context, env *model.Environment) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	res := runtime.Probe(probeCtx, env.Endpoint)
	cancel()
	if !res.Reachable {
		return fmt.Errorf("环境不可达: %s", res.Error)
	}
	primary := e.currentEnvID()
	if env.ID == primary {
		env.Enabled = true
		env.Status = "online"
		return e.store.UpsertEnvironment(*env)
	}
	// Allocate a disjoint proxy port range + admin/web ports for this env.
	owned := map[int]bool{}
	s := e.settings.Get()
	for p := s.ProxyPortStart; p <= s.ProxyPortEnd; p++ {
		owned[p] = true
	}
	owned[s.AdminPort] = true
	owned[s.WebPort] = true
	// Both reads feed the port-allocation exclusion set: a failed read would
	// make the "owned" set incomplete and could hand out an already-used
	// range. Fail closed instead.
	envs, err := e.store.ListEnvironments()
	if err != nil {
		return fmt.Errorf("读取环境列表失败，拒绝分配端口: %w", err)
	}
	for _, other := range envs {
		if other.ID == env.ID {
			continue
		}
		for p := other.ProxyPortStart; p <= other.ProxyPortEnd; p++ {
			owned[p] = true
		}
		if other.AdminPort > 0 {
			owned[other.AdminPort] = true
		}
		if other.WebPort > 0 {
			owned[other.WebPort] = true
		}
	}
	// Managed service proxy ports are also reserved regardless of env
	// (a managed service's proxy port must stay inside its own env range, but
	// never reuse another env's).
	services, err := e.store.ListServices()
	if err != nil {
		return fmt.Errorf("读取托管服务失败，拒绝分配端口: %w", err)
	}
	for _, svc := range services {
		for _, b := range svc.Bindings {
			owned[b.ProxyPort] = true
		}
	}
	size := s.ProxyPortEnd - s.ProxyPortStart + 1
	if size <= 0 {
		size = 256
	}
	start, end, ok := findFreeRangeExcluding(size, owned, nil)
	if !ok {
		return fmt.Errorf("找不到不相交的空闲端口段（需要 %d 个端口）", size)
	}
	env.ProxyPortStart, env.ProxyPortEnd = start, end
	adminPort, err := chooseManagedPort(0, nil, owned)
	if err != nil {
		return fmt.Errorf("分配管理端口失败: %w", err)
	}
	owned[adminPort] = true
	webPort, err := chooseManagedPort(0, nil, owned)
	if err != nil {
		return fmt.Errorf("分配状态端口失败: %w", err)
	}
	env.AdminPort, env.WebPort = adminPort, webPort
	env.Enabled = true
	env.Status = "online"
	if err := e.store.UpsertEnvironment(*env); err != nil {
		return err
	}
	e.dropEnvClient(env.ID)
	e.logger.Printf("info", "环境 %s 已启用（代理端口段 %d-%d，管理端口 %d/%d）",
		env.Name, start, end, adminPort, webPort)
	e.triggerReconcile()
	return nil
}

// proxyForEnv returns the proxy identity of an environment: the primary env
// keeps the historical container/project/file so nothing about existing
// installs changes; additional environments get suffixed identities.
type proxyEnvSpec struct {
	EnvID         string
	Primary       bool
	ContainerName string
	Project       string
	ComposePath   string
	ConfigDir     string
	Start, End    int
	Admin, Web    int
	SocketPath    string
	// LoopbackPublish is true when the engine publishes container ports
	// directly on the macOS side (OrbStack / Docker Desktop / plain engine):
	// the proxy must bind 127.0.0.1. VM-forwarded runtimes (Colima, Lima,
	// Finch, Rancher Desktop) need an in-VM 0.0.0.0 bind so the host
	// forwarder lands the ports on the macOS loopback.
	LoopbackPublish bool
}

// envLoopbackPublish reports the publishing model of an engine kind.
func envLoopbackPublish(kind string) bool {
	switch kind {
	case "colima", "lima", "finch", "rancher-desktop", "podman":
		return false
	default:
		return true
	}
}

func (e *Engine) proxySpecForEnv(envID string) proxyEnvSpec {
	s := e.settings.Get()
	primary := envID == "" || envID == e.currentEnvID()
	if primary {
		return proxyEnvSpec{
			EnvID: envID, Primary: true,
			ContainerName: proxy.ContainerName,
			Project:       "orbsleep-proxy",
			ComposePath:   e.proxyComposePath(),
			ConfigDir:     e.proxyConfigDir(),
			Start:         s.ProxyPortStart, End: s.ProxyPortEnd,
			Admin: s.AdminPort, Web: s.WebPort,
			SocketPath:      vmSocketPath(string(envKindForEndpoint(e.currentEnvEndpoint()))),
			LoopbackPublish: envLoopbackPublish(string(envKindForEndpoint(e.currentEnvEndpoint()))),
		}
	}
	suffix := strings.TrimPrefix(envID, "env-")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	env, ok, _ := e.store.GetEnvironment(envID)
	spec := proxyEnvSpec{
		EnvID:         envID,
		ContainerName: proxy.ContainerName + "-" + suffix,
		Project:       "orbsleep-proxy-" + suffix,
		ComposePath:   filepath.Join(e.dataDir, "proxy", envID, "docker-compose.yml"),
		ConfigDir:     filepath.Join(e.dataDir, "proxy", envID),
		SocketPath:    strings.TrimPrefix(env.Endpoint, "unix://"),
	}
	if ok {
		spec.Start, spec.End = env.ProxyPortStart, env.ProxyPortEnd
		spec.Admin, spec.Web = env.AdminPort, env.WebPort
	}
	spec.SocketPath = vmSocketPath(env.Kind)
	spec.LoopbackPublish = envLoopbackPublish(env.Kind)
	return spec
}

// envKindForEndpoint classifies without touching the store (best effort).
func envKindForEndpoint(endpoint string) runtime.Kind {
	return runtime.KindFromPath(endpoint)
}

// vmSocketPath returns the socket path INSIDE the engine VM for the proxy
// mount. The macOS-side endpoint (e.g. ~/.colima/default/docker.sock) is how
// the host reaches the API; a container inside the VM must mount the daemon's
// own socket, which every Docker-compatible VM runtime exposes at
// /var/run/docker.sock. Per-kind overrides belong here when a runtime that
// differs shows up.
func vmSocketPath(kind string) string {
	switch kind {
	case "podman":
		return "/var/run/podman/podman.sock"
	default:
		return "/var/run/docker.sock"
	}
}

// adminClientForEnv returns the admin API client for an environment's proxy.
func (e *Engine) adminClientForEnv(envID string) (*proxy.AdminClient, error) {
	spec := e.proxySpecForEnv(envID)
	key, err := e.proxyAPIKey()
	if err != nil {
		return nil, err
	}
	return proxy.NewAdminClient(fmt.Sprintf("127.0.0.1:%d", spec.Admin), key), nil
}

// cliForEnv resolves the docker/compose CLI runner bound to an environment's
// endpoint. The primary environment keeps the cached e.cli semantics.
func (e *Engine) cliForEnv(ctx context.Context, envID string) (composeCLI, error) {
	c, err := e.clientForEnv(ctx, envID)
	if err != nil {
		return nil, err
	}
	if envID == "" || envID == e.currentEnvID() {
		// Primary: reuse the cached runner machinery.
		e.mu.Lock()
		cli := e.cli
		e.mu.Unlock()
		if cli != nil {
			return cli, nil
		}
	}
	find := e.findCLI
	if find == nil {
		find = func(host string) (composeCLI, error) { return dockercli.Find(host) }
	}
	cli, err := find(c.DockerHost())
	if err != nil {
		return nil, err
	}
	if envID == "" || envID == e.currentEnvID() {
		e.mu.Lock()
		e.cli = cli
		e.mu.Unlock()
	}
	return cli, nil
}
