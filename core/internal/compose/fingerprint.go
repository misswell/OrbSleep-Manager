package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// FileFingerprint identifies the exact on-disk state of a Compose file.
// Content alone is insufficient: permissions, ownership, file type and symlink
// identity also matter when deciding whether it is safe to overwrite.
type FileFingerprint struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
	Kind       string `json:"kind"` // regular, symlink, other
	LinkTarget string `json:"linkTarget,omitempty"`
	ModTime    string `json:"modTime,omitempty"`
}

// FingerprintPath fingerprints the resolved content and metadata of path.
// For a symlink the link target is recorded and the target is fingerprinted.
func FingerprintPath(path string) (FileFingerprint, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return FileFingerprint{}, err
	}
	info := lfi
	kind := "regular"
	linkTarget := ""
	if lfi.Mode()&os.ModeSymlink != 0 {
		kind = "symlink"
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			return FileFingerprint{}, fmt.Errorf("解析符号链接 %s: %w", path, rerr)
		}
		linkTarget = resolved
		info, err = os.Stat(resolved)
		if err != nil {
			return FileFingerprint{}, err
		}
	}
	if !info.Mode().IsRegular() {
		return FileFingerprint{}, fmt.Errorf("%s 不是普通文件（%s）", path, info.Mode().Type())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return FileFingerprint{}, err
	}
	hash := sha256.Sum256(raw)
	return FileFingerprint{
		SHA256: hex.EncodeToString(hash[:]), Size: info.Size(), Mode: uint32(info.Mode().Perm()),
		UID: ownerUID(info), GID: ownerGID(info), Kind: kind, LinkTarget: linkTarget,
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}, nil
}

// FingerprintBytes builds a content/metadata fingerprint for a regular file.
func FingerprintBytes(data []byte, info os.FileInfo) FileFingerprint {
	hash := sha256.Sum256(data)
	return FileFingerprint{
		SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), Mode: uint32(info.Mode().Perm()),
		UID: ownerUID(info), GID: ownerGID(info), Kind: "regular",
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
}

func ownerUID(info os.FileInfo) uint32 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Uid)
	}
	return 0
}

func ownerGID(info os.FileInfo) uint32 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Gid)
	}
	return 0
}

// SameContent reports whether two fingerprints describe identical content.
func (f FileFingerprint) SameContent(other FileFingerprint) bool {
	return f.SHA256 == other.SHA256 && f.Size == other.Size
}

// SameFile reports whether content and the safety-relevant metadata match.
func (f FileFingerprint) SameFile(other FileFingerprint) bool {
	return f.SameContent(other) && f.Mode == other.Mode && f.UID == other.UID &&
		f.GID == other.GID && f.Kind == other.Kind && f.LinkTarget == other.LinkTarget
}
