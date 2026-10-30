package state

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"orbsleep/internal/model"
)

func TestServicesAndOperations(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := model.Service{
		ID: "svc-1", ContainerID: "c1", ContainerName: "demo-web-1",
		ComposeProject: "demo", ComposeService: "web", ComposeFile: "/tmp/docker-compose.yml",
		Status:          "managed_running",
		Bindings:        []model.Binding{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, ProxyPort: 20000, Protocol: "tcp"}},
		IdleTimeoutSecs: 1800, StartTimeoutSecs: 60, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListServices()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	if list[0].Bindings[0].ProxyPort != 20000 {
		t.Fatalf("bindings = %+v", list[0].Bindings)
	}
	if err := st.SetServiceStatus("svc-1", "managed_stopped", "", "c2"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetService("svc-1")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if got.Status != "managed_stopped" || got.ContainerID != "c2" {
		t.Fatalf("got = %+v", got)
	}

	op := model.Operation{ID: "op-1", Kind: "attach", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339),
		Log: []model.LogEntry{{At: "now", Level: "info", Message: "hello"}}}
	if err := st.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	op.Status = "succeeded"
	if err := st.UpdateOperation(op); err != nil {
		t.Fatal(err)
	}
	gotOp, ok, err := st.GetOperation("op-1")
	if err != nil || !ok || gotOp.Status != "succeeded" || len(gotOp.Log) != 1 {
		t.Fatalf("op = %+v ok=%v err=%v", gotOp, ok, err)
	}
	ops, err := st.ListOperations(10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops=%d err=%v", len(ops), err)
	}

	if err := st.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, ok := st.GetSetting("k"); !ok || v != "v" {
		t.Fatalf("setting = %q %v", v, ok)
	}
	if err := st.SetMeta("m", "1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := st.GetMeta("m"); !ok || v != "1" {
		t.Fatalf("meta = %q %v", v, ok)
	}
	if err := st.DeleteService("svc-1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListServices(); len(list) != 0 {
		t.Fatalf("services after delete = %d", len(list))
	}
}

func TestOpenTwiceIgnoresDuplicateColumnMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("second open must ignore duplicate column: %v", err)
	}
	second.Close()
}

func TestDuplicateColumnDetection(t *testing.T) {
	if !isDuplicateColumn(errors.New("SQL logic error: duplicate column name: operation_id")) {
		t.Fatal("duplicate column error must be recognized")
	}
	if isDuplicateColumn(errors.New("attempt to write a readonly database")) {
		t.Fatal("unrelated migration errors must not be ignored")
	}
}

func TestSQLiteSynchronousFull(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 { // 2 = FULL
		t.Fatalf("synchronous = %d, want FULL(2)", mode)
	}
}
