package engine

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orbsleep/internal/state"
)

func writeZip(t *testing.T, path, name string, data []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupIDFromPath(t *testing.T) {
	cases := map[string]string{
		"OrbSleep-backup-20261030-091959.zip": "backup-20261030-091959",
		"custom.zip":                          "custom",
	}
	for input, want := range cases {
		if got := backupIDFromPath(input); got != want {
			t.Fatalf("backupIDFromPath(%q)=%q want %q", input, got, want)
		}
	}
}

func TestValidateBackupZip(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.zip")
	writeZip(t, good, "state.sqlite", append([]byte("SQLite format 3\x00"), make([]byte, 64)...))
	if err := validateBackupZip(good); err != nil {
		t.Fatalf("valid backup rejected: %v", err)
	}
	bad := filepath.Join(dir, "bad.zip")
	writeZip(t, bad, "state.sqlite", []byte("not a sqlite file"))
	if err := validateBackupZip(bad); err == nil {
		t.Fatal("invalid backup accepted")
	}
	missing := filepath.Join(dir, "missing.zip")
	writeZip(t, missing, "settings.json", []byte("{}"))
	if err := validateBackupZip(missing); err == nil {
		t.Fatal("backup without state.sqlite accepted")
	}
}

func TestApplyPendingRestore(t *testing.T) {
	dir := t.TempDir()

	// A real (empty) SQLite database to use as the backup snapshot.
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
	backup := filepath.Join(dir, "backup.zip")
	writeZip(t, backup, "state.sqlite", dbBytes)

	// A deliberately invalid current database that restore must replace.
	current := filepath.Join(dir, "state.sqlite")
	if err := os.WriteFile(current, []byte("broken-current-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Markers written by the live daemon carry the restore allowlist; this
	// backup has no file manifest, so an empty scope is enough.
	marker, _ := json.Marshal(map[string]any{
		"backupPath": backup,
		"allow":      map[string]any{"files": []string{}, "dirs": []string{dir}},
	})
	if err := os.WriteFile(filepath.Join(dir, "restore-request.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("ApplyPendingRestore: %v", err)
	}
	restored, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) < 16 || string(restored[:16]) != "SQLite format 3\x00" {
		t.Fatalf("database was not restored: %q", string(restored[:min(16, len(restored))]))
	}
	orig, _ := filepath.Glob(filepath.Join(dir, "restore-orig-*"))
	if len(orig) != 1 {
		t.Fatalf("expected one restore-orig snapshot dir, got %d", len(orig))
	}
	if _, err := os.Stat(filepath.Join(orig[0], "state.sqlite")); err != nil {
		t.Fatalf("original database missing from the snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-request.json")); !os.IsNotExist(err) {
		t.Fatal("restore marker was not removed")
	}
}

func TestApplyPendingRestoreNoMarker(t *testing.T) {
	dir := t.TempDir()
	if err := ApplyPendingRestore(dir); err != nil {
		t.Fatalf("no-op restore failed: %v", err)
	}
}
