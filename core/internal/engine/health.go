package engine

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orbsleep/internal/compose"
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
	running, needsRepair, txnErr := e.store.CountUnresolvedTransactions()
	ops, opsErr := e.store.ListRunningOperations()
	if txnErr != nil || opsErr != nil {
		// Never report a fake zero: record the failure so the GUI sees it.
		e.setLastErrorQuiet(fmt.Sprintf("读取运行状态失败: %v %v", txnErr, opsErr))
	}
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

// PrepareShutdown reports whether stopping now is safe. It covers every
// unresolved state: running operations, running transactions and recoveries
// that failed (rollback_failed / needs_repair). A database read error is
// fail-closed: the caller must not stop on an unknown board.
func (e *Engine) PrepareShutdown() model.ShutdownPreparation {
	ops, opsErr := e.store.ListRunningOperations()
	txns, txnErr := e.store.ListUnresolvedTransactions()
	var readErrs []string
	if opsErr != nil {
		readErrs = append(readErrs, "读取进行中操作失败: "+opsErr.Error())
	}
	if txnErr != nil {
		readErrs = append(readErrs, "读取未完成事务失败: "+txnErr.Error())
	}
	safe := len(readErrs) == 0 && len(ops) == 0 && len(txns) == 0
	msg := "可以安全停止"
	switch {
	case len(readErrs) > 0:
		msg = "无法确认当前状态（fail-closed）：" + strings.Join(readErrs, "；")
	case !safe:
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

// beginSafeWrite is the single entry for every business write: it takes the
// write mutex and then re-checks, under the lock, that no unresolved
// recovery material and no pending maintenance state exists. The check at
// the synchronous entry (ApplyPlan, ServiceAction, …) is only advisory — the
// authoritative one happens here, because a previous operation may have
// failed into rollback_failed between the entry check and this lock.
func (e *Engine) beginSafeWrite() (func(), error) {
	e.writeMu.Lock()
	if err := e.ensureNoUnresolvedWork(); err != nil {
		e.writeMu.Unlock()
		return nil, err
	}
	return e.writeMu.Unlock, nil
}

// ensureNoUnresolvedWork refuses every new business operation (attach,
// restore/remove, start/stop, proxy install, settings changes) while
// recovery material is unresolved. A rollback_failed transaction whose
// Compose was already modified must be resolved first: a new operation could
// edit the same file and a later recovery would overwrite the new state with
// its stale snapshot. Database read errors fail closed. Recovery operations
// themselves are the only callers exempt from this gate.
func (e *Engine) ensureNoUnresolvedWork() error {
	txns, err := e.store.ListUnresolvedTransactions()
	if err != nil {
		return fmt.Errorf("读取未完成事务失败，已拒绝操作: %w", err)
	}
	if len(txns) == 0 {
		return nil
	}
	needsRepair := 0
	for _, t := range txns {
		if t.Status != "running" {
			needsRepair++
		}
	}
	return fmt.Errorf("存在 %d 个未完成事务（其中 %d 个需修复）；请先在「诊断与修复」中恢复后再执行新操作", len(txns), needsRepair)
}

// CheckStopSafety guards the plain "stop daemon" action. Unlike shutdown
// during an in-flight operation (which cancels and rolls back), a stop with
// managed services present leaves them without gateway listeners: their
// original host ports live inside the daemon process. Restart/upgrade flows
// pass force=true because they immediately re-launch the daemon; uninstall
// flows must instead restore services and resolve transactions first.
func (e *Engine) CheckStopSafety(force bool) error {
	if force {
		return nil
	}
	services, err := e.store.ListServices()
	if err != nil {
		return fmt.Errorf("读取托管服务失败，已拒绝停止: %w", err)
	}
	if len(services) > 0 {
		return fmt.Errorf("仍有 %d 个托管服务依赖端口网关监听；停止后台后原端口将无法访问。请先恢复这些服务，或使用「重启服务」", len(services))
	}
	running, needsRepair, err := e.store.CountUnresolvedTransactions()
	if err != nil {
		return fmt.Errorf("读取未完成事务失败，已拒绝停止: %w", err)
	}
	if running+needsRepair > 0 {
		return fmt.Errorf("存在 %d 个未完成事务（其中 %d 个需修复）；请先在「诊断与修复」中处理后再停止后台", running+needsRepair, needsRepair)
	}
	return nil
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
func (e *Engine) RecoveryList() (model.RecoveryList, error) {
	txns, txnErr := e.store.ListUnresolvedTransactions()
	_, needsRepair, countErr := e.store.CountUnresolvedTransactions()
	if txnErr != nil || countErr != nil {
		// An unreadable board must not be presented as "nothing to recover".
		return model.RecoveryList{}, fmt.Errorf("读取未完成事务失败: %v %v", txnErr, countErr)
	}
	msg := fmt.Sprintf("%d 个未完成事务", len(txns))
	if needsRepair > 0 {
		msg = fmt.Sprintf("%d 个未完成事务，其中 %d 个恢复失败需人工重试", len(txns), needsRepair)
	}
	return model.RecoveryList{
		Transactions: mapRecoveryTransactions(txns),
		Message:      msg,
	}, nil
}

// RecoveryResolve retries recovery. With an empty id it re-runs batch
// recovery for running transactions; with an id it retries that specific
// transaction, including rollback_failed / needs_repair ones.
func (e *Engine) RecoveryResolve(id string) (model.RecoveryList, error) {
	if id != "" {
		_ = e.RecoverTransaction(id)
	} else {
		e.RecoverTransactions()
	}
	txns, txnErr := e.store.ListUnresolvedTransactions()
	if txnErr != nil {
		return model.RecoveryList{}, fmt.Errorf("读取未完成事务失败: %w", txnErr)
	}
	return model.RecoveryList{
		Transactions: mapRecoveryTransactions(txns),
		Message:      fmt.Sprintf("剩余 %d 个未解决事务", len(txns)),
	}, nil
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

// ensureContextSwitchSafe refuses a Docker instance switch while the current
// instance still owns anything: managed services and in-flight transactions
// are pinned to the instance they were created on, and switching under them
// can attach wrong containers or corrupt Compose state. Read errors fail
// closed: an unreadable board is not a clean board.
func (e *Engine) ensureContextSwitchSafe() error {
	services, serr := e.store.ListServices()
	if serr != nil {
		return fmt.Errorf("读取托管服务失败，拒绝切换: %w", serr)
	}
	if len(services) > 0 {
		return fmt.Errorf("仍有 %d 个托管服务，请先恢复后再切换 Docker 环境", len(services))
	}
	// CountUnresolvedTransactions returns (running, needsRepair, err); both
	// states block the switch.
	running, needsRepair, terr := e.store.CountUnresolvedTransactions()
	if terr != nil {
		return fmt.Errorf("读取未完成事务失败，拒绝切换: %w", terr)
	}
	if running+needsRepair > 0 {
		return fmt.Errorf("存在 %d 个未完成事务（其中 %d 个需修复），请先完成恢复后再切换 Docker 环境", running+needsRepair, needsRepair)
	}
	return nil
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
		// Exclusive maintenance phase: a context switch must not interleave
		// with attach/restore/proxy-setup (runProxySetup snapshots and later
		// saves the full settings — a concurrent switch would either be
		// clobbered or clobber the socket choice). Guards are re-checked
		// while the lock is held.
		if !e.writeMu.TryLock() {
			return list, errors.New("正在执行接入/恢复等操作，请稍后重试")
		}
		if e.maintenance.Load() {
			e.writeMu.Unlock()
			return list, errors.New("备份恢复/维护进行中，已拒绝切换 Docker 环境")
		}
		ops, opsErr := e.store.ListRunningOperations()
		if opsErr != nil {
			e.writeMu.Unlock()
			return list, fmt.Errorf("读取进行中操作失败，拒绝切换: %w", opsErr)
		}
		if len(ops) > 0 {
			e.writeMu.Unlock()
			return list, fmt.Errorf("有 %d 个进行中的操作，请等待完成后再切换 Docker 环境", len(ops))
		}
		if err := e.ensureContextSwitchSafe(); err != nil {
			e.writeMu.Unlock()
			return list, err
		}
		s := e.settings.Get()
		s.DockerSocket = chosenEndpoint
		if err := e.settings.Save(s); err != nil {
			e.writeMu.Unlock()
			return list, err
		}
		e.invalidateDocker()
		// Plans snapshot containers of the previous instance; applying one
		// after the switch would fail its own file check, but dropping them
		// makes the invalidity explicit.
		e.plansMu.Lock()
		e.plans = map[string]*planEntry{}
		e.plansMu.Unlock()
		e.writeMu.Unlock()
		e.logger.Printf("info", "Docker 上下文已切换为 %s（%s）", chosen.Name, chosen.Endpoint)
		// Return a fresh view: reachability/current flags changed.
		return e.DockerContexts(ctx), nil
	}
	return list, nil
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

// backupFile is one file captured into a backup archive. Path is the absolute
// location the file must be restored to; Entry is its name inside the zip.
// Kind "compose" is a managed Compose file snapshot (the DB expects this
// content); kind "service-backup" is a managed service's stable backup dir
// (original Compose + metadata), which the serviceBackup: meta points at.
type backupFile struct {
	Path  string `json:"path"`
	Entry string `json:"entry"`
	Kind  string `json:"kind"`
	Mode  uint32 `json:"mode"`
}

// collectBackupFiles gathers the on-disk state that belongs to the current
// database: every managed service's Compose file and stable backup material.
// Without them a restored database would claim services are unmanaged while
// the Compose files still have their ports removed.
func (e *Engine) collectBackupFiles() ([]backupFile, map[string][]byte, error) {
	services, err := e.store.ListServices()
	if err != nil {
		return nil, nil, fmt.Errorf("读取托管服务失败: %w", err)
	}
	var files []backupFile
	blobs := map[string][]byte{}
	seenDir := map[string]bool{}
	seenCompose := map[string]bool{}
	for _, svc := range services {
		if svc.ComposeFile != "" && !seenCompose[svc.ComposeFile] {
			seenCompose[svc.ComposeFile] = true
			raw, err := os.ReadFile(svc.ComposeFile)
			if err != nil {
				return nil, nil, fmt.Errorf("读取 %s 失败: %w", svc.ComposeFile, err)
			}
			mode := os.FileMode(0o644)
			if st, serr := os.Stat(svc.ComposeFile); serr == nil {
				mode = st.Mode()
			}
			entry := fmt.Sprintf("compose/%04d", len(files))
			files = append(files, backupFile{Path: svc.ComposeFile, Entry: entry, Kind: "compose", Mode: uint32(mode)})
			blobs[entry] = raw
		}
		dir, ok := e.store.GetMeta("serviceBackup:" + svc.ID)
		if !ok || dir == "" || seenDir[dir] {
			continue
		}
		seenDir[dir] = true
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			return nil, nil, fmt.Errorf("读取服务备份目录 %s 失败: %w", dir, rerr)
		}
		for _, fe := range entries {
			if fe.IsDir() {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, fe.Name()))
			if rerr != nil {
				return nil, nil, fmt.Errorf("读取服务备份文件失败: %w", rerr)
			}
			mode := os.FileMode(0o600)
			if info, ierr := fe.Info(); ierr == nil {
				mode = info.Mode()
			}
			entry := fmt.Sprintf("services/%s/%s", svc.ID, fe.Name())
			files = append(files, backupFile{Path: filepath.Join(dir, fe.Name()), Entry: entry, Kind: "service-backup", Mode: uint32(mode)})
			blobs[entry] = raw
		}
	}
	return e.collectTxnBackupFiles(files, blobs, seenDir)
}

// CreateBackup writes a consistent snapshot: the SQLite database, settings,
// and the on-disk Compose/backup material the database refers to — including
// the recovery material of unresolved transactions. writeMu is held for the
// whole run so the database snapshot and the files come from one point in
// time; the archive is written to a temp name and renamed only on success.
func (e *Engine) CreateBackup() (model.BackupEntry, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
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
	files, blobs, err := e.collectBackupFiles()
	if err != nil {
		return model.BackupEntry{}, fmt.Errorf("收集 Compose/服务备份失败（请检查托管服务状态）: %w", err)
	}
	target := filepath.Join(dir, "OrbSleep-backup-"+stamp+".zip")
	tmp := target + ".tmp"
	if err := writeBackupZip(tmp, snapshot, e.settings.Get(), e.version, files, blobs); err != nil {
		_ = os.Remove(tmp)
		return model.BackupEntry{}, err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return model.BackupEntry{}, fmt.Errorf("完成备份失败: %w", err)
	}
	info, _ := os.Stat(target)
	entry := model.BackupEntry{
		ID:        "backup-" + stamp,
		Path:      target,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Kind:      "manual",
		Note:      fmt.Sprintf("手动备份（数据库 + 设置 + %d 个关联文件）", len(files)),
	}
	if info != nil {
		entry.SizeBytes = info.Size()
	}
	e.logger.Printf("info", "已创建备份 %s（含 %d 个关联文件）", target, len(files))
	return entry, nil
}

// collectTxnBackupFiles packages the on-disk recovery material referenced by
// unresolved transactions: without it a restored database would carry
// rollback_failed rows whose backup files no longer exist.
func (e *Engine) collectTxnBackupFiles(files []backupFile, blobs map[string][]byte, seenDir map[string]bool) ([]backupFile, map[string][]byte, error) {
	txns, err := e.store.ListUnresolvedTransactions()
	if err != nil {
		return files, blobs, fmt.Errorf("读取未完成事务失败: %w", err)
	}
	includeDir := func(dir, prefix string) error {
		if dir == "" || seenDir[dir] {
			return nil
		}
		seenDir[dir] = true
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return nil // nothing materialised for this item yet
			}
			return fmt.Errorf("读取恢复目录 %s 失败: %w", dir, rerr)
		}
		for _, fe := range entries {
			if fe.IsDir() {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, fe.Name()))
			if rerr != nil {
				return fmt.Errorf("读取恢复文件失败: %w", rerr)
			}
			mode := os.FileMode(0o600)
			if info, ierr := fe.Info(); ierr == nil {
				mode = info.Mode()
			}
			entry := fmt.Sprintf("%s/%04d-%s", prefix, len(files), fe.Name())
			files = append(files, backupFile{Path: filepath.Join(dir, fe.Name()), Entry: entry, Kind: "txn-backup", Mode: uint32(mode)})
			blobs[entry] = raw
		}
		return nil
	}
	for _, txn := range txns {
		var payload struct {
			Items []struct {
				BackupPath string `json:"backupPath"`
				ServiceID  string `json:"serviceId"`
			} `json:"items"`
			Service struct {
				ID string `json:"id"`
			} `json:"service"`
		}
		if err := json.Unmarshal([]byte(txn.Data), &payload); err != nil {
			return files, blobs, fmt.Errorf("事务 %s 数据损坏: %w", txn.ID, err)
		}
		for _, it := range payload.Items {
			if it.BackupPath != "" {
				if err := includeDir(filepath.Dir(it.BackupPath), "txns/"+txn.ID); err != nil {
					return files, blobs, err
				}
			}
			if it.ServiceID != "" {
				if bd, ok := e.store.GetMeta("serviceBackup:" + it.ServiceID); ok {
					if err := includeDir(bd, "txns/"+txn.ID); err != nil {
						return files, blobs, err
					}
				}
			}
		}
		if payload.Service.ID != "" {
			if bd, ok := e.store.GetMeta("serviceBackup:" + payload.Service.ID); ok {
				if err := includeDir(bd, "txns/"+txn.ID); err != nil {
					return files, blobs, err
				}
			}
		}
	}
	return files, blobs, nil
}

func writeBackupZip(target, dbPath string, s model.Settings, version string, files []backupFile, blobs map[string][]byte) error {
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
		"files":     files,
	}, "", "  ")
	if err == nil {
		err = write("manifest.json", manifest)
	}
	if err != nil {
		zw.Close()
		f.Close()
		return fmt.Errorf("写入备份条目失败: %w", err)
	}
	// Associated files come last: a truncated archive then still yields a
	// valid database-only backup for older daemons to restore.
	for _, bf := range files {
		w, werr := zw.CreateHeader(&zip.FileHeader{Name: bf.Entry, Method: zip.Deflate, Modified: time.Now()})
		if werr != nil {
			zw.Close()
			f.Close()
			return fmt.Errorf("写入备份条目 %s 失败: %w", bf.Entry, werr)
		}
		if _, werr := w.Write(blobs[bf.Entry]); werr != nil {
			zw.Close()
			f.Close()
			return fmt.Errorf("写入备份条目 %s 失败: %w", bf.Entry, werr)
		}
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
//
// Restore is fail-closed with respect to in-flight work: running operations
// and unresolved transactions would be silently erased by the database swap
// (including rollback_failed ones whose Compose was already modified and
// whose service record never existed). Managed services themselves are fine:
// the archive carries their Compose snapshots, so database and files are
// restored together.
func (e *Engine) RestoreBackup(idOrPath string) (model.RestoreResult, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if e.maintenance.Load() {
		return model.RestoreResult{}, errors.New("恢复/维护进行中，已拒绝新的恢复请求")
	}
	ops, opsErr := e.store.ListRunningOperations()
	txns, txnErr := e.store.ListUnresolvedTransactions()
	if opsErr != nil || txnErr != nil {
		return model.RestoreResult{}, fmt.Errorf("无法确认当前操作/事务状态（fail-closed）: %v %v", opsErr, txnErr)
	}
	if len(ops) > 0 || len(txns) > 0 {
		return model.RestoreResult{}, fmt.Errorf("存在 %d 个进行中的操作、%d 个未完成事务；请先完成或恢复后再恢复备份", len(ops), len(txns))
	}
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
	// The path allowlist is captured from THIS daemon's database and settings
	// — the trusted source — and stored in the marker. The next start trusts
	// only the marker, never the imported archive's own contents. The
	// archive's own database may contribute paths, but only ones that already
	// exist on this machine (a backup can never introduce a brand-new write
	// location such as a LaunchAgents plist).
	allow := e.liveRestoreAllowlist()
	allow = e.extendAllowlistFromBackup(target, allow)
	if err := validateManifestAgainstAllowlist(target, allow); err != nil {
		return model.RestoreResult{}, err
	}
	marker := filepath.Join(e.dataDir, "restore-request.json")
	payload, _ := json.Marshal(map[string]any{
		"backupPath":  target,
		"requestedAt": time.Now().UTC().Format(time.RFC3339),
		"allow":       allow,
	})
	if err := os.WriteFile(marker, payload, 0o600); err != nil {
		return model.RestoreResult{}, fmt.Errorf("写入恢复请求失败: %w", err)
	}
	// From this point every new business write is refused until the daemon
	// exits and the restore applies on the next start.
	e.maintenance.Store(true)
	e.logger.Printf("info", "已登记备份恢复请求: %s", target)
	return model.RestoreResult{OK: true, RestartRequired: true, Message: "已登记恢复请求；后台服务将退出，重启后应用备份", BackupPath: target}, nil
}

func validateBackupZip(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("无法读取备份文件: %w", err)
	}
	defer r.Close()
	if len(r.File) > maxZipEntries {
		return fmt.Errorf("备份包含 %d 个条目，超过上限", len(r.File))
	}
	var total uint64
	byName := map[string]*zip.File{}
	for _, f := range r.File {
		byName[f.Name] = f
		cap := uint64(maxZipEntryBytes)
		if f.Name == "state.sqlite" {
			cap = maxZipStateBytes
		}
		if f.UncompressedSize64 > cap {
			return fmt.Errorf("备份条目 %s 超过大小上限", f.Name)
		}
		total += f.UncompressedSize64
		if total > maxZipTotalBytes {
			return fmt.Errorf("备份解压总量超过上限")
		}
	}
	if _, ok := byName["state.sqlite"]; !ok {
		return fmt.Errorf("备份缺少 state.sqlite")
	}
	if f, ok := byName["manifest.json"]; ok {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		var manifest struct {
			Files []backupFile `json:"files"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("备份 manifest.json 无效: %w", err)
		}
		for _, bf := range manifest.Files {
			if _, ok := byName[bf.Entry]; !ok {
				return fmt.Errorf("备份缺少清单声明的文件条目 %s", bf.Entry)
			}
		}
	}
	// The database itself must be a real SQLite file.
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

// ApplyPendingRestore is called by orbsleepd before opening SQLite. The whole
// restore is a journalled transaction:
//
//	restore-orig-<ts>/   complete snapshot of the overwritten state — the
//	                    database, its WAL/SHM sidecars and every replaced file
//	                    (content + original mode)
//	restore-journal.json phase (ORIGINALS_SAVED → FILES_COMMITTED →
//	                    DATABASE_COMMITTED) with per-file commit flags
//
// A crash between phases is resolved on the next start from the journal:
// ORIGINALS_SAVED rolls the files back and quarantines the request (no
// automatic retry loop over a deterministic failure); FILES_COMMITTED
// finishes the database swap; DATABASE_COMMITTED only completes the cleanup.
// The live WAL/SHM sidecars are moved (not deleted) into the snapshot, so no
// committed WAL record can be lost to a partial sidecar removal.
func ApplyPendingRestore(dataDir string) error {
	marker := filepath.Join(dataDir, "restore-request.json")
	journalPath := filepath.Join(dataDir, "restore-journal.json")
	raw, err := os.ReadFile(marker)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// No pending request. A leftover journal means the previous restore
		// completed but crashed during cleanup — finish it.
		if j, jerr := readRestoreJournal(journalPath); jerr == nil && j != nil && j.Phase == journalDatabaseCommitted {
			finishRestoreCleanup(dataDir, j)
		}
		return nil
	}

	j, jReadErr := readRestoreJournal(journalPath)
	if jReadErr != nil {
		// A corrupted journal cannot describe the on-disk state: refuse to
		// touch anything and quarantine the request for manual recovery.
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("恢复日志损坏，已隔离恢复请求（fail-closed）: %v", jReadErr)}
	}
	if j != nil {
		switch j.Phase {
		case journalOriginalsSaved:
			// The database was never swapped. Roll the files back to their
			// journalled original state and quarantine the request: retrying
			// automatically would loop on a deterministic failure.
			rbErr := rollbackJournalFiles(j)
			quarantineRestoreMarker(dataDir, marker)
			_ = os.Remove(journalPath)
			cause := "恢复在文件阶段中断，已回滚到原始状态"
			if rbErr != nil {
				cause = fmt.Sprintf("恢复中断且回滚不完整（原始文件保留于 %s）: %v", j.OrigDir, rbErr)
			}
			return &RestoreError{Cause: cause}
		case journalFilesCommitted, journalDatabaseCommitting:
			// Decide by the actual state on disk, not by the phase string
			// alone: the crash may have happened before or after the rename.
			dbPath := filepath.Join(dataDir, "state.sqlite")
			_, tmpErr := os.Stat(filepath.Join(dataDir, ".restore-state.tmp"))
			tmpExists := tmpErr == nil
			if !tmpExists && j.Phase == journalDatabaseCommitting {
				// The rename may already have happened. When the staged
				// database is gone and the snapshot holds the original
				// sidecars, the swap completed — finish the cleanup.
				if _, err := os.Stat(filepath.Join(j.OrigDir, "state.sqlite-wal")); err == nil {
					if _, err2 := os.Stat(dbPath); err2 == nil {
						finishRestoreCleanup(dataDir, j)
						return nil
					}
				}
			}
			// Files are already the backup version; finish the database swap.
			staged := filepath.Join(dataDir, ".restore-state.tmp")
			if !tmpExists {
				// The staged database vanished without the swap completing —
				// rebuild it from the archive so the swap can proceed.
				if j.BackupPath == "" {
					quarantineRestoreMarker(dataDir, marker)
					_ = os.Remove(journalPath)
					return &RestoreError{Cause: "暂存数据库丢失且无法从备份重建"}
				}
				zr, zerr := zip.OpenReader(j.BackupPath)
				if zerr != nil {
					quarantineRestoreMarker(dataDir, marker)
					_ = os.Remove(journalPath)
					return &RestoreError{Cause: fmt.Sprintf("打开备份失败: %v", zerr)}
				}
				var src *zip.File
				for _, f := range zr.File {
					if f.Name == "state.sqlite" {
						src = f
						break
					}
				}
				if src == nil {
					zr.Close()
					quarantineRestoreMarker(dataDir, marker)
					_ = os.Remove(journalPath)
					return &RestoreError{Cause: "备份缺少 state.sqlite"}
				}
				if err := extractZipEntry(src, staged); err != nil {
					zr.Close()
					quarantineRestoreMarker(dataDir, marker)
					_ = os.Remove(journalPath)
					return &RestoreError{Cause: fmt.Sprintf("重建暂存数据库失败: %v", err)}
				}
				zr.Close()
			}
			if err := completeRestoreDatabase(dataDir, j, marker, journalPath, staged); err != nil {
				return err
			}
			return nil
		case journalDatabaseCommitted:
			finishRestoreCleanup(dataDir, j)
			return nil
		}
	}

	// Fresh restore: parse the request written by the live daemon.
	var req struct {
		BackupPath string            `json:"backupPath"`
		Allow      *restoreAllowlist `json:"allow"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.BackupPath == "" || req.Allow == nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("恢复请求无效（%v）或缺少路径白名单", err)}
	}
	r, err := zip.OpenReader(req.BackupPath)
	if err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("打开备份失败: %v", err)}
	}
	defer r.Close()
	byName := map[string]*zip.File{}
	for _, f := range r.File {
		byName[f.Name] = f
	}
	src := byName["state.sqlite"]
	if src == nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: "备份缺少 state.sqlite"}
	}
	temp := filepath.Join(dataDir, ".restore-state.tmp")
	if err := extractZipEntry(src, temp); err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("解压备份数据库失败: %v", err)}
	}
	// A matching file header is not enough: verify the database actually
	// opens and passes integrity_check before replacing the live one.
	if err := sqliteIntegrityCheck(temp); err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("备份数据库完整性校验失败: %v", err)}
	}

	stamp := time.Now().Format("20060102-150405")
	origDir := filepath.Join(dataDir, "restore-orig-"+stamp)
	plan, err := stageRestoreFiles(byName, req.Allow)
	if err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("准备恢复文件失败: %v", err)}
	}
	j, err = snapshotRestoreOriginals(plan, dataDir, origDir, stamp)
	if err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("备份原状态失败: %v", err)}
	}
	j.BackupPath = req.BackupPath
	if err := writeRestoreJournal(journalPath, j); err != nil {
		quarantineRestoreMarker(dataDir, marker)
		return &RestoreError{Cause: fmt.Sprintf("写入恢复日志失败: %v", err)}
	}
	if err := commitRestoreFiles(j, byName, journalPath); err != nil {
		rbErr := rollbackJournalFiles(j)
		quarantineRestoreMarker(dataDir, marker)
		_ = os.Remove(journalPath)
		cause := fmt.Sprintf("恢复关联文件失败: %v", err)
		if rbErr != nil {
			cause += fmt.Sprintf("；回滚不完整（原始文件保留于 %s）: %v", j.OrigDir, rbErr)
		}
		return &RestoreError{Cause: cause}
	}
	j.Phase = journalFilesCommitted
	if err := writeRestoreJournal(journalPath, j); err != nil {
		rollbackJournalFiles(j)
		quarantineRestoreMarker(dataDir, marker)
		_ = os.Remove(journalPath)
		return &RestoreError{Cause: fmt.Sprintf("记录恢复阶段失败: %v", err)}
	}

	if err := completeRestoreDatabase(dataDir, j, marker, journalPath, temp); err != nil {
		return err
	}
	// A successful restore supersedes earlier failed attempts.
	for _, stale := range restoreFailedMarkers(dataDir) {
		_ = os.Remove(stale)
	}
	fmt.Fprintf(os.Stderr, "已从备份恢复数据库与关联文件（原状态保留于 %s）: %s\n", j.OrigDir, req.BackupPath)
	return nil
}

const (
	journalOriginalsSaved     = "ORIGINALS_SAVED"
	journalFilesCommitted     = "FILES_COMMITTED"
	journalDatabaseCommitting = "DATABASE_COMMITTING"
	journalDatabaseCommitted  = "DATABASE_COMMITTED"
)

// restoreJournal is the durable state of one restore attempt. Written
// atomically at every phase transition and after every file commit.
type restoreJournal struct {
	Phase      string        `json:"phase"`
	BackupPath string        `json:"backupPath"`
	Stamp      string        `json:"stamp"`
	OrigDir    string        `json:"origDir"`
	Files      []journalFile `json:"files"`
}

// journalFile records one overwritten target and its original state. Saved is
// empty when the target did not exist before the restore (rollback deletes).
type journalFile struct {
	Target       string `json:"target"`
	Entry        string `json:"entry"` // archive entry providing the backup content
	Saved        string `json:"saved,omitempty"`
	OriginalMode uint32 `json:"originalMode"` // mode of the live file before the restore
	RestoreMode  uint32 `json:"restoreMode"`  // mode the backup wants on the target
	Committed    bool   `json:"committed"`
}

func readRestoreJournal(path string) (*restoreJournal, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var j restoreJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

func writeRestoreJournal(path string, j *restoreJournal) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	// The journal is the crash-recovery contract: it must reach the disk
	// before the state it describes changes.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// completeRestoreDatabase performs the database swap using the journalled
// snapshot. The sidecars are MOVED into the snapshot (never deleted), the
// DATABASE_COMMITTING phase is persisted before the rename, and every failure
// restores the original database, sidecars and files. Cleanup happens only
// after a fully committed restore.
func completeRestoreDatabase(dataDir string, j *restoreJournal, marker, journalPath, stagedDB string) error {
	dbPath := filepath.Join(dataDir, "state.sqlite")
	move := func(from, to string) error {
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Restore the original database and sidecars from the snapshot.
	restoreOriginalDB := func() error {
		if err := copyFile(filepath.Join(j.OrigDir, "state.sqlite"), dbPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := copyFile(filepath.Join(j.OrigDir, "state.sqlite-wal"), dbPath+"-wal"); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := copyFile(filepath.Join(j.OrigDir, "state.sqlite-shm"), dbPath+"-shm"); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	fail := func(cause string) error {
		// Files may already be the backup version and the database may be
		// swapped or untouched: restore everything the snapshot holds.
		rbFiles := rollbackJournalFiles(j)
		rbDB := restoreOriginalDB()
		quarantineRestoreMarker(dataDir, marker)
		if rbFiles == nil && rbDB == nil {
			// Rollback fully succeeded: the journal did its job and is no
			// longer needed.
			_ = os.Remove(journalPath)
			return &RestoreError{Cause: cause + "；已回滚到原始状态"}
		}
		// Rollback incomplete: keep the journal and snapshot as evidence and
		// leave the request quarantined — never retry automatically.
		cause += fmt.Sprintf("；回滚不完整（文件: %v；数据库: %v；原始状态保留于 %s）", rbFiles, rbDB, j.OrigDir)
		return &RestoreError{Cause: cause}
	}

	// Persist the committing phase BEFORE touching anything, so a crash here
	// is resumable by inspecting the actual database state.
	j.Phase = journalDatabaseCommitting
	if err := writeRestoreJournal(journalPath, j); err != nil {
		return fail("记录数据库提交阶段失败: " + err.Error())
	}
	if err := move(dbPath+"-wal", filepath.Join(j.OrigDir, "state.sqlite-wal")); err != nil {
		return fail("移出 WAL 失败: " + err.Error())
	}
	if err := move(dbPath+"-shm", filepath.Join(j.OrigDir, "state.sqlite-shm")); err != nil {
		return fail("移出 SHM 失败: " + err.Error())
	}
	if err := os.Rename(stagedDB, dbPath); err != nil {
		return fail("替换数据库失败: " + err.Error())
	}
	j.Phase = journalDatabaseCommitted
	if err := writeRestoreJournal(journalPath, j); err != nil {
		return fail("记录数据库完成阶段失败: " + err.Error())
	}
	_ = os.Remove(marker)
	_ = os.Remove(journalPath)
	return nil
}

// finishRestoreCleanup removes the journal evidence of a completed restore.
func finishRestoreCleanup(dataDir string, j *restoreJournal) {
	_ = os.Remove(filepath.Join(dataDir, "restore-request.json"))
	_ = os.Remove(filepath.Join(dataDir, "restore-journal.json"))
	for _, stale := range restoreFailedMarkers(dataDir) {
		_ = os.Remove(stale)
	}
}

// quarantineRestoreMarker renames a pending restore request into the failed
// evidence set so the next start cannot retry it automatically.
func quarantineRestoreMarker(dataDir, marker string) {
	stamp := time.Now().Format("20060102-150405")
	_ = os.Rename(marker, filepath.Join(dataDir, "restore-failed-"+stamp+".json"))
}

// rollbackJournalFiles restores every journalled target to its original state:
// previously existing files are rewritten with their saved content and mode,
// files created by the restore are deleted.
func rollbackJournalFiles(j *restoreJournal) error {
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, f := range j.Files {
		if !f.Committed {
			continue
		}
		if f.Saved == "" {
			if err := os.Remove(f.Target); err != nil && !os.IsNotExist(err) {
				note(fmt.Errorf("删除 %s 失败: %w", f.Target, err))
			}
			continue
		}
		raw, err := os.ReadFile(f.Saved)
		if err != nil {
			note(err)
			continue
		}
		mode := os.FileMode(f.OriginalMode)
		if mode == 0 {
			// A journal without a recorded mode (hand-crafted or from an
			// older writer) must not produce an unreadable 0000 file.
			mode = 0o644
		}
		if err := compose.WriteAtomic(f.Target, raw, mode); err != nil {
			note(fmt.Errorf("还原 %s 失败: %w", f.Target, err))
		}
	}
	return firstErr
}

// restoreAllowlist is the set of writable locations captured by the live
// daemon when the restore was requested: exact Compose file paths of managed
// services and OrbSleep's own backup directories. The archive manifest may
// only reference these — an import can never invent write targets.
type restoreAllowlist struct {
	Files []string `json:"files"`
	Dirs  []string `json:"dirs"`
}

func (a *restoreAllowlist) allows(path string) bool {
	for _, f := range a.Files {
		if f == path {
			return true
		}
	}
	for _, dir := range a.Dirs {
		if dir != "" && strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// liveRestoreAllowlist derives the allowlist from the running daemon's own
// persistent state — the trusted source, not the imported archive. Compose
// paths come from the registry of every path OrbSleep has ever managed (so a
// service deleted after the backup can still be restored) plus the live
// service records; directories are limited to OrbSleep's own backup
// directory. The whole data directory is deliberately NOT writable. Every
// entry is symlink-resolved so stage-time resolution matches.
func (e *Engine) liveRestoreAllowlist() *restoreAllowlist {
	allow := &restoreAllowlist{}
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" {
			return
		}
		resolved := path
		if r, rerr := filepath.EvalSymlinks(path); rerr == nil {
			resolved = r
		}
		if !seen[resolved] {
			seen[resolved] = true
			allow.Files = append(allow.Files, resolved)
		}
	}
	if raw, ok := e.store.GetMeta("knownComposePaths"); ok {
		var known []string
		if json.Unmarshal([]byte(raw), &known) == nil {
			for _, path := range known {
				add(path)
			}
		}
	}
	if services, err := e.store.ListServices(); err == nil {
		for _, svc := range services {
			add(svc.ComposeFile)
		}
	}
	s := e.settings.Get()
	if resolved, rerr := filepath.EvalSymlinks(s.BackupDir); rerr == nil {
		allow.Dirs = append(allow.Dirs, resolved)
	} else {
		allow.Dirs = append(allow.Dirs, s.BackupDir)
	}
	return allow
}

// registerKnownComposePath records a Compose path OrbSleep took under
// management. The registry persists after the service record is deleted so a
// later backup restore can still address the file.
func registerKnownComposePath(e *Engine, path string) {
	if path == "" {
		return
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	known := []string{}
	if raw, ok := e.store.GetMeta("knownComposePaths"); ok {
		_ = json.Unmarshal([]byte(raw), &known)
	}
	for _, existing := range known {
		if existing == path {
			return
		}
	}
	known = append(known, path)
	if raw, err := json.Marshal(known); err == nil {
		_ = e.store.SetMeta("knownComposePaths", string(raw))
	}
}

// unregisterKnownComposePath drops a path when OrbSleep stops managing it.
func unregisterKnownComposePath(e *Engine, path string) {
	if path == "" {
		return
	}
	known := []string{}
	if raw, ok := e.store.GetMeta("knownComposePaths"); ok {
		_ = json.Unmarshal([]byte(raw), &known)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	var kept []string
	for _, existing := range known {
		if existing != path {
			kept = append(kept, existing)
		}
	}
	if raw, err := json.Marshal(kept); err == nil {
		_ = e.store.SetMeta("knownComposePaths", string(raw))
	}
}

// extendAllowlistFromBackup adds recovery-material directories referenced by
// the backup's own database — but only those that exist on this machine AND
// sit under OrbSleep's managed backup directory. The archive database can
// never authorize a Compose path or any location outside the backup dir.
func (e *Engine) extendAllowlistFromBackup(backupPath string, live *restoreAllowlist) *restoreAllowlist {
	r, err := zip.OpenReader(backupPath)
	if err != nil {
		return live
	}
	defer r.Close()
	byName := map[string]*zip.File{}
	for _, f := range r.File {
		byName[f.Name] = f
	}
	dbEntry, ok := byName["state.sqlite"]
	if !ok {
		return live
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf(".orbsleep-allowlist-%d.sqlite", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := extractZipEntry(dbEntry, tmp); err != nil {
		return live
	}
	db, err := sql.Open("sqlite", "file:"+tmp+"?_pragma=busy_timeout(3000)")
	if err != nil {
		return live
	}
	defer db.Close()
	s := e.settings.Get()
	backupRoot, rerr := filepath.EvalSymlinks(s.BackupDir)
	if rerr != nil {
		backupRoot = s.BackupDir
	}
	underBackup := func(dir string) bool {
		if dir == "" {
			return false
		}
		resolved, rerr := filepath.EvalSymlinks(dir)
		if rerr != nil {
			return false
		}
		return resolved == backupRoot || strings.HasPrefix(resolved, backupRoot+string(filepath.Separator))
	}
	dirRows, err := db.Query(`SELECT value FROM meta WHERE key LIKE 'serviceBackup:%'`)
	if err == nil {
		for dirRows.Next() {
			var dir string
			if dirRows.Scan(&dir) == nil && underBackup(dir) {
				live.Dirs = append(live.Dirs, dir)
			}
		}
		dirRows.Close()
	}
	txnRows, err := db.Query(`SELECT data FROM transactions WHERE status IN ('running','rollback_failed','needs_repair')`)
	if err == nil {
		for txnRows.Next() {
			var data string
			if txnRows.Scan(&data) != nil {
				continue
			}
			var payload struct {
				Items []struct {
					BackupPath string `json:"backupPath"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(data), &payload) == nil {
				for _, it := range payload.Items {
					if it.BackupPath != "" && underBackup(filepath.Dir(it.BackupPath)) {
						live.Dirs = append(live.Dirs, filepath.Dir(it.BackupPath))
					}
				}
			}
		}
		txnRows.Close()
	}
	return live
}

// validateManifestAgainstAllowlist refuses archives whose manifest writes
// outside the live allowlist: the failure surfaces at request time instead of
// on the next start.
func validateManifestAgainstAllowlist(backupPath string, allow *restoreAllowlist) error {
	r, err := zip.OpenReader(backupPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		var manifest struct {
			Files []backupFile `json:"files"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("备份 manifest.json 无效: %w", err)
		}
		for _, bf := range manifest.Files {
			if !filepath.IsAbs(bf.Path) || filepath.Clean(bf.Path) != bf.Path || strings.Contains(bf.Path, "..") {
				return fmt.Errorf("备份清单包含不安全的路径: %q", bf.Path)
			}
			if !allow.allows(resolveRestoreTarget(bf.Path)) {
				return fmt.Errorf("备份清单路径 %q 不属于当前管理的服务或备份目录，拒绝恢复", bf.Path)
			}
		}
		return nil
	}
	return nil
}

// resolveRestoreTarget resolves the target path for allowlist comparison:
// the file itself when it exists (a symlink there would redirect the write),
// otherwise the deepest existing ancestor directory.
func resolveRestoreTarget(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolved, filepath.Base(path))
	}
	// The parent does not exist yet; resolve the closest existing ancestor.
	ancestor := filepath.Dir(dir)
	for ancestor != dir {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			rel, rerr := filepath.Rel(ancestor, path)
			if rerr == nil {
				return filepath.Join(resolved, rel)
			}
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			break
		}
		ancestor = next
	}
	return path
}

// extractZipEntry decompresses one archive entry to path with hard size caps.
func extractZipEntry(src *zip.File, path string) error {
	if src.UncompressedSize64 > maxZipEntryBytes {
		return fmt.Errorf("条目 %s 过大（%d 字节）", src.Name, src.UncompressedSize64)
	}
	rc, err := src.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxZipEntryBytes+1))
	out.Close()
	if err != nil {
		return err
	}
	if n > maxZipEntryBytes {
		return fmt.Errorf("条目 %s 超过大小上限", src.Name)
	}
	return nil
}

const (
	maxZipEntries    = 512
	maxZipEntryBytes = 64 << 20   // 64 MiB per file
	maxZipStateBytes = 256 << 20  // the database snapshot may be larger
	maxZipTotalBytes = 1024 << 20 // 1 GiB decompressed in total
)

// stageRestoreFiles validates and reads every manifest entry into memory. No
// filesystem write happens here. A read failure on an existing target aborts
// the restore: overwriting a file whose current content could not be read
// (and therefore not backed up) risks silent data loss.
func stageRestoreFiles(byName map[string]*zip.File, allow *restoreAllowlist) ([]stagedRestoreFile, error) {
	manifestRaw, ok := byName["manifest.json"]
	if !ok {
		return nil, nil // pre-0.2.2 archives restore database-only
	}
	manifestData, err := readZipEntry(manifestRaw)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Files []backupFile `json:"files"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, err
	}
	if len(manifest.Files) > maxZipEntries {
		return nil, fmt.Errorf("备份清单包含 %d 个文件，超过上限", len(manifest.Files))
	}
	var staged []stagedRestoreFile
	var total int64
	for _, bf := range manifest.Files {
		if !filepath.IsAbs(bf.Path) || filepath.Clean(bf.Path) != bf.Path || strings.Contains(bf.Path, "..") {
			return nil, fmt.Errorf("备份清单包含不安全的路径: %q", bf.Path)
		}
		if !allow.allows(resolveRestoreTarget(bf.Path)) {
			return nil, fmt.Errorf("备份清单路径 %q 不属于该备份记录的服务或恢复材料，拒绝写入", bf.Path)
		}
		entry, ok := byName[bf.Entry]
		if !ok {
			return nil, fmt.Errorf("备份缺少文件条目 %s", bf.Entry)
		}
		if entry.UncompressedSize64 > maxZipEntryBytes {
			return nil, fmt.Errorf("条目 %s 超过单文件大小上限", bf.Entry)
		}
		total += int64(entry.UncompressedSize64)
		if total > maxZipTotalBytes {
			return nil, fmt.Errorf("备份解压总量超过上限")
		}
		data, err := readZipEntry(entry)
		if err != nil {
			return nil, err
		}
		st := stagedRestoreFile{bf: bf, data: data}
		current, rerr := os.ReadFile(bf.Path)
		switch {
		case rerr == nil:
			st.current = current
		case os.IsNotExist(rerr):
			st.current = nil
		default:
			// The current content is unreadable (permissions, I/O error):
			// overwriting it without a snapshot risks silent data loss.
			return nil, fmt.Errorf("读取 %s 失败，已中止恢复: %w", bf.Path, rerr)
		}
		staged = append(staged, st)
	}
	return staged, nil
}

func readZipEntry(src *zip.File) ([]byte, error) {
	if src.UncompressedSize64 > maxZipEntryBytes {
		return nil, fmt.Errorf("条目 %s 超过单文件大小上限", src.Name)
	}
	rc, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxZipEntryBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxZipEntryBytes {
		return nil, fmt.Errorf("条目 %s 超过单文件大小上限", src.Name)
	}
	return data, nil
}

type stagedRestoreFile struct {
	bf      backupFile
	data    []byte
	current []byte // nil when the target does not exist yet
}

// snapshotRestoreOriginals copies every existing target that differs from the
// backup content, plus the live database and its WAL/SHM sidecars, into the
// restore-orig staging directory — before anything is overwritten. Each
// journalled file records whether it existed and its original mode.
func snapshotRestoreOriginals(staged []stagedRestoreFile, dataDir, origDir, stamp string) (*restoreJournal, error) {
	j := &restoreJournal{Stamp: stamp, OrigDir: origDir}
	if err := os.MkdirAll(origDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "state.sqlite")
	for i, st := range staged {
		entry := journalFile{
			Target:       st.bf.Path,
			Entry:        st.bf.Entry,
			OriginalMode: uint32(os.FileMode(0o644)),
			RestoreMode:  uint32(st.bf.Mode),
		}
		if st.current != nil {
			if info, ierr := os.Stat(st.bf.Path); ierr == nil {
				entry.OriginalMode = uint32(info.Mode())
			}
			saved := filepath.Join(origDir, fmt.Sprintf("%04d-%s", i, filepath.Base(st.bf.Path)))
			if err := os.WriteFile(saved, st.current, 0o600); err != nil {
				return nil, err
			}
			entry.Saved = saved
		}
		j.Files = append(j.Files, entry)
	}
	// Snapshot the database and both sidecars: the swap later MOVES the live
	// sidecars here, so no committed WAL record can ever be lost to a partial
	// sidecar removal.
	if _, err := os.Stat(dbPath); err == nil {
		if err := copyFile(dbPath, filepath.Join(origDir, "state.sqlite")); err != nil {
			return nil, fmt.Errorf("备份当前数据库失败: %w", err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			if err := copyFile(dbPath+suffix, filepath.Join(origDir, "state.sqlite"+suffix)); err != nil {
				return nil, fmt.Errorf("备份 %s 失败: %w", suffix, err)
			}
		}
	}
	return j, nil
}

// commitRestoreFiles writes the staged files in order, recording each commit
// in the journal so a crash leaves a resumable, per-file state.
func commitRestoreFiles(j *restoreJournal, byName map[string]*zip.File, journalPath string) error {
	for i := range j.Files {
		jf := &j.Files[i]
		bf, ok := byName[jf.Entry]
		if !ok {
			return fmt.Errorf("备份缺少文件条目 %s", jf.Entry)
		}
		data, err := readZipEntry(bf)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(jf.Target), 0o700); err != nil {
			return err
		}
		// Record the intent BEFORE the write and fsync it. A crash in the
		// window between the marker and the write leaves the file untouched,
		// and rolling it back just re-restores the original — an idempotent
		// no-op on top of an unchanged file. The reverse order (write first,
		// journal after) would leave a modified file the journal believes is
		// untouched.
		jf.Committed = true
		if err := writeRestoreJournal(journalPath, j); err != nil {
			return fmt.Errorf("记录提交意图失败: %w", err)
		}
		if err := compose.WriteAtomic(jf.Target, data, os.FileMode(jf.RestoreMode)); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", jf.Target, err)
		}
		if err := writeRestoreJournal(journalPath, j); err != nil {
			return fmt.Errorf("刷新提交状态失败: %w", err)
		}
	}
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
	return len(restoreFailedMarkers(dataDir)) > 0
}

// restoreFailedMarkers lists the quarantined restore-failure markers.
func restoreFailedMarkers(dataDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "restore-failed-*.json"))
	return matches
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
