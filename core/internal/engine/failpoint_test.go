package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"orbsleep/internal/failpoint"
	"orbsleep/internal/state"
)

func TestFailpointHelperProcess(t *testing.T) {
	if os.Getenv("ORBSLEEP_FAILPOINT_HELPER") != "1" {
		t.Skip("helper process only")
	}
	dir := os.Getenv("ORBSLEEP_HELPER_DIR")
	store, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	txn := state.Transaction{
		ID: "txn-kill", Kind: "attach", OperationID: "op-kill", Status: "running",
		Phase: "compose_write_pending", StartedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339), Data: `{"operationId":"op-kill"}`,
	}
	if err := store.SaveTransaction(txn); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	// This must terminate the process; the parent checks the file survived.
	failpoint.Hit("test_kill")
	t.Fatal("failpoint did not terminate the helper")
}

func TestSIGKILLPersistsTransactionJournal(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestFailpointHelperProcess")
	cmd.Env = append(os.Environ(),
		"ORBSLEEP_FAILPOINT_HELPER=1",
		"ORBSLEEP_HELPER_DIR="+dir,
		"ORBSLEEP_FAILPOINT=test_kill",
		"ORBSLEEP_FAILPOINT_MODE=kill",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper unexpectedly exited normally: %s", out)
	}
	store, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, ok, err := store.GetTransaction("txn-kill")
	if err != nil || !ok {
		t.Fatalf("transaction lost after SIGKILL: ok=%v err=%v", ok, err)
	}
	if got.Status != "running" || got.Phase != "compose_write_pending" {
		t.Fatalf("transaction = %+v", got)
	}
}
