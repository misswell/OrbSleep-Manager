package engine

// v0.2.9 crash matrix for the journalled restore transaction:
//   - a crash during rollback must resume the rollback, never the forward commit
//   - a journal without a request must still be resolved (rollback, not commit)
//   - the archive is bound to the transaction by digest
//   - identical main-file bytes must never be read as "swap already done" while
//     a WAL holds newer committed records
//   - a failed marker quarantine must keep the journal
//   - an unresolved journal freezes business writes at startup
//   - services attached by older versions get their Compose path registered

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbsleep/internal/model"
	"orbsleep/internal/state"
)

// writeRestoreRequest writes the marker exactly as the live daemon does.
func writeRestoreRequest(t *testing.T, dataDir, backup string, files []string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"backupPath":  backup,
		"requestedAt": time.Now().UTC().Format(time.RFC3339),
		"allow":       map[string]any{"files": files, "dirs": []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "restore-request.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Review P0-01: a crash after the rollback intent was journalled must resume the
// rollback. The files are still the backup version here (the crash landed
// between "persist ROLLING_BACK" and "restore the first file"), and the live
// database may already be the swapped one.
func TestRestoreRollingBackResumeContinuesRollback(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalRollingBack, committed: true, swapped: true,
		liveWAL: []byte("committed-wal-records"), snapshotWAL: []byte("committed-wal-records"),
	})

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("ROLLING_BACK must finish the rollback, got %v", err)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatalf("the file was not rolled back:\n%s", raw)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("the swapped-in database survived the rollback")
	}
	if got := readAll(t, l.db+"-wal"); string(got) != "committed-wal-records" {
		t.Fatalf("the original WAL was not restored: %q", got)
	}
	mustNotExist(t, l.journal)
	if n := failedMarkerCount(t, dir); n != 1 {
		t.Fatalf("request must be quarantined as evidence, got %d markers", n)
	}

	// A second start must not resurrect anything: no journal, no marker.
	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("a resolved transaction must not be retried: %v", err)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatal("resolved compose was modified again")
	}
}

// The other crash point inside the rollback: the files are already back, the
// database still holds the backup version.
func TestRestoreRollingBackResumeRestoresDatabaseAfterFilesLanded(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalRollingBack, committed: true, swapped: true,
		// The file phase already reverted: the journal still marks the target
		// committed, but its content is the original again.
		snapshotWAL: []byte("original-wal-records"),
	})
	if err := os.WriteFile(l.composeFile, l.drifted, 0o644); err != nil {
		t.Fatal(err)
	}

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("rollback must complete, got %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("database was not rolled back")
	}
	if got := readAll(t, l.db+"-wal"); string(got) != "original-wal-records" {
		t.Fatalf("snapshot WAL was not restored: %q", got)
	}
	mustNotExist(t, l.journal)
}

// Review P0-02: a journal that survived without its request (quarantine rename
// crashed) must never be resumed forward. Only DATABASE_COMMITTED is safe to
// finish; everything else is rolled back, because the files may already hold
// their original content.
func TestRestoreWithoutMarkerNeverResumesForward(t *testing.T) {
	dir := t.TempDir()
	backupDB := distinctDatabase(t, "markerless-backup-db")
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalDatabaseCommitting, committed: true, swapped: true,
		backupDB: backupDB, digest: sha256Hex(backupDB),
	})
	if err := os.Remove(l.marker); err != nil {
		t.Fatal(err)
	}

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("a marker-less journal must roll back, got %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("the backup database was kept without a request")
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatal("the backup compose was kept without a request")
	}
	mustNotExist(t, l.journal)
}

// Review P1-01: the archive is bound to the transaction by digest. A backup
// replaced after the journal was written can never supply the bytes that
// finish the commit.
func TestRestoreRefusesArchiveSwappedAfterTransactionStarted(t *testing.T) {
	dir := t.TempDir()
	origDB := distinctDatabase(t, "bound-backup-db")
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalFilesCommitted, committed: true, noStaged: true,
		backupDB: origDB,
	})
	// Replace the archive with a different database and recompute nothing: the
	// journal still names the original digest.
	var j restoreJournal
	raw, err := os.ReadFile(l.journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	j.BackupSHA256 = sha256Hex(rawFile(t, l.backup))
	if err := writeRestoreJournal(l.journal, &j); err != nil {
		t.Fatal(err)
	}
	otherDB := distinctDatabase(t, "swapped-backup-db")
	writeBackupArchive(t, l.backup, otherDB, l.composeFile, l.archived)

	err = ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "备份文件在恢复事务开始后发生变化") {
		t.Fatalf("a swapped archive must be refused, got %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("the swapped archive's database was installed")
	}
	mustNotExist(t, l.journal)
	if n := failedMarkerCount(t, dir); n != 1 {
		t.Fatalf("request must be quarantined, got %d markers", n)
	}
}

// Review P0-03: restoring the same archive twice must really restore it. The
// live main file can be byte-identical to the backup while the WAL still holds
// newer committed records — that must never be read as "swap already done".
func TestRestoreReplacesWALWhenMainFileMatchesBackup(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "state.sqlite")

	// A real WAL-mode database, cleanly closed: a consistent main file that the
	// backup will carry.
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("generation", "backup"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	backupDB := readAll(t, dbPath)

	// Put exactly those bytes back and commit newer records: they land in the
	// WAL, the main file stays identical to the backup.
	if err := os.WriteFile(dbPath, backupDB, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("generation", "later"); err != nil {
		t.Fatal(err)
	}
	walBytes, err := os.ReadFile(dbPath + "-wal")
	if err != nil || len(walBytes) == 0 {
		t.Fatalf("the newer records must live in the WAL: err=%v len=%d", err, len(walBytes))
	}
	mainBytes := readAll(t, dbPath)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mainBytes, backupDB) {
		t.Fatalf("precondition: main file must equal the backup while the WAL holds newer records (main %d bytes, backup %d bytes)", len(mainBytes), len(backupDB))
	}
	// Rebuild the crash image: main + WAL as they were, no SHM (the process
	// died before SQLite could checkpoint).
	if err := os.WriteFile(dbPath, mainBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", walBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(dbPath + "-shm")

	composeFile := filepath.Join(t.TempDir(), "docker-compose.yml")
	archived := []byte("services:\n  web:\n    image: nginx:alpine\n")
	if err := os.WriteFile(composeFile, archived, 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.zip")
	writeBackupArchive(t, backup, backupDB, composeFile, archived)
	resolved, err := filepath.EvalSymlinks(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	writeRestoreRequest(t, dataDir, backup, []string{resolved})

	if err := ApplyPendingRestore(dataDir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if raw := readAll(t, dbPath); !bytes.Equal(raw, backupDB) {
		t.Fatal("the backup database was not installed")
	}
	// The newer records must be moved into the snapshot, not left to be
	// replayed by the next SQLite connection.
	mustNotExist(t, dbPath+"-wal")
	origDirs, _ := filepath.Glob(filepath.Join(dataDir, "restore-orig-*"))
	if len(origDirs) != 1 {
		t.Fatalf("expected one snapshot directory, got %v", origDirs)
	}
	if kept := readAll(t, filepath.Join(origDirs[0], "state.sqlite-wal")); !bytes.Equal(kept, walBytes) {
		t.Fatal("the WAL that held the newer records was lost")
	}

	// Prove the rollback-relevant outcome with a real connection: the restored
	// database must show the backup generation, not the later one.
	st, err = state.Open(dbPath)
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	defer st.Close()
	if got, ok := st.GetMeta("generation"); !ok || got != "backup" {
		t.Fatalf("restored database generation = %q (ok=%v), want backup", got, ok)
	}
	if v, ok := st.GetMeta("generation"); ok && v == "later" {
		t.Fatal("the pre-restore WAL was replayed over the restored database")
	}
}

// Review P1-02: a failed quarantine must keep the journal. Without the rename
// the request can be replayed on the next start, so deleting the journal would
// destroy the only description of the half-applied state.
func TestRestoreMarkerQuarantineFailureKeepsJournal(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{phase: journalOriginalsSaved, committed: true})
	saved := quarantineRename
	quarantineRename = func(from, to string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { quarantineRename = saved })

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "回滚不完整") {
		t.Fatalf("an unquarantined request must be reported, got %v", err)
	}
	if !strings.Contains(err.Error(), "隔离恢复请求") {
		t.Fatalf("the error must explain the failed quarantine: %v", err)
	}
	var j restoreJournal
	raw, rerr := os.ReadFile(l.journal)
	if rerr != nil {
		t.Fatalf("journal must survive a failed quarantine: %v", rerr)
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	if j.Phase != journalRollingBack {
		t.Fatalf("journal phase = %q, want %q (idempotent rollback on next start)", j.Phase, journalRollingBack)
	}
	if _, serr := os.Stat(l.marker); serr != nil {
		t.Fatalf("the request must still be in place: %v", serr)
	}

	// The retry after the injected failure succeeds and resolves the whole
	// transaction. It still reports the abandoned restore, but the evidence is
	// cleaned up and nothing half-applied survives.
	quarantineRename = saved
	err = ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("retry must finish the rollback: %v", err)
	}
	mustNotExist(t, l.journal)
	mustNotExist(t, l.marker)
	if n := failedMarkerCount(t, dir); n != 1 {
		t.Fatalf("request must finally be quarantined, got %d markers", n)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatal("compose was not restored to its original content")
	}
}

// Review P0-02: a journal that startup could not resolve freezes business
// writes while keeping reads (health) available.
func TestEngineFreezesWritesWhenJournalStaysUnresolved(t *testing.T) {
	dir := t.TempDir()
	buildCrashRestore(t, dir, crashRestore{phase: journalFilesCommitted, committed: true, evidenceGone: true})
	// Resolve nothing: put the journal back in a phase whose rollback cannot
	// complete, so it survives startup.
	var j restoreJournal
	raw, err := os.ReadFile(filepath.Join(dir, "restore-journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	j.Phase = journalRollbackFailed
	if err := writeRestoreJournal(filepath.Join(dir, "restore-journal.json"), &j); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "restore-request.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	e, err := New("0.2.9-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	frozen, reason := e.writesFrozen()
	if !frozen {
		t.Fatal("an unresolved restore journal must freeze business writes")
	}
	if !strings.Contains(reason, "未完成的备份恢复事务") {
		t.Fatalf("freeze reason = %q", reason)
	}
	health := e.Health()
	if !health.WriteFrozen || health.FreezeReason == "" {
		t.Fatalf("health must expose the freeze: %+v", health)
	}
	if err := e.ensureNoUnresolvedWork(); err == nil {
		t.Fatal("business writes must be refused")
	}
}

// Review P1-03: services attached by older versions have no registry entry. The
// startup migration must backfill them so a later detach cannot drop the only
// reference to their Compose file.
func TestStartupMigratesComposePathRegistryForExistingServices(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	svc := model.Service{
		ID: "svc-legacy", ContainerName: "legacy-web", ComposeFile: composeFile,
		ComposeService: "web", Status: "managed_running",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	// A registry written by an older version does not contain this path.
	if err := st.SetMeta("knownComposePaths", "[]"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	e, err := New("0.2.9-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	known, rerr := readKnownComposePaths(e.store)
	if rerr != nil {
		t.Fatalf("registry unreadable: %v", rerr)
	}
	resolved, _ := filepath.EvalSymlinks(composeFile)
	found := false
	for _, path := range known {
		if path == resolved {
			found = true
		}
	}
	if !found {
		t.Fatalf("startup migration did not register %s: %v", resolved, known)
	}
	if e.registryErr != nil {
		t.Fatalf("migration reported an error: %v", e.registryErr)
	}
	// The record can now be deleted without losing the path.
	if err := deleteServiceRecord(e, svc.ID); err != nil {
		t.Fatalf("detach after migration: %v", err)
	}
	if _, ok, gerr := e.store.GetService(svc.ID); gerr != nil || ok {
		t.Fatalf("service record should be gone: ok=%v err=%v", ok, gerr)
	}
}

// A corrupt registry must refuse deletions instead of silently overwriting the
// table with a fresh entry.
func TestDeleteServiceRefusesCorruptRegistry(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertService(model.Service{
		ID: "svc-1", ContainerName: "web-1", ComposeFile: composeFile,
		ComposeService: "web", Status: "managed_running",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("knownComposePaths", "{not json"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	e, err := New("0.2.9-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.registryErr == nil {
		t.Fatal("a corrupt registry must be reported at startup")
	}
	if err := deleteServiceRecord(e, "svc-1"); err == nil {
		t.Fatal("deletion must be refused while the registry is unusable")
	}
	if _, ok, gerr := e.store.GetService("svc-1"); gerr != nil || !ok {
		t.Fatalf("service record must survive the refusal: ok=%v err=%v", ok, gerr)
	}
}

func rawFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// writeBackupArchive replaces path with a valid backup containing db, the
// compose manifest and its content.
func writeBackupArchive(t *testing.T, path string, db []byte, composeFile string, compose []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(db); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"files": []map[string]any{{"path": composeFile, "entry": "compose/0000", "kind": "compose", "mode": 420}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(manifest); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("compose/0000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(compose); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
