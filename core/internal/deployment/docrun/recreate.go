package docrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/runtime"
)

// Engine is the container lifecycle surface the strategy needs. dockerapi.Runtime
// satisfies it; tests inject fakes.
type Engine interface {
	InspectContainer(ctx context.Context, id string) (*dockerx.ContainerInspect, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSecs int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	CreateContainer(ctx context.Context, spec *runtime.ContainerSpec) (string, error)
	ConnectNetwork(ctx context.Context, network, containerID string, endpoint *runtime.EndpointConfig) error
}

// PreparedAttach is the fully mapped attach input persisted BEFORE any
// container mutation: if the process dies anywhere below, recovery rebuilds
// the exact same plan from this struct.
type PreparedAttach struct {
	OriginalID   string                 `json:"originalId"`
	OriginalName string                 `json:"originalName"`
	BackupName   string                 `json:"backupName"`
	Fingerprint  string                 `json:"fingerprint"`
	OriginalSpec *runtime.ContainerSpec `json:"originalSpec"`
	ManagedSpec  *runtime.ContainerSpec `json:"managedSpec"`
	Secondary    []string               `json:"secondary,omitempty"`
	StopTimeout  int                    `json:"stopTimeout,omitempty"`
}

// PrepareAttach maps the inspect payload into the attach snapshot. takeover
// keys are the container port keys ("8080/tcp") whose TCP host bindings the
// gateway takes over; they are stripped from the managed spec only.
func PrepareAttach(ins *dockerx.ContainerInspect, serviceID string, takeoverKeys []string) (*PreparedAttach, error) {
	original, err := SpecFromInspect(ins)
	if err != nil {
		return nil, err
	}
	managed, err := deepCopySpec(original)
	if err != nil {
		return nil, err
	}
	removals := map[string]struct{}{}
	for _, key := range takeoverKeys {
		removals[key] = struct{}{}
	}
	RemoveTCPPortBindings(managed, removals)
	name := strings.TrimPrefix(ins.Name, "/")
	fp := Fingerprint(original)
	managed.AddLabel(LabelManaged, "true")
	managed.AddLabel(LabelServiceID, serviceID)
	managed.AddLabel(LabelStrategy, "docrun")
	managed.AddLabel(LabelFingerprint, Fingerprint(managed))
	prep := &PreparedAttach{
		OriginalID:   ins.ID,
		OriginalName: name,
		BackupName:   BackupName(name, ins.ID),
		Fingerprint:  fp,
		OriginalSpec: original,
		ManagedSpec:  managed,
		Secondary:    SecondaryNetworks(ins),
		StopTimeout:  stopTimeoutOf(ins),
	}
	return prep, nil
}

func stopTimeoutOf(ins *dockerx.ContainerInspect) int {
	if ins.Config.StopTimeout != nil && *ins.Config.StopTimeout > 0 {
		return *ins.Config.StopTimeout
	}
	return 10
}

// deepCopySpec clones a spec via canonical JSON so mutating the managed copy
// (port stripping, labels) can never touch the original snapshot.
func deepCopySpec(in *runtime.ContainerSpec) (*runtime.ContainerSpec, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out runtime.ContainerSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyUnchanged re-checks that the container still matches the planned
// fingerprint. Guards against user edits between plan and apply.
func VerifyUnchanged(ctx context.Context, eng Engine, prep *PreparedAttach) error {
	ins, err := eng.InspectContainer(ctx, prep.OriginalID)
	if err != nil {
		return fmt.Errorf("目标容器在接入前已不可读: %w", err)
	}
	current, err := SpecFromInspect(ins)
	if err != nil {
		return err
	}
	if got := Fingerprint(current); got != prep.Fingerprint {
		return errors.New("容器配置在计划生成后被修改过，请重新扫描并生成计划")
	}
	return nil
}

// RecreateManaged executes the safe replacement sequence:
// stop original → rename original (rollback anchor) → create replacement →
// start replacement → reconnect secondary networks. The original container is
// NOT removed here; the caller removes it only after the full attach is
// verified. onPhase (optional) persists durable phase markers; a phase-persist
// failure aborts with the internal rollback already applied.
func RecreateManaged(ctx context.Context, eng Engine, prep *PreparedAttach, onPhase func(phase string) error) (newID string, cleanup func(removeOriginal bool) error, err error) {
	// 1. Stop the original container. A stopped container keeps its name and
	// its volumes; nothing is lost.
	if err := eng.StopContainer(ctx, prep.OriginalID, prep.StopTimeout); err != nil {
		return "", nil, fmt.Errorf("停止原容器失败: %w", err)
	}
	// State-aware undo: the initial rename may not have taken effect, so
	// inspect the container's CURRENT name and only rename when needed.
	// Always attempt the start; collect every error instead of swallowing.
	undoRename := func() error {
		var errs []string
		if ins, err := eng.InspectContainer(ctx, prep.OriginalID); err == nil {
			if cur := strings.TrimPrefix(ins.Name, "/"); cur != prep.OriginalName {
				if rerr := eng.RenameContainer(ctx, prep.OriginalID, prep.OriginalName); rerr != nil {
					errs = append(errs, "恢复原容器名称失败: "+rerr.Error())
				}
			}
		} else {
			errs = append(errs, "复核原容器状态失败: "+err.Error())
		}
		if serr := eng.StartContainer(ctx, prep.OriginalID); serr != nil {
			errs = append(errs, "重启原容器失败: "+serr.Error())
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	}
	// 2. Rename the original out of the way. It stays as a stopped rollback
	// anchor until the replacement is fully verified.
	if err := eng.RenameContainer(ctx, prep.OriginalID, prep.BackupName); err != nil {
		_ = undoRename()
		return "", nil, fmt.Errorf("重命名原容器失败（%s → %s）: %w", prep.OriginalName, prep.BackupName, err)
	}
	if onPhase != nil {
		if perr := onPhase("original_renamed"); perr != nil {
			// Rollback errors must surface: a silently failed undo would leave
			// the caller believing the original container is back in place.
			if uerr := undoRename(); uerr != nil {
				return "", nil, fmt.Errorf("%v（且回滚失败: %v）", perr, uerr)
			}
			return "", nil, perr
		}
	}
	// 3. Create the replacement under the original name.
	created, err := eng.CreateContainer(ctx, prep.ManagedSpec)
	if err != nil {
		if uerr := undoRename(); uerr != nil {
			return "", nil, fmt.Errorf("创建新容器失败: %v；且回滚失败: %w", err, uerr)
		}
		return "", nil, fmt.Errorf("创建新容器失败: %w", err)
	}
	if onPhase != nil {
		if perr := onPhase("replacement_created"); perr != nil {
			var rbErrs []string
			if rerr := eng.RemoveContainer(ctx, created, true); rerr != nil {
				rbErrs = append(rbErrs, "删除替换容器: "+rerr.Error())
			}
			if uerr := undoRename(); uerr != nil {
				rbErrs = append(rbErrs, uerr.Error())
			}
			if len(rbErrs) > 0 {
				return "", nil, fmt.Errorf("%v（且回滚失败: %s）", perr, strings.Join(rbErrs, "; "))
			}
			return "", nil, perr
		}
	}
	undoCreate := func() error {
		rerr := eng.RemoveContainer(ctx, created, true)
		if uerr := undoRename(); uerr != nil {
			if rerr != nil {
				return fmt.Errorf("删除新容器失败: %v；%w", rerr, uerr)
			}
			return uerr
		}
		return rerr
	}
	// 4. Start it.
	if err := eng.StartContainer(ctx, created); err != nil {
		if uerr := undoCreate(); uerr != nil {
			return "", nil, fmt.Errorf("启动新容器失败: %v；且回滚失败: %w", err, uerr)
		}
		return "", nil, fmt.Errorf("启动新容器失败: %w", err)
	}
	// 5. Reconnect secondary networks (aliases preserved).
	for _, netName := range prep.Secondary {
		ep := endpointForNetwork(prep, netName)
		if err := eng.ConnectNetwork(ctx, netName, created, ep); err != nil {
			if uerr := undoCreate(); uerr != nil {
				return "", nil, fmt.Errorf("重连网络 %s 失败: %v；且回滚失败: %w", netName, err, uerr)
			}
			return "", nil, fmt.Errorf("重连网络 %s 失败: %w", netName, err)
		}
	}
	cleanup = func(removeOriginal bool) error {
		if removeOriginal {
			if err := eng.RemoveContainer(ctx, prep.OriginalID, true); err != nil {
				return fmt.Errorf("移除原容器备份失败: %w", err)
			}
		}
		return nil
	}
	return created, cleanup, nil
}

// RollbackManaged reverses a partially applied replacement: stop+remove the
// replacement, rename the original back and start it. The replacement is
// verified against the managed spec before it is stopped/removed — recovery
// callers may pass an ID whose state changed since the crash.
func RollbackManaged(ctx context.Context, eng Engine, prep *PreparedAttach, replacementID string) error {
	var firstErr error
	if replacementID != "" {
		ins, ierr := eng.InspectContainer(ctx, replacementID)
		if ierr != nil && !isNotFound(ierr) {
			return fmt.Errorf("接管容器不可读: %w", ierr)
		}
		if ierr == nil {
			if ins.Config.Labels[LabelServiceID] != prep.ManagedSpec.Config.Labels[LabelServiceID] ||
				ins.Config.Labels[LabelManaged] != "true" {
				return fmt.Errorf("接管容器 %s 缺少 OrbSleep 身份，拒绝回滚删除", shortID(replacementID))
			}
		}
		_ = eng.StopContainer(ctx, replacementID, 10)
		if err := eng.RemoveContainer(ctx, replacementID, true); err != nil {
			firstErr = fmt.Errorf("删除接管容器失败: %w", err)
		}
	}
	if err := eng.RenameContainer(ctx, prep.OriginalID, prep.OriginalName); err != nil {
		if firstErr != nil {
			return fmt.Errorf("%v；恢复原容器名称失败: %w", firstErr, err)
		}
		return fmt.Errorf("恢复原容器名称失败: %w", err)
	}
	if err := eng.StartContainer(ctx, prep.OriginalID); err != nil {
		if firstErr != nil {
			return fmt.Errorf("%v；重启原容器失败: %w", firstErr, err)
		}
		return fmt.Errorf("重启原容器失败: %w", err)
	}
	return firstErr
}

// RemoveOriginalBackup removes the renamed rollback anchor. Called only after
// the replacement is verified end-to-end and the managed record is durable.
func RemoveOriginalBackup(ctx context.Context, eng Engine, prep *PreparedAttach) error {
	return eng.RemoveContainer(ctx, prep.OriginalID, true)
}

// InspectRunning waits until the named container reports running. Poll-based
// on purpose: engine events alone are not reliable right after create.
func InspectRunning(ctx context.Context, eng Engine, name string, timeout time.Duration) (*dockerx.ContainerInspect, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ins, err := eng.InspectContainer(ctx, name)
		if err == nil && ins.State.Running {
			return ins, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("容器状态为 %s", ins.State.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("容器 %s 未在 %s 内就绪: %v", name, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func endpointForNetwork(prep *PreparedAttach, netName string) *runtime.EndpointConfig {
	if prep == nil || prep.OriginalSpec == nil || prep.OriginalSpec.Networks == nil {
		return nil
	}
	if ep, ok := prep.OriginalSpec.Networks[netName]; ok {
		cp := ep
		return &cp
	}
	return nil
}
