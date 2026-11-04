package docrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"orbsleep/internal/dockerx"
)

// stagedManaged builds a realistic parked managed container fixture.
func stagedManaged(t *testing.T, serviceID string, withLabel bool) (*dockerx.ContainerInspect, *ManagedSnapshot, *PreparedAttach) {
	t.Helper()
	// The recreation spec derives from a container logically named "webapp";
	// the PARKED container (what restore actually finds in the engine) carries
	// the stale name and the managed labels.
	base := loadGolden(t)
	base.Name = "/webapp"
	prep, err := PrepareAttach(base, serviceID, []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	ins := loadGolden(t)
	ins.ID = "parked-1"
	ins.Name = "/" + StaleName("webapp")
	delete(ins.HostConfig.PortBindings, "80/tcp")
	ins.HostConfig.Binds = append(ins.HostConfig.Binds,
		"9f8e7d6c5b4a3210fedcba9876543210fedcba9876543210fedcba9876543210:/var/cache/nginx")
	if withLabel {
		ins.Config.Labels[LabelServiceID] = serviceID
		ins.Config.Labels[LabelManaged] = "true"
	}
	snap := &ManagedSnapshot{
		ServiceID:    serviceID,
		OriginalName: "webapp",
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	return ins, snap, prep
}

// TestRemoveManagedContainerIdentityMatrix covers every identity branch of
// the cleanup entry point: only an exact-label match may be removed.
func TestRemoveManagedContainerIdentityMatrix(t *testing.T) {
	tests := []struct {
		name        string
		serviceID   string
		withLabel   bool
		inspectErr  error
		wantRemoved bool
		wantErrSub  string
	}{
		{name: "正确标签允许删除", serviceID: "svc-1", withLabel: true, wantRemoved: true},
		{name: "无标签外部容器拒绝删除", serviceID: "svc-1", withLabel: false, wantErrSub: "缺少 OrbSleep 身份"},
		{name: "标签不匹配拒绝删除", serviceID: "svc-1", withLabel: true, inspectErr: nil, wantErrSub: ""},
		{name: "ServiceID 为空拒绝删除", serviceID: "", withLabel: true, wantErrSub: "缺少服务身份"},
		{name: "Inspect 500 不删除", serviceID: "svc-1", withLabel: true, inspectErr: &dockerx.APIError{Status: 500, Body: "boom"}, wantErrSub: "boom"},
		{name: "404 视为已删除", serviceID: "svc-1", withLabel: true, inspectErr: &dockerx.APIError{Status: 404, Body: "no such container"}, wantRemoved: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ins, snap, _ := stagedManaged(t, tc.serviceID, tc.withLabel)
			// The "wrong label" variant: parked container carries a DIFFERENT
			// service's id.
			if tc.name == "标签不匹配拒绝删除" {
				ins.Config.Labels[LabelServiceID] = "svc-other"
				ins.Config.Labels[LabelManaged] = "true"
			}
			inspections := map[string]*dockerx.ContainerInspect{
				StaleName("webapp"): ins,
				ins.ID:              ins,
			}
			eng := &fakeEngine{inspections: inspections}
			if tc.inspectErr != nil {
				eng.failNext = map[string]error{"inspect": tc.inspectErr}
			}
			// "parked-1" is the container id the last verified state recorded.
			err := RemoveManagedContainer(context.Background(), eng, snap, "parked-1")
			is404 := tc.inspectErr != nil && tc.inspectErr.(*dockerx.APIError).Status == 404
			switch {
			case is404:
				// 404 case: treated as already removed, no error, no delete call.
				if err != nil {
					t.Fatalf("404 must be treated as already gone: %v", err)
				}
				if len(eng.removed) != 0 {
					t.Fatalf("404 must not issue a remove call: %v", eng.removed)
				}
			case tc.wantRemoved:
				if err != nil {
					t.Fatalf("expected removal, got error: %v", err)
				}
				if len(eng.removed) != 1 || eng.removed[0] != ins.ID {
					t.Fatalf("must remove by the verified ID, got %v", eng.removed)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErrSub, err)
				}
				if len(eng.removed) != 0 {
					t.Fatalf("refused cleanup must not remove anything: %v", eng.removed)
				}
			}
		})
	}
}

// TestRestoreOriginalRenameAndStartFail pins the P1-02 rule: a failed rename
// followed by a failed restart surfaces BOTH errors — a silently stopped
// original is a service outage, not a detail.
func TestRestoreOriginalRenameAndStartFail(t *testing.T) {
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
			"webapp":     managed,
			"managed-1":  managed,
			"managed-1x": managed,
		},
		failNext: map[string]error{
			"rename": &dockerx.APIError{Status: 500, Body: "rename refused"},
			"start":  &dockerx.APIError{Status: 500, Body: "start refused"},
		},
	}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	_, _, err = RestoreOriginal(context.Background(), eng, snap, "managed-1")
	if err == nil {
		t.Fatal("rename failure must surface")
	}
	// BOTH faults must appear in the combined error: a silent restart failure
	// would leave the original stopped with only the rename error reported.
	if !strings.Contains(err.Error(), "rename refused") {
		t.Fatalf("error must contain the rename failure: %v", err)
	}
	if !strings.Contains(err.Error(), "start refused") {
		t.Fatalf("error must contain the restart failure: %v", err)
	}
}

// TestHTTP500WithNoSuchBodyIsNotNotFound pins the P1-03 rule: error-TEXT
// sniffing must never authorise a destructive branch — only a real 404 does.
func TestHTTP500WithNoSuchBodyIsNotNotFound(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{},
		failNext: map[string]error{
			"inspect": &dockerx.APIError{Status: 500, Body: `{"message":"no such container"}`},
		},
	}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	_, _, err = RestoreOriginal(context.Background(), eng, snap, "")
	if err == nil || !strings.Contains(err.Error(), "无法读取托管容器状态") {
		t.Fatalf("a 500 whose body mentions 'no such' must abort as unreadable state, got %v", err)
	}
	if len(eng.removed) != 0 || len(eng.created) != 0 ||
		len(eng.renames) != 0 || len(eng.stopped) != 0 {
		t.Fatalf("unreadable state must not trigger ANY mutation: removed=%v created=%v renames=%v stopped=%v",
			eng.removed, eng.created, eng.renames, eng.stopped)
	}
}

// TestVerifyManagedRequiresLabel pins the strict identity rule for the
// pre-restore verification: a container without our service label is refused
// even when the snapshot carries a ServiceID.
func TestVerifyManagedRequiresLabel(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	managed := loadGolden(t)
	managed.ID = "managed-1"
	delete(managed.HostConfig.PortBindings, "80/tcp")
	// no service label at all
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{"webapp": managed}}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	if _, err := VerifyManaged(context.Background(), eng, snap); err == nil {
		t.Fatal("VerifyManaged must refuse a container without the service label")
	}
}

// TestLoadSnapshotRequiresServiceID pins the fail-closed rule for identity:
// a snapshot without a service id never enters automatic destructive flows.
func TestLoadSnapshotRequiresServiceID(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	snap := &ManagedSnapshot{
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(raw); err == nil || !strings.Contains(err.Error(), "服务身份") {
		t.Fatalf("LoadSnapshot must refuse snapshots without serviceId, got %v", err)
	}
}

// TestRestoreOriginalSocketDownKeepsState pins the triage rule for a dead
// socket: a non-404 Inspect failure aborts with the managed wiring intact and
// performs NO mutation (no stop, no rename, no create, no remove).
func TestRestoreOriginalSocketDownKeepsState(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	managed := loadGolden(t)
	managed.ID = "managed-1"
	managed.Config.Labels[LabelServiceID] = "svc-1"
	delete(managed.HostConfig.PortBindings, "80/tcp")
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{"webapp": managed},
		failNext:    map[string]error{"inspect": &dockerx.APIError{Status: 503, Body: "dial unix /var/run/docker.sock: connect: connection refused"}},
	}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	_, _, err = RestoreOriginal(context.Background(), eng, snap, "managed-1")
	if err == nil || !strings.Contains(err.Error(), "无法读取托管容器状态") {
		t.Fatalf("socket-down must abort as unreadable state, got %v", err)
	}
	if len(eng.created) != 0 || len(eng.removed) != 0 || len(eng.renames) != 0 || len(eng.stopped) != 0 {
		t.Fatalf("socket-down must not mutate anything: created=%v removed=%v renames=%v stopped=%v",
			eng.created, eng.removed, eng.renames, eng.stopped)
	}
}

// TestCreateAndStartSurfacesCleanupFailure pins the P2-04 rule: when the
// started container cannot be cleaned up, the error carries the orphan id.
func TestCreateAndStartSurfacesCleanupFailure(t *testing.T) {
	ins := loadGolden(t)
	prep, err := PrepareAttach(ins, "svc-1", []string{"80/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{
		inspections: map[string]*dockerx.ContainerInspect{},
		failNext: map[string]error{
			"start":  &dockerx.APIError{Status: 500, Body: "start refused"},
			"remove": &dockerx.APIError{Status: 500, Body: "remove refused"},
		},
	}
	_, err = CreateAndStart(context.Background(), eng, prep.ManagedSpec)
	if err == nil || !strings.Contains(err.Error(), "清理失败") || !strings.Contains(err.Error(), "孤儿容器") {
		t.Fatalf("cleanup failure must surface with the orphan id, got %v", err)
	}
}

// TestRestoreTwiceIsSafe pins recovery idempotency at the strategy layer:
// after a successful restore the original container is the user's own
// (label-less) container again — a second restore attempt must refuse it
// instead of stopping/renaming/removing it.
func TestRestoreTwiceIsSafe(t *testing.T) {
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
	eng := &fakeEngine{inspections: map[string]*dockerx.ContainerInspect{
		"webapp":    managed,
		"managed-1": managed,
	}}
	snap := &ManagedSnapshot{
		ServiceID:    "svc-1",
		OriginalName: prep.OriginalName,
		OriginalSpec: prep.OriginalSpec,
		ManagedSpec:  prep.ManagedSpec,
		Secondary:    prep.Secondary,
	}
	if _, _, err := RestoreOriginal(context.Background(), eng, snap, "managed-1"); err != nil {
		t.Fatalf("first restore failed: %v", err)
	}
	// After the restore, "webapp" is the ORIGINAL spec container (no
	// OrbSleep labels). Simulate that state by replacing the live entry in
	// place (the fake's list is already built; map writes would be invisible).
	restored := loadGolden(t)
	restored.ID = "restored-1"
	*managed = *restored
	before := len(eng.removed) + len(eng.renames) + len(eng.stopped)
	if _, _, err := RestoreOriginal(context.Background(), eng, snap, ""); err == nil {
		t.Fatal("second restore must refuse a label-less container")
	}
	after := len(eng.removed) + len(eng.renames) + len(eng.stopped)
	if after != before {
		t.Fatalf("refused restore must not mutate anything (before=%d after=%d)", before, after)
	}
}
