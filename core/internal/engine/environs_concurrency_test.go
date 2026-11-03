package engine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orbsleep/internal/model"
)

// fakeEngineServer starts an HTTP server on a fresh unix socket that answers
// the probes enableEnvironment performs (dockerx.New pings /version,
// runtime.Probe reads the full /version payload). This lets the port
// allocation logic be tested without a real container engine.
func fakeEngineServer(t *testing.T) string {
	t.Helper()
	// macOS unix sockets cap the path at ~104 bytes: use a short /tmp dir
	// instead of the long test temp root.
	dir, err := os.MkdirTemp("/tmp", "orb-env-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.54","Components":[{"Name":"Docker Engine","Version":"27.0.0"}]}`))
	})
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return "unix://" + sock
}

// TestConcurrentEnableAllocatesDisjointRanges pins the P1-06 concurrency
// rule: two simultaneous environment enables must serialise behind writeMu
// and allocate disjoint proxy port segments (plus distinct admin/web ports).
func TestConcurrentEnableAllocatesDisjointRanges(t *testing.T) {
	e := newBareEngine(t)
	sockA := fakeEngineServer(t)
	sockB := fakeEngineServer(t)

	idA, idB := "env-aaa000000001", "env-bbb000000002"
	for _, pair := range []struct{ id, sock string }{{idA, sockA}, {idB, sockB}} {
		if err := e.store.UpsertEnvironment(model.Environment{
			ID:       pair.id,
			Name:     "Fake " + pair.id,
			Kind:     "docker-engine",
			Endpoint: pair.sock,
			Enabled:  false,
			Status:   "online",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	for i, id := range []string{idA, idB} {
		go func(i int, id string) {
			defer wg.Done()
			<-start // release both as simultaneously as possible
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, errs[i] = e.EnvironmentAction(ctx, id, "enable")
		}(i, id)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("enable %d failed: %v", i, err)
		}
	}

	envs, err := e.store.ListEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	var ranges [2][2]int
	var admins [2]int
	seen := 0
	for _, env := range envs {
		if env.ID != idA && env.ID != idB {
			continue
		}
		idx := 0
		if env.ID == idB {
			idx = 1
		}
		if !env.Enabled {
			t.Fatalf("env %s must be enabled", env.ID)
		}
		if env.ProxyPortStart <= 0 || env.ProxyPortEnd <= env.ProxyPortStart {
			t.Fatalf("env %s has no port segment: %+v", env.ID, env)
		}
		ranges[idx] = [2]int{env.ProxyPortStart, env.ProxyPortEnd}
		admins[idx] = env.AdminPort
		seen++
	}
	if seen != 2 {
		t.Fatalf("expected both envs enabled, got %d", seen)
	}
	overlap := ranges[0][1] >= ranges[1][0] && ranges[0][0] <= ranges[1][1]
	if overlap {
		t.Fatalf("port segments must be disjoint: A=%v B=%v", ranges[0], ranges[1])
	}
	if admins[0] == admins[1] {
		t.Fatalf("admin ports must differ: %d", admins[0])
	}
	// The global settings range must never be handed to an additional env.
	s := e.settings.Get()
	for _, r := range ranges {
		if r[0] >= s.ProxyPortStart && r[1] <= s.ProxyPortEnd {
			t.Fatalf("additional env received the primary range: %v", r)
		}
	}
}
