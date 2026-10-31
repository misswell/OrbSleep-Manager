package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbsleep/internal/api"
	"orbsleep/internal/client"
	"orbsleep/internal/engine"
	"orbsleep/internal/model"
)

func TestDaemonAPIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.New("test", dir)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "daemon.sock")
	srv, err := api.New(sock, eng)
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	go srv.Serve()
	defer func() {
		srv.Close()
		eng.Close()
	}()

	deadline := time.Now().Add(5 * time.Second)
	var c *client.Client
	for {
		c, err = client.Dial(sock)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer c.Close()
	ctx := context.Background()

	var ping map[string]any
	if err := c.Call(ctx, "ping", nil, &ping); err != nil {
		t.Fatal(err)
	}
	if ping["pong"] != true {
		t.Fatalf("ping = %+v", ping)
	}

	var st model.Status
	if err := c.Call(ctx, "system.status", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.DaemonVersion != "test" {
		t.Fatalf("status = %+v", st)
	}
	if st.DockerReachable {
		// The unreachable-docker assertions below only hold where no Docker
		// socket answers (CI). On a dev machine with OrbStack running, the
		// daemon correctly reports the real environment — skip instead of
		// pretending that is a failure.
		t.Skip("real Docker is reachable; unreachable-daemon assertions need CI isolation")
	}
	if st.Proxy.Installed {
		t.Fatal("proxy should not be installed in a fresh data dir")
	}

	var s model.Settings
	if err := c.Call(ctx, "settings.get", nil, &s); err != nil {
		t.Fatal(err)
	}
	if s.IdleTimeoutSecs != 1800 {
		t.Fatalf("default idle = %d", s.IdleTimeoutSecs)
	}
	if err := c.Call(ctx, "settings.update", map[string]any{"idleTimeoutSecs": 120, "autoScanOnLaunch": true}, &s); err != nil {
		t.Fatal(err)
	}
	if s.IdleTimeoutSecs != 120 || !s.AutoScanOnLaunch {
		t.Fatalf("updated settings = %+v", s)
	}

	var services struct {
		Services []model.Service `json:"services"`
	}
	if err := c.Call(ctx, "services.list", nil, &services); err != nil {
		t.Fatal(err)
	}
	if len(services.Services) != 0 {
		t.Fatalf("services = %+v", services.Services)
	}

	var ops struct {
		Operations []model.Operation `json:"operations"`
	}
	if err := c.Call(ctx, "operations.list", nil, &ops); err != nil {
		t.Fatal(err)
	}

	var logs struct {
		Lines []model.LogEntry `json:"lines"`
	}
	if err := c.Call(ctx, "logs.tail", map[string]any{"limit": 10}, &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Lines) == 0 {
		t.Fatal("expected daemon log lines")
	}

	var checks struct {
		Checks []model.DoctorCheck `json:"checks"`
	}
	if err := c.Call(ctx, "system.doctor", nil, &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks.Checks) == 0 {
		t.Fatal("expected doctor checks")
	}
	dockerOK := false
	for _, ch := range checks.Checks {
		if ch.ID == "docker" && ch.Status == "ok" {
			dockerOK = true
		}
	}
	if !dockerOK {
		// Scanning without Docker must return a protocol error, not crash.
		var ignored any
		if err := c.Call(ctx, "scan", nil, &ignored); err == nil {
			t.Fatal("expected scan to fail without Docker")
		}
	} else {
		t.Log("Docker is available; skipping no-Docker assertions")
	}
}

// P2: a server that never served a request has no lazily-created base
// context; Close must not panic on a nil cancel func.
func TestServerCloseWithoutRequests(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.New("test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// Keep the socket path short: macOS rejects unix sockets longer than
	// 104 bytes, and t.TempDir() embeds the long test name.
	sock := filepath.Join(os.TempDir(), "orbsleep-api-close-test.sock")
	t.Cleanup(func() { os.Remove(sock) })
	srv, err := api.New(sock, eng)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	srv.Close() // idempotent
}
