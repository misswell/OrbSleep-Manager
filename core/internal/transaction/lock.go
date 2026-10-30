package transaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// FileLocker provides both in-process and cross-process exclusion keyed by
// canonical file path. Flock is released automatically if the process dies.
type FileLocker struct {
	root string

	mu    sync.Mutex
	held  map[string]bool
	waitQ map[string][]chan struct{}
}

func NewFileLocker(root string) *FileLocker {
	return &FileLocker{root: root, held: map[string]bool{}, waitQ: map[string][]chan struct{}{}}
}

func canonicalKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// Acquire blocks until the per-file lock is obtained or ctx expires.
func (l *FileLocker) Acquire(ctx context.Context, path string) (*FileLock, error) {
	key := canonicalKey(path)
	if err := l.acquireLocal(ctx, key); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			l.releaseLocal(key)
		}
	}()

	if err := os.MkdirAll(l.root, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(key))
	lockPath := filepath.Join(l.root, hex.EncodeToString(sum[:])+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if ferr == nil {
			ok = true
			return &FileLock{locker: l, key: key, file: f, path: lockPath}, nil
		}
		if ferr != syscall.EWOULDBLOCK && ferr != syscall.EAGAIN {
			f.Close()
			return nil, fmt.Errorf("获取 Compose 文件锁失败 %s: %w", path, ferr)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("等待 Compose 文件锁超时 %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (l *FileLocker) acquireLocal(ctx context.Context, key string) error {
	for {
		l.mu.Lock()
		if !l.held[key] {
			l.held[key] = true
			l.mu.Unlock()
			return nil
		}
		ch := make(chan struct{}, 1)
		l.waitQ[key] = append(l.waitQ[key], ch)
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (l *FileLocker) releaseLocal(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if q := l.waitQ[key]; len(q) > 0 {
		l.waitQ[key] = q[1:]
		// Transfer ownership: the waiter will re-check held and claim it.
		delete(l.held, key)
		q[0] <- struct{}{}
		return
	}
	delete(l.waitQ, key)
	delete(l.held, key)
}

// FileLock is an acquired per-file lock.
type FileLock struct {
	locker *FileLocker
	key    string
	file   *os.File
	path   string
	once   sync.Once
}

func (f *FileLock) Release() {
	f.once.Do(func() {
		_ = syscall.Flock(int(f.file.Fd()), syscall.LOCK_UN)
		_ = f.file.Close()
		_ = os.Remove(f.path)
		f.locker.releaseLocal(f.key)
	})
}
