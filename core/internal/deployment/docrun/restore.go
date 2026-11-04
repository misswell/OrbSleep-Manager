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

// validateSnapshotIdentity is the entry gate for every destructive restore
// operation. Snapshots can be built in memory (crash recovery builds them
// from txn payloads), so entry functions must never assume the caller passed
// the data through LoadSnapshot.
func validateSnapshotIdentity(snap *ManagedSnapshot) error {
	if snap == nil {
		return errors.New("快照为空")
	}
	if strings.TrimSpace(snap.ServiceID) == "" {
		return errors.New("快照缺少服务身份（serviceId），拒绝执行破坏性操作")
	}
	if snap.OriginalSpec == nil || snap.ManagedSpec == nil {
		return errors.New("快照规格不完整（缺少原始或托管规格）")
	}
	return nil
}

// VerifyManaged confirms the running managed container still matches the
// snapshot's managed spec. Identity (service label + managed label) is
// verified BEFORE the config fingerprint: an untrusted container must not be
// fingerprint-compared at all, let alone restored.
func VerifyManaged(ctx context.Context, eng Engine, snap *ManagedSnapshot) (*dockerx.ContainerInspect, error) {
	if err := validateSnapshotIdentity(snap); err != nil {
		return nil, err
	}
	ins, err := eng.InspectContainer(ctx, snap.OriginalName)
	if err != nil {
		return nil, fmt.Errorf("托管容器不可读: %w", err)
	}
	if ins.Config.Labels[LabelServiceID] != snap.ServiceID {
		return nil, fmt.Errorf("容器的 OrbSleep 服务标识不匹配（%q ≠ %q），拒绝自动恢复", ins.Config.Labels[LabelServiceID], snap.ServiceID)
	}
	if ins.Config.Labels[LabelManaged] != "true" {
		return nil, fmt.Errorf("容器 %s 不是 OrbSleep 托管容器（缺少 %s 标签），拒绝自动恢复", snap.OriginalName, LabelManaged)
	}
	want := Fingerprint(snap.ManagedSpec)
	current, err := SpecFromInspect(ins)
	if err != nil {
		return nil, err
	}
	if got := Fingerprint(current); got != want {
		return nil, fmt.Errorf("容器配置与 OrbSleep 快照不一致（可能被外部修改或重建），拒绝自动恢复；请检查容器 %s 后手动处理", snap.OriginalName)
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
	if err := validateSnapshotIdentity(snap); err != nil {
		return "", nil, err
	}
	ins, ierr := eng.InspectContainer(ctx, snap.OriginalName)
	var managedID string
	switch {
	case ierr == nil:
		if expectedManagedID != "" && ins.ID != expectedManagedID {
			return "", nil, fmt.Errorf("名称 %s 下的容器已不是 OrbSleep 管理的那个（%s ≠ %s），拒绝恢复以保护外部容器",
				snap.OriginalName, shortID(ins.ID), shortID(expectedManagedID))
		}
		// Strict identity: the container under our name must carry OUR
		// service id AND the managed label. A label-less container means
		// something external took the name while the managed one was parked —
		// touching it is forbidden.
		if ins.Config.Labels[LabelServiceID] != snap.ServiceID {
			return "", nil, fmt.Errorf("名称 %s 下的容器缺少 OrbSleep 服务标识（可能是外部新建的容器），拒绝恢复以保护外部容器",
				snap.OriginalName)
		}
		if ins.Config.Labels[LabelManaged] != "true" {
			return "", nil, fmt.Errorf("名称 %s 下的容器不是 OrbSleep 托管容器（缺少 %s 标签），拒绝恢复以保护外部容器",
				snap.OriginalName, LabelManaged)
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
		// Post-stop re-inspect: the identity window between verify and the
		// next mutation is closed by re-reading the SAME container ID and
		// re-checking our label. Stop/Rename address the container by ID, so
		// this only guards against the container being force-replaced.
		if re, rerr := eng.InspectContainer(ctx, managedID); rerr != nil {
			if serr := eng.StartContainer(ctx, managedID); serr != nil {
				return "", nil, fmt.Errorf("停止后无法复核容器状态: %v；重新启动也失败: %w（容器 %s 需人工检查）", rerr, serr, shortID(managedID))
			}
			return "", nil, fmt.Errorf("停止后无法复核容器状态: %v；已重新启动托管容器，恢复中止", rerr)
		} else if re.ID != managedID || re.Config.Labels[LabelServiceID] != snap.ServiceID {
			return "", nil, fmt.Errorf("停止后容器身份发生变化（%s），拒绝继续恢复", shortID(re.ID))
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
	// Name-occupancy check right before create: if something external took
	// the original name while the managed one was parked, fail deterministically
	// here (rollback restores the parked container by ID) instead of relying
	// on the create error path.
	if occ, oerr := eng.InspectContainer(ctx, snap.OriginalName); oerr == nil {
		return "", nil, fmt.Errorf("原名称 %s 已被其他容器占用（%s），拒绝恢复以保护外部容器", snap.OriginalName, shortID(occ.ID))
	} else if !isNotFound(oerr) {
		return "", nil, fmt.Errorf("无法确认原名称 %s 是否空闲: %v；已中止恢复", snap.OriginalName, oerr)
	}
	// createdID feeds the rollback: the half-restored container is removed BY
	// ID, never by name (a create failure may mean an external container now
	// holds the name — touching it is forbidden).
	createdID = ""
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
	if err := validateSnapshotIdentity(snap); err != nil {
		return err
	}
	ins, err := eng.InspectContainer(ctx, StaleName(snap.OriginalName))
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	// Strict identity: a container without OUR exact service label is never
	// ours, no matter how it came to hold the stale name.
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
			// The FULL orphan id must reach the log/txn so a later pass can
			// clean it up by ID.
			return "", fmt.Errorf("启动失败: %v；清理失败（孤儿容器 id=%s 需按 ID 人工删除）: %w", err, id, cerr)
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
