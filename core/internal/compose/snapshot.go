package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ComposeSnapshot is the file-level recovery material for one Compose file.
// OriginalContent is the first content OrbSleep ever saw for this file.
// LastKnownGood is the most recent content OrbSleep successfully committed.
type ComposeSnapshot struct {
	FilePath                 string          `json:"filePath"`
	OriginalContent          []byte          `json:"originalContent"`
	LastKnownGood            []byte          `json:"lastKnownGood"`
	OriginalHash             string          `json:"originalHash"`
	LastKnownGoodHash        string          `json:"lastKnownGoodHash"`
	FileMode                 uint32          `json:"fileMode"`
	Services                 []string        `json:"services"`
	CapturedAt               string          `json:"capturedAt"`
	LastKnownGoodAt          string          `json:"lastKnownGoodAt"`
	LastKnownGoodFingerprint FileFingerprint `json:"lastKnownGoodFingerprint"`
}

// CaptureComposeSnapshot captures the current file as both the original and
// last-known-good content. It returns an error for symlinks or non-regular files.
func CaptureComposeSnapshot(path string) (*ComposeSnapshot, error) {
	fp, err := FingerprintPath(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	services, _ := Services(raw)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return &ComposeSnapshot{
		FilePath: path, OriginalContent: raw, LastKnownGood: raw,
		OriginalHash: hashBytes(raw), LastKnownGoodHash: hashBytes(raw),
		FileMode: fp.Mode, Services: services, CapturedAt: now, LastKnownGoodAt: now,
		LastKnownGoodFingerprint: fp,
	}, nil
}

// MarkLastKnownGood records a successfully committed file content.
func (s *ComposeSnapshot) MarkLastKnownGood(raw []byte, fp FileFingerprint) {
	s.LastKnownGood = append([]byte(nil), raw...)
	s.LastKnownGoodHash = hashBytes(raw)
	s.LastKnownGoodFingerprint = fp
	s.LastKnownGoodAt = time.Now().UTC().Format(time.RFC3339Nano)
	if services, err := Services(raw); err == nil {
		s.Services = services
	}
}

func hashBytes(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// SnapshotStore persists file-level snapshots next to transaction backups.
type SnapshotStore struct {
	Root string
}

func (st SnapshotStore) dir(id string) string { return filepath.Join(st.Root, id) }

// Save writes metadata plus before/last-good files and verifies their hashes.
func (st SnapshotStore) Save(id string, snap ComposeSnapshot) (string, error) {
	dir := st.dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := WriteAtomic(filepath.Join(dir, "before.yml"), snap.OriginalContent, 0o600); err != nil {
		return "", err
	}
	if err := WriteAtomic(filepath.Join(dir, "lastgood.yml"), snap.LastKnownGood, 0o600); err != nil {
		return "", err
	}
	meta, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	if err := WriteAtomic(filepath.Join(dir, "metadata.json"), meta, 0o600); err != nil {
		return "", err
	}
	// Verify integrity before returning the path to the caller.
	if _, err := st.Load(id); err != nil {
		return "", err
	}
	return dir, nil
}

// Load reads and validates a persisted snapshot.
func (st SnapshotStore) Load(id string) (ComposeSnapshot, error) {
	dir := st.dir(id)
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return ComposeSnapshot{}, err
	}
	var snap ComposeSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ComposeSnapshot{}, err
	}
	before, err := os.ReadFile(filepath.Join(dir, "before.yml"))
	if err != nil {
		return ComposeSnapshot{}, err
	}
	good, err := os.ReadFile(filepath.Join(dir, "lastgood.yml"))
	if err != nil {
		return ComposeSnapshot{}, err
	}
	if hashBytes(before) != snap.OriginalHash {
		return ComposeSnapshot{}, fmt.Errorf("Compose 原始快照校验失败: %s", dir)
	}
	if hashBytes(good) != snap.LastKnownGoodHash {
		return ComposeSnapshot{}, fmt.Errorf("Compose 最后可信快照校验失败: %s", dir)
	}
	snap.OriginalContent = before
	snap.LastKnownGood = good
	return snap, nil
}
