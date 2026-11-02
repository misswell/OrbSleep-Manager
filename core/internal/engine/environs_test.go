package engine

import (
	"path/filepath"
	"testing"

	"orbsleep/internal/model"
)

func newBareEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func mkEnv(id, endpoint string) model.Environment {
	return model.Environment{
		ID: id, Name: "Test " + id, Kind: "docker-engine", Endpoint: endpoint,
		Enabled: true, Status: "online", AddedAt: "2026-01-01T00:00:00Z",
		ProxyPortStart: 31000, ProxyPortEnd: 31099, AdminPort: 31001, WebPort: 31002,
	}
}

var _ = filepath.Join

// TestProxySpecPrimaryKeepsLegacyIdentity pins the back-compat guarantee: the
// primary environment keeps the historical proxy container name, project and
// compose path so existing installs are untouched by the multi-env upgrade.
func TestProxySpecPrimaryKeepsLegacyIdentity(t *testing.T) {
	e := newBareEngine(t)
	spec := e.proxySpecForEnv(e.currentEnvID())
	if spec.Primary != true {
		t.Fatal("current env must be primary")
	}
	if spec.ContainerName != "orbsleep-lazy-tcp-proxy" {
		t.Fatalf("primary container name = %s", spec.ContainerName)
	}
	if spec.Project != "orbsleep-proxy" {
		t.Fatalf("primary project = %s", spec.Project)
	}
	s := e.settings.Get()
	if spec.ComposePath != e.proxyComposePath() || spec.Admin != s.AdminPort || spec.Start != s.ProxyPortStart {
		t.Fatalf("primary spec must mirror global settings: %+v", spec)
	}
}

// TestProxySpecSecondaryDistinct verifies suffixed identity for additional
// environments so two proxies never collide.
func TestProxySpecSecondaryDistinct(t *testing.T) {
	e := newBareEngine(t)
	envID := "env-abcdef123456"
	if err := e.store.UpsertEnvironment(mkEnv(envID, "unix:///tmp/other-docker.sock")); err != nil {
		t.Fatal(err)
	}
	spec := e.proxySpecForEnv(envID)
	if spec.Primary {
		t.Fatal("non-current env must not be primary")
	}
	if spec.ContainerName == "orbsleep-lazy-tcp-proxy" || spec.Project == "orbsleep-proxy" {
		t.Fatalf("secondary identity must be suffixed: %+v", spec)
	}
	if spec.ComposePath == e.proxyComposePath() {
		t.Fatal("secondary compose path must live in its own directory")
	}
}
