package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/state"
)

func TestRecoverInterruptedAttachTransaction(t *testing.T) {
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
	docker.setPorts(snap.removals, false)

	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: snap.containerName, Ports: []string{"20010:80"}}}}
	admin.mu.Unlock()

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

	e.RecoverTransactions()

	got, ok, err := e.store.GetTransaction(txnID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "rolled_back" {
		t.Fatalf("txn status = %s error=%s", got.Status, got.Error)
	}
	raw, err := os.ReadFile(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("compose not restored:\n%s", raw)
	}
	if cfg := admin.config(); len(cfg.Services) != 0 {
		t.Fatalf("proxy target not removed: %+v", cfg.Services)
	}
}

func TestRecoveryKeepsExternallyModifiedCompose(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)

	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	original := append([]byte(nil), snap.content...)
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	external := []byte("services:\n  web:\n    image: nginx:alpine\n    environment:\n      USER_EDIT: \"1\"\n")
	if err := os.WriteFile(snap.composeFile, external, 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "compose_updated", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].ManagedContent = managed
	}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rollback_failed" {
		t.Fatalf("status = %s, want rollback_failed", got.Status)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(external) {
		t.Fatalf("external edits were overwritten:\n%s", raw)
	}
}

func TestShutdownCancelsAndWaitsOperations(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	ctx, done, err := e.beginOperation(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		done()
		close(finished)
	}()
	go e.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("operation was not cancelled and awaited")
	}
	if _, _, err := e.beginOperation(time.Minute); err == nil {
		t.Fatal("new operations must be rejected after shutdown starts")
	}
}

func TestRecoveryAfterProxyUpdateKeepsCompose(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)

	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	original := append([]byte(nil), snap.content...)
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: snap.containerName, Ports: []string{"20011:80"}}}}
	admin.mu.Unlock()

	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "proxy_updated", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].Phase = "proxy_updated"
	}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rolled_back" {
		t.Fatalf("status = %s, want rolled_back", got.Status)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(original) {
		t.Fatalf("compose should be untouched:\n%s", raw)
	}
	if cfg := admin.config(); len(cfg.Services) != 0 {
		t.Fatalf("proxy target not removed: %+v", cfg.Services)
	}
}

func TestRecoveryMarksCommittedTransaction(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	if err := os.WriteFile(snap.composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{ID: "svc-committed", ContainerID: snap.containerID, ContainerName: snap.containerName,
		ComposeProject: snap.composeProject, ComposeService: snap.composeService, ComposeFile: snap.composeFile,
		Status: "managed_running", Bindings: bindings, StartTimeoutSecs: 1}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "committed_item", func(p *txnPayload) {
		p.Items[0].BackupPath = filepath.Join(t.TempDir(), "docker-compose.yml")
		p.Items[0].ManagedContent = managed
		p.Items[0].ServiceID = svc.ID
		p.Items[0].Phase = "committed_item"
	}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "committed" {
		t.Fatalf("status = %s, want committed", got.Status)
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("committed service record must be kept")
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(managed) {
		t.Fatalf("committed compose file must be kept:\n%s", raw)
	}
}

func TestRecoveryAfterComposeWriteBeforeTxnUpdate(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)

	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	original := append([]byte(nil), snap.content...)
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	// Simulate: atomic rename finished, but ManagedContent was not persisted.
	if err := os.WriteFile(snap.composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	docker.setPorts(snap.removals, false)
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: snap.containerName, Ports: []string{"20012:80"}}}}
	admin.mu.Unlock()

	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "compose_write_pending", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].PendingContent = managed
		p.Items[0].Phase = "compose_write_pending"
	}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rolled_back" {
		t.Fatalf("status = %s error=%s", got.Status, got.Error)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(original) {
		t.Fatalf("compose was not restored:\n%s", raw)
	}
	if cfg := admin.config(); len(cfg.Services) != 0 {
		t.Fatalf("proxy target not removed: %+v", cfg.Services)
	}
}

func TestRecoverInterruptedDetachRevertsToManaged(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)

	// Simulate detach having restored the original Compose before crashing.
	original := []byte("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:12345:80\"\n")
	if err := os.WriteFile(snap.composeFile, original, 0o644); err != nil {
		t.Fatal(err)
	}
	docker.setPorts(snap.removals, true)
	workDir := t.TempDir()
	txnID, err := e.startTxn("op-detach", "detach", detachPayload{
		OperationID: "op-detach", Service: svc, WorkingDir: workDir, ManagedContent: managed, ExpectedRestored: original, Phase: "compose_restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rolled_back" {
		t.Fatalf("status = %s error=%s", got.Status, got.Error)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(managed) {
		t.Fatalf("compose was not reverted to managed:\n%s", raw)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener missing: %v", e.gw.RouteKeys())
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept")
	}
	if cli.lastWorkdir != workDir {
		t.Fatalf("recovery used workdir %q, want %q", cli.lastWorkdir, workDir)
	}
}

func TestRecoverCompletedDetachMarksCommitted(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	svc, snap, _, _ := managedServiceFixture(t, e, admin, docker, e.cli)
	if err := e.store.DeleteService(svc.ID); err != nil {
		t.Fatal(err)
	}
	txnID, err := e.startTxn("op-detach", "detach", detachPayload{
		OperationID: "op-detach", Service: svc, ManagedContent: snap.content, Phase: "verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "committed" {
		t.Fatalf("status = %s error=%s", got.Status, got.Error)
	}
}

func TestReconcileOrphanOperations(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	orphan := model.Operation{ID: "op-orphan", Kind: "proxy_setup", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := e.store.InsertOperation(orphan); err != nil {
		t.Fatal(err)
	}
	committed := model.Operation{ID: "op-committed", Kind: "attach", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := e.store.InsertOperation(committed); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SaveTransaction(state.Transaction{
		ID: "txn-committed", Kind: "attach", OperationID: committed.ID, Status: "committed",
		Phase: "committed", StartedAt: time.Now().UTC().Format(time.RFC3339), Data: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	e.ReconcileOrphanOperations()

	got, _, _ := e.store.GetOperation(orphan.ID)
	if got.Status != "failed" {
		t.Fatalf("orphan status = %s", got.Status)
	}
	got, _, _ = e.store.GetOperation(committed.ID)
	if got.Status != "succeeded" {
		t.Fatalf("committed op status = %s", got.Status)
	}
}

func TestFinishTxnAtomicUpdatesBothRecords(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	op := model.Operation{ID: "op-atomic", Kind: "attach", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	txnID, err := e.startTxn(op.ID, "attach", txnPayload{OperationID: op.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.finishTxnAtomic(txnID, op.ID, "committed", nil, "committed")
	txn, _, _ := e.store.GetTransaction(txnID)
	if txn.Status != "committed" {
		t.Fatalf("txn status = %s", txn.Status)
	}
	got, _, _ := e.store.GetOperation(op.ID)
	if got.Status != "succeeded" {
		t.Fatalf("op status = %s", got.Status)
	}
}

func TestRecoveryRevertsToManagedWhenOriginalRestoreFails(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)

	// Crash state: Compose is managed and a gateway listener exists.
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, snap.content, 0o600); err != nil {
		t.Fatal(err)
	}
	cli.upFn = func(file string) error {
		raw, _ := os.ReadFile(file)
		if bytes.Contains(raw, []byte("ports:")) {
			return errors.New("simulated original restore failure")
		}
		return nil
	}
	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "compose_updated", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].ServiceID = svc.ID
		p.Items[0].ManagedContent = managed
	}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)

	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(managed) {
		t.Fatalf("service was not reverted to managed:\n%s", raw)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener missing after revert: %v", e.gw.RouteKeys())
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept")
	}
	if got.Status != "rollback_failed" {
		t.Fatalf("status = %s, want rollback_failed", got.Status)
	}
}

func TestDeferredRecoveryWorksAfterReconcileRebuildsGateway(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	backupDir, _ := e.store.GetMeta("serviceBackup:" + svc.ID)
	backupPath := filepath.Join(backupDir, "docker-compose.yml")

	txnID, err := e.startTxn("op-crashed", "attach", txnFromPlan("op-crashed", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.updateTxn(txnID, "compose_updated", func(p *txnPayload) {
		p.Items[0].BackupPath = backupPath
		p.Items[0].ServiceID = svc.ID
		p.Items[0].ManagedContent = managed
	}); err != nil {
		t.Fatal(err)
	}
	// First recovery attempt cannot reach the proxy: transaction stays running
	// and the reconcile-created gateway listener remains.
	s := e.settings.Get()
	s.AdminPort = freePort(t)
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	if got, _, _ := e.store.GetTransaction(txnID); got.Status != "running" {
		t.Fatalf("first recovery should leave the transaction running, got %s", got.Status)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("expected existing gateway listener, got %v", e.gw.RouteKeys())
	}
	// Proxy is reachable again: the second attempt must release the gateway
	// before restoring the original Compose file.
	s.AdminPort = admin.port()
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	if got, _, _ := e.store.GetTransaction(txnID); got.Status != "rolled_back" {
		t.Fatalf("second recovery status = %s error=%s", got.Status, got.Error)
	}
	if len(e.gw.RouteKeys()) != 0 {
		t.Fatalf("gateway listener must be released before restore: %v", e.gw.RouteKeys())
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(snap.content) {
		t.Fatalf("compose not restored:\n%s", raw)
	}
}

func TestRecoveryMarksCommittedSharedComposeItems(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)

	file := filepath.Join(t.TempDir(), "docker-compose.yml")
	intermediate := []byte("services:\n  web:\n    image: nginx:alpine\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:19002:80\"\n")
	final := []byte("services:\n  web:\n    image: nginx:alpine\n  api:\n    image: nginx:alpine\n")
	if err := os.WriteFile(file, final, 0o644); err != nil {
		t.Fatal(err)
	}
	base := model.Service{ComposeFile: file, Status: "managed_running", StartTimeoutSecs: 1}
	web := base
	web.ID, web.ContainerName, web.ComposeProject, web.ComposeService = "svc-web", "shared-web-1", "shared", "web"
	api := base
	api.ID, api.ContainerName, api.ComposeProject, api.ComposeService = "svc-api", "shared-api-1", "shared", "api"
	for _, svc := range []model.Service{web, api} {
		if err := e.store.UpsertService(svc); err != nil {
			t.Fatal(err)
		}
	}
	payload := txnPayload{OperationID: "op-shared", Items: []txnItem{
		{ContainerName: web.ContainerName, ComposeFile: file, ComposeService: "web", ServiceID: web.ID,
			ManagedContent: intermediate, Phase: "committed_item"},
		{ContainerName: api.ContainerName, ComposeFile: file, ComposeService: "api", ServiceID: api.ID,
			ManagedContent: final, Phase: "committed_item"},
	}}
	txnID, err := e.startTxn("op-shared", "attach", payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.InsertOperation(model.Operation{ID: "op-shared", Kind: "attach", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "committed" {
		t.Fatalf("status = %s error=%s; shared compose items must be treated as committed", got.Status, got.Error)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != string(final) {
		t.Fatalf("committed shared file was modified:\n%s", raw)
	}
}

func TestDetachRecoveryWithoutExpectedSnapshotDoesNotOverwrite(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	external := []byte("services:\n  web:\n    image: nginx:alpine\n    environment:\n      USER_EDIT: \"1\"\n")
	if err := os.WriteFile(snap.composeFile, external, 0o644); err != nil {
		t.Fatal(err)
	}
	txnID, err := e.startTxn("op-detach", "detach", detachPayload{
		OperationID: "op-detach", Service: svc, ManagedContent: managed, Phase: "compose_restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rollback_failed" {
		t.Fatalf("status = %s, want rollback_failed", got.Status)
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(external) {
		t.Fatalf("external edit was overwritten:\n%s", raw)
	}
}

func TestRecoverySharedComposeItemCrashedBeforeComposeWrite(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "shared-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	// fakeCLI.UpService re-publishes ports it finds in the restored file.
	docker.setPorts([]model.PortBinding{{HostIP: "127.0.0.1", HostPort: 19001, ContainerPort: 80, Protocol: "tcp"}}, false)

	file := filepath.Join(t.TempDir(), "docker-compose.yml")
	original := []byte("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:19001:80\"\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:19002:80\"\n")
	intermediate := []byte("services:\n  web:\n    image: nginx:alpine\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:19002:80\"\n")
	if err := os.WriteFile(file, intermediate, 0o644); err != nil {
		t.Fatal(err)
	}
	web := model.Service{ID: "svc-web", ContainerName: "shared-web-1", ComposeProject: "shared",
		ComposeService: "web", ComposeFile: file, Status: "managed_running", StartTimeoutSecs: 1}
	if err := e.store.UpsertService(web); err != nil {
		t.Fatal(err)
	}

	backupDir := t.TempDir()
	backupA := filepath.Join(backupDir, "item-1", "docker-compose.yml")
	backupB := filepath.Join(backupDir, "item-2", "docker-compose.yml")
	for _, b := range []struct {
		path    string
		content []byte
	}{
		{backupA, original}, {backupB, intermediate},
	} {
		if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b.path, b.content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A committed; B registered its proxy target and crashed before writing
	// the shared file. The file on disk is A's committed state.
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{
		{Name: "shared-web-1", Ports: []string{"21010:80"}},
		{Name: "shared-api-1", Ports: []string{"21011:80"}},
	}}
	admin.mu.Unlock()

	txnID, err := e.startTxn("op-shared-crash", "attach", txnPayload{
		OperationID: "op-shared-crash",
		Baseline: proxy.Config{Services: []proxy.ServiceConfig{
			{Name: "shared-web-1", Ports: []string{"20001:80"}},
		}},
		Items: []txnItem{
			{ContainerName: web.ContainerName, ComposeFile: file, ComposeService: "web",
				ServiceID: web.ID, Content: original, Removals: []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 19001, ContainerPort: 80, Protocol: "tcp"}},
				BackupPath: backupA, ManagedContent: intermediate, Phase: "committed_item"},
			{ContainerName: "shared-api-1", ComposeFile: file, ComposeService: "api",
				ServiceID: "svc-api", Content: original, BackupPath: backupB, Phase: "proxy_updated"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.InsertOperation(model.Operation{ID: "op-shared-crash", Kind: "attach", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	e.RecoverTransactions()

	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rolled_back" {
		t.Fatalf("status = %s error=%s", got.Status, got.Error)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("shared compose not restored to pre-attach content:\n%s", raw)
	}
	for _, id := range []string{web.ID, "svc-api"} {
		if _, ok, _ := e.store.GetService(id); ok {
			t.Fatalf("record %s must be gone after full rollback", id)
		}
	}
	cfg := admin.config()
	if len(cfg.Services) != 1 {
		t.Fatalf("proxy must equal the baseline, got %+v", cfg.Services)
	}
	if sc, ok := cfg.Service("shared-web-1"); !ok || len(sc.Ports) != 1 || sc.Ports[0] != "20001:80" {
		t.Fatalf("web baseline entry not restored: %+v", cfg.Services)
	}
	if _, ok := cfg.Service("shared-api-1"); ok {
		t.Fatalf("crashed item's proxy entry must be revoked: %+v", cfg.Services)
	}
}

func TestRecoverDetachWithMissingCompose(t *testing.T) {
	t.Run("deleted during detach with managed snapshot", func(t *testing.T) {
		admin := newFakeAdmin(t)
		docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
		e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
		svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
		if err := os.Remove(snap.composeFile); err != nil {
			t.Fatal(err)
		}
		txnID, err := e.startTxn("op-detach-del", "detach", detachPayload{
			OperationID: "op-detach-del", Service: svc, ManagedContent: managed, Phase: "prepared",
		})
		if err != nil {
			t.Fatal(err)
		}
		e.RecoverTransactions()
		got, _, _ := e.store.GetTransaction(txnID)
		if got.Status != "rolled_back" {
			t.Fatalf("status = %s error=%s", got.Status, got.Error)
		}
		raw, err := os.ReadFile(snap.composeFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(managed) {
			t.Fatalf("managed content was not rebuilt:\n%s", raw)
		}
		if _, ok, _ := e.store.GetService(svc.ID); !ok {
			t.Fatal("managed record must be kept")
		}
		if len(e.gw.RouteKeys()) != 1 {
			t.Fatalf("gateway listener missing: %v", e.gw.RouteKeys())
		}
	})

	t.Run("already missing at detach start without managed snapshot", func(t *testing.T) {
		admin := newFakeAdmin(t)
		docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
		e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
		svc, snap, original, _ := managedServiceFixture(t, e, admin, docker, e.cli)
		if err := os.Remove(snap.composeFile); err != nil {
			t.Fatal(err)
		}
		txnID, err := e.startTxn("op-detach-gone", "detach", detachPayload{
			OperationID: "op-detach-gone", Service: svc, Phase: "prepared",
		})
		if err != nil {
			t.Fatal(err)
		}
		e.RecoverTransactions()
		got, _, _ := e.store.GetTransaction(txnID)
		if got.Status != "rolled_back" {
			t.Fatalf("status = %s error=%s", got.Status, got.Error)
		}
		raw, err := os.ReadFile(snap.composeFile)
		if err != nil {
			t.Fatal(err)
		}
		// Rebuilt from the pre-attach backup with this service's ports
		// removed again: the managed equivalent of the original content.
		managed, _, err := compose.RemoveBindings(original, svc.ComposeService, bindingsFor(svc))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(managed) {
			t.Fatalf("rebuilt managed content mismatch:\n got=%q\nwant=%q", raw, managed)
		}
		if _, ok, _ := e.store.GetService(svc.ID); !ok {
			t.Fatal("managed record must be kept")
		}
	})
}

func TestReconcileContainerIPMissingProxyIPSkipsListener(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	s := e.settings.Get()
	s.ProxyDialMode = "container_ip"
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: freePort(t), ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	svc := model.Service{ID: "svc-rec", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web",
		ComposeFile: filepath.Join(t.TempDir(), "docker-compose.yml"),
		Status:      "managed_running", Bindings: bindings, StartTimeoutSecs: 1}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}

	e.reconcile()

	got, ok, _ := e.store.GetService(svc.ID)
	if !ok {
		t.Fatal("record missing")
	}
	if got.Status != "warning" || !strings.Contains(got.LastError, "代理容器 IP") {
		t.Fatalf("service must warn about the unresolvable proxy IP: status=%s lastError=%s", got.Status, got.LastError)
	}
	if keys := e.gw.RouteKeys(); len(keys) != 0 {
		t.Fatalf("no fallback listener may be created, got %v", keys)
	}
}

func TestRecoverDetachMissingComposeRecreatesManagedState(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	if err := os.Remove(snap.composeFile); err != nil {
		t.Fatal(err)
	}
	docker.setPorts(snap.removals, true)
	workDir := t.TempDir()
	txnID, err := e.startTxn("op-detach-missing", "detach", detachPayload{
		OperationID: "op-detach-missing", Service: svc, WorkingDir: workDir,
		ManagedContent: managed, Phase: "prepared_missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.RecoverTransactions()
	got, _, _ := e.store.GetTransaction(txnID)
	if got.Status != "rolled_back" {
		t.Fatalf("status = %s error=%s", got.Status, got.Error)
	}
	raw, err := os.ReadFile(snap.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(managed) {
		t.Fatalf("managed Compose was not rebuilt:\n%s", raw)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener missing: %v", e.gw.RouteKeys())
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept")
	}
	if _, ok := e.store.GetMeta("serviceBackup:" + svc.ID); !ok {
		t.Fatal("backup metadata must be kept")
	}
}
