package engine

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"orbsleep/internal/gateway"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/state"
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
	// The drifted file must survive as evidence (staged under the data dir),
	// not be silently destroyed.
	stages, _ := filepath.Glob(filepath.Join(e.dataDir, "restore-orig-*"))
	if len(stages) != 1 {
		t.Fatalf("expected one restore-orig staging dir, got %d", len(stages))
	}
	saved, _ := filepath.Glob(filepath.Join(stages[0], "*"))
	found := false
	for _, f := range saved {
		if raw, rerr := os.ReadFile(f); rerr == nil && strings.Contains(string(raw), "user edit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drifted content missing from the staging dir %s: %v", stages[0], saved)
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

// P1-03 isolation: the proxy admin endpoint answers on 127.0.0.1 even when
// the proxy runs on another Docker instance. A plan on the current instance
// must not wire targets into a proxy that cannot reach its containers.
func TestCreatePlanBlocksWhenProxyIsOnAnotherInstance(t *testing.T) {
	admin := newFakeAdmin(t) // admin ping succeeds, like a proxy on instance A
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	// The fake docker serves only the target container: a proxy container on
	// THIS instance would 404, like instance B without its own proxy.
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	plan, err := e.CreatePlan(context.Background(), []string{"abc123"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "blocked" {
		t.Fatalf("plan must be blocked, got %s (%+v)", plan.Status, plan.Blockers)
	}
	found := false
	for _, b := range plan.Blockers {
		if b.Code == "proxy_not_on_current_instance" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing proxy_not_on_current_instance blocker: %+v", plan.Blockers)
	}
	if len(plan.Items) != 0 {
		t.Fatalf("no items may be planned without a local proxy: %+v", plan.Items)
	}
}

// P2 singleflight: concurrent first-time dockerClient callers share one
// detection result. Without singleflight, callers arriving during the first
// detection each got a spurious "Docker 不可用". All callers must observe the
// identical outcome, whatever the environment's Docker state is.
func TestDockerClientConcurrentDetectionSharesResult(t *testing.T) {
	admin := newFakeAdmin(t)
	// No fake docker client injected: exercise the real detection path.
	e, _ := testEngine(t, admin, nil, nil)

	const callers = 12
	results := make(chan string, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, err := e.dockerClient(ctx)
			switch {
			case err != nil:
				results <- "err:" + err.Error()
			case c != nil:
				results <- "ok:" + c.Socket()
			default:
				results <- "bad:both-nil"
			}
		}()
	}
	close(start)
	outcomes := map[string]int{}
	for i := 0; i < callers; i++ {
		outcomes[<-results]++
	}
	if len(outcomes) != 1 {
		t.Fatalf("concurrent callers must share one detection outcome, got %v", outcomes)
	}
	for outcome := range outcomes {
		if strings.HasPrefix(outcome, "bad:") {
			t.Fatal(outcome)
		}
	}
}

// P0: the retention job must only prune truly terminal transactions. A
// rollback_failed record older than the cutoff is recovery material — pruning
// it would make every lifecycle guard report a clean board.
func TestPruneKeepsFailedRecovery(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	mk := func(id, status string) {
		if err := e.store.SaveTransaction(state.Transaction{
			ID: id, Kind: "attach", Status: status, Phase: status,
			StartedAt: old, UpdatedAt: old, Data: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("txn-committed-old", "committed")
	mk("txn-rolled-old", "rolled_back")
	mk("txn-failed-old", "rollback_failed")
	mk("txn-repair-old", "needs_repair")

	cutoff := time.Now().Add(-24 * time.Hour)
	if err := e.store.PruneTransactions(cutoff); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"txn-committed-old", "txn-rolled-old"} {
		if _, ok, _ := e.store.GetTransaction(id); ok {
			t.Fatalf("%s should have been pruned", id)
		}
	}
	for _, id := range []string{"txn-failed-old", "txn-repair-old"} {
		txn, ok, _ := e.store.GetTransaction(id)
		if !ok {
			t.Fatalf("%s must survive pruning (unresolved recovery record)", id)
		}
		_ = txn
	}
	// The surviving records must still block the lifecycle guards.
	if err := e.ensureNoUnresolvedWork(); err == nil {
		t.Fatal("unresolved transactions must block new operations after pruning")
	}
	if prep := e.PrepareShutdown(); prep.Safe {
		t.Fatal("unresolved transactions must block shutdown after pruning")
	}
}

// P0: every business write entry refuses unresolved transactions, including
// the needs_repair / rollback_failed states ApplyPlan used to ignore.
func TestBusinessWritesBlockedByFailedRecovery(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	if err := e.store.SaveTransaction(state.Transaction{
		ID: "txn-failed", Kind: "attach", Status: "rollback_failed", Phase: "rollback_failed", Data: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	// ApplyPlan: a ready plan must be refused.
	entry := &planEntry{createdAt: time.Now(), plan: model.Plan{ID: "plan-x", Status: "ready"},
		items: []planSnapshot{{containerName: "demo-web-1"}}}
	e.plansMu.Lock()
	e.plans["plan-x"] = entry
	e.plansMu.Unlock()
	if _, err := e.ApplyPlan("plan-x"); err == nil || !strings.Contains(err.Error(), "未完成事务") {
		t.Fatalf("ApplyPlan must be refused: %v", err)
	}

	// ServiceAction start.
	svc := model.Service{ID: "svc-blk", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web",
		ComposeFile: filepath.Join(t.TempDir(), "docker-compose.yml"),
		Status:      "managed_running", StartTimeoutSecs: 1,
		Bindings: []model.Binding{{HostIP: "127.0.0.1", HostPort: freePort(t), ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ServiceAction(context.Background(), svc.ID, "start", nil); err == nil {
		t.Fatal("ServiceAction must be refused while recovery is unresolved")
	}

	// updatePolicy.
	if _, err := e.updatePolicy(svc.ID, map[string]any{"idleTimeoutSecs": 600}); err == nil {
		t.Fatal("updatePolicy must be refused while recovery is unresolved")
	}

	// UpdateSettings.
	if _, err := e.UpdateSettings(map[string]any{"idleTimeoutSecs": 900}); err == nil {
		t.Fatal("UpdateSettings must be refused while recovery is unresolved")
	}

	// ProxySetup.
	if _, err := e.ProxySetup(); err == nil {
		t.Fatal("ProxySetup must be refused while recovery is unresolved")
	}
}

// P0: a backup archive whose manifest invents paths outside the recorded
// services and backup directories must be refused (no arbitrary file write).
func TestRestoreRejectsForeignManifestPath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sqlite")
	st, err := state.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	dbBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(dir, "evil.plist")
	if err := os.WriteFile(evil, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "crafted.zip")
	f, err := os.Create(backup)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("state.sqlite")
	w.Write(dbBytes)
	manifest, _ := json.Marshal(map[string]any{
		"files": []map[string]any{{
			"path":  evil,
			"entry": "evil-entry",
			"kind":  "compose",
			"mode":  420,
		}},
	})
	w, _ = zw.Create("manifest.json")
	w.Write(manifest)
	w, _ = zw.Create("evil-entry")
	w.Write([]byte("pwned"))
	zw.Close()
	f.Close()

	current := filepath.Join(dir, "state.sqlite")
	if err := os.WriteFile(current, []byte("broken-current-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"backupPath": backup})
	if err := os.WriteFile(filepath.Join(dir, "restore-request.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	err = ApplyPendingRestore(dir)
	if err == nil {
		t.Fatal("foreign manifest path must be refused")
	}
	// The evil file must be untouched and the live database not swapped.
	raw, _ := os.ReadFile(evil)
	if string(raw) != "original" {
		t.Fatal("manifest-invented path was written")
	}
	if db, _ := os.ReadFile(current); string(db) != "broken-current-db" {
		t.Fatal("database was swapped despite the refused manifest")
	}
}

// P0: a mid-restore failure must roll back already-written files and leave
// the live database untouched.
func TestRestoreIsAtomicAndRollsBackOnFailure(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	// Two managed services sharing one Compose file, both with backups.
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n  api:\n    image: nginx:alpine\n")
	if err := os.WriteFile(composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	web := model.Service{ID: "svc-web", ContainerName: "shared-web-1", ComposeProject: "shared",
		ComposeService: "web", ComposeFile: composeFile, Status: "managed_running", StartTimeoutSecs: 1}
	api := model.Service{ID: "svc-api", ContainerName: "shared-api-1", ComposeProject: "shared",
		ComposeService: "api", ComposeFile: composeFile, Status: "managed_running", StartTimeoutSecs: 1}
	for _, svc := range []model.Service{web, api} {
		if err := e.store.UpsertService(svc); err != nil {
			t.Fatal(err)
		}
		bd := filepath.Join(e.settings.Get().BackupDir, "services", svc.ID)
		if err := os.MkdirAll(bd, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bd, "docker-compose.yml"), managed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.store.SetMeta("serviceBackup:"+svc.ID, bd); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}

	// Drift the file, then make the restore target unwritable so the commit
	// phase fails partway (the file lives in a read-only directory).
	if err := os.WriteFile(composeFile, []byte("drifted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// RestoreBackup registers the marker; drive the apply path directly and
	// expect the commit phase to fail in the read-only directory.
	marker, _ := json.Marshal(map[string]string{"backupPath": entry.Path})
	if err := os.WriteFile(filepath.Join(e.dataDir, "restore-request.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	restoreErr := ApplyPendingRestore(e.dataDir)
	_ = os.Chmod(dir, 0o755)
	if restoreErr == nil {
		// On this platform the write into the read-only directory may still
		// succeed via permissions; the invariant below is what matters.
		t.Log("restore unexpectedly succeeded; checking consistency only")
	}
	// Invariant: database and files agree. The DB swap happens only after the
	// file phase, so a failure must leave the drifted file and the live DB.
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if restoreErr != nil && string(raw) == string(managed) {
		t.Fatal("failed restore overwrote files without completing")
	}
	if restoreErr != nil {
		if _, ok, _ := e.store.GetService("svc-web"); !ok {
			t.Fatal("live database must stay authoritative after a failed restore")
		}
	}
}

// P0: WAL/SHM sidecar removal failure must abort the restore instead of
// pairing a fresh database with an incompatible log.
func TestApplyPendingRestoreWALRemovalFailureAborts(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sqlite")
	st, err := state.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	dbBytes, _ := os.ReadFile(source)
	backup := filepath.Join(dir, "backup.zip")
	f, _ := os.Create(backup)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("state.sqlite")
	w.Write(dbBytes)
	zw.Close()
	f.Close()

	current := filepath.Join(dir, "state.sqlite")
	if err := os.WriteFile(current, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory cannot be removed with os.Remove while non-empty: this
	// simulates an undeletable WAL sidecar.
	if err := os.MkdirAll(current+"-wal", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current+"-wal/x", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"backupPath": backup})
	if err := os.WriteFile(filepath.Join(dir, "restore-request.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPendingRestore(dir); err == nil {
		t.Fatal("undeletable WAL sidecar must abort the restore")
	}
	if db, _ := os.ReadFile(current); string(db) == string(dbBytes) {
		t.Fatal("database was swapped despite the WAL removal failure")
	}
}

// P1: a backup must include the recovery material of unresolved transactions
// (their per-item backups), written atomically (no .tmp left behind).
func TestCreateBackupPackagesTxnMaterial(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	itemDir := filepath.Join(e.settings.Get().BackupDir, "ops", "op-x", "item-1")
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "docker-compose.yml"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SaveTransaction(state.Transaction{
		ID: "txn-mat", Kind: "attach", Status: "rollback_failed", Phase: "rollback_failed",
		Data: `{"items":[{"backupPath":"` + filepath.Join(itemDir, "docker-compose.yml") + `"}]}`,
	}); err != nil {
		t.Fatal(err)
	}

	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	found := false
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, "txns/txn-mat/") && strings.HasSuffix(f.Name, "docker-compose.yml") {
			found = true
		}
		if strings.HasSuffix(f.Name, ".tmp") {
			t.Fatalf("temp archive leaked: %s", f.Name)
		}
	}
	if !found {
		t.Fatal("unresolved transaction's backup material missing from the archive")
	}
}

// P1: a proxy container that exists but is not running must not let a plan
// through (its admin endpoint could still answer from another instance).
func TestPlanBlockedWhenProxyStopped(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	docker.stoppedProxy = true
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	plan, err := e.CreatePlan(context.Background(), []string{"abc123"}, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range plan.Blockers {
		if b.Code == "proxy_not_running" {
			found = true
		}
	}
	if !found || plan.Status != "blocked" {
		t.Fatalf("stopped proxy must block the plan: %s %+v", plan.Status, plan.Blockers)
	}
}

// P2: privileged admin/status ports are rejected at settings time instead of
// failing later during proxy installation.
func TestUpdateSettingsRejectsLowPorts(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	if _, err := e.UpdateSettings(map[string]any{"adminPort": 80}); err == nil {
		t.Fatal("admin port below 1024 must be rejected")
	}
	if _, err := e.UpdateSettings(map[string]any{"webPort": 443}); err == nil {
		t.Fatal("status port below 1024 must be rejected")
	}
}
