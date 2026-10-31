package engine

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbsleep/internal/gateway"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

// crashAttachTxnFixture builds an engine plus one attach transaction frozen in
// the compose_updated phase: the file on disk is managed, the backup carries
// the original. It is the standing "daemon crashed mid-attach" state.
func crashAttachTxnFixture(t *testing.T) (*Engine, *planSnapshot, string, []byte, []byte, string) {
	t.Helper()
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)

	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	original := append([]byte(nil), snap.content...)
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	if err := os.WriteFile(snap.composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: snap.containerName, Ports: []string{"20010:80"}}}}
	admin.mu.Unlock()
	// The fake container re-publishes ports it finds declared in the restored
	// file; it must know the port exists for recoverVerifyOriginal to pass.
	docker.setPorts(snap.removals, false)

	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "compose_updated", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].ManagedContent = managed
		p.Items[0].Phase = "compose_updated"
	}); err != nil {
		t.Fatal(err)
	}
	return e, snap, backupPath, managed, original, txnID
}

// P1-01: reconcile used to call RecoverTransactions while still holding
// writeMu, and recoverAttachTxn re-acquired writeMu — a self-deadlock that
// froze the whole reconcile loop whenever a crashed transaction existed.
// reconcile must complete and recover the transaction instead.
func TestReconcileRecoversCrashedTransactionWithoutDeadlock(t *testing.T) {
	e, snap, _, _, original, txnID := crashAttachTxnFixture(t)

	done := make(chan struct{})
	go func() {
		e.reconcile()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reconcile deadlocked on recovery (writeMu held across RecoverTransactions)")
	}

	txn, _, err := e.store.GetTransaction(txnID)
	if err != nil {
		t.Fatal(err)
	}
	if txn.Status != "rolled_back" {
		t.Fatalf("txn status = %s error=%s", txn.Status, txn.Error)
	}
	raw, err := os.ReadFile(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("compose was not restored:\n%s", raw)
	}
}

// P1-01: batch recovery must skip (not deadlock) while a write operation is in
// flight, and recover afterwards.
func TestRecoverTransactionsSkipsWhileWriteLocked(t *testing.T) {
	e, snap, _, _, original, txnID := crashAttachTxnFixture(t)

	e.writeMu.Lock()
	done := make(chan struct{})
	go func() {
		e.RecoverTransactions()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RecoverTransactions blocked on writeMu instead of skipping")
	}
	e.writeMu.Unlock()

	txn, _, _ := e.store.GetTransaction(txnID)
	if txn.Status != "running" {
		t.Fatalf("busy recovery must leave the transaction untouched, got %s", txn.Status)
	}
	e.RecoverTransactions()
	txn, _, _ = e.store.GetTransaction(txnID)
	if txn.Status != "rolled_back" {
		t.Fatalf("status = %s error=%s", txn.Status, txn.Error)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(original) {
		t.Fatalf("compose not restored after recovery:\n%s", raw)
	}
}

// P1-01: a transaction that committed while recovery was waiting for the
// locks must not be rolled back from its stale snapshot.
func TestRecoverySkipsTransactionThatCompletedUnderLock(t *testing.T) {
	e, snap, _, managed, _, txnID := crashAttachTxnFixture(t)

	e.writeMu.Lock()
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		// Runs while writeMu is held: the batch recovery inside must skip.
		e.RecoverTransactions()
	}()
	time.Sleep(100 * time.Millisecond)
	// The "attach" finishes and marks the transaction committed.
	txn, _, _ := e.store.GetTransaction(txnID)
	txn.Status = "committed"
	txn.Phase = "committed"
	if err := e.store.SaveTransaction(txn); err != nil {
		t.Fatal(err)
	}
	e.writeMu.Unlock()
	<-completed

	raw, err := os.ReadFile(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(managed) {
		t.Fatalf("a committed transaction was rolled back from a stale snapshot:\n%s", raw)
	}
	if _, ok, _ := e.store.GetTransaction(txnID); !ok {
		t.Fatal("transaction record must stay")
	}
}

// P1-03: a needs-repair transaction blocks the context switch too — the old
// code checked the wrong return value of CountUnresolvedTransactions and only
// refused when needsRepair > 0, silently allowing switches with running
// transactions. The guard also fails closed on database errors.
func TestContextSwitchGuardBlocksUnresolvedTransactions(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	// A running transaction (no service record, no managed service) must block.
	txnID, err := e.startTxn("op-running", "attach", txnPayload{OperationID: "op-running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ensureContextSwitchSafe(); err == nil {
		t.Fatal("running transaction must block a Docker context switch")
	}

	// needs_repair must block as well (this is the case the old bug let through).
	txn, _, _ := e.store.GetTransaction(txnID)
	txn.Status = "rollback_failed"
	if err := e.store.SaveTransaction(txn); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureContextSwitchSafe(); err == nil {
		t.Fatal("rollback_failed transaction must block a Docker context switch")
	}

	// A clean board allows the switch.
	if err := e.store.FinishTransaction(txn.ID, "rolled_back", "", "rolled_back"); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureContextSwitchSafe(); err != nil {
		t.Fatalf("clean board must allow the switch: %v", err)
	}
}

// P1-03: settings.update must not be a side door around docker.context.select.
func TestUpdateSettingsRejectsDockerSocket(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	if _, err := e.UpdateSettings(map[string]any{"dockerSocket": "/tmp/other.sock"}); err == nil {
		t.Fatal("settings.update must refuse to change the Docker socket")
	}
	if s := e.Settings(); s.DockerSocket != "" {
		t.Fatalf("socket was changed via settings.update: %q", s.DockerSocket)
	}
}

// P1-04: a plain stop must be refused while managed services or unresolved
// transactions exist; restart/upgrade flows with force are allowed.
func TestCheckStopSafety(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	txnID, err := e.startTxn("op-txn", "attach", txnPayload{OperationID: "op-txn"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CheckStopSafety(false); err == nil {
		t.Fatal("unresolved transaction must block a plain stop")
	}
	if err := e.CheckStopSafety(true); err != nil {
		t.Fatalf("forced stop must be allowed: %v", err)
	}
	if err := e.store.FinishTransaction(txnID, "rolled_back", "", "rolled_back"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckStopSafety(false); err != nil {
		t.Fatalf("clean board must allow a plain stop: %v", err)
	}

	// A managed service brings the gateway listeners with the daemon: stop
	// would leave the original ports dead.
	svc := model.Service{ID: "svc-stop", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web",
		ComposeFile: filepath.Join(t.TempDir(), "docker-compose.yml"),
		Status:      "managed_running", StartTimeoutSecs: 1,
		Bindings: []model.Binding{{HostIP: "127.0.0.1", HostPort: freePort(t), ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckStopSafety(false); err == nil {
		t.Fatal("managed services must block a plain stop")
	}
}

// P1-06: PrepareShutdown covers rollback_failed / needs_repair transactions,
// not just running ones, and fails closed when the board cannot be read.
func TestPrepareShutdownIncludesFailedRecovery(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	txnID, err := e.startTxn("op-crash", "attach", txnPayload{OperationID: "op-crash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.FinishTransaction(txnID, "rollback_failed", "恢复不完整", "rollback_failed"); err != nil {
		t.Fatal(err)
	}
	prep := e.PrepareShutdown()
	if prep.Safe {
		t.Fatalf("rollback_failed transaction must block shutdown: %+v", prep)
	}
	if len(prep.PendingTransactions) != 1 {
		t.Fatalf("pending transactions = %+v", prep.PendingTransactions)
	}
	if err := e.store.FinishTransaction(txnID, "rolled_back", "", "rolled_back"); err != nil {
		t.Fatal(err)
	}
	if prep := e.PrepareShutdown(); !prep.Safe {
		t.Fatalf("clean board must be safe: %+v", prep)
	}
}

// P1-05: a backup must carry the managed Compose files and per-service backup
// material, and restoring it must put the files back in sync with the
// restored database — even when the live files drifted or disappeared.
func TestBackupRestoreRestoresComposeFiles(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)

	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	// The archive must contain the managed Compose snapshot and the service's
	// stable backup material.
	r, err := zip.OpenReader(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range r.File {
		names[f.Name] = true
	}
	r.Close()
	if !names["manifest.json"] || !names["state.sqlite"] {
		t.Fatalf("backup archive incomplete: %v", names)
	}
	hasCompose, hasServiceBackup := false, false
	for name := range names {
		if strings.HasPrefix(name, "compose/") {
			hasCompose = true
		}
		if strings.HasPrefix(name, "services/svc-test/") {
			hasServiceBackup = true
		}
	}
	if !hasCompose || !hasServiceBackup {
		t.Fatalf("backup missing compose=%v service-backup=%v", hasCompose, hasServiceBackup)
	}

	// Simulate drift: the managed Compose file is replaced by user edits and
	// the service record disappears (the T1/T2/T3 scenario from the review).
	external := []byte("services:\n  web:\n    image: nginx:alpine\n    # user edit\n")
	if err := os.WriteFile(snap.composeFile, external, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.store.DeleteService(svc.ID); err != nil {
		t.Fatal(err)
	}

	// RestoreBackup must refuse while unresolved work exists (fail-closed),
	// and proceed on a clean board.
	txnID, err := e.startTxn("op-restore-block", "attach", txnPayload{OperationID: "op-restore-block"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RestoreBackup(entry.ID); err == nil {
		t.Fatal("restore must be refused while unresolved transactions exist")
	}
	txn, _, _ := e.store.GetTransaction(txnID)
	txn.Status = "rolled_back"
	if err := e.store.SaveTransaction(txn); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RestoreBackup(entry.ID); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}

	// Apply the marker the way the daemon does on its next start.
	if err := ApplyPendingRestore(e.dataDir); err != nil {
		t.Fatalf("ApplyPendingRestore: %v", err)
	}
	raw, err := os.ReadFile(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(managed) {
		t.Fatalf("compose file was not restored from the backup:\n%s", raw)
	}
	// The drifted file must survive as evidence, not be silently destroyed.
	pre, _ := filepath.Glob(snap.composeFile + ".pre-restore-*")
	if len(pre) != 1 {
		t.Fatalf("expected one pre-restore copy of the drifted file, got %d", len(pre))
	}
	// The service's stable backup material must be back on disk, or the
	// restored database would point at missing recovery files.
	backupDir, ok := e.store.GetMeta("serviceBackup:" + svc.ID)
	if !ok {
		t.Fatal("serviceBackup meta missing after restore")
	}
	if _, err := os.Stat(filepath.Join(backupDir, filepath.Base(snap.composeFile))); err != nil {
		t.Fatalf("service backup file missing after restore: %v", err)
	}
}

// P1-05: a backup made before anything was managed restores a database-only
// state; the file manifest must then be empty and restore must be a no-op for
// files (backward compatible with pre-0.2.2 archives).
func TestBackupWithoutServicesHasNoFileEntries(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, "compose/") || strings.HasPrefix(f.Name, "services/") {
			t.Fatalf("unexpected file entry %q in a service-free backup", f.Name)
		}
	}
}

// P1-03 isolation: services pinned to a different Docker instance must not
// get gateway listeners from this instance's reconcile, and stale routes for
// them are pruned. Two instances can run same-named containers.
func TestReconcileSkipsForeignSocketServices(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	svc := model.Service{ID: "svc-foreign",
		// The pinned instance runs a container with this ID; the fake daemon
		// on this instance does not know it, like a real cross-instance case.
		ContainerID: "foreign-id", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web",
		ComposeFile: filepath.Join(t.TempDir(), "docker-compose.yml"),
		Status:      "managed_running", StartTimeoutSecs: 1,
		DockerSocket: "/tmp/other-docker.sock", // pinned to a different instance
		Bindings:     bindings}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	// A stale listener from before the switch must be pruned.
	stale := gateway.Route{Key: svc.ID + "|127.0.0.1|" + itoa(hostPort), ServiceID: svc.ID,
		BindHost: "127.0.0.1", BindPort: hostPort, TargetHost: "127.0.0.1", TargetPort: bindings[0].ProxyPort}
	if err := e.gw.Ensure(stale); err != nil {
		t.Fatal(err)
	}

	e.reconcile()

	if keys := e.gw.RouteKeys(); len(keys) != 0 {
		t.Fatalf("foreign-socket service must not keep listeners, got %v", keys)
	}
	got, ok, _ := e.store.GetService(svc.ID)
	if !ok {
		t.Fatal("record missing")
	}
	if got.Status != "missing" || !strings.Contains(got.LastError, "Docker 实例") {
		t.Fatalf("foreign service must warn about its instance: %+v", got)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
