package docrun

import (
	"context"
	"strings"
	"testing"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// fakeEngine is an in-memory Engine recording every mutation, used to prove
// the recreate/rollback sequences never lose the original container. Lookup
// semantics mirror the real engine: a container is found by ID or by its
// CURRENT name (a rename frees the old name immediately), which is what makes
// name-occupancy checks testable.
type fakeEngine struct {
	inspections map[string]*dockerx.ContainerInspect // construction input (deduped by pointer)
	list        []*dockerx.ContainerInspect          // live containers
	built       bool
	created     []*specRecord
	removed     []string
	renames     [][2]string
	started     []string
	stopped     [][2]string
	connected   [][3]string
	failNext    map[string]error
}

func (f *fakeEngine) all() []*dockerx.ContainerInspect {
	if f.built {
		return f.list
	}
	seen := map[*dockerx.ContainerInspect]bool{}
	for _, v := range f.inspections {
		if v != nil && !seen[v] {
			seen[v] = true
			f.list = append(f.list, v)
		}
	}
	f.built = true
	return f.list
}

func (f *fakeEngine) find(key string) *dockerx.ContainerInspect {
	for _, c := range f.all() {
		if c.ID == key || strings.TrimPrefix(c.Name, "/") == key {
			return c
		}
	}
	return nil
}

func (f *fakeEngine) dropByID(id string) {
	for i, c := range f.list {
		if c.ID == id {
			f.list = append(f.list[:i], f.list[i+1:]...)
			return
		}
	}
}

type specRecord struct {
	name string
	spec *runtime.ContainerSpec
}

func (f *fakeEngine) err(op string) error {
	if f.failNext == nil {
		return nil
	}
	if e, ok := f.failNext[op]; ok {
		delete(f.failNext, op)
		return e
	}
	return nil
}

func (f *fakeEngine) InspectContainer(ctx context.Context, id string) (*dockerx.ContainerInspect, error) {
	if err := f.err("inspect"); err != nil {
		return nil, err
	}
	if ins := f.find(id); ins != nil {
		return ins, nil
	}
	return nil, &dockerx.APIError{Status: 404, Body: "no such container"}
}

func (f *fakeEngine) StartContainer(ctx context.Context, id string) error {
	if err := f.err("start"); err != nil {
		return err
	}
	f.started = append(f.started, id)
	return nil
}

func (f *fakeEngine) StopContainer(ctx context.Context, id string, timeoutSecs int) error {
	if err := f.err("stop"); err != nil {
		return err
	}
	f.stopped = append(f.stopped, [2]string{id, "t"})
	return nil
}

func (f *fakeEngine) RemoveContainer(ctx context.Context, id string, force bool) error {
	if err := f.err("remove"); err != nil {
		return err
	}
	f.removed = append(f.removed, id)
	f.dropByID(id)
	return nil
}

func (f *fakeEngine) RenameContainer(ctx context.Context, id, name string) error {
	if err := f.err("rename"); err != nil {
		return err
	}
	f.renames = append(f.renames, [2]string{id, name})
	ins := f.find(id)
	if ins == nil {
		return &dockerx.APIError{Status: 404, Body: "no such container"}
	}
	// Real engines refuse a rename when the TARGET name is already taken.
	if other := f.find(name); other != nil && other.ID != id {
		return &dockerx.APIError{Status: 409, Body: "name already in use"}
	}
	ins.Name = "/" + name
	return nil
}

func (f *fakeEngine) CreateContainer(ctx context.Context, spec *runtime.ContainerSpec) (string, error) {
	if err := f.err("create"); err != nil {
		return "", err
	}
	// Real engines refuse a create when the NAME is already taken.
	if f.find(spec.Name) != nil {
		return "", &dockerx.APIError{Status: 409, Body: "Conflict. The container name is already in use"}
	}
	id := "new-" + spec.Name
	f.created = append(f.created, &specRecord{name: spec.Name, spec: spec})
	// Created containers join the live list so post-create inspects (and the
	// occupancy checks) see the same state a real engine would.
	ci := &dockerx.ContainerInspect{ID: id, Name: "/" + spec.Name}
	ci.Config.Image = spec.Config.Image
	ci.Config.Labels = spec.Config.Labels
	ci.Config.Env = spec.Config.Env
	ci.Config.Entrypoint = spec.Config.Entrypoint
	ci.Config.Cmd = spec.Config.Cmd
	ci.Config.WorkingDir = spec.Config.WorkingDir
	ci.Config.User = spec.Config.User
	ci.Config.Volumes = spec.Config.Volumes
	ci.Config.ExposedPorts = spec.Config.ExposedPorts
	ci.Config.Healthcheck = spec.Config.Healthcheck
	ci.Config.StopSignal = spec.Config.StopSignal
	ci.Config.StopTimeout = spec.Config.StopTimeout
	ci.Config.Tty = spec.Config.Tty
	ci.Config.OpenStdin = spec.Config.OpenStdin
	ci.HostConfig.PortBindings = spec.HostConfig.PortBindings
	ci.HostConfig.Binds = spec.HostConfig.Binds
	ci.HostConfig.NetworkMode = spec.HostConfig.NetworkMode
	ci.HostConfig.RestartPolicy = spec.HostConfig.RestartPolicy
	ci.HostConfig.AutoRemove = spec.HostConfig.AutoRemove
	ci.HostConfig.Privileged = spec.HostConfig.Privileged
	ci.HostConfig.ReadonlyRootfs = spec.HostConfig.ReadonlyRootfs
	ci.HostConfig.CapAdd = spec.HostConfig.CapAdd
	ci.HostConfig.CapDrop = spec.HostConfig.CapDrop
	ci.HostConfig.SecurityOpt = spec.HostConfig.SecurityOpt
	ci.HostConfig.Devices = spec.HostConfig.Devices
	ci.HostConfig.Dns = spec.HostConfig.Dns
	ci.HostConfig.ExtraHosts = spec.HostConfig.ExtraHosts
	ci.HostConfig.GroupAdd = spec.HostConfig.GroupAdd
	ci.HostConfig.IpcMode = spec.HostConfig.IpcMode
	ci.HostConfig.Links = spec.HostConfig.Links
	ci.HostConfig.LogConfig = dockerx.LogConfig{}
	if spec.HostConfig.LogConfig != nil {
		ci.HostConfig.LogConfig = *spec.HostConfig.LogConfig
	}
	ci.HostConfig.Mounts = spec.HostConfig.Mounts
	ci.HostConfig.Memory = spec.HostConfig.Memory
	ci.HostConfig.NanoCpus = spec.HostConfig.NanoCpus
	ci.HostConfig.CPUShares = spec.HostConfig.CPUShares
	ci.HostConfig.CPUQuota = spec.HostConfig.CPUQuota
	ci.HostConfig.CPUPeriod = spec.HostConfig.CPUPeriod
	ci.HostConfig.PidsLimit = spec.HostConfig.PidsLimit
	ci.HostConfig.ShmSize = spec.HostConfig.ShmSize
	ci.HostConfig.Sysctls = spec.HostConfig.Sysctls
	ci.HostConfig.Tmpfs = spec.HostConfig.Tmpfs
	ci.HostConfig.Ulimits = spec.HostConfig.Ulimits
	ci.HostConfig.OomScoreAdj = spec.HostConfig.OomScoreAdj
	ci.HostConfig.Init = spec.HostConfig.Init
	ci.HostConfig.CgroupnsMode = spec.HostConfig.CgroupnsMode
	ci.State.Status = "created"
	f.list = append(f.list, ci)
	return id, nil
}

func (f *fakeEngine) ConnectNetwork(ctx context.Context, network, containerID string, endpoint *runtime.EndpointConfig) error {
	aliases := ""
	if endpoint != nil {
		aliases = strings.Join(endpoint.Aliases, ",")
	}
	f.connected = append(f.connected, [3]string{network, containerID, aliases})
	return nil
}

func TestRecreateManagedHappyPath(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{prep.OriginalID: ins}}
	var phases []string
	created, cleanup, err := RecreateManaged(context.Background(), eng, prep, func(phase string) error {
		phases = append(phases, phase)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created != "new-webapp" {
		t.Fatalf("created = %s", created)
	}
	if len(phases) != 2 || phases[0] != "original_renamed" || phases[1] != "replacement_created" {
		t.Fatalf("phases = %v", phases)
	}
	if len(eng.stopped) != 1 || eng.stopped[0][0] != prep.OriginalID {
		t.Fatalf("stop sequence wrong: %v", eng.stopped)
	}
	if len(eng.renames) != 1 || eng.renames[0][1] != prep.BackupName {
		t.Fatalf("rename sequence wrong: %v", eng.renames)
	}
	if len(eng.connected) != 1 || eng.connected[0][0] != "backnet" {
		t.Fatalf("network reconnect wrong: %v", eng.connected)
	}
	if err := cleanup(true); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 1 || eng.removed[0] != prep.OriginalID {
		t.Fatalf("cleanup removal wrong: %v", eng.removed)
	}
}

func TestRecreateManagedRollsBackOnCreateFailure(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{prep.OriginalID: ins},
		failNext:    map[string]error{"create": &dockerx.APIError{Status: 409, Body: "conflict"}},
	}
	_, _, err = RecreateManaged(context.Background(), eng, prep, nil)
	if err == nil {
		t.Fatal("create failure must surface")
	}
	if len(eng.renames) != 2 || eng.renames[1][1] != prep.OriginalName {
		t.Fatalf("rollback rename wrong: %v", eng.renames)
	}
	if len(eng.started) == 0 || eng.started[len(eng.started)-1] != prep.OriginalID {
		t.Fatalf("rollback start wrong: %v", eng.started)
	}
	for _, c := range eng.created {
		if c.name == prep.OriginalName {
			t.Fatal("replacement must be removed after failed create")
		}
	}
}

func TestRollbackManagedRestoresOriginal(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{prep.OriginalID: ins}}
	created, _, err := RecreateManaged(context.Background(), eng, prep, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RollbackManaged(context.Background(), eng, prep, created); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range eng.removed {
		if id == created {
			found = true
		}
	}
	if !found {
		t.Fatalf("replacement not removed: %v", eng.removed)
	}
	if len(eng.renames) != 2 || eng.renames[1][1] != prep.OriginalName {
		t.Fatalf("rename-back wrong: %v", eng.renames)
	}
}

func TestRestoreOriginalSequence(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	// The managed container reflects the MANAGED spec: takeover port removed,
	// anonymous volume re-attached by name, management labels present.
	managed := loadGolden(t)
	managed.ID = "managed-1"
	delete(managed.HostConfig.PortBindings, "80/tcp")
	managed.HostConfig.Binds = append(managed.HostConfig.Binds,
		"9f8e7d6c5b4a3210fedcba9876543210fedcba9876543210fedcba9876543210:/var/cache/nginx")
	managed.Config.Labels[LabelServiceID] = "svc-1"
	managed.Config.Labels[LabelManaged] = "true"
	// Mirror a modern engine: user aliases land in DNSNames alongside the
	// container's own identities. A container created from the spec carries
	// only the user alias — the original's short-ID alias does not survive.
	wn := managed.NetworkSettings.Networks["webnet"]
	wn.Aliases = []string{"web"}
	wn.DNSNames = []string{"webapp", "web"}
	managed.NetworkSettings.Networks["webnet"] = wn
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{
		"managed-1": managed,
		managed.ID:  managed,
		"webapp":    managed, // VerifyManaged looks the container up by name
	}}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalID:   "managed-1",
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	verified, err := VerifyManaged(context.Background(), eng, snap)
	if err != nil {
		t.Fatalf("managed verification failed: %v", err)
	}
	tampered := loadGolden(t)
	tampered.ID = "managed-1"
	tampered.Config.Env = append(tampered.Config.Env, "INTRUDER=1")
	eng2 := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{"managed-1": tampered, "webapp": tampered}}
	if _, err := VerifyManaged(context.Background(), eng2, snap); err == nil {
		t.Fatal("tampered container must be refused")
	}
	created, rollback, err := RestoreOriginal(context.Background(), eng, snap, verified.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created == "" || rollback == nil {
		t.Fatal("restore must return id and rollback")
	}
	found := false
	for _, r := range eng.renames {
		if r[0] == "managed-1" && strings.HasSuffix(r[1], StaleSuffix) {
			found = true
		}
	}
	if !found {
		t.Fatalf("managed container not parked: %v", eng.renames)
	}
	var origCreated bool
	for _, c := range eng.created {
		if c.name == prep.OriginalName {
			origCreated = true
		}
	}
	if !origCreated {
		t.Fatal("original container not recreated")
	}
	if len(eng.connected) != 1 || eng.connected[0][0] != "backnet" {
		t.Fatalf("restore network reconnect wrong: %v", eng.connected)
	}
}

// TestRestoreOriginalProtectsExternalContainer pins the P0 safety rule: when
// the original name was taken by an EXTERNAL container while the managed one
// was parked, restore must abort without stopping, renaming or removing the
// foreign container.
func TestRestoreOriginalProtectsExternalContainer(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := loadGolden(t)
	foreign.ID = "foreign-1"
	foreign.Config.Labels = map[string]string{" intruder": "1"}
	delete(foreign.Config.Labels, " intruder")
	foreign.Config.Labels = map[string]string{}
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{
			"webapp":   foreign, // external container took the name
			foreign.ID: foreign,
		},
		failNext: map[string]error{"create": &dockerx.APIError{Status: 409, Body: "name conflict"}},
	}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	_, _, err = RestoreOriginal(context.Background(), eng, snap, "")
	if err == nil {
		t.Fatal("restore must abort when an unlabeled container holds the name")
	}
	for _, id := range eng.removed {
		if id == foreign.ID {
			t.Fatal("external container must never be removed by restore rollback")
		}
	}
	for _, r := range eng.renames {
		if r[0] == foreign.ID {
			t.Fatal("external container must never be renamed by restore")
		}
	}
	for _, s := range eng.stopped {
		if s[0] == foreign.ID {
			t.Fatal("external container must never be stopped by restore")
		}
	}
}

// TestRestoreOriginalRollbackRemovesByID pins the by-ID rollback rule: the
// half-restored container is removed by its own ID, never by name.
func TestRestoreOriginalRollbackRemovesByID(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	managed := loadGolden(t)
	managed.ID = "managed-1"
	managed.Config.Labels[LabelServiceID] = "svc-1"
	managed.Config.Labels[LabelManaged] = "true"
	delete(managed.HostConfig.PortBindings, "80/tcp")
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{
			"webapp":    managed,
			"managed-1": managed,
		},
		failNext: map[string]error{"start": &dockerx.APIError{Status: 500, Body: "boom"}},
	}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	// A start failure must roll back INTERNALLY: the error surfaces with the
	// rollback already applied (by ID), no rollback handle for a failed call.
	_, rollback, err := RestoreOriginal(context.Background(), eng, snap, "managed-1")
	if err == nil {
		t.Fatal("start failure must surface")
	}
	if rollback != nil {
		t.Fatal("failed restore must not hand out a rollback handle")
	}
	found := false
	for _, id := range eng.removed {
		if id == "new-webapp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("half-restored container must be removed by its recorded ID, got %v", eng.removed)
	}
	if len(eng.renames) < 2 || eng.renames[len(eng.renames)-1][1] != prep.OriginalName {
		t.Fatalf("managed container must be renamed back, got %v", eng.renames)
	}
	if n := len(eng.started); n == 0 || eng.started[n-1] != "managed-1" {
		t.Fatalf("managed container must be started again, got %v", eng.started)
	}
}
