package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportRecoveryArchive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.sqlite"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backups", "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := exportRecoveryArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = true
		if h.Name == "state.sqlite" {
			raw, _ := io.ReadAll(tr)
			if string(raw) != "db" {
				t.Fatalf("state.sqlite content = %q", raw)
			}
		}
	}
	for _, want := range []string{"state.sqlite", "backups/compose.yml"} {
		if !found[want] {
			t.Fatalf("archive missing %s: %v", want, found)
		}
	}
	if strings.Contains(strings.Join(keys(found), " "), "recovery-export-") {
		t.Fatal("archive must not contain itself")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
