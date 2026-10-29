package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbsleep/internal/model"
)

func TestCleanupBackupsHonoursOperationStatus(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	root := t.TempDir()
	s := e.settings.Get()
	s.BackupDir = root
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -30)
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	op := func(id, status string) {
		if err := e.store.InsertOperation(model.Operation{ID: id, Kind: "attach", Status: status,
			StartedAt: old.Format(time.RFC3339), FinishedAt: old.Format(time.RFC3339), BackupDir: filepath.Join(root, id)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("op-succeeded")
	op("op-succeeded", "succeeded")
	mk("op-failed")
	op("op-failed", "failed")
	mk("op-rollback")
	op("op-rollback", "rollback_failed")
	mk("unknown")
	mk("proxy/op-proxy-ok")
	op("op-proxy-ok", "succeeded")
	mk("proxy/op-proxy-fail")
	op("op-proxy-fail", "rollback_failed")
	mk("services")

	e.cleanupOldBackups(old.Add(time.Hour))

	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return err == nil
	}
	for _, keep := range []string{"op-failed", "op-rollback", "unknown", "proxy/op-proxy-fail", "services"} {
		if !exists(keep) {
			t.Fatalf("%s must be retained", keep)
		}
	}
	for _, gone := range []string{"op-succeeded", "proxy/op-proxy-ok"} {
		if exists(gone) {
			t.Fatalf("%s should have been pruned", gone)
		}
	}
}
