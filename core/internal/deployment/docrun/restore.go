package docrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// ManagedSnapshot is the durable record for a managed docrun service. It is
// written into the service backup directory before the first mutation and is
// the ONLY truth used by restore — the original container no longer exists at
// that point.
type ManagedSnapshot struct {
	Version      int                    `json:"version"`
	ServiceID    string                 `json:"serviceId"`
	OriginalName string                 `json:"originalName"`
	OriginalID   string                 `json:"originalId"`
	Fingerprint  string                 `json:"fingerprint"`
	OriginalSpec *runtime.ContainerSpec `json:"originalSpec"`
	ManagedSpec  *runtime.ContainerSpec `json:"managedSpec"`
	Secondary    []string               `json:"secondary,omitempty"`
	AttachedAt   string                 `json:"attachedAt"`
}

// StaleName is where the managed container parks during a restore. It is
// deterministic so a crash mid-restore can always find it.
const StaleSuffix = ".orbsleep-old"

func StaleName(originalName string) string { return originalName + StaleSuffix }

// LoadSnapshot reads and validates a managed snapshot from bytes.
func LoadSnapshot(raw []byte) (*ManagedSnapshot, error) {
	var snap ManagedSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("解析容器快照失败: %w", err)
	}
	if snap.OriginalSpec == nil || snap.ManagedSpec == nil || snap.OriginalName == "" {
		return nil, errors.New("容器快照不完整（缺少规格或名称）")
	}
	// Identity is mandatory: a snapshot that cannot prove WHICH service it
	// belongs to must never enter an automatic destructive flow. Such
	// snapshots need a manual migration, not a guess.
	if strings.TrimSpace(snap.ServiceID) == "" {
		return nil, errors.New("容器快照缺少服务身份（serviceId），拒绝自动恢复；请人工核对后处理")
	}
	if snap.Version == 0 {
		snap.Version = 1
	}
	if snap.ManagedSpec.Config.Labels[LabelFingerprint] == "" {
		snap.ManagedSpec.AddLabel(LabelFingerprint, Fingerprint(snap.ManagedSpec))
	}
	return &snap, nil
}

// VerifyManaged confirms the running managed container still matches the
// snapshot's managed spec (OrbSleep labels and runtime-resolved state are
// ignored by the fingerprint). A mismatch means the user replaced or edited
// the container externally; restore refuses rather than overwriting.
func VerifyManaged(ctx context.Context, eng Engine, snap *ManagedSnapshot) (*dockerx.ContainerInspect, error) {
	ins, err := eng.InspectContainer(ctx, snap.OriginalName)
	if err != nil {
		return nil, fmt.Errorf("托管容器不可读: %w", err)
	}
	want := Fingerprint(snap.ManagedSpec)
	current, err := SpecFromInspect(ins)
	if err != nil {
		return nil, err
	}
	if got := Fingerprint(current); got != want {
		return nil, fmt.Errorf("容器配置与 OrbSleep 快照不一致（可能被外部修改或重建），拒绝自动恢复；请检查容器 %s 后手动处理", snap.OriginalName)
	}
	// Strict identity: a missing label is just as wrong as a mismatched one.
	if snap.ServiceID != "" && ins.Config.Labels[LabelServiceID] != snap.ServiceID {
		return nil, fmt.Errorf("容器的 OrbSleep 服务标识不匹配（%q ≠ %q），拒绝自动恢复", ins.Config.Labels[LabelServiceID], snap.ServiceID)
	}
	return ins, nil
}

// RestoreOriginal puts the original container back under its original name:
// verify managed identity → stop managed → rename managed aside → create
// original spec (all ports) → start → reconnect networks.
//
// Safety rules:
//   - expectedManagedID (from VerifyManaged) must match the container found
//     under the original name; a mismatch aborts instead of parking someone
//     else's container.
//   - Inspect failures other than NotFound abort the restore: an unknown
//     state must never be treated as "absent".
//   - Rollback removes containers BY ID only. A name conflict at create time
//     (an external container took the name while we worked) must never make
//     us delete the foreign container.
func RestoreOriginal(ctx context.Context, eng Engine, snap *ManagedSnapshot, expectedManagedID string) (createdID string, rollback func() error, err error) {
	ins, ierr := eng.InspectContainer(ctx, snap.OriginalName)
	var managedID string
	switch {
	case ierr == nil:
		if expectedManagedID != "" && ins.ID != expectedManagedID {
			return "", nil, fmt.Errorf("名称 %s 下的容器已不是 OrbSleep 管理的那个（%s ≠ %s），拒绝恢复以保护外部容器",
				snap.OriginalName, shortID(ins.ID), shortID(expectedManagedID))
		}
		// The container under our name must carry OUR service label. A
		// label-less container means something external took the name while
		// the managed one was parked — touching it is forbidden.
		if snap.ServiceID != "" && ins.Config.Labels[LabelServiceID] != snap.ServiceID {
			return "", nil, fmt.Errorf("名称 %s 下的容器缺少 OrbSleep 服务标识（可能是外部新建的容器），拒绝恢复以保护外部容器",
				snap.OriginalName)
		}
		managedID = ins.ID
	case isNotFound(ierr):
		managedID = ""
	default:
		// Unreadable state (API down, timeout, …): abort, keep the managed
		// wiring, let the caller retry later.
		return "", nil, fmt.Errorf("无法读取托管容器状态: %v；为保护现场已中止恢复，请确认引擎可用后重试", ierr)
	}
	stale := StaleName(snap.OriginalName)
	if managedID != "" {
		stopTimeout := 10
		if t := snap.ManagedSpec.Config.StopTimeout; t != nil && *t > 0 {
			stopTimeout = *t
		}
		if err := eng.StopContainer(ctx, managedID, stopTimeout); err != nil {
			return "", nil, fmt.Errorf("停止托管容器失败: %w", err)
		}
		if err := eng.RenameContainer(ctx, managedID, stale); err != nil {
			// Never swallow the restart error: a stopped, unrestorable
			// original is a service outage, not a cosmetic detail.
			if serr := eng.StartContainer(ctx, managedID); serr != nil {
				return "", nil, fmt.Errorf("重命名托管容器失败: %v；重新启动托管容器也失败: %w（容器 %s 处于停止状态，需人工恢复）", err, serr, shortID(managedID))
			}
			return "", nil, fmt.Errorf("重命名托管容器失败: %w", err)
		}
	}
	// createdID feeds the rollback: the half-restored container is removed BY
	// ID, never by name (a create failure may mean an external container now
	// holds the name — touching it is forbidden).
	createdID = ""
	rollback = func() error {
		// Put the managed state back: remove the half-restored original by
		// its recorded ID, rename the managed one back, start it.
		var firstErr error
		if createdID != "" {
			if rerr := eng.RemoveContainer(ctx, createdID, true); rerr != nil && !isNotFound(rerr) {
				firstErr = rerr
			}
		}
		if managedID != "" {
			if rerr := eng.RenameContainer(ctx, managedID, snap.OriginalName); rerr != nil && !isNotFound(rerr) {
				if firstErr != nil {
					return fmt.Errorf("%v；%w", firstErr, rerr)
				}
				firstErr = rerr
			}
			if serr := eng.StartContainer(ctx, managedID); serr != nil && firstErr == nil {
				firstErr = serr
			}
		}
		return firstErr
	}
	created, err := eng.CreateContainer(ctx, snap.OriginalSpec)
	if err != nil {
		if rbErr := rollback(); rbErr != nil {
			return "", nil, fmt.Errorf("恢复原容器失败: %v；且回滚失败: %w", err, rbErr)
		}
		return "", nil, fmt.Errorf("恢复原容器失败: %w", err)
	}
	createdID = created
	if err := eng.StartContainer(ctx, created); err != nil {
		if rbErr := rollback(); rbErr != nil {
			return "", nil, fmt.Errorf("启动恢复容器失败: %v；且回滚失败: %w", err, rbErr)
		}
		return "", nil, fmt.Errorf("启动恢复容器失败: %w", err)
	}
	for _, netName := range snap.Secondary {
		ep := endpointFor(&snap.OriginalSpec.Networks, netName)
		if err := eng.ConnectNetwork(ctx, netName, created, ep); err != nil {
			if rbErr := rollback(); rbErr != nil {
				return "", nil, fmt.Errorf("重连网络 %s 失败: %v；且回滚失败: %w", netName, err, rbErr)
			}
			return "", nil, fmt.Errorf("重连网络 %s 失败: %w", netName, err)
		}
	}
	return created, rollback, nil
}

// RemoveManagedContainer removes the parked managed container after a
// verified restore. It resolves the parked container by name but verifies our
// service label before removing, and never touches a container that carries
// someone else's (or no) identity.
func RemoveManagedContainer(ctx context.Context, eng Engine, snap *ManagedSnapshot) error {
	ins, err := eng.InspectContainer(ctx, StaleName(snap.OriginalName))
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	// Strict identity: a container without OUR exact service label is never
	// ours, no matter how it came to hold the stale name. An empty snapshot
	// ServiceID means the caller cannot prove identity either — refuse.
	if snap.ServiceID == "" {
		return fmt.Errorf("快照缺少服务身份，拒绝清理 %s", StaleName(snap.OriginalName))
	}
	if ins.Config.Labels[LabelServiceID] != snap.ServiceID ||
		ins.Config.Labels[LabelManaged] != "true" {
		return fmt.Errorf("停用的托管容器 %s 缺少 OrbSleep 身份（service-id=%q managed=%q），拒绝删除",
			StaleName(snap.OriginalName), ins.Config.Labels[LabelServiceID], ins.Config.Labels[LabelManaged])
	}
	return eng.RemoveContainer(ctx, ins.ID, true)
}

// shortID renders an abbreviated container id for messages.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// CreateAndStart creates a container from a spec and starts it, removing the
// container again when start fails. Used by crash recovery.
func CreateAndStart(ctx context.Context, eng Engine, spec *runtime.ContainerSpec) (string, error) {
	id, err := eng.CreateContainer(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := eng.StartContainer(ctx, id); err != nil {
		if cerr := eng.RemoveContainer(ctx, id, true); cerr != nil {
			// The orphan id must reach the log/txn so a later pass can clean it.
			return "", fmt.Errorf("启动失败: %v；清理失败（孤儿容器 %s 需人工删除）: %w", err, shortID(id), cerr)
		}
		return "", err
	}
	return id, nil
}

func endpointFor(networks *map[string]runtime.EndpointConfig, netName string) *runtime.EndpointConfig {
	if networks == nil {
		return nil
	}
	if ep, ok := (*networks)[netName]; ok {
		cp := ep
		return &cp
	}
	return nil
}

// isNotFound accepts ONLY a structured 404. Error-text sniffing would let an
// unrelated failure whose body happens to contain "no such" authorise
// destructive branches — the exact opposite of the fail-closed rule.
func isNotFound(err error) bool {
	return dockerx.IsErrNotFound(err)
}
