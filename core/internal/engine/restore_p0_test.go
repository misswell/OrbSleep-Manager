package engine

// v0.2.8 fault injection for the journalled restore transaction. Every test
// drives ApplyPendingRestore — the function the daemon runs before it opens
// SQLite — from an on-disk state that matches a crash in ONE specific window,
// which is the only way to prove what the next start decides.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/state"
)

// crashRestore is the on-disk state of an interrupted restore. Its defaults
// match what the production phases leave behind; the flags inject the losses a
// crash can cause on top of them.
type crashRestore struct {
	phase      string // "" keeps the request fresh (journal never written)
	committed  bool   // the file commit flag inside the journal
	swapped    bool   // the database rename landed before the crash
	noStaged   bool   // .restore-state.tmp was lost
	backupGone bool   // the archive disappeared after the request was registered

	backupDB          []byte // archive database bytes ("" digest inputs need this)
	digest            string // journal databaseSha256 ("" = pre-0.2.8 journal)
	hadDB             *bool
	legacyJournal     bool   // write no hadOriginalDb field at all (pre-0.2.8 journal)
	snapshotDB        []byte // nil: default to a copy of the pre-restore database
	snapshotDBMissing bool   // the snapshot lost the database it recorded
	snapshotWAL       []byte
	liveWAL           []byte
	evidenceGone      bool // the snapshot lost the compose file it recorded
}

type crashLayout struct {
	backup      string
	composeFile string
	archived    []byte // compose content inside the archive
	drifted     []byte // live compose content before the restore
	backupDB    []byte
	liveDB      []byte
	origDir     string
	journal     string
	marker      string
	staged      string
	db          string
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// distinctDatabase creates a real SQLite database whose bytes differ per tag, so
// a digest comparison can tell two databases apart.
func distinctDatabase(t *testing.T, tag string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), tag+".sqlite")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("probe", tag); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// buildCrashRestore lays out dataDir exactly as a crash would have left it.
func buildCrashRestore(t *testing.T, dataDir string, c crashRestore) crashLayout {
	t.Helper()
	archived := []byte("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:18080:80\"\n")
	drifted := []byte("services:\n  web:\n    image: nginx:alpine\n    # drifted by the user\n")
	// The file phase writes the archived content, so any phase past it — and a
	// crash with the per-file commit flag set — already left it on disk.
	live := drifted
	switch c.phase {
	case journalFilesCommitted, journalDatabaseCommitting, journalDatabaseCommitted:
		live = archived
	case journalOriginalsSaved:
		if c.committed {
			live = archived
		}
	}

	backupDB := c.backupDB
	if backupDB == nil {
		backupDB = distinctDatabase(t, "backup-db")
	}
	liveDB := distinctDatabase(t, "live-db")
	if bytes.Equal(liveDB, backupDB) {
		t.Fatal("the two databases must differ for a digest comparison to mean anything")
	}
	if c.swapped {
		liveDB = backupDB
	}

	composeFile := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(composeFile, live, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "state.sqlite")
	if err := os.WriteFile(dbPath, liveDB, 0o600); err != nil {
		t.Fatal(err)
	}
	if c.liveWAL != nil {
		if err := os.WriteFile(dbPath+"-wal", c.liveWAL, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	backup := filepath.Join(t.TempDir(), "backup.zip")
	f, err := os.Create(backup)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(backupDB); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"files": []map[string]any{{"path": composeFile, "entry": "compose/0000", "kind": "compose", "mode": 420}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _ = zw.Create("manifest.json")
	w.Write(manifest)
	w, _ = zw.Create("compose/0000")
	w.Write(archived)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	resolved, err := filepath.EvalSymlinks(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dataDir, "restore-request.json")
	payload, err := json.Marshal(map[string]any{
		"backupPath":  backup,
		"requestedAt": time.Now().UTC().Format(time.RFC3339),
		"allow":       map[string]any{"files": []string{resolved}, "dirs": []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	l := crashLayout{backup: backup, composeFile: composeFile, archived: archived, drifted: drifted,
		backupDB: backupDB, liveDB: liveDB, marker: marker, db: dbPath,
		journal: filepath.Join(dataDir, "restore-journal.json"),
		staged:  filepath.Join(dataDir, ".restore-state.tmp")}
	if c.phase == "" {
		if c.backupGone {
			os.Remove(backup)
		}
		return l
	}

	l.origDir = filepath.Join(dataDir, "restore-orig-20260101-000000")
	if err := os.MkdirAll(l.origDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotDB := c.snapshotDB
	if snapshotDB == nil {
		snapshotDB = liveDB
	}
	if !c.snapshotDBMissing {
		if err := os.WriteFile(filepath.Join(l.origDir, "state.sqlite"), snapshotDB, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.snapshotWAL != nil {
		if err := os.WriteFile(filepath.Join(l.origDir, "state.sqlite-wal"), c.snapshotWAL, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	saved := filepath.Join(l.origDir, "0000-docker-compose.yml")
	if !c.evidenceGone {
		if err := os.WriteFile(saved, drifted, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hadDB := c.hadDB
	if hadDB == nil && !c.legacyJournal {
		yes := true
		hadDB = &yes
	}
	j := restoreJournal{Phase: c.phase, BackupPath: backup, Stamp: "20260101-000000", OrigDir: l.origDir,
		DatabaseSHA256: c.digest, HadOriginalDB: hadDB,
		Files: []journalFile{{Target: composeFile, Entry: "compose/0000", Saved: saved,
			OriginalMode: uint32(0o644), RestoreMode: uint32(0o644), Committed: c.committed}}}
	if err := writeRestoreJournal(l.journal, &j); err != nil {
		t.Fatal(err)
	}
	if !c.noStaged {
		if err := os.WriteFile(l.staged, backupDB, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.backupGone {
		os.Remove(backup)
	}
	return l
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s must not exist (err %v)", path, err)
	}
}

func failedMarkerCount(t *testing.T, dataDir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dataDir, "restore-failed-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// Review item 1: crash after the journal reached ORIGINALS_SAVED but before the
// compose write landed. Nothing was modified, so the rollback must be a no-op
// that keeps the drifted file and the live database byte-for-byte intact.
func TestRestoreCrashBeforeFileCommitTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{
		phase:   journalOriginalsSaved,
		liveWAL: []byte("committed-wal"), snapshotWAL: []byte("committed-wal"),
	})

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("interrupted file phase must roll back cleanly, got %v", err)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatalf("untouched compose was modified:\n%s", raw)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("live database was replaced")
	}
	if got := readAll(t, l.db+"-wal"); string(got) != "committed-wal" {
		t.Fatalf("live WAL lost its committed records: %q", got)
	}
	mustNotExist(t, l.journal)
	if n := failedMarkerCount(t, dir); n != 1 {
		t.Fatalf("request must be quarantined as evidence, got %d markers", n)
	}
}

// Review item 3: the rename landed and the process died before the journal
// could record DATABASE_COMMITTED. The staged database's digest is the proof,
// so the next start must complete the restore instead of rolling a good swap
// back over.
func TestRestoreCompletesWhenRenameLandedBeforeJournalWrite(t *testing.T) {
	dir := t.TempDir()
	backupDB := distinctDatabase(t, "swapped-backup-db")
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalDatabaseCommitting, committed: true, swapped: true,
		backupDB: backupDB, digest: sha256Hex(backupDB),
	})

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("a completed swap must finish the restore, got %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, backupDB) {
		t.Fatal("restored database was replaced again")
	}
	mustNotExist(t, l.marker)
	mustNotExist(t, l.journal)
	mustNotExist(t, l.staged)
	if n := failedMarkerCount(t, dir); n != 0 {
		t.Fatalf("a completed restore leaves no failed marker, got %d", n)
	}
}

// Review item 3, the other direction: the snapshot carries a WAL copy whether or
// not the swap ran, so sidecar presence (and a stale journalled digest) must not
// be read as completion. The live database is still the original here, so the
// resume has to perform the swap.
func TestRestoreDoesNotTrustSnapshotSidecarsAsCompletionProof(t *testing.T) {
	dir := t.TempDir()
	backupDB := distinctDatabase(t, "resume-backup-db")
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalDatabaseCommitting, committed: true,
		backupDB:    backupDB,
		digest:      strings.Repeat("0", 64), // stale value from an earlier attempt
		snapshotWAL: []byte("original-wal-records"),
	})

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, backupDB) {
		t.Fatal("the database swap never ran: completion was decided from leftover sidecars")
	}
	mustNotExist(t, l.db+"-wal")
	if kept := readAll(t, filepath.Join(l.origDir, "state.sqlite-wal")); string(kept) != "original-wal-records" {
		t.Fatalf("original WAL records lost: %q", kept)
	}
	mustNotExist(t, l.journal)
}

// Review item 4: the staged database was lost while the live WAL holds
// committed records. The resume rebuilds the staged file from the archive, and
// the WAL must be preserved rather than deleted with the guess.
func TestRestoreRebuildsLostStagedDatabaseFromBackup(t *testing.T) {
	dir := t.TempDir()
	backupDB := distinctDatabase(t, "rebuild-backup-db")
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalFilesCommitted, committed: true, noStaged: true,
		backupDB:    backupDB,
		liveWAL:     []byte("committed-wal-records"),
		snapshotWAL: []byte("committed-wal-records"),
	})

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("lost staged file must be rebuildable: %v", err)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, backupDB) {
		t.Fatal("database was not swapped to the backup version")
	}
	if kept := readAll(t, filepath.Join(l.origDir, "state.sqlite-wal")); string(kept) != "committed-wal-records" {
		t.Fatalf("committed WAL records lost: %q", kept)
	}
	mustNotExist(t, l.db+"-wal")
	mustNotExist(t, l.journal)
}

// Review items 4 and 6 together: the staged file is gone AND the archive was
// deleted after the request. Nothing can be completed, so the whole transaction
// — files, database and the WAL that belongs to it — must go back.
func TestRestoreRollsBackWhenStagedAndBackupAreBothLost(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalFilesCommitted, committed: true, noStaged: true, backupGone: true,
		liveWAL:     []byte("committed-wal-records"),
		snapshotWAL: []byte("committed-wal-records"),
	})

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "暂存数据库丢失") {
		t.Fatalf("must report the lost staged database, got %v", err)
	}
	if !strings.Contains(err.Error(), "已回滚到原始状态") {
		t.Fatalf("a complete rollback must be claimed only when it is: %v", err)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatalf("committed file was not rolled back:\n%s", raw)
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("database was left at the backup version")
	}
	if got := readAll(t, l.db+"-wal"); string(got) != "committed-wal-records" {
		t.Fatalf("original WAL was not put back next to the original database: %q", got)
	}
	mustNotExist(t, l.journal)
	if n := failedMarkerCount(t, dir); n != 1 {
		t.Fatalf("request must be quarantined, got %d markers", n)
	}
}

// Review item 5: the rollback evidence itself is gone. A restore that cannot
// verify its rollback keeps the journal and the snapshot, and the next start
// neither retries nor discards either.
func TestRestoreKeepsEvidenceWhenRollbackCannotComplete(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{
		phase: journalOriginalsSaved, committed: true, evidenceGone: true,
	})

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "回滚不完整") {
		t.Fatalf("a blocked rollback must be reported as incomplete, got %v", err)
	}
	if !strings.Contains(err.Error(), l.origDir) {
		t.Fatalf("the error must name the snapshot holding the recovery material: %v", err)
	}
	var kept restoreJournal
	raw, jerr := os.ReadFile(l.journal)
	if jerr != nil {
		t.Fatalf("journal must survive an unverified rollback: %v", jerr)
	}
	if err := json.Unmarshal(raw, &kept); err != nil || kept.Phase != journalOriginalsSaved {
		t.Fatalf("journal = %+v err %v", kept, err)
	}
	if failedMarkerCount(t, dir) != 1 {
		t.Fatal("request must be quarantined")
	}
	halfApplied := readAll(t, l.composeFile)

	// Restart with the request already quarantined: nothing is touched, no retry
	// loop, and the evidence is still there for the operator.
	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("second start must leave the unresolved state alone: %v", err)
	}
	if got := readAll(t, l.composeFile); !bytes.Equal(got, halfApplied) {
		t.Fatalf("half-applied compose changed between restarts:\n%s", got)
	}
	if _, err := os.Stat(l.journal); err != nil {
		t.Fatalf("journal evidence lost: %v", err)
	}
	if _, err := os.Stat(l.origDir); err != nil {
		t.Fatalf("snapshot evidence lost: %v", err)
	}
}

// Review item 6: a fresh request whose archive disappeared. Fail closed before
// journalling anything, so no half-described transaction is left behind.
func TestRestoreRefusesDeletedBackupBeforeJournalling(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{backupGone: true})

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "打开备份失败") {
		t.Fatalf("missing archive must be reported, got %v", err)
	}
	mustNotExist(t, l.journal)
	if stages, _ := filepath.Glob(filepath.Join(dir, "restore-orig-*")); len(stages) != 0 {
		t.Fatalf("no snapshot may exist before the archive is readable: %v", stages)
	}
	if raw := readAll(t, l.composeFile); !bytes.Equal(raw, l.drifted) {
		t.Fatal("compose modified")
	}
	if raw := readAll(t, l.db); !bytes.Equal(raw, l.liveDB) {
		t.Fatal("database modified")
	}
	if failedMarkerCount(t, dir) != 1 {
		t.Fatal("request must be quarantined")
	}
}

// The unknown-original-database tri-state: a journal that cannot say whether the
// restore created state.sqlite must never delete the live database, while an
// explicit answer is acted on.
func TestRestoreRollbackDatabaseTriState(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name        string
		hadDB       *bool
		legacy      bool
		wantErr     string
		wantDBGone  bool
		wantJournal bool
	}{
		{"legacy journal keeps the live database", nil, true, "已回滚到原始状态", false, false},
		{"recorded original demands the snapshot", &yes, false, "原始数据库快照缺失", false, true},
		{"restore-created database is removed", &no, false, "已回滚到原始状态", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := buildCrashRestore(t, dir, crashRestore{
				phase: journalOriginalsSaved, hadDB: tc.hadDB, legacyJournal: tc.legacy,
				snapshotDBMissing: true,
			})
			err := ApplyPendingRestore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			_, statErr := os.Stat(l.db)
			if tc.wantDBGone {
				if !os.IsNotExist(statErr) {
					t.Fatalf("database written by the restore must be removed, stat err %v", statErr)
				}
			} else if statErr != nil {
				t.Fatalf("live database must survive: %v", statErr)
			}
			// Only a verified rollback may drop the journal.
			_, jErr := os.Stat(l.journal)
			if tc.wantJournal != (jErr == nil) {
				t.Fatalf("journal presence wrong for %q: stat err %v", tc.wantErr, jErr)
			}
		})
	}
}

// Cleanup ordering: a committed restore whose marker cannot be removed must
// keep the journal. A marker without its journal is read as a brand-new request
// and would replay the restore over anything edited since.
func TestFinishRestoreCleanupKeepsJournalWhileMarkerIsStuck(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "restore-journal.json")
	j := restoreJournal{Phase: journalDatabaseCommitted, Stamp: "s", BackupPath: "gone.zip",
		OrigDir: filepath.Join(dir, "restore-orig-s")}
	if err := writeRestoreJournal(journal, &j); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "restore-request.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read-only data directory: unlinking the marker is denied.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("a committed restore must not fail because cleanup is blocked: %v", err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("journal must be kept while the marker cannot be removed: %v", err)
	}

	// The next start finishes what the blocked cleanup could not.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, journal)
	mustNotExist(t, marker)
}

// The same ordering seen from a successful restore: DATABASE_COMMITTED only
// completes the cleanup. The archive is never re-applied over edits made after
// the restore succeeded.
func TestRestoreCommittedPhaseNeverReplaysOverLaterEdits(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{phase: journalDatabaseCommitted, committed: true})
	edited := append(l.archived, []byte("# edited after the restore succeeded\n")...)
	if err := os.WriteFile(l.composeFile, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	laterDB := distinctDatabase(t, "written-after-restore")
	if err := os.WriteFile(l.db, laterDB, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("committed phase must only clean up: %v", err)
	}
	if got := readAll(t, l.composeFile); !bytes.Equal(got, edited) {
		t.Fatalf("the restore was replayed over later edits:\n%s", got)
	}
	if got := readAll(t, l.db); !bytes.Equal(got, laterDB) {
		t.Fatal("the database was overwritten by a replayed restore")
	}
	mustNotExist(t, l.marker)
	mustNotExist(t, l.journal)
}

// A journal whose phase maps onto no on-disk state must not be guessed at.
func TestRestoreUnknownPhaseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{phase: journalOriginalsSaved})
	hadDB := true
	j := restoreJournal{Phase: "HALF_DONE", Stamp: "s", BackupPath: l.backup,
		OrigDir: l.origDir, HadOriginalDB: &hadDB}
	if err := writeRestoreJournal(l.journal, &j); err != nil {
		t.Fatal(err)
	}
	before := readAll(t, l.composeFile)

	err := ApplyPendingRestore(dir)
	if err == nil || !strings.Contains(err.Error(), "无法识别的恢复阶段") {
		t.Fatalf("unknown phase must fail closed, got %v", err)
	}
	var restored restoreJournal
	raw, err := os.ReadFile(l.journal)
	if err != nil {
		t.Fatalf("the journal is the only description of the state: %v", err)
	}
	if json.Unmarshal(raw, &restored) != nil || restored.Phase != "HALF_DONE" {
		t.Fatalf("journal lost or rewritten: %s", raw)
	}
	if got := readAll(t, l.composeFile); !bytes.Equal(got, before) {
		t.Fatal("a refused restore still modified the compose file")
	}
}

// Review item 7: a pending restore request freezes business writes, and the
// freeze holds under concurrent API calls instead of letting one of them slip a
// modification past the gate.
func TestMaintenanceGateFreezesConcurrentBusinessWrites(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc := model.Service{ID: "svc-frozen", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web",
		ComposeFile: filepath.Join(t.TempDir(), "docker-compose.yml"),
		Status:      "managed_running", StartTimeoutSecs: 1,
		Bindings: []model.Binding{{HostIP: "127.0.0.1", HostPort: freePort(t), ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.ComposeFile,
		[]byte("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:8080:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RestoreBackup(entry.ID); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if !e.maintenance.Load() {
		t.Fatal("registering a restore request must freeze business writes")
	}

	before := e.Settings().IdleTimeoutSecs
	// Sequentially, so the message proves it was the maintenance gate and not
	// lock contention: every business write entry refuses.
	if _, err := e.UpdateSettings(map[string]any{"idleTimeoutSecs": 1234}); err == nil || !strings.Contains(err.Error(), "冻结") {
		t.Fatalf("UpdateSettings must be frozen by the pending request: %v", err)
	}
	if _, err := e.ServiceAction(context.Background(), svc.ID, "start", nil); err == nil || !strings.Contains(err.Error(), "冻结") {
		t.Fatalf("ServiceAction must be frozen by the pending request: %v", err)
	}
	if _, err := e.ServiceAction(context.Background(), svc.ID, "updatePolicy", map[string]any{"idleTimeoutSecs": 600}); err == nil || !strings.Contains(err.Error(), "冻结") {
		t.Fatalf("updatePolicy must be frozen by the pending request: %v", err)
	}
	if _, err := e.ProxySetup(); err == nil || !strings.Contains(err.Error(), "冻结") {
		t.Fatalf("ProxySetup must be frozen by the pending request: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var errs []error
	var mu sync.Mutex
	note := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for i := 0; i < 8; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.UpdateSettings(map[string]any{"idleTimeoutSecs": 1234})
			note(err)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := e.ServiceAction(context.Background(), svc.ID, "start", nil)
			note(err)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := e.ServiceAction(context.Background(), svc.ID, "updatePolicy", map[string]any{"idleTimeoutSecs": 600})
			note(err)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := e.ProxySetup()
			note(err)
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) != 32 {
		t.Fatalf("expected every concurrent call to return, got %d", len(errs))
	}
	for i := 0; i < len(errs); i++ {
		// Either refusal is acceptable here: the gate itself or the write lock
		// held by a sibling that the gate then refused. What must never happen
		// is a concurrent call that got through.
		if errs[i] == nil {
			t.Fatalf("call %d modified state during maintenance", i)
		}
	}
	if e.Settings().IdleTimeoutSecs != before {
		t.Fatal("a frozen write still changed the settings")
	}
	// Another restore request must not stack on the pending one.
	if _, err := e.RestoreBackup(entry.ID); err == nil {
		t.Fatal("a second restore request must be refused while one is pending")
	}
	// Reads stay available: the GUI has to show the pending request.
	if h := e.Health(); h.DaemonVersion == "" {
		t.Fatal("health must stay readable during maintenance")
	}
	if _, err := e.RecoveryList(); err != nil {
		t.Fatalf("recovery list must stay readable: %v", err)
	}

	// The daemon applies the request on its next start; in-process that is
	// exactly clearing the flag. Whatever else the settings patch is judged on,
	// the maintenance refusal must be gone.
	e.maintenance.Store(false)
	if _, err := e.UpdateSettings(map[string]any{"idleTimeoutSecs": 1234}); err != nil && strings.Contains(err.Error(), "冻结") {
		t.Fatalf("writes must resume once the request is applied: %v", err)
	}
}

// Review item 9: a service deleted after the backup takes its database record
// with it, so the persistent Compose registry written by the real attach path is
// the only thing that can still name its file.
func TestDeletedServiceComposeFileStaysRestorable(t *testing.T) {
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	docker.setPorts(snap.removals, true)
	e.cli.(*fakeCLI).configFn = composeConfigFor(snap.removals)
	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")
	txnID, err := e.startTxn("op-registry", "attach", txnFromPlan("op-registry", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, txnID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := e.store.FinishTransaction(txnID, "committed", "", "committed"); err != nil {
		t.Fatal(err)
	}
	services, err := e.store.ListServices()
	if err != nil || len(services) != 1 {
		t.Fatalf("managed services = %+v err %v", services, err)
	}

	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	archivedCompose := zipEntryBytes(t, entry.Path, "compose/")
	if len(archivedCompose) == 0 {
		t.Fatal("the backup must carry the managed compose file")
	}

	if err := e.store.DeleteService(services[0].ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !e.liveRestoreAllowlist().allows(resolved) {
		t.Fatal("a deleted service's compose file must stay restorable")
	}

	// Control: with the registry emptied the same archive is refused, which
	// proves the registry — not the deleted record — is what authorizes the path.
	raw, ok := e.store.GetMeta("knownComposePaths")
	if !ok || !strings.Contains(raw, resolved) {
		t.Fatalf("the production attach path must register %q (meta %q ok %v)", resolved, raw, ok)
	}
	if err := e.store.SetMeta("knownComposePaths", "[]"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RestoreBackup(entry.ID); err == nil || !strings.Contains(err.Error(), "拒绝恢复") {
		t.Fatalf("without the registry the restore must be refused, got %v", err)
	}
	if err := e.store.SetMeta("knownComposePaths", raw); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(snap.composeFile, []byte("user rewrite after the detach\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RestoreBackup(entry.ID); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if err := ApplyPendingRestore(e.dataDir); err != nil {
		t.Fatalf("ApplyPendingRestore: %v", err)
	}
	if got := readAll(t, snap.composeFile); !bytes.Equal(got, archivedCompose) {
		t.Fatalf("deleted service's compose file was not restored:\n%s", got)
	}
}

func zipEntryBytes(t *testing.T, archive, prefix string) []byte {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, prefix) {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
	return nil
}

// The database snapshot is allowed to be larger than a per-file entry: a daemon
// state file over the 64 MiB file cap still has to restore, while the generic
// file reader keeps its own limit.
func TestStagedDatabaseExceedsPerFileCap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a database snapshot just over the 64 MiB per-file cap")
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "big.zip")
	f, err := os.Create(backup)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	// A real SQLite header padded past the per-file cap: the header is what
	// validateBackupZip checks, the size is what this test is about.
	block := make([]byte, 1<<20)
	copy(block, distinctDatabase(t, "big-db-header"))
	for i := 0; i < maxZipEntryBytes/(1<<20)+1; i++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var src *zip.File
	for _, zf := range r.File {
		if zf.Name == "state.sqlite" {
			src = zf
		}
	}
	if src == nil {
		t.Fatal("missing state.sqlite entry")
	}

	if err := validateBackupZip(backup); err != nil {
		t.Fatalf("a database snapshot over the per-file cap must be accepted: %v", err)
	}
	staged := filepath.Join(dir, ".restore-state.tmp")
	if err := extractZipEntry(src, staged, maxZipStateBytes); err != nil {
		t.Fatalf("extracting the staged database: %v", err)
	}
	if info, err := os.Stat(staged); err != nil || info.Size() <= maxZipEntryBytes {
		t.Fatalf("staged database incomplete: %+v %v", info, err)
	}
	err = extractZipEntry(src, filepath.Join(dir, "over-cap.tmp"), maxZipEntryBytes)
	if err == nil || (!strings.Contains(err.Error(), "过大") && !strings.Contains(err.Error(), "超过")) {
		t.Fatalf("the per-file cap must still reject it, got %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "over-cap.tmp"))
}

// A backup must not be able to hand OrbSleep's own files privileged modes.
func TestSanitizeRestoreModeStripsPrivilegedBits(t *testing.T) {
	cases := []struct {
		name string
		raw  uint32
		want os.FileMode
	}{
		{"plain file", uint32(0o644), 0o644},
		{"world writable removed", uint32(0o666), 0o664},
		{"setuid and setgid dropped", uint32(os.ModeSetuid | os.ModeSetgid | 0o755), 0o755},
		{"directory type bits dropped", uint32(os.ModeDir | 0o755), 0o755},
		{"all bits set", uint32(0o7777), 0o775},
		{"nothing readable becomes usable", uint32(os.ModeSetuid | 0o002), 0o644},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRestoreMode(tc.raw); got != tc.want {
				t.Fatalf("sanitizeRestoreMode(%o) = %o, want %o", tc.raw, got, tc.want)
			}
			if got := sanitizeRestoreMode(tc.raw); got&0o7000 != 0 || got&0o002 != 0 {
				t.Fatalf("privileged or world-writable bits survived: %o", got)
			}
		})
	}
}

// The corrupt journal is the fail-closed case that must not touch anything.
func TestRestoreCorruptJournalQuarantinesWithoutTouching(t *testing.T) {
	dir := t.TempDir()
	l := buildCrashRestore(t, dir, crashRestore{phase: journalFilesCommitted, committed: true})
	if err := os.WriteFile(l.journal, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var target *RestoreError
	err := ApplyPendingRestore(dir)
	if !errors.As(err, &target) {
		t.Fatalf("expected *RestoreError, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "恢复日志损坏") {
		t.Fatalf("unexpected cause: %v", err)
	}
	mustNotExist(t, l.marker)
	if _, err := os.Stat(l.journal); err != nil {
		t.Fatalf("the corrupt journal is the operator's evidence: %v", err)
	}
	if failedMarkerCount(t, dir) != 1 {
		t.Fatal("request must be quarantined")
	}
}
