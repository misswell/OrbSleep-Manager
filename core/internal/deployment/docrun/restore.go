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
	if label := ins.Config.Labels[LabelServiceID]; label != "" && snap.ServiceID != "" && label != snap.ServiceID {
		return nil, fmt.Errorf("容器的 OrbSleep 服务标识不匹配（%s ≠ %s），拒绝自动恢复", label, snap.ServiceID)
	}
	return ins, nil
}

// RestoreOriginal puts the original container back under its original name:
// stop managed → rename managed aside → create original spec (all ports) →
// start → reconnect networks. The parked managed container is removed by the
// caller only after the restored container is verified.
func RestoreOriginal(ctx context.Context, eng Engine, snap *ManagedSnapshot) (createdID string, rollback func() error, err error) {
	var managedID string
	if ins, ierr := eng.InspectContainer(ctx, snap.OriginalName); ierr == nil {
		managedID = ins.ID
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
			_ = eng.StartContainer(ctx, managedID)
			return "", nil, fmt.Errorf("重命名托管容器失败: %w", err)
		}
	}
	rollback = func() error {
		// Put the managed state back: remove the half-restored original
		// container (it holds the name), rename the managed one back, start it.
		var firstErr error
		if err := eng.RemoveContainer(ctx, snap.OriginalName, true); err != nil && !isNotFound(err) {
			firstErr = err
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
// verified restore.
func RemoveManagedContainer(ctx context.Context, eng Engine, staleName string) error {
	return eng.RemoveContainer(ctx, staleName, true)
}

// CreateAndStart creates a container from a spec and starts it, removing the
// container again when start fails. Used by crash recovery.
func CreateAndStart(ctx context.Context, eng Engine, spec *runtime.ContainerSpec) (string, error) {
	id, err := eng.CreateContainer(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := eng.StartContainer(ctx, id); err != nil {
		_ = eng.RemoveContainer(ctx, id, true)
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

func isNotFound(err error) bool {
	return dockerx.IsErrNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such")
}
