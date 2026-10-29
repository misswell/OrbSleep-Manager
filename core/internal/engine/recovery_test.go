package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
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
