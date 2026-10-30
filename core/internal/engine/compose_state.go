package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orbsleep/internal/compose"
)

func composeSnapshotID(path string) string {
	canonical := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonical = resolved
	} else if abs, aerr := filepath.Abs(path); aerr == nil {
		canonical = filepath.Clean(abs)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) composeSnapshotStore() compose.SnapshotStore {
	return compose.SnapshotStore{Root: filepath.Join(e.settings.Get().BackupDir, "compose")}
}

// ensureComposeSnapshot captures the first-seen original before any mutation.
func (e *Engine) ensureComposeSnapshot(path string) (*compose.ComposeSnapshot, error) {
	store := e.composeSnapshotStore()
	id := composeSnapshotID(path)
	loaded, err := store.Load(id)
	if err == nil {
		return &loaded, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
		return nil, fmt.Errorf("读取 Compose 快照失败: %w", err)
	}
	snap, err := compose.CaptureComposeSnapshot(path)
	if err != nil {
		return nil, fmt.Errorf("捕获 Compose 快照失败: %w", err)
	}
	if _, err := store.Save(id, *snap); err != nil {
		return nil, fmt.Errorf("保存 Compose 快照失败: %w", err)
	}
	return snap, nil
}

// markComposeLastKnownGood records a successfully committed file state.
func (e *Engine) markComposeLastKnownGood(path string, raw []byte) error {
	store := e.composeSnapshotStore()
	id := composeSnapshotID(path)
	snap, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("读取 Compose 快照失败: %w", err)
	}
	fp, ferr := compose.FingerprintPath(path)
	if ferr != nil {
		return fmt.Errorf("读取 Compose 指纹失败: %w", ferr)
	}
	snap.MarkLastKnownGood(raw, fp)
	if _, err := store.Save(id, snap); err != nil {
		return fmt.Errorf("保存 Compose 最后可信快照失败: %w", err)
	}
	return nil
}

// composeLastKnownGood returns the file-level last committed content, if any.
func (e *Engine) composeLastKnownGood(path string) ([]byte, bool) {
	snap, err := e.composeSnapshotStore().Load(composeSnapshotID(path))
	if err != nil || len(snap.LastKnownGood) == 0 {
		return nil, false
	}
	return snap.LastKnownGood, true
}
