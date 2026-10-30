package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
)

// --- fake Docker API over a Unix socket ---

type fakeDocker struct {
	mu        sync.Mutex
	socket    string
	server    *http.Server
	published map[int]bool
	ports     []model.PortBinding
	name      string
	id        string
}

func newFakeDocker(t *testing.T, name, id string, ports []model.PortBinding) *fakeDocker {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "orbsleep-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	f := &fakeDocker{socket: filepath.Join(sockDir, "d.sock"), published: map[int]bool{}, ports: ports, name: name, id: id}
	for _, p := range ports {
		f.published[p.HostPort] = true
	}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Version": "fake", "ApiVersion": "1.47"})
	})
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/containers/")
		path = strings.TrimSuffix(path, "/json")
		if path != f.id && path != f.name {
			http.NotFound(w, r)
			return
		}
		ins := &dockerx.ContainerInspect{ID: f.id, Name: "/" + f.name}
		ins.State.Status = "running"
		ins.State.Running = true
		ins.Config.Image = "nginx:alpine"
		ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{}
		for _, p := range f.ports {
			if f.published[p.HostPort] {
				key := fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol)
				ins.HostConfig.PortBindings[key] = append(ins.HostConfig.PortBindings[key], dockerx.PortBinding{
					HostIP: p.HostIP, HostPort: fmt.Sprintf("%d", p.HostPort),
				})
			}
		}
		json.NewEncoder(w).Encode(ins)
	})
	f.server = &http.Server{Handler: mux}
	go f.server.Serve(ln)
	t.Cleanup(func() { f.server.Close() })
	return f
}

func (f *fakeDocker) setPorts(ports []model.PortBinding, published bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ports = ports
	f.published = map[int]bool{}
	for _, p := range ports {
		f.published[p.HostPort] = published
	}
}

func (f *fakeDocker) setPublished(published bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.ports {
		f.published[p.HostPort] = published
	}
}

func (f *fakeDocker) client(t *testing.T) *dockerx.Client {
	t.Helper()
	c, err := dockerx.New(context.Background(), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// --- fake docker CLI ---

type fakeCLI struct {
	configFn    func(file string) (map[string]any, error)
	docker      *fakeDocker
	upErr       error
	proxyUpErr  error
	upFn        func(file string) error
	upCalls     int
	lastWorkdir string
}

func (f *fakeCLI) ComposeConfig(ctx context.Context, project, file, workdir string) (map[string]any, error) {
	return f.configFn(file)
}
func (f *fakeCLI) UpService(ctx context.Context, project, file, workdir, service string) error {
	f.upCalls++
	f.lastWorkdir = workdir
	if f.upErr != nil {
		return f.upErr
	}
	if f.upFn != nil {
		if err := f.upFn(file); err != nil {
			return err
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	// Publish again when the restored file contains the mapping.
	published := false
	for _, p := range f.docker.ports {
		if strings.Contains(string(raw), fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort)) ||
			strings.Contains(string(raw), fmt.Sprintf("%s:%d:%d", p.HostIP, p.HostPort, p.ContainerPort)) {
			published = true
		}
	}
	f.docker.setPublished(published)
	return nil
}
func (f *fakeCLI) ProxyUp(ctx context.Context, file string) error   { return f.proxyUpErr }
func (f *fakeCLI) ProxyDown(ctx context.Context, file string) error { return nil }
func (f *fakeCLI) Version(ctx context.Context) (string, error)      { return "fake", nil }

func composeConfigFor(ports []model.PortBinding) func(string) (map[string]any, error) {
	return func(file string) (map[string]any, error) {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		content := string(raw)
		var entries []any
		for _, p := range ports {
			short := fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort)
			ipShort := fmt.Sprintf("%s:%d:%d", p.HostIP, p.HostPort, p.ContainerPort)
			if strings.Contains(content, short) || strings.Contains(content, ipShort) {
				entry := map[string]any{"target": p.ContainerPort, "published": fmt.Sprintf("%d", p.HostPort), "protocol": p.Protocol}
				if p.HostIP != "" {
					entry["host_ip"] = p.HostIP
				}
				entries = append(entries, entry)
			}
		}
		svc := map[string]any{"image": "nginx:alpine"}
		if len(entries) > 0 {
			svc["ports"] = entries
		}
		return map[string]any{"services": map[string]any{"web": svc}}, nil
	}
}

// --- fake proxy admin API ---

type fakeAdmin struct {
	mu     sync.Mutex
	cfg    proxy.Config
	gets   int
	puts   int
	getFn  func(call int) error
	putFn  func(call int) error
	server *httptest.Server
}

type portHolder struct {
	mu sync.Mutex
	ln net.Listener
}

func (h *portHolder) set(ln net.Listener) { h.mu.Lock(); h.ln = ln; h.mu.Unlock() }
func (h *portHolder) close() {
	h.mu.Lock()
	if h.ln != nil {
		_ = h.ln.Close()
	}
	h.mu.Unlock()
}

func (a *fakeAdmin) port() int {
	u, err := url.Parse(a.server.URL)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

func newFakeAdmin(t *testing.T) *fakeAdmin {
	t.Helper()
	a := &fakeAdmin{}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config":
			a.gets++
			if a.getFn != nil {
				if err := a.getFn(a.gets); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			json.NewEncoder(w).Encode(a.cfg)
		case r.Method == http.MethodPut && r.URL.Path == "/config/update":
			a.puts++
			if a.putFn != nil {
				if err := a.putFn(a.puts); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			var body struct {
				Services []proxy.ServiceConfig `json:"services"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a.cfg = proxy.Config{Services: body.Services}
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "services_loaded": len(body.Services)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *fakeAdmin) hostPort(t *testing.T) string {
	u, err := url.Parse(a.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func (a *fakeAdmin) config() proxy.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := proxy.Config{Services: append([]proxy.ServiceConfig(nil), a.cfg.Services...)}
	return cp
}

// --- test harness ---

func testEngine(t *testing.T, admin *fakeAdmin, docker *fakeDocker, cli composeCLI) (*Engine, *opLogger) {
	t.Helper()
	e, err := New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	s := e.settings.Get()
	if admin != nil {
		u, _ := url.Parse(admin.server.URL)
		host, portStr, _ := net.SplitHostPort(u.Host)
		_ = host
		var port int
		fmt.Sscanf(portStr, "%d", &port)
		s.AdminPort = port
	}
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	e.proxyKey = "test-key"
	if docker != nil {
		e.docker = docker.client(t)
	}
	if cli != nil {
		e.cli = cli
	}
	op := model.Operation{ID: "op-test", BackupDir: filepath.Join(t.TempDir(), "backup"), Status: "running"}
	logger := e.newOpLogger(op)
	return e, logger
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func makeSnapshot(t *testing.T, hostPort int, bindings []model.Binding) *planSnapshot {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	content := fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n", hostPort)
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &planSnapshot{
		containerID:    "abc123",
		containerName:  "demo-web-1",
		composeProject: "demo",
		composeService: "web",
		composeFile:    file,
		workingDir:     dir,
		content:        []byte(content),
		removals:       []model.PortBinding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, Protocol: "tcp"}},
		proxyMappings:  []model.ProxyMapping{{ListenPort: bindings[0].ProxyPort, TargetPort: 80}},
		bindings:       bindings,
	}
}

func TestAttachRestoresProxyWhenVerificationReadFails(t *testing.T) {
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	admin.getFn = func(call int) error {
		if call == 2 {
			return errors.New("simulated read timeout")
		}
		return nil
	}
	docker := newFakeDocker(t, "demo-web-1", "abc123", []model.PortBinding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, Protocol: "tcp"}})
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(snap.removals), docker: docker})

	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")
	_, err := e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, "test-txn")
	if err == nil {
		t.Fatal("expected attach to fail")
	}
	var ae *attachError
	if !errors.As(err, &ae) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if len(admin.config().Services) != 0 {
		t.Fatalf("proxy config was not restored: %+v", admin.config())
	}
	got, _ := os.ReadFile(snap.composeFile)
	if string(got) != string(snap.content) {
		t.Fatalf("compose file was not restored:\n%s", got)
	}
	services, _ := e.store.ListServices()
	if len(services) != 0 {
		t.Fatalf("unexpected managed service: %+v", services)
	}
	if ln, lerr := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort)); lerr != nil {
		t.Fatalf("gateway listener leaked on %d: %v", hostPort, lerr)
	} else {
		ln.Close()
	}
}

func TestAttachCleansUpPartialGatewayListeners(t *testing.T) {
	hostPort1 := freePort(t)
	hostPort2 := freePort(t)
	// Occupy the second host port so its gateway listener fails.
	blocker, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort2))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", []model.PortBinding{
		{HostIP: "127.0.0.1", HostPort: hostPort1, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: hostPort2, ContainerPort: 80, Protocol: "tcp"},
	})
	bindings := []model.Binding{
		{HostIP: "127.0.0.1", HostPort: hostPort1, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: hostPort2, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"},
	}
	snap := makeSnapshot(t, hostPort1, bindings)
	// The compose file only declares the first mapping in this scenario; add the second.
	content := string(snap.content) + fmt.Sprintf("      - \"127.0.0.1:%d:80\"\n", hostPort2)
	if err := os.WriteFile(snap.composeFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	snap.content = []byte(content)
	snap.removals = append(snap.removals, model.PortBinding{HostIP: "127.0.0.1", HostPort: hostPort2, ContainerPort: 80, Protocol: "tcp"})
	snap.proxyMappings = []model.ProxyMapping{{ListenPort: bindings[0].ProxyPort, TargetPort: 80}, {ListenPort: bindings[1].ProxyPort, TargetPort: 80}}

	cli := &fakeCLI{configFn: composeConfigFor(snap.removals), docker: docker}
	e, logger := testEngine(t, admin, docker, cli)
	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")

	_, err = e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, "test-txn")
	if err == nil {
		t.Fatal("expected attach to fail on the occupied gateway port")
	}
	if len(admin.config().Services) != 0 {
		t.Fatalf("proxy config was not restored: %+v", admin.config())
	}
	got, _ := os.ReadFile(snap.composeFile)
	if string(got) != string(snap.content) {
		t.Fatalf("compose file was not restored:\n%s", got)
	}
	if services, _ := e.store.ListServices(); len(services) != 0 {
		t.Fatalf("unexpected managed service: %+v", services)
	}
	if ln, lerr := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort1)); lerr != nil {
		t.Fatalf("first gateway listener leaked on %d: %v", hostPort1, lerr)
	} else {
		ln.Close()
	}
}

func TestMain(m *testing.M) {
	bindingProbeTimeout = 300 * time.Millisecond
	os.Exit(m.Run())
}

func TestRollbackKeepsRecordWhenContainerCannotBeRestored(t *testing.T) {
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	// Managed content = file with the port removed.
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	if err := os.WriteFile(snap.composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(backup, snap.content, 0o644); err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker(t, "demo-web-1", "abc123", snap.removals)
	cli := &fakeCLI{configFn: composeConfigFor(snap.removals), docker: docker, upErr: errors.New("simulated recreate failure")}
	e, logger := testEngine(t, admin, docker, cli)
	docker.setPorts(snap.removals, false)
	svc := model.Service{ID: "svc-test", ContainerID: snap.containerID, ContainerName: snap.containerName,
		ComposeProject: snap.composeProject, ComposeService: snap.composeService, ComposeFile: snap.composeFile,
		Status: "managed_running", Bindings: bindings, StartTimeoutSecs: 1}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	ai := appliedItem{snap: snap, serviceID: svc.ID, backupPath: backup, managedContent: managed}
	err := e.rollbackApplied(logger, []appliedItem{ai}, proxy.Config{})
	if err == nil {
		t.Fatal("expected rollback to report failure")
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must not be deleted when the container cannot be restored")
	}
}

func TestRunAttachAbortsWithoutProxyBaseline(t *testing.T) {
	admin := newFakeAdmin(t)
	e, _ := testEngine(t, admin, nil, nil)
	// Point the admin port at a closed port by replacing the settings.
	s := e.settings.Get()
	s.AdminPort = freePort(t) // nothing listening
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	plan := model.Plan{ID: "plan-test", Status: "ready"}
	op := model.Operation{ID: "op-baseline", BackupDir: t.TempDir(), Status: "running"}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	e.runAttach(context.Background(), plan, nil, op, func() {})
	got, ok, err := e.store.GetOperation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "failed" || !strings.Contains(got.Error, "未做任何修改") {
		t.Fatalf("unexpected operation: %+v", got)
	}
	if services, _ := e.store.ListServices(); len(services) != 0 {
		t.Fatal("unexpected service record")
	}
}

func TestBackupPrunable(t *testing.T) {
	ok := model.Operation{Status: "succeeded", FinishedAt: "now"}
	if !backupPrunable(ok) {
		t.Fatal("succeeded operation backup should be prunable")
	}
	for _, status := range []string{"failed", "rollback_failed", "running"} {
		if backupPrunable(model.Operation{Status: status, FinishedAt: "now"}) {
			t.Fatalf("%s backup must be retained", status)
		}
	}
	if backupPrunable(model.Operation{Status: "succeeded"}) {
		t.Fatal("unfinished operation backup must be retained")
	}
}

func TestProxyOwnedHostPorts(t *testing.T) {
	ins := &dockerx.ContainerInspect{}
	ins.NetworkSettings.Ports = map[string][]dockerx.PortBinding{
		"20000/tcp": {{HostPort: "20000"}},
		"20001/tcp": {{HostPort: "20001"}},
	}
	ins.HostConfig.PortBindings = map[string][]dockerx.PortBinding{
		"20002/tcp": {{HostPort: "20002"}},
		"20003/tcp": {{HostPort: "20003-20005"}},
	}
	owned := proxyOwnedHostPorts(ins)
	for _, p := range []int{20000, 20001, 20002, 20003, 20004, 20005} {
		if !owned[p] {
			t.Fatalf("port %d not marked as owned: %+v", p, owned)
		}
	}
	if owned[20006] {
		t.Fatalf("unexpected owned port 20006")
	}
}

func TestChooseManagedPortAvoidsReservedAndOccupied(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	reserved := map[int]bool{occupied + 1: true}
	chosen, err := chooseManagedPort(occupied, nil, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if chosen == occupied || chosen == occupied+1 {
		t.Fatalf("chose reserved/occupied port %d", chosen)
	}
}

func TestValidateProxyConfig(t *testing.T) {
	bad := []proxy.Config{
		{Services: []proxy.ServiceConfig{{Name: "", Ports: []string{"9000:80"}}}},
		{Services: []proxy.ServiceConfig{{Name: "a", Ports: nil}}},
		{Services: []proxy.ServiceConfig{{Name: "a", Ports: []string{"abc"}}}},
		{Services: []proxy.ServiceConfig{{Name: "a", Ports: []string{"9000:80"}}, {Name: "a", Ports: []string{"9001:80"}}}},
	}
	for i, cfg := range bad {
		if err := validateProxyConfig(cfg); err == nil {
			t.Fatalf("case %d should be invalid", i)
		}
	}
	good := proxy.Config{Services: []proxy.ServiceConfig{{Name: "a", Ports: []string{"9000:80"}}}}
	if err := validateProxyConfig(good); err != nil {
		t.Fatal(err)
	}
}

func managedServiceFixture(t *testing.T, e *Engine, admin *fakeAdmin, docker *fakeDocker, cli composeCLI) (model.Service, *planSnapshot, []byte, []byte) {
	t.Helper()
	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	original := append([]byte(nil), snap.content...)
	managed := []byte("services:\n  web:\n    image: nginx:alpine\n")
	if err := os.WriteFile(snap.composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "docker-compose.yml")
	if err := os.WriteFile(backupPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	metaRaw, _ := json.Marshal(map[string]any{"composeFile": snap.composeFile, "workingDir": snap.workingDir})
	if err := os.WriteFile(filepath.Join(backupDir, "metadata.json"), metaRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	docker.setPorts(snap.removals, false)
	svc := model.Service{ID: "svc-test", ContainerID: snap.containerID, ContainerName: snap.containerName,
		ComposeProject: snap.composeProject, ComposeService: snap.composeService, ComposeFile: snap.composeFile,
		Status: "managed_running", Bindings: bindings, StartTimeoutSecs: 1}
	if err := e.store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetMeta("serviceBackup:"+svc.ID, backupDir); err != nil {
		t.Fatal(err)
	}
	// Baseline proxy config and a listener for the managed route.
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: svc.ContainerName, Ports: []string{fmt.Sprintf("%d:80", bindings[0].ProxyPort)}, IdleTimeoutSecs: 1800, StartTimeoutSecs: 60}}}
	admin.mu.Unlock()
	route, err := e.gatewayRoute(context.Background(), svc.ID, svc.ContainerName, bindings[0], e.settings.Get())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.gw.Ensure(route); err != nil {
		t.Fatal(err)
	}
	return svc, snap, original, managed
}

func TestRestoreAbortsWhenProxyRemovalFails(t *testing.T) {
	hostPort := freePort(t)
	_ = hostPort
	admin := newFakeAdmin(t)
	admin.putFn = func(call int) error { return errors.New("simulated admin write failure") }
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)

	err := e.restoreService(context.Background(), logger, svc)
	if err == nil {
		t.Fatal("expected restore to abort when the proxy target cannot be removed")
	}
	got, _ := os.ReadFile(snap.composeFile)
	if string(got) != string(managed) {
		t.Fatalf("compose file must stay managed:\n%s", got)
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept")
	}
	if keys := e.gw.RouteKeys(); len(keys) != 1 {
		t.Fatalf("gateway listener must stay installed, keys=%v", keys)
	}
	if len(admin.config().Services) != 1 {
		t.Fatal("proxy target must be kept")
	}
}

func TestRestoreConnectivityFailureRevertsManaged(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)

	// No real container listens on the restored host port, so the connectivity
	// check must fail and the service must be put back into managed state.
	err := e.restoreService(context.Background(), logger, svc)
	if err == nil {
		t.Fatal("expected restore to fail its connectivity check")
	}
	var ae *attachError
	if !errors.As(err, &ae) {
		t.Fatalf("expected attachError, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "连通性测试失败") {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(snap.composeFile)
	if string(got) != string(managed) {
		t.Fatalf("compose file was not reverted to managed state:\n%s", got)
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept after a failed restore")
	}
	if len(admin.config().Services) != 1 {
		t.Fatal("proxy target must be restored")
	}
	if keys := e.gw.RouteKeys(); len(keys) != 1 {
		t.Fatalf("gateway listener must be restored, keys=%v", keys)
	}
}

func TestRestoreFailurePreservesUnrelatedProxyServices(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, _, _, _ := managedServiceFixture(t, e, admin, docker, e.cli)
	proxyPort := svc.Bindings[0].ProxyPort
	baseline := proxy.Config{Services: []proxy.ServiceConfig{
		{Name: "service-A", Ports: []string{"21001:80"}, IdleTimeoutSecs: 1800, StartTimeoutSecs: 60},
		{Name: svc.ContainerName, Ports: []string{fmt.Sprintf("%d:80", proxyPort)}, IdleTimeoutSecs: 1800, StartTimeoutSecs: 60},
		{Name: "service-C", Ports: []string{"21003:80"}, IdleTimeoutSecs: 1800, StartTimeoutSecs: 60},
	}}
	admin.mu.Lock()
	admin.cfg = baseline.Clone()
	admin.mu.Unlock()

	// The restored port has no listener, so restore fails and reverts.
	if err := e.restoreService(context.Background(), logger, svc); err == nil {
		t.Fatal("expected restore to fail connectivity")
	}
	got := admin.config()
	if !proxyConfigEqual(got, baseline) {
		t.Fatalf("proxy snapshot corrupted:\n got=%+v\nwant=%+v", got.Services, baseline.Services)
	}
	for _, name := range []string{"service-A", "service-C"} {
		if _, ok := got.Service(name); !ok {
			t.Fatalf("lost service %s", name)
		}
	}
}

func TestProxySetupWriteFailureRestoresAbsentCompose(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker, proxyUpErr: errors.New("simulated proxy start failure")}
	e, _ := testEngine(t, admin, docker, cli)
	op := model.Operation{ID: "op-proxy-a", BackupDir: filepath.Join(t.TempDir(), "backup"), Status: "running"}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	e.runProxySetup(context.Background(), op, func() {})
	got, ok, err := e.store.GetOperation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	if _, err := os.Stat(e.proxyComposePath()); !os.IsNotExist(err) {
		t.Fatalf("new compose file was not removed: %v", err)
	}
}

func TestProxySetupRollbackFailureIsReported(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker, proxyUpErr: errors.New("simulated proxy start failure")}
	e, _ := testEngine(t, admin, docker, cli)
	composePath := e.proxyComposePath()
	if err := os.MkdirAll(filepath.Dir(composePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, []byte("old-compose-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := model.Operation{ID: "op-proxy-b", BackupDir: filepath.Join(t.TempDir(), "backup"), Status: "running"}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	e.runProxySetup(context.Background(), op, func() {})
	got, ok, err := e.store.GetOperation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "rollback_failed" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	raw, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "old-compose-content\n" {
		t.Fatalf("old compose not restored:\n%s", raw)
	}
}

func TestRestoreRevertsWhenDeletingManagedRecordFails(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	hostPort := svc.Bindings[0].HostPort

	// Once the gateway releases the port, occupy it with a real listener so the
	// connectivity probe succeeds. The delete hook closes it just before the
	// revert, allowing the gateway to bind again.
	holder := &portHolder{}
	holderReady := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
			if err == nil {
				holder.set(ln)
				close(holderReady)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(holderReady)
	}()

	originalDelete := deleteServiceRecord
	deleteServiceRecord = func(e *Engine, id string) error {
		holder.close()
		for i := 0; i < 200; i++ {
			ln, lerr := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
			if lerr == nil {
				_ = ln.Close()
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return errors.New("simulated sqlite failure")
	}
	t.Cleanup(func() {
		deleteServiceRecord = originalDelete
		holder.close()
	})

	err := e.restoreService(context.Background(), logger, svc)
	<-holderReady
	if err == nil {
		t.Fatal("expected restore to fail when the record cannot be deleted")
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("record should remain after a failed delete")
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(managed) {
		t.Fatalf("service was not reverted to managed state:\n%s", raw)
	}
	if len(admin.config().Services) != 1 {
		t.Fatalf("proxy target was not restored: %+v", admin.config().Services)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener was not restored: %v", e.gw.RouteKeys())
	}
}

func TestRollbackDeferredWhenDependencyUnavailable(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	// Point the admin port at a closed port after setup.
	s := e.settings.Get()
	s.AdminPort = freePort(t)
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	hostPort := freePort(t)
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	ai := appliedItem{snap: snap, serviceID: "svc-x", managedContent: []byte("managed")}
	err := e.rollbackApplied(logger, []appliedItem{ai}, proxy.Config{})
	if err == nil {
		t.Fatal("expected deferred rollback error")
	}
	var deferred *rollbackDeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("expected rollbackDeferredError, got %T %v", err, err)
	}
}

func TestRollbackAppliedRevertsWhenRecordDeletionFails(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, snap, _, managed := managedServiceFixture(t, e, admin, docker, e.cli)
	hostPort := svc.Bindings[0].HostPort

	holder := &portHolder{}
	holderReady := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
			if err == nil {
				holder.set(ln)
				close(holderReady)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(holderReady)
	}()
	originalDelete := deleteServiceRecord
	deleteServiceRecord = func(e *Engine, id string) error {
		holder.close()
		for i := 0; i < 200; i++ {
			ln, lerr := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
			if lerr == nil {
				_ = ln.Close()
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return errors.New("simulated sqlite failure")
	}
	t.Cleanup(func() {
		deleteServiceRecord = originalDelete
		holder.close()
	})

	backupDir, _ := e.store.GetMeta("serviceBackup:" + svc.ID)
	ai := appliedItem{
		snap:           snap,
		serviceID:      svc.ID,
		backupPath:     filepath.Join(backupDir, "docker-compose.yml"),
		managedContent: managed,
	}
	err := e.rollbackApplied(logger, []appliedItem{ai}, proxy.Config{})
	<-holderReady
	if err == nil {
		t.Fatal("expected rollback to report incomplete state")
	}
	raw, _ := os.ReadFile(snap.composeFile)
	if string(raw) != string(managed) {
		t.Fatalf("service was not reverted to managed:\n%s", raw)
	}
	if _, ok, _ := e.store.GetService(svc.ID); !ok {
		t.Fatal("managed record must be kept")
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener missing: %v", e.gw.RouteKeys())
	}
	if len(admin.config().Services) != 1 {
		t.Fatal("proxy target must be kept")
	}
}

func TestProxySetupAbortsWhenBackupCannotBeWritten(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	cli := &fakeCLI{configFn: composeConfigFor(nil), docker: docker}
	e, _ := testEngine(t, admin, docker, cli)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := model.Operation{ID: "op-proxy-backup", BackupDir: blocker, Status: "running"}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	e.runProxySetup(context.Background(), op, func() {})
	got, ok, err := e.store.GetOperation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	if _, err := os.Stat(e.proxyComposePath()); !os.IsNotExist(err) {
		t.Fatalf("proxy compose must not be created after backup failure: %v", err)
	}
}

func TestGatewayRouteContainerIPFailsWhenProxyUnavailable(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	s := e.settings.Get()
	s.ProxyDialMode = "container_ip"
	b := model.Binding{HostIP: "127.0.0.1", HostPort: freePort(t), ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}
	if _, err := e.gatewayRoute(context.Background(), "svc-x", "demo-web-1", b, s); err == nil {
		t.Fatal("container_ip without a proxy container must return an error")
	}
}

func TestRestoreMissingSharedComposeRemovesOtherManagedPorts(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	aPort, bPort := freePort(t), freePort(t)
	aProxy, bProxy := freePort(t), freePort(t)
	aBindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, ProxyPort: aProxy, Protocol: "tcp"}}
	bBindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: bPort, ContainerPort: 80, ProxyPort: bProxy, Protocol: "tcp"}}
	udpPort := freePort(t)
	backup := []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n      - \"127.0.0.1:%d:9090/udp\"\n", aPort, bPort, udpPort))
	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "docker-compose.yml"), backup, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"composeFile": composeFile, "workingDir": dir})
	if err := os.WriteFile(filepath.Join(backupDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}

	svc := model.Service{ID: "svc-web", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "shared", ComposeService: "web", ComposeFile: composeFile,
		Status: "managed_running", Bindings: aBindings, StartTimeoutSecs: 1}
	other := model.Service{ID: "svc-api", ContainerID: "def456", ContainerName: "shared-api-1",
		ComposeProject: "shared", ComposeService: "api", ComposeFile: composeFile,
		Status: "managed_running", Bindings: bBindings, StartTimeoutSecs: 1}
	for _, s := range []model.Service{svc, other} {
		if err := e.store.UpsertService(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.SetMeta("serviceBackup:svc-web", backupDir); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: svc.ContainerName, Ports: []string{fmt.Sprintf("%d:80", aProxy)}}}}
	admin.mu.Unlock()
	docker.setPorts([]model.PortBinding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, Protocol: "tcp"}}, false)

	holder := &portHolder{}
	holderReady := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", aPort))
			if err == nil {
				holder.set(ln)
				close(holderReady)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(holderReady)
	}()
	t.Cleanup(holder.close)

	if err := e.restoreService(context.Background(), logger, svc); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	<-holderReady
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fmt.Sprintf("%d:80", aPort)) {
		t.Fatalf("this service's port was not restored:\n%s", raw)
	}
	if strings.Contains(string(raw), fmt.Sprintf("%d:80", bPort)) {
		t.Fatalf("other managed service's TCP port was reintroduced:\n%s", raw)
	}
	if !strings.Contains(string(raw), fmt.Sprintf("%d:9090/udp", udpPort)) {
		t.Fatalf("other managed service's UDP port was dropped:\n%s", raw)
	}
	if _, ok, _ := e.store.GetService(svc.ID); ok {
		t.Fatal("restored service record must be deleted")
	}
	if _, ok, _ := e.store.GetService(other.ID); !ok {
		t.Fatal("other managed service record must be kept")
	}
}

func TestAttachRollbackKeepsManagedStateWhenComposeExternallyModified(t *testing.T) {
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	docker.setPorts(snap.removals, true)
	e.cli.(*fakeCLI).configFn = composeConfigFor(snap.removals)
	external := []byte("services:\n  web:\n    image: nginx:alpine\n    # user edit during attach\n")
	// After OrbSleep rewrote the file, an external tool edits it; the
	// subsequent recreate then fails.
	e.cli.(*fakeCLI).upFn = func(file string) error {
		if err := os.WriteFile(file, external, 0o644); err != nil {
			return err
		}
		return errors.New("simulated recreate failure")
	}
	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")
	txnID, err := e.startTxn("op-ext", "attach", txnFromPlan("op-ext", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, txnID)
	if err == nil {
		t.Fatal("expected attach to fail")
	}
	var ae *attachError
	if !errors.As(err, &ae) || !ae.RollbackFailed {
		t.Fatalf("expected rollback_failed attachError, got %v", err)
	}
	if !strings.Contains(ae.Detail, "外部修改") {
		t.Fatalf("unexpected detail: %s", ae.Detail)
	}
	got, _ := os.ReadFile(snap.composeFile)
	if string(got) != string(external) {
		t.Fatalf("user edits were clobbered by the rollback:\n%s", got)
	}
	services, _ := e.store.ListServices()
	if len(services) != 1 || services[0].Status != "warning" {
		t.Fatalf("a managed record must be kept to avoid orphaned listeners: %+v", services)
	}
	if _, ok := e.store.GetMeta("serviceBackup:" + services[0].ID); !ok {
		t.Fatal("stable backup must be kept for manual recovery")
	}
	if len(admin.config().Services) != 1 {
		t.Fatalf("proxy entry must be kept, got %+v", admin.config().Services)
	}
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("gateway listener must be kept, got %v", e.gw.RouteKeys())
	}
}

func TestRunDetachRebuildsMissingCompose(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	aPort, bPort := freePort(t), freePort(t)
	aProxy, bProxy := freePort(t), freePort(t)
	aBindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, ProxyPort: aProxy, Protocol: "tcp"}}
	bBindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: bPort, ContainerPort: 80, ProxyPort: bProxy, Protocol: "tcp"}}
	udpPort := freePort(t)
	backup := []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n      - \"127.0.0.1:%d:9090/udp\"\n", aPort, bPort, udpPort))
	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "docker-compose.yml"), backup, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"composeFile": composeFile, "workingDir": dir})
	if err := os.WriteFile(filepath.Join(backupDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	web := model.Service{ID: "svc-web", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "shared", ComposeService: "web", ComposeFile: composeFile,
		Status: "managed_running", Bindings: aBindings, StartTimeoutSecs: 1}
	api := model.Service{ID: "svc-api", ContainerID: "def456", ContainerName: "shared-api-1",
		ComposeProject: "shared", ComposeService: "api", ComposeFile: composeFile,
		Status: "managed_running", Bindings: bBindings, StartTimeoutSecs: 1}
	for _, s := range []model.Service{web, api} {
		if err := e.store.UpsertService(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.SetMeta("serviceBackup:svc-web", backupDir); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: web.ContainerName, Ports: []string{fmt.Sprintf("%d:80", aProxy)}}}}
	admin.mu.Unlock()
	docker.setPorts([]model.PortBinding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, Protocol: "tcp"}}, false)

	// The entry point under test: the Compose file is already missing when
	// the restore starts. It must rebuild instead of aborting.
	managed := []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n  api:\n    image: nginx:alpine\n"))
	if err := os.WriteFile(composeFile, managed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(composeFile); err != nil {
		t.Fatal(err)
	}
	holder := &portHolder{}
	holderReady := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", aPort))
			if err == nil {
				holder.set(ln)
				close(holderReady)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(holderReady)
	}()
	t.Cleanup(holder.close)

	op := model.Operation{ID: "op-detach-missing", Kind: "restore", Status: "running", BackupDir: t.TempDir()}
	if err := e.store.InsertOperation(op); err != nil {
		t.Fatal(err)
	}
	e.runDetach(context.Background(), web, op, false, func() {})
	got, ok, err := e.store.GetOperation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("restore did not succeed: %s %s", got.Status, got.Error)
	}
	<-holderReady
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fmt.Sprintf("%d:80", aPort)) {
		t.Fatalf("this service's port was not rebuilt:\n%s", raw)
	}
	if strings.Contains(string(raw), fmt.Sprintf("%d:80", bPort)) {
		t.Fatalf("other managed service's TCP port was reintroduced:\n%s", raw)
	}
	if !strings.Contains(string(raw), fmt.Sprintf("%d:9090/udp", udpPort)) {
		t.Fatalf("other managed service's UDP port was dropped:\n%s", raw)
	}
	if _, ok, _ := e.store.GetService(web.ID); ok {
		t.Fatal("restored service record must be deleted")
	}
	if _, ok, _ := e.store.GetService(api.ID); !ok {
		t.Fatal("other managed service record must be kept")
	}
	cfg := admin.config()
	if _, ok := cfg.Service(web.ContainerName); ok {
		t.Fatalf("proxy target must be removed: %+v", cfg.Services)
	}
	if _, ok := cfg.Service(api.ContainerName); !ok {
		t.Fatalf("other managed service's proxy entry must be kept: %+v", cfg.Services)
	}
	txns, err := e.store.ListTransactions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tx := range txns {
		if tx.Kind != "detach" {
			continue
		}
		var p detachPayload
		if err := json.Unmarshal([]byte(tx.Data), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.ManagedContent) == 0 || len(p.ExpectedRestored) == 0 {
			t.Fatalf("detach transaction lacks pre-write snapshots: managed=%d expected=%d", len(p.ManagedContent), len(p.ExpectedRestored))
		}
		found = true
	}
	if !found {
		t.Fatal("detach transaction record missing")
	}
}

func TestRestoreMissingSharedComposeRemovesStalePorts(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yml")
	aPort, bStale, bPort := freePort(t), freePort(t), freePort(t)
	aProxy := freePort(t)
	aBindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, ProxyPort: aProxy, Protocol: "tcp"}}
	// The backup predates api's re-attach on a new port: it still declares
	// the stale bStale mapping instead of the current bPort.
	backup := []byte(fmt.Sprintf("services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n  api:\n    image: nginx:alpine\n    ports:\n      - \"127.0.0.1:%d:80\"\n", aPort, bStale))
	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "docker-compose.yml"), backup, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"composeFile": composeFile, "workingDir": dir})
	if err := os.WriteFile(filepath.Join(backupDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{ID: "svc-web", ContainerID: "abc123", ContainerName: "demo-web-1",
		ComposeProject: "shared", ComposeService: "web", ComposeFile: composeFile,
		Status: "managed_running", Bindings: aBindings, StartTimeoutSecs: 1}
	api := model.Service{ID: "svc-api", ContainerID: "def456", ContainerName: "shared-api-1",
		ComposeProject: "shared", ComposeService: "api", ComposeFile: composeFile,
		Status:           "managed_running",
		Bindings:         []model.Binding{{HostIP: "127.0.0.1", HostPort: bPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}},
		StartTimeoutSecs: 1}
	for _, s := range []model.Service{svc, api} {
		if err := e.store.UpsertService(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.SetMeta("serviceBackup:svc-web", backupDir); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	admin.cfg = proxy.Config{Services: []proxy.ServiceConfig{{Name: svc.ContainerName, Ports: []string{fmt.Sprintf("%d:80", aProxy)}}}}
	admin.mu.Unlock()
	docker.setPorts([]model.PortBinding{{HostIP: "127.0.0.1", HostPort: aPort, ContainerPort: 80, Protocol: "tcp"}}, false)

	holder := &portHolder{}
	holderReady := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", aPort))
			if err == nil {
				holder.set(ln)
				close(holderReady)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(holderReady)
	}()
	t.Cleanup(holder.close)

	if err := e.restoreService(context.Background(), logger, svc); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	<-holderReady
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []int{bStale, bPort} {
		if strings.Contains(string(raw), fmt.Sprintf("%d:80", stale)) {
			t.Fatalf("stale port %d was reintroduced from the backup:\n%s", stale, raw)
		}
	}
	if !strings.Contains(string(raw), fmt.Sprintf("%d:80", aPort)) {
		t.Fatalf("this service's port was not restored:\n%s", raw)
	}
}

func attachExternalEditDelete(t *testing.T, chmod bool) {
	t.Helper()
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	docker.setPorts(snap.removals, true)
	e.cli.(*fakeCLI).configFn = composeConfigFor(snap.removals)
	e.cli.(*fakeCLI).upFn = func(file string) error {
		if chmod {
			if err := os.Chmod(file, 0); err != nil {
				return err
			}
		} else if err := os.Remove(file); err != nil {
			return err
		}
		return errors.New("simulated recreate failure")
	}
	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")
	txnID, err := e.startTxn("op-missing", "attach", txnFromPlan("op-missing", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, txnID)
	if err == nil {
		t.Fatal("expected attach to fail")
	}
	var ae *attachError
	if !errors.As(err, &ae) || !ae.RollbackFailed {
		t.Fatalf("expected rollback_failed, got %v", err)
	}
	if !strings.Contains(ae.Detail, "缺失或无法读取") {
		t.Fatalf("unexpected detail: %s", ae.Detail)
	}
	services, _ := e.store.ListServices()
	if len(services) != 1 || services[0].Status != "warning" {
		t.Fatalf("managed warning record missing: %+v", services)
	}
	if _, ok := e.store.GetMeta("serviceBackup:" + services[0].ID); !ok {
		t.Fatal("backup metadata must be retained")
	}
	if chmod {
		raw, rerr := os.ReadFile(snap.composeFile)
		if rerr == nil {
			t.Fatalf("file must remain unreadable, got %q", raw)
		}
		_ = os.Chmod(snap.composeFile, 0o644)
	} else {
		if _, serr := os.Stat(snap.composeFile); !os.IsNotExist(serr) {
			t.Fatal("deleted Compose file must not be recreated")
		}
	}
}

func TestAttachFailureDoesNotRecreateDeletedCompose(t *testing.T) {
	attachExternalEditDelete(t, false)
}

func TestAttachFailureDoesNotReplaceUnreadableCompose(t *testing.T) {
	attachExternalEditDelete(t, true)
}

func TestAttachExternalEditDoesNotClaimGatewayWhenPortBusy(t *testing.T) {
	hostPort := freePort(t)
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, logger := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	bindings := []model.Binding{{HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: 80, ProxyPort: freePort(t), Protocol: "tcp"}}
	snap := makeSnapshot(t, hostPort, bindings)
	docker.setPorts(snap.removals, true)
	e.cli.(*fakeCLI).configFn = composeConfigFor(snap.removals)
	e.cli.(*fakeCLI).upFn = func(file string) error {
		_ = os.WriteFile(file, []byte("services:\n  web:\n    image: nginx:alpine\n    # user edit\n"), 0o644)
		return errors.New("simulated recreate failure")
	}
	// A real listener simulates the container still publishing the host port.
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	adminClient := proxy.NewAdminClient(admin.hostPort(t), "test-key")
	txnID, err := e.startTxn("op-busy", "attach", txnFromPlan("op-busy", proxy.Config{}, []planSnapshot{*snap}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.attachOne(context.Background(), logger, 0, snap, map[string][]byte{}, adminClient, txnID)
	if err == nil {
		t.Fatal("expected attach to fail")
	}
	var ae *attachError
	if !errors.As(err, &ae) || !ae.RollbackFailed {
		t.Fatalf("expected rollback_failed, got %v", err)
	}
	if !strings.Contains(ae.Detail, "重建网关监听失败") {
		t.Fatalf("busy port must be reported, detail=%s", ae.Detail)
	}
	if len(e.gw.RouteKeys()) != 0 {
		t.Fatalf("no gateway listener may be claimed, got %v", e.gw.RouteKeys())
	}
}

func TestReconcileRemovesStaleContainerIPRoute(t *testing.T) {
	admin := newFakeAdmin(t)
	docker := newFakeDocker(t, "demo-web-1", "abc123", nil)
	e, _ := testEngine(t, admin, docker, &fakeCLI{configFn: composeConfigFor(nil), docker: docker})
	svc, _, _, _ := managedServiceFixture(t, e, admin, docker, e.cli)
	if len(e.gw.RouteKeys()) != 1 {
		t.Fatalf("fixture route missing: %v", e.gw.RouteKeys())
	}
	s := e.settings.Get()
	s.ProxyDialMode = "container_ip"
	if err := e.settings.Save(s); err != nil {
		t.Fatal(err)
	}
	// The fake Docker has no proxy container, so proxyIP is empty and the old
	// route must not survive reconcile.
	e.reconcile()
	if keys := e.gw.RouteKeys(); len(keys) != 0 {
		t.Fatalf("stale container_ip route retained: %v", keys)
	}
	got, ok, err := e.store.GetService(svc.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Status != "warning" || !strings.Contains(got.LastError, "container_ip") {
		t.Fatalf("service status = %s, error = %s", got.Status, got.LastError)
	}
}
