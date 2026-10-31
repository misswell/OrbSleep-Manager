package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orbsleep/internal/model"
)

// A broken backup must quarantine the restore marker and return a RestoreError
// so the daemon can keep starting on the existing database instead of looping
// under launchd's KeepAlive.
func TestApplyPendingRestoreQuarantinesBrokenBackup(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.zip")
	if err := os.WriteFile(broken, []byte("this is not a zip file"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "restore-request.json")
	if err := os.WriteFile(marker, []byte(`{"backupPath":"`+broken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ApplyPendingRestore(dir)
	if err == nil {
		t.Fatal("expected restore to fail on a broken backup")
	}
	if _, ok := err.(*RestoreError); !ok {
		t.Fatalf("expected *RestoreError, got %T: %v", err, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("restore marker must be quarantined, stat err: %v", statErr)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "restore-failed-*.json")); len(matches) != 1 {
		t.Fatalf("expected exactly one restore-failed marker, got %v", matches)
	}
	if !restoreFailedMarkerExists(dir) {
		t.Fatal("restoreFailedMarkerExists must report the quarantine")
	}
}

// A valid restore still replaces the database and clears the marker.
func TestApplyPendingRestoreStillWorksWithValidBackup(t *testing.T) {
	dir := t.TempDir()
	if _, err := New("test", dir); err != nil {
		t.Fatal(err)
	}
	e, err := New("test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	entry, err := e.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	// Markers written by the live daemon carry the restore allowlist; this
	// backup has no file manifest, so an empty scope is enough.
	marker := filepath.Join(dir, "restore-request.json")
	markerPayload, _ := json.Marshal(map[string]any{
		"backupPath": entry.Path,
		"allow":      map[string]any{"files": []string{}, "dirs": []string{dir}},
	})
	if err := os.WriteFile(marker, markerPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("valid restore must succeed: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("marker must be removed after a successful restore")
	}
}

// If the Compose file lock cannot be acquired, runDetach must still call done()
// so Engine.Close() is not blocked forever by a leaked WaitGroup count.
func TestRecoverDetachLockTimeoutReleasesTxnWG(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, _, _, _ := managedServiceFixture(t, e, admin, docker, e.cli)

	// Hold the per-file lock the way an in-flight attach would.
	held, err := e.composeLocker.Acquire(context.Background(), svc.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	done := make(chan struct{})
	e.txnWG.Add(1)
	op := model.Operation{ID: "op-lock-timeout", Kind: "restore", Status: "running"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	go e.runDetach(ctx, svc, op, false, func() {
		e.txnWG.Done() // the real beginOperation done() also releases the WaitGroup
		close(done)
	})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done() was not called after the lock acquisition failed: WaitGroup leak")
	}

	closed := make(chan struct{})
	go func() {
		e.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Engine.Close() blocked: transaction WaitGroup leak")
	}
}

// A rollback_failed transaction stays visible and can be retried by ID.
func TestRecoverTransactionRetriesFailedByID(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	if err := os.Remove(snap.composeFile); err != nil {
		t.Fatal(err)
	}
	txnID, err := e.startTxn("op-detach-retry", "detach", detachPayload{
		OperationID: "op-detach-retry", Service: svc, ManagedContent: managed, Phase: "prepared",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	if got, _, _ := e.store.GetTransaction(txnID); got.Status != "rolled_back" {
		t.Fatalf("precondition: expected rolled_back, got %s", got.Status)
	}

	// Simulate a failed recovery attempt left behind by an earlier crash.
	if err := e.store.FinishTransaction(txnID, "rollback_failed", "test: forced failure", "rollback_failed"); err != nil {
		t.Fatal(err)
	}
	list, lerr := e.RecoveryList()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(list.Transactions) != 1 {
		t.Fatalf("rollback_failed transaction must be listed, got %+v", list.Transactions)
	}
	if err := e.RecoverTransaction(txnID); err != nil {
		t.Fatalf("retry by id must run: %v", err)
	}
	if got, _, _ := e.store.GetTransaction(txnID); got.Status != "rolled_back" {
		t.Fatalf("retry must complete the rollback, got %s (%s)", got.Status, got.Error)
	}
	if list2, lerr := e.RecoveryList(); lerr != nil || len(list2.Transactions) != 0 {
		t.Fatal("transaction must be resolved after the retry")
	}
}

// Concurrent single and batch recovery must serialize instead of racing on the
// same Compose file and transaction rows.
func TestRecoveryConcurrentSingleAndBatch(t *testing.T) {
	for i := 0; i < 5; i++ {
		admin := newFakeAdmin(t)
		docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
		e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
		svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
		if err := os.Remove(snap.composeFile); err != nil {
			t.Fatal(err)
		}
		txnID, err := e.startTxn("op-detach-conc", "detach", detachPayload{
			OperationID: "op-detach-conc", Service: svc, ManagedContent: managed, Phase: "prepared",
		})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); e.RecoverTransactions() }()
		go func() { defer wg.Done(); _ = e.RecoverTransaction(txnID) }()
		go func() { defer wg.Done(); _, _ = e.RecoveryResolve(txnID) }()
		wg.Wait()
		got, _, _ := e.store.GetTransaction(txnID)
		if got.Status != "rolled_back" {
			t.Fatalf("iteration %d: status = %s error=%s", i, got.Status, got.Error)
		}
		raw, err := os.ReadFile(snap.composeFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(managed) {
			t.Fatalf("iteration %d: managed content not in place", i)
		}
	}
}
