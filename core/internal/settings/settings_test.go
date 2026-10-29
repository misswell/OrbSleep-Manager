package settings

import (
	"path/filepath"
	"testing"

	"orbsleep/internal/state"
)

func TestDefaultsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, dir)
	s := m.Get()
	if s.IdleTimeoutSecs != 1800 || s.ProxyPortStart != 20000 || s.ProxyDialMode != "published" {
		t.Fatalf("defaults = %+v", s)
	}
	s.AutoScanOnLaunch = true
	s.IdleTimeoutSecs = 60
	s.ExcludeImagePatterns = []string{"redis"}
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}
	got := m.Get()
	if !got.AutoScanOnLaunch || got.IdleTimeoutSecs != 60 || len(got.ExcludeImagePatterns) != 1 {
		t.Fatalf("got = %+v", got)
	}
	// Turning the boolean back off must persist.
	got.AutoScanOnLaunch = false
	if err := m.Save(got); err != nil {
		t.Fatal(err)
	}
	if m.Get().AutoScanOnLaunch {
		t.Fatal("bool reset was not persisted")
	}
}

func TestInvalidPortRange(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, dir)
	s := m.Get()
	s.ProxyPortEnd = s.ProxyPortStart
	if err := m.Save(s); err == nil {
		t.Fatal("expected invalid range error")
	}
	s = m.Get()
	s.ProxyPortStart = 1000
	s.ProxyPortEnd = 40000
	if err := m.Save(s); err == nil {
		t.Fatal("expected oversized range error")
	}
}
