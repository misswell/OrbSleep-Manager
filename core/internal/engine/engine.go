package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orbsleep/internal/dockercli"
	"orbsleep/internal/dockerx"
	"orbsleep/internal/gateway"
	"orbsleep/internal/launchagent"
	"orbsleep/internal/logging"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/settings"
	"orbsleep/internal/state"
	"orbsleep/internal/transaction"
)

// Engine is the daemon's application core.
type Engine struct {
	version  string
	dataDir  string
	store    *state.Store
	settings *settings.Manager
	logger   *logging.Logger
	gw       *gateway.Gateway

	mu         sync.Mutex
	docker     *dockerx.Client
	cli        composeCLI
	dockerErr  string
	lastDetect time.Time
	detect     *detectState
	lastErr    model.LastError
	scanCache  []model.Container
	scanAt     time.Time
	proxyKey   string

	writeMu sync.Mutex
	plansMu sync.Mutex
	plans   map[string]*planEntry

	activityMu sync.Mutex
	activity   map[string]time.Time

	reconcileMu sync.Mutex
	eventMu     sync.Mutex
	lastProxyID string

	txnMu      sync.Mutex
	txnWG      sync.WaitGroup
	txnCancels map[string]context.CancelFunc
	closing    bool

	dockerGeneration int

	recoveryMu   sync.Mutex
	lastRecovery time.Time

	composeLocker *transaction.FileLocker
	txns          *transaction.Manager
	maintenance   atomic.Bool
	findCLI       func(dockerHost string) (composeCLI, error)

	stopCh    chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	eventsCh  chan struct{}
	startedAt time.Time
}

// New opens the state database and prepares the engine.
func New(version, dataDir string) (*Engine, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dataDir, 0o700)
	store, err := state.Open(filepath.Join(dataDir, "state.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("打开状态数据库失败: %w", err)
	}
	logger, err := logging.New(filepath.Join(dataDir, "logs"))
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	settingsMgr := settings.New(store, dataDir)
	migrated, migrateErr := settingsMgr.MigrateLegacy()
	e := &Engine{
		version:       version,
		dataDir:       dataDir,
		store:         store,
		settings:      settingsMgr,
		logger:        logger,
		gw:            gateway.New(func(level, format string, args ...any) { logger.Printf(level, format, args...) }),
		plans:         map[string]*planEntry{},
		activity:      map[string]time.Time{},
		txnCancels:    map[string]context.CancelFunc{},
		eventsCh:      make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		composeLocker: transaction.NewFileLocker(filepath.Join(dataDir, "locks")),
		txns:          transaction.NewManager(store),
		findCLI: func(host string) (composeCLI, error) {
			return dockercli.Find(host)
		},
		startedAt:     time.Now(),
	}
	if migrateErr != nil {
		logger.Printf("warn", "迁移旧设置失败: %v", migrateErr)
	} else if migrated {
		logger.Printf("info", "已迁移旧设置：低端口助手未实现，已自动关闭")
	}
	e.gw.SetActivityHook(func(serviceID string, at time.Time) {
		e.activityMu.Lock()
		e.activity[serviceID] = at
		e.activityMu.Unlock()
	})
	if err := os.MkdirAll(filepath.Join(dataDir, "proxy"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.settings.Get().BackupDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(e.settings.Get().BackupDir, 0o700)
	return e, nil
}

// Start launches the background loops.
func (e *Engine) Start() {
	e.logger.Printf("info", "OrbSleep daemon %s 启动，数据目录 %s", e.version, e.dataDir)
	// Roll back transactions interrupted by a crash before restoring listeners
	// and accepting new work.
	e.RecoverTransactions()
	e.ReconcileOrphanOperations()
	// Restore host port listeners before the API accepts requests, so a daemon
	// restart does not leave a window where managed services are unreachable.
	e.reconcile()
	e.wg.Add(3)
	go e.reconcileLoop()
	go e.eventsLoop()
	go e.retentionLoop()
}

// Close shuts the daemon down.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		// Cancel in-flight transactions and wait for their rollback to finish
		// before closing the database, logger and gateway.
		if !e.waitTransactions(10 * time.Minute) {
			// Keep SQLite, logging and listeners open: a still-running goroutine
			// must not observe closed dependencies. The process exits shortly
			// after Close(), and the durable transaction journal covers recovery.
			e.logger.Printf("error", "取消后仍有事务未退出；跳过资源关闭，事务将在下次启动恢复")
			return
		}
		close(e.stopCh)
		writeDone := make(chan struct{})
		go func() {
			e.writeMu.Lock()
			e.writeMu.Unlock()
			close(writeDone)
		}()
		select {
		case <-writeDone:
		case <-time.After(30 * time.Second):
			e.logger.Printf("error", "写锁等待超时；跳过资源关闭，避免关闭仍在使用中的依赖")
			return
		}
		e.gw.StopAll()
		e.wg.Wait()
		e.logger.Printf("info", "daemon 已停止")
		e.logger.Close()
		e.store.Close()
	})
}

// Version returns the daemon version string.
func (e *Engine) Version() string { return e.version }

// Status returns the dashboard state.
func (e *Engine) Status() model.Status {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st := model.Status{
		DaemonVersion:        e.version,
		DataDir:              e.dataDir,
		LaunchAgentInstalled: launchagent.Active(),
	}
	if c, err := e.dockerClient(ctx); err == nil {
		st.Context = c.Context()
		st.DockerReachable = true
	} else {
		st.Context = model.ContextInfo{Name: "unavailable", Kind: "none"}
	}
	services, err := e.store.ListServices()
	if err != nil {
		e.setLastError("读取服务列表失败: " + err.Error())
	}
	var counts model.Counts
	counts.Managed = len(services)
	for _, svc := range services {
		switch svc.Status {
		case "managed_running":
			counts.Running++
		case "managed_idle":
			counts.Idle++
		case "managed_stopped":
			counts.Stopped++
		case "missing":
			counts.Missing++
		}
	}
	e.mu.Lock()
	for _, c := range e.scanCache {
		switch c.Classification.Verdict {
		case "eligible":
			counts.Eligible++
		case "needs_review":
			counts.NeedsReview++
		case "excluded":
			counts.Excluded++
		}
	}
	lastScan := e.scanAt
	lastErr := e.lastErr
	e.mu.Unlock()
	st.Counts = counts
	if !lastScan.IsZero() {
		st.LastScanAt = lastScan.UTC().Format(time.RFC3339)
	}
	st.LastError = lastErr
	st.Proxy = e.proxyStatus(ctx)
	return st
}

// proxyInstalled reports whether the OrbSleep proxy container exists.
func (e *Engine) proxyInstalled() (bool, error) {
	composeExists := false
	if _, err := os.Stat(e.proxyComposePath()); err == nil {
		composeExists = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := e.dockerClient(ctx)
	if err != nil {
		return composeExists, err
	}
	_, ierr := c.InspectContainer(ctx, proxy.ContainerName)
	if ierr == nil {
		return true, nil
	}
	if dockerx.IsErrNotFound(ierr) {
		return false, nil
	}
	return false, ierr
}

// validateContainerIPDialMode only accepts container_ip mode when the macOS
// host can actually reach the proxy container's internal listener.
func (e *Engine) validateContainerIPDialMode() error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	ip := e.proxyContainerIP(ctx)
	if ip == "" {
		return errors.New("无法获取代理容器 IP，不能启用 container_ip 模式")
	}
	d := netDialer(3 * time.Second)
	// Probe the exact ports the gateway would use. Without managed services,
	// fall back to the proxy web listener as a basic routing check.
	services, _ := e.store.ListServices()
	probed := 0
	for _, svc := range services {
		for _, b := range svc.Bindings {
			conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", ip, b.ProxyPort))
			if err != nil {
				return fmt.Errorf("无法直连代理容器 IP 的端口 %s:%d: %v；请保持 published 模式", ip, b.ProxyPort, err)
			}
			conn.Close()
			probed++
		}
	}
	if probed == 0 {
		conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:8080", ip))
		if err != nil {
			return fmt.Errorf("当前环境无法直连代理容器 IP（%s:8080）: %v；请保持 published 模式", ip, err)
		}
		conn.Close()
	}
	return nil
}

// InstallAgent installs the LaunchAgent pointing at the running daemon binary.
func (e *Engine) InstallAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := launchagent.Install(exe, e.dataDir); err != nil {
		return err
	}
	e.logger.Printf("info", "已安装 LaunchAgent: %s", exe)
	return nil
}

// UninstallAgent removes the LaunchAgent.
func (e *Engine) UninstallAgent() error {
	if err := launchagent.Uninstall(); err != nil {
		return err
	}
	e.logger.Printf("info", "已移除 LaunchAgent（当前进程继续运行，直到退出）")
	return nil
}

// Settings returns the current settings.
func (e *Engine) Settings() model.Settings {
	s := e.settings.Get()
	s.LaunchAgentInstalled = launchagent.Active()
	return s
}

// UpdateSettings applies a JSON patch (only known keys are accepted).
func (e *Engine) UpdateSettings(patch map[string]any) (model.Settings, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if err := e.ensureNoUnresolvedWork(); err != nil {
		return model.Settings{}, err
	}
	s := e.settings.Get()
	if v, ok := patch["idleTimeoutSecs"]; ok {
		s.IdleTimeoutSecs = toInt(v)
	}
	if v, ok := patch["startTimeoutSecs"]; ok {
		s.StartTimeoutSecs = toInt(v)
	}
	if v, ok := patch["proxyPortStart"]; ok {
		s.ProxyPortStart = toInt(v)
	}
	if v, ok := patch["proxyPortEnd"]; ok {
		s.ProxyPortEnd = toInt(v)
	}
	if v, ok := patch["adminPort"]; ok {
		s.AdminPort = toInt(v)
	}
	if v, ok := patch["webPort"]; ok {
		s.WebPort = toInt(v)
	}
	if v, ok := patch["logRetentionDays"]; ok {
		s.LogRetentionDays = toInt(v)
	}
	if v, ok := patch["backupDir"]; ok {
		if str, ok := v.(string); ok {
			s.BackupDir = str
		}
	}
	if v, ok := patch["autoScanOnLaunch"]; ok {
		s.AutoScanOnLaunch = toBool(v)
	}
	if v, ok := patch["enableLowPortHelper"]; ok {
		s.EnableLowPortHelper = toBool(v)
	}
	if s.AdminPort < 1024 || s.WebPort < 1024 {
		return s, errors.New("管理端口与状态端口不能低于 1024（特权端口无法可靠绑定）")
	}
	if v, ok := patch["proxyDialMode"]; ok {
		if str, ok := v.(string); ok {
			s.ProxyDialMode = str
		}
	}
	if v, ok := patch["excludeImagePatterns"]; ok {
		if arr, ok := v.([]any); ok {
			var patterns []string
			for _, item := range arr {
				if str, ok := item.(string); ok {
					patterns = append(patterns, str)
				}
			}
			s.ExcludeImagePatterns = patterns
		}
	}
	if v, ok := patch["dockerSocket"]; ok {
		// The Docker socket decides which containers attach/recovery operate
		// on. Changing it silently here would bypass every guard of
		// docker.context.select (managed services, unresolved transactions,
		// reachability). It must go through SelectDockerContext.
		if str, ok := v.(string); ok && strings.TrimPrefix(strings.TrimSpace(str), "unix://") != strings.TrimPrefix(strings.TrimSpace(s.DockerSocket), "unix://") {
			return s, errors.New("Docker Socket 只能通过 docker.context.select 切换（该入口会检查托管服务与未完成事务）")
		}
	}
	if s.AdminPort < 1 || s.AdminPort > 65535 || s.WebPort < 1 || s.WebPort > 65535 {
		return s, errors.New("代理管理端口/状态端口无效")
	}
	if s.AdminPort == s.WebPort {
		return s, errors.New("代理管理端口与状态端口不能相同")
	}
	if s.AdminPort >= s.ProxyPortStart && s.AdminPort <= s.ProxyPortEnd ||
		s.WebPort >= s.ProxyPortStart && s.WebPort <= s.ProxyPortEnd {
		return s, errors.New("管理端口/状态端口不能落在代理发布端口范围内")
	}
	if s.EnableLowPortHelper {
		return s, errors.New("低端口特权助手尚未实现，无法启用；低于 1024 的端口仍会被排除")
	}
	if s.ProxyDialMode == "container_ip" {
		if err := e.validateContainerIPDialMode(); err != nil {
			return s, err
		}
	}
	old := e.settings.Get()
	portsChanged := old.AdminPort != s.AdminPort || old.WebPort != s.WebPort ||
		old.ProxyPortStart != s.ProxyPortStart || old.ProxyPortEnd != s.ProxyPortEnd
	if portsChanged {
		if installed, _ := e.proxyInstalled(); installed {
			return s, errors.New("代理已安装：修改管理端口/状态端口/端口范围会导致管理接口失联。请先卸载代理（docker compose -p orbsleep-proxy down）后再修改，并重新运行 proxy-setup")
		}
	}
	// Refuse a range change that would orphan already managed services.
	// A failed read is not a clean board: fail closed instead of letting the
	// change through unchecked.
	services, err := e.store.ListServices()
	if err != nil {
		return s, fmt.Errorf("读取托管服务失败，拒绝修改端口范围: %w", err)
	}
	for _, svc := range services {
		for _, b := range svc.Bindings {
			if b.ProxyPort < s.ProxyPortStart || b.ProxyPort > s.ProxyPortEnd {
				return s, fmt.Errorf("新的端口范围不包含已托管服务 %s 的代理端口 %d；请先恢复该服务再修改范围", svc.ContainerName, b.ProxyPort)
			}
		}
	}
	if err := e.settings.Save(s); err != nil {
		return s, err
	}
	e.logger.Printf("info", "设置已更新: 空闲超时 %ds，端口范围 %d-%d", s.IdleTimeoutSecs, s.ProxyPortStart, s.ProxyPortEnd)
	e.triggerReconcile()
	return e.Settings(), nil
}

// Logs returns recent daemon log lines.
func (e *Engine) Logs(limit int) ([]model.LogEntry, error) {
	lines, err := e.logger.Tail(limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.LogEntry, 0, len(lines))
	for _, line := range lines {
		entry := model.LogEntry{At: time.Now().UTC().Format(time.RFC3339), Level: "info", Message: line}
		if len(line) > len("2006-01-02T15:04:05Z07:00")+6 {
			// The line already starts with RFC3339 + level.
			entry.At = line[:len("2006-01-02T15:04:05Z07:00")]
			rest := line[len("2006-01-02T15:04:05Z07:00"):]
			rest = strings.TrimLeft(rest, " ")
			if len(rest) >= 5 {
				entry.Level = strings.TrimSpace(rest[:5])
				entry.Message = strings.TrimSpace(rest[5:])
			} else {
				entry.Message = rest
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// detectState is shared by every goroutine waiting on one detection run:
// without singleflight, concurrent first callers each saw "Docker 不可用"
// while the very first detection was still in progress. The done channel is
// closed once, broadcasting the result to every waiter.
type detectState struct {
	done chan struct{}
	c    *dockerx.Client
	err  error
}

func (e *Engine) dockerClient(ctx context.Context) (*dockerx.Client, error) {
	e.mu.Lock()
	if e.docker != nil {
		c := e.docker
		e.mu.Unlock()
		return c, nil
	}
	if e.detect != nil {
		d := e.detect
		e.mu.Unlock()
		// A detection is already running: wait for its broadcast result
		// instead of failing this request spuriously.
		select {
		case <-d.done:
			return d.c, d.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !e.lastDetect.IsZero() && time.Since(e.lastDetect) < 3*time.Second {
		msg := e.dockerErr
		e.mu.Unlock()
		if msg == "" {
			msg = "Docker 不可用"
		}
		return nil, errors.New(msg)
	}
	e.lastDetect = time.Now()
	generation := e.dockerGeneration
	e.detect = &detectState{done: make(chan struct{})}
	d := e.detect
	e.mu.Unlock()

	detectCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var c *dockerx.Client
	var err error
	if selected := strings.TrimSpace(e.settings.Get().DockerSocket); selected != "" {
		// The user pinned a Docker instance. Never silently fall back to
		// another one: attach/recovery would then run against the wrong
		// containers. Report the failure instead.
		c, err = dockerx.New(detectCtx, strings.TrimPrefix(selected, "unix://"))
		if err != nil {
			err = fmt.Errorf("选定的 Docker Socket 不可用（%s）: %w；不回退到其他 Docker 实例，请在设置中检查或更换", selected, err)
		}
	} else {
		c, err = dockerx.Detect(detectCtx)
	}

	e.mu.Lock()
	stale := generation != e.dockerGeneration
	if stale {
		// The environment was switched while this detection was running: its
		// result describes the previous Docker instance and must neither be
		// cached nor handed to the (possibly new) waiters.
		e.dockerErr = "Docker 环境已切换，请重试"
		if err == nil {
			err = errors.New(e.dockerErr)
			c = nil
		}
	} else if err != nil {
		e.dockerErr = err.Error()
	} else {
		e.docker = c
		e.dockerErr = ""
		if cli, cliErr := dockercli.Find(c.DockerHost()); cliErr == nil {
			e.cli = cli
		}
	}
	if e.detect == d {
		e.detect = nil
	}
	e.mu.Unlock()
	d.c, d.err = c, err
	close(d.done)
	return c, err
}

func (e *Engine) cliRunner() (composeCLI, error) {
	e.mu.Lock()
	cli := e.cli
	c := e.docker
	gen := e.dockerGeneration
	e.mu.Unlock()
	if cli != nil {
		return cli, nil
	}
	if c == nil {
		return nil, errors.New("Docker 不可用")
	}
	find := e.findCLI
	if find == nil {
		find = func(host string) (composeCLI, error) { return dockercli.Find(host) }
	}
	cli, err := find(c.DockerHost())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	// The CLI runner is bound to the Docker host it was resolved against. If
	// the environment switched while the lookup ran, the runner must never
	// reach the cache — otherwise compose commands would target the previous
	// instance while the API client targets the new one.
	if gen != e.dockerGeneration {
		e.mu.Unlock()
		return nil, errors.New("Docker 环境已切换，请重试")
	}
	e.cli = cli
	e.mu.Unlock()
	return cli, nil
}

func (e *Engine) invalidateDocker() {
	e.mu.Lock()
	e.docker = nil
	e.cli = nil
	e.lastDetect = time.Time{}
	// Invalidate any detection already in flight: when it completes it must
	// not repopulate the cache with a client for the previous environment.
	e.dockerGeneration++
	e.mu.Unlock()
}

func (e *Engine) adminClient() (*proxy.AdminClient, error) {
	s := e.settings.Get()
	e.mu.Lock()
	key := e.proxyKey
	e.mu.Unlock()
	if key == "" {
		k, err := proxy.APIKey(e.dataDir)
		if err != nil {
			return nil, fmt.Errorf("读取代理管理密钥失败: %w", err)
		}
		e.mu.Lock()
		e.proxyKey = k
		e.mu.Unlock()
		key = k
	}
	return proxy.NewAdminClient(fmt.Sprintf("127.0.0.1:%d", s.AdminPort), key), nil
}

// setLastErrorQuiet records a last error without flooding the daemon log:
// used by frequently polled read paths (system.health) where the condition
// is usually transient.
func (e *Engine) setLastErrorQuiet(msg string) {
	e.mu.Lock()
	e.lastErr = model.LastError{At: time.Now().UTC().Format(time.RFC3339), Message: msg}
	e.mu.Unlock()
}

func (e *Engine) setLastError(msg string) {
	e.mu.Lock()
	e.lastErr = model.LastError{At: time.Now().UTC().Format(time.RFC3339), Message: msg}
	e.mu.Unlock()
	e.logger.Printf("error", "%s", msg)
}

func (e *Engine) triggerReconcile() {
	select {
	case e.eventsCh <- struct{}{}:
	default:
	}
}

func newID(prefix string) string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}
