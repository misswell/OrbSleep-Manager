package engine

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orbsleep/internal/dockercli"
	"orbsleep/internal/launchagent"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/settings"
	"orbsleep/internal/state"

	_ "modernc.org/sqlite" // register the pure-Go driver for integrity checks
)

// Health returns the compact runtime health payload for the GUI.
func (e *Engine) Health() model.Health {
	st := e.Status()
	running, needsRepair, _ := e.store.CountUnresolvedTransactions()
	ops, _ := e.store.ListRunningOperations()
	return model.Health{
		DaemonVersion:           e.version,
		ProtocolVersion:         1,
		DataDir:                 e.dataDir,
		SocketPath:              settings.SocketPath(e.dataDir),
		UptimeSecs:              int64(time.Since(e.startedAt).Seconds()),
		DockerReachable:         st.DockerReachable,
		DockerContext:           st.Context,
		Proxy:                   st.Proxy,
		Counts:                  st.Counts,
		PendingTransactions:     running,
		NeedsRepairTransactions: needsRepair,
		RunningOperations:       len(ops),
		RestoreFailed:           restoreFailedMarkerExists(e.dataDir),
		LaunchAgentLoaded:       launchagent.Loaded(),
		LegacyLaunchAgent:       launchagent.LegacyLoaded() || launchagent.LegacyInstalled(),
		LegacyBinaries:          e.legacyBinaries(),
		LastError:               st.LastError,
	}
}

func (e *Engine) legacyBinaries() []string {
	out := []string{}
	for _, p := range []string{"/usr/local/bin/orbsleepd", "/usr/local/bin/orbsleepctl"} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// PrepareShutdown reports whether stopping now is safe.
func (e *Engine) PrepareShutdown() model.ShutdownPreparation {
	ops, _ := e.store.ListRunningOperations()
	txns, _ := e.store.ListRunningTransactions()
	safe := len(ops) == 0 && len(txns) == 0
	msg := "可以安全停止"
	if !safe {
		msg = fmt.Sprintf("有 %d 个进行中的操作、%d 个未完成事务；停止时后台会先取消并回滚", len(ops), len(txns))
	}
	if ops == nil {
		ops = []model.Operation{}
	}
	return model.ShutdownPreparation{
		Safe:                safe,
		RunningOperations:   ops,
		PendingTransactions: mapRecoveryTransactions(txns),
		Message:             msg,
	}
}

// PrepareUpdate reports whether an in-app upgrade can proceed safely.
func (e *Engine) PrepareUpdate() model.ShutdownPreparation {
	p := e.PrepareShutdown()
	if p.Safe {
		p.Message = "可以安全升级"
	} else {
		p.Message = "存在未完成操作或事务，建议先恢复再升级：" + p.Message
	}
	return p
}

// RecoveryList returns every unresolved transaction: running ones (auto
// recovery candidates) plus rollback_failed / needs_repair ones that need a
// manual retry. Hiding failed recoveries would make the GUI report "no
// pending transactions" while ports are still detached.
func (e *Engine) RecoveryList() model.RecoveryList {
	txns, _ := e.store.ListUnresolvedTransactions()
	_, needsRepair, _ := e.store.CountUnresolvedTransactions()
	msg := fmt.Sprintf("%d 个未完成事务", len(txns))
	if needsRepair > 0 {
		msg = fmt.Sprintf("%d 个未完成事务，其中 %d 个恢复失败需人工重试", len(txns), needsRepair)
	}
	return model.RecoveryList{
		Transactions: mapRecoveryTransactions(txns),
		Message:      msg,
	}
}

// RecoveryResolve retries recovery. With an empty id it re-runs batch
// recovery for running transactions; with an id it retries that specific
// transaction, including rollback_failed / needs_repair ones.
func (e *Engine) RecoveryResolve(id string) model.RecoveryList {
	if id != "" {
		_ = e.RecoverTransaction(id)
	} else {
		e.RecoverTransactions()
	}
	txns, _ := e.store.ListUnresolvedTransactions()
	return model.RecoveryList{
		Transactions: mapRecoveryTransactions(txns),
		Message:      fmt.Sprintf("剩余 %d 个未解决事务", len(txns)),
	}
}

func mapRecoveryTransactions(txns []state.Transaction) []model.RecoveryTransaction {
	out := make([]model.RecoveryTransaction, 0, len(txns))
	for _, t := range txns {
		out = append(out, model.RecoveryTransaction{
			ID:          t.ID,
			Kind:        t.Kind,
			OperationID: t.OperationID,
			Status:      t.Status,
			Phase:       t.Phase,
			StartedAt:   t.StartedAt,
			UpdatedAt:   t.UpdatedAt,
			Error:       t.Error,
		})
	}
	return out
}

// DockerContexts enumerates discoverable local Docker endpoints without
// changing the user's global Docker context.
func (e *Engine) DockerContexts(ctx context.Context) model.DockerContexts {
	home, _ := os.UserHomeDir()
	type candidate struct {
		name, endpoint, kind, source string
	}
	var cands []candidate
	add := func(name, endpoint, kind, source string) {
		endpoint = strings.TrimPrefix(strings.TrimSpace(endpoint), "unix://")
		if endpoint == "" {
			return
		}
		for _, existing := range cands {
			if existing.endpoint == endpoint {
				return
			}
		}
		cands = append(cands, candidate{name: name, endpoint: endpoint, kind: kind, source: source})
	}

	if v := os.Getenv("DOCKER_HOST"); strings.HasPrefix(v, "unix://") {
		add("DOCKER_HOST", v, "env", "env")
	}
	add("OrbStack", filepath.Join(home, ".orbstack", "run", "docker.sock"), "orbstack", "orbstack")
	add("Docker Desktop", filepath.Join(home, ".docker", "run", "docker.sock"), "docker-desktop", "docker-desktop")
	add("Default", "/var/run/docker.sock", "docker", "default")

	metaDir := filepath.Join(home, ".docker", "contexts", "meta")
	if entries, err := os.ReadDir(metaDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(metaDir, entry.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var meta struct {
				Name      string `json:"Name"`
				Endpoints map[string]struct {
					Host string `json:"Host"`
				} `json:"Endpoints"`
			}
			if json.Unmarshal(raw, &meta) != nil {
				continue
			}
			if host := meta.Endpoints["docker"].Host; host != "" {
				add(meta.Name, host, "context", "context")
			}
		}
	}

	selected := strings.TrimPrefix(strings.TrimSpace(e.settings.Get().DockerSocket), "unix://")
	current := selected
	if current == "" {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if c, err := e.dockerClient(dctx); err == nil {
			current = c.Socket()
		}
		cancel()
	}

	out := make([]model.DockerContext, 0, len(cands))
	for _, c := range cands {
		out = append(out, model.DockerContext{
			Name:      c.name,
			Endpoint:  "unix://" + c.endpoint,
			Kind:      c.kind,
			Source:    c.source,
			Current:   c.endpoint == current,
			Reachable: unixReachable(c.endpoint, 300*time.Millisecond),
		})
	}
	return model.DockerContexts{Contexts: out, Selected: "unix://" + current}
}

// SelectDockerContext stores an OrbSleep-local Docker socket choice.
func (e *Engine) SelectDockerContext(ctx context.Context, nameOrEndpoint string) (model.DockerContexts, error) {
	list := e.DockerContexts(ctx)
	needle := strings.TrimPrefix(strings.TrimSpace(nameOrEndpoint), "unix://")
	var chosen *model.DockerContext
	for i := range list.Contexts {
		c := list.Contexts[i]
		endpoint := strings.TrimPrefix(c.Endpoint, "unix://")
		if c.Name == nameOrEndpoint || c.Endpoint == nameOrEndpoint || endpoint == needle {
			chosen = &list.Contexts[i]
			break
		}
	}
	if chosen == nil {
		return list, fmt.Errorf("未找到 Docker 上下文: %s", nameOrEndpoint)
	}
	if !chosen.Reachable {
		return list, fmt.Errorf("该 Docker 上下文不可达: %s", chosen.Endpoint)
	}
	current := strings.TrimPrefix(strings.TrimSpace(e.settings.Get().DockerSocket), "unix://")
	chosenEndpoint := strings.TrimPrefix(chosen.Endpoint, "unix://")
	if chosenEndpoint != current {
		// Managed services and in-flight transactions are pinned to the
		// Docker instance they were created on; switching instances under
		// them can attach wrong containers or corrupt Compose state.
		services, serr := e.store.ListServices()
		if serr != nil {
			return list, fmt.Errorf("读取托管服务失败: %w", serr)
		}
		if len(services) > 0 {
			return list, fmt.Errorf("仍有 %d 个托管服务，请先恢复后再切换 Docker 环境", len(services))
		}
		if _, running, _ := e.store.CountUnresolvedTransactions(); running > 0 {
			return list, fmt.Errorf("存在未完成事务，请先完成恢复后再切换 Docker 环境")
		}
	}
	s := e.settings.Get()
	s.DockerSocket = strings.TrimPrefix(chosen.Endpoint, "unix://")
	if err := e.settings.Save(s); err != nil {
		return list, err
	}
	e.invalidateDocker()
	e.logger.Printf("info", "Docker 上下文已切换为 %s（%s）", chosen.Name, chosen.Endpoint)
	return e.DockerContexts(ctx), nil
}

func unixReachable(path string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("unix", path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// uniqueStamp returns a second-precision timestamp plus a random suffix so
// two backups (or log exports) created in the same second never collide.
func uniqueStamp() string {
	rand := strings.TrimPrefix(newID(""), "-")
	if len(rand) > 8 {
		rand = rand[:8]
	}
	return fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), rand)
}

// CreateBackup writes a consistent SQLite snapshot plus settings into a zip.
func (e *Engine) CreateBackup() (model.BackupEntry, error) {
	dir := filepath.Join(e.settings.Get().BackupDir, "export")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.BackupEntry{}, err
	}
	stamp := uniqueStamp()
	snapshot := filepath.Join(dir, ".state-"+stamp+".sqlite")
	if err := e.store.BackupTo(snapshot); err != nil {
		return model.BackupEntry{}, fmt.Errorf("备份数据库失败: %w", err)
	}
	defer os.Remove(snapshot)
	target := filepath.Join(dir, "OrbSleep-backup-"+stamp+".zip")
	if err := writeBackupZip(target, snapshot, e.settings.Get(), e.version); err != nil {
		return model.BackupEntry{}, err
	}
	info, _ := os.Stat(target)
	entry := model.BackupEntry{
		ID:        "backup-" + stamp,
		Path:      target,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Kind:      "manual",
		Note:      "手动备份（数据库 + 设置）",
	}
	if info != nil {
		entry.SizeBytes = info.Size()
	}
	e.logger.Printf("info", "已创建备份 %s", target)
	return entry, nil
}

func writeBackupZip(target, dbPath string, s model.Settings, version string) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	write := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	dbFile, err := os.Open(dbPath)
	if err != nil {
		zw.Close()
		f.Close()
		return err
	}
	defer dbFile.Close()
	dbEntry, err := zw.Create("state.sqlite")
	if err != nil {
		zw.Close()
		f.Close()
		return err
	}
	// Stream instead of loading the whole database into memory.
	if _, err := io.Copy(dbEntry, dbFile); err != nil {
		zw.Close()
		f.Close()
		return err
	}
	dbFile.Close()
	settingsData, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		err = write("settings.json", settingsData)
	}
	if err != nil {
		zw.Close()
		f.Close()
		return fmt.Errorf("写入备份条目失败: %w", err)
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"version":   version,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
		"dataDir":   s.DataDir,
	}, "", "  ")
	if err == nil {
		err = write("manifest.json", manifest)
	}
	if err != nil {
		zw.Close()
		f.Close()
		return fmt.Errorf("写入备份条目失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ListBackups lists manual backup archives.
func (e *Engine) ListBackups() model.BackupsResult {
	dir := filepath.Join(e.settings.Get().BackupDir, "export")
	entries, _ := os.ReadDir(dir)
	out := []model.BackupEntry{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, model.BackupEntry{
			ID:        backupIDFromPath(entry.Name()),
			Path:      filepath.Join(dir, entry.Name()),
			CreatedAt: info.ModTime().UTC().Format(time.RFC3339),
			SizeBytes: info.Size(),
			Kind:      "manual",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return model.BackupsResult{Backups: out}
}

// ExportLogs writes recent daemon log lines to a file.
func (e *Engine) ExportLogs(limit int) (model.LogsExportResult, error) {
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	lines, err := e.logger.Tail(limit)
	if err != nil {
		return model.LogsExportResult{}, err
	}
	dir := filepath.Join(e.dataDir, "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.LogsExportResult{}, err
	}
	path := filepath.Join(dir, "logs-"+uniqueStamp()+".log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return model.LogsExportResult{}, err
	}
	return model.LogsExportResult{Path: path, Lines: len(lines)}, nil
}

// RestoreBackup validates a backup archive and writes a restore marker. The
// daemon exits after the RPC returns; main applies the marker before opening
// SQLite on the next start, which keeps the restore atomic and crash-safe.
func (e *Engine) RestoreBackup(idOrPath string) (model.RestoreResult, error) {
	var target string
	for _, entry := range e.ListBackups().Backups {
		if entry.ID == idOrPath || entry.Path == idOrPath {
			target = entry.Path
			break
		}
	}
	if target == "" {
		if _, err := os.Stat(idOrPath); err == nil {
			target = idOrPath
		}
	}
	if target == "" {
		return model.RestoreResult{}, fmt.Errorf("找不到备份: %s", idOrPath)
	}
	if err := validateBackupZip(target); err != nil {
		return model.RestoreResult{}, err
	}
	marker := filepath.Join(e.dataDir, "restore-request.json")
	payload, _ := json.Marshal(map[string]string{
		"backupPath":  target,
		"requestedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(marker, payload, 0o600); err != nil {
		return model.RestoreResult{}, fmt.Errorf("写入恢复请求失败: %w", err)
	}
	e.logger.Printf("info", "已登记备份恢复请求: %s", target)
	return model.RestoreResult{OK: true, RestartRequired: true, Message: "已登记恢复请求；后台服务将退出，重启后应用备份", BackupPath: target}, nil
}

func validateBackupZip(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("无法读取备份文件: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != "state.sqlite" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		header := make([]byte, 16)
		n, _ := rc.Read(header)
		rc.Close()
		if n < 16 || string(header[:16]) != "SQLite format 3\x00" {
			return fmt.Errorf("备份中的 state.sqlite 不是有效的 SQLite 文件")
		}
		return nil
	}
	return fmt.Errorf("备份缺少 state.sqlite")
}

// ApplyPendingRestore is called by orbsleepd before opening SQLite. It replaces
// state.sqlite with the snapshot from a previously requested backup, preserving
// the current database as state.sqlite.pre-restore-<ts>.
//
// Any failure quarantines the marker as restore-failed-<ts>.json and returns a
// *RestoreError. The caller must keep starting with the existing database:
// the daemon must never enter a launchd restart loop over a broken backup.
func ApplyPendingRestore(dataDir string) error {
	marker := filepath.Join(dataDir, "restore-request.json")
	raw, err := os.ReadFile(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fail := func(format string, args ...any) error {
		cause := fmt.Sprintf(format, args...)
		stamp := time.Now().Format("20060102-150405")
		quarantine := filepath.Join(dataDir, "restore-failed-"+stamp+".json")
		if qerr := os.Rename(marker, quarantine); qerr != nil {
			_ = os.Remove(marker)
			cause += "（恢复标记已删除，证据丢失于 " + qerr.Error() + "）"
		}
		return &RestoreError{Cause: cause}
	}
	var req struct {
		BackupPath string `json:"backupPath"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.BackupPath == "" {
		return fail("恢复请求无效: %v", err)
	}
	r, err := zip.OpenReader(req.BackupPath)
	if err != nil {
		return fail("打开备份失败: %v", err)
	}
	defer r.Close()
	var src *zip.File
	for _, f := range r.File {
		if f.Name == "state.sqlite" {
			src = f
			break
		}
	}
	if src == nil {
		return fail("备份缺少 state.sqlite")
	}
	rc, err := src.Open()
	if err != nil {
		return fail("读取备份条目失败: %v", err)
	}
	temp := filepath.Join(dataDir, ".restore-state.tmp")
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		rc.Close()
		return fail("写入临时数据库失败: %v", err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		rc.Close()
		return fail("解压备份数据库失败: %v", err)
	}
	out.Close()
	rc.Close()

	// A matching file header is not enough: verify the database actually
	// opens and passes integrity_check before replacing the live one.
	if err := sqliteIntegrityCheck(temp); err != nil {
		return fail("备份数据库完整性校验失败: %v", err)
	}

	dbPath := filepath.Join(dataDir, "state.sqlite")
	if _, err := os.Stat(dbPath); err == nil {
		backup := filepath.Join(dataDir, "state.sqlite.pre-restore-"+time.Now().Format("20060102-150405"))
		if err := copyFile(dbPath, backup); err != nil {
			return fail("备份当前数据库失败: %v", err)
		}
	}
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	if err := os.Rename(temp, dbPath); err != nil {
		return fail("替换数据库失败: %v", err)
	}
	_ = os.Remove(marker)
	fmt.Fprintf(os.Stderr, "已从备份恢复数据库: %s\n", req.BackupPath)
	return nil
}

// RestoreError marks a failed backup restore. The daemon continues starting
// with the existing database instead of exiting into a launchd restart loop.
type RestoreError struct {
	Cause string
}

func (e *RestoreError) Error() string { return e.Cause }

// sqliteIntegrityCheck opens path with the project's pure-Go SQLite driver
// and runs PRAGMA integrity_check.
func sqliteIntegrityCheck(path string) error {
	dsn := "file:" + path + "?_pragma=busy_timeout(3000)&_pragma=journal_mode(DELETE)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	return nil
}

// restoreFailedMarkerExists reports whether a quarantined restore marker is
// present, so the GUI can tell the user a restore attempt failed.
func restoreFailedMarkerExists(dataDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "restore-failed-*.json"))
	return len(matches) > 0
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func backupIDFromPath(name string) string {
	base := strings.TrimSuffix(name, ".zip")
	if strings.HasPrefix(base, "OrbSleep-backup-") {
		return "backup-" + strings.TrimPrefix(base, "OrbSleep-backup-")
	}
	return base
}

// LogSources lists log streams the GUI can show.
func (e *Engine) LogSources() []string {
	sources := []string{"daemon"}
	if installed, _ := e.proxyInstalled(); installed {
		sources = append(sources, "proxy")
	}
	return sources
}

// ProxyLogs returns recent logs from the OrbSleep proxy container.
func (e *Engine) ProxyLogs(ctx context.Context, limit int) ([]model.LogEntry, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	cli, err := dockercli.Find(c.DockerHost())
	if err != nil {
		return nil, err
	}
	raw, err := cli.Logs(ctx, proxy.ContainerName, limit)
	if err != nil {
		return nil, err
	}
	return parseDockerLogs(string(raw)), nil
}

func parseDockerLogs(raw string) []model.LogEntry {
	raw = strings.TrimRight(raw, "\n")
	if raw == "" {
		return []model.LogEntry{}
	}
	lines := strings.Split(raw, "\n")
	out := make([]model.LogEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		at, msg := "", line
		if idx := strings.IndexByte(line, ' '); idx > 0 {
			at = line[:idx]
			msg = strings.TrimSpace(line[idx+1:])
		}
		level := "info"
		low := strings.ToLower(msg)
		if strings.Contains(low, "error") || strings.Contains(low, "fatal") || strings.Contains(low, "panic") {
			level = "error"
		} else if strings.Contains(low, "warn") {
			level = "warn"
		}
		out = append(out, model.LogEntry{At: at, Level: level, Message: msg})
	}
	return out
}
