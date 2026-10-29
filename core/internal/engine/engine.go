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
	cli        *dockercli.Runner
	dockerErr  string
	lastDetect time.Time
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

	stopCh    chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	eventsCh  chan struct{}
}

// New opens the state database and prepares the engine.
func New(version, dataDir string) (*Engine, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	store, err := state.Open(filepath.Join(dataDir, "state.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("打开状态数据库失败: %w", err)
	}
	logger, err := logging.New(filepath.Join(dataDir, "logs"))
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("打开日志失败: %w", err)
	}
	e := &Engine{
		version:  version,
		dataDir:  dataDir,
		store:    store,
		settings: settings.New(store, dataDir),
		logger:   logger,
		gw:       gateway.New(func(level, format string, args ...any) { logger.Printf(level, format, args...) }),
		plans:    map[string]*planEntry{},
		activity: map[string]time.Time{},
		eventsCh: make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
	e.gw.SetActivityHook(func(serviceID string, at time.Time) {
		e.activityMu.Lock()
		e.activity[serviceID] = at
		e.activityMu.Unlock()
	})
	if err := os.MkdirAll(filepath.Join(dataDir, "proxy"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.settings.Get().BackupDir, 0o700); err != nil {
		return nil, err
	}
	return e, nil
}

// Start launches the background loops.
func (e *Engine) Start() {
	e.logger.Printf("info", "OrbSleep daemon %s 启动，数据目录 %s", e.version, e.dataDir)
	// Restore host port listeners before the API accepts requests, so a daemon
	// restart does not leave a window where managed services are unreachable.
	e.reconcile()
	e.wg.Add(2)
	go e.reconcileLoop()
	go e.eventsLoop()
}

// Close shuts the daemon down.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		close(e.stopCh)
		// Wait for an in-flight transactional operation to finish (or roll
		// back) before tearing down the listeners and database.
		done := make(chan struct{})
		go func() {
			e.writeMu.Lock()
			e.writeMu.Unlock()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			e.logger.Printf("warn", "等待写操作超时，强制关闭")
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
		LaunchAgentInstalled: launchagent.Installed(),
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
	s.LaunchAgentInstalled = launchagent.Installed()
	return s
}

// UpdateSettings applies a JSON patch (only known keys are accepted).
func (e *Engine) UpdateSettings(patch map[string]any) (model.Settings, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
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
	if s.AdminPort < 1 || s.AdminPort > 65535 || s.WebPort < 1 || s.WebPort > 65535 {
		return s, errors.New("代理管理端口/状态端口无效")
	}
	if s.AdminPort == s.WebPort {
		return s, errors.New("代理管理端口与状态端口不能相同")
	}
	// Refuse a range change that would orphan already managed services.
	services, _ := e.store.ListServices()
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

func (e *Engine) dockerClient(ctx context.Context) (*dockerx.Client, error) {
	e.mu.Lock()
	if e.docker != nil {
		c := e.docker
		e.mu.Unlock()
		return c, nil
	}
	if time.Since(e.lastDetect) < 3*time.Second {
		msg := e.dockerErr
		e.mu.Unlock()
		if msg == "" {
			msg = "Docker 不可用"
		}
		return nil, errors.New(msg)
	}
	e.lastDetect = time.Now()
	e.mu.Unlock()

	detectCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	c, err := dockerx.Detect(detectCtx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.dockerErr = err.Error()
		return nil, err
	}
	e.docker = c
	e.dockerErr = ""
	if cli, cliErr := dockercli.Find(c.DockerHost()); cliErr == nil {
		e.cli = cli
	}
	return c, nil
}

func (e *Engine) cliRunner() (*dockercli.Runner, error) {
	e.mu.Lock()
	cli := e.cli
	c := e.docker
	e.mu.Unlock()
	if cli != nil {
		return cli, nil
	}
	if c == nil {
		return nil, errors.New("Docker 不可用")
	}
	cli, err := dockercli.Find(c.DockerHost())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.cli = cli
	e.mu.Unlock()
	return cli, nil
}

func (e *Engine) invalidateDocker() {
	e.mu.Lock()
	e.docker = nil
	e.cli = nil
	e.lastDetect = time.Time{}
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
