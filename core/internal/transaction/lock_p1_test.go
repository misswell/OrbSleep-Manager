package transaction

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A waiter that gives up while queued must be removed from the queue:
// later releases must hand the lock to a live waiter, not to a canceled one.
func TestFileLockerCanceledWaiterDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "compose.yml")
	l := NewFileLocker(dir)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first, err := l.Acquire(ctx1, target)
	if err != nil {
		t.Fatal(err)
	}

	// Waiter cancels while queued.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = l.Acquire(ctx2, target)
	if err == nil {
		t.Fatal("queued acquire must fail when its context is canceled")
	}
	cancel2()

	// A fresh waiter must receive ownership on the next release.
	acquired := make(chan *FileLock, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lock, err := l.Acquire(ctx, first.path)
		if err == nil {
			acquired <- lock
		}
	}()
	time.Sleep(50 * time.Millisecond)
	first.Release()
	select {
	case lock := <-acquired:
		lock.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("live waiter never got the lock after the canceled one was dropped")
	}
}

// The lock file must survive Release: removing it lets a third process create
// a new inode while a second process is still flock-waiting on the old one.
func TestFileLockerReleaseKeepsLockFile(t *testing.T) {
	dir := t.TempDir()
	l := NewFileLocker(dir)
	lock, err := l.Acquire(context.Background(), filepath.Join(dir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	if _, err := os.Stat(lock.path); err != nil {
		t.Fatalf("lock file must be kept after release: %v", err)
	}
}

// Ownership transfer must remain correct when a waiter is signaled at the same
// moment its context is canceled: the lock may never end up held by nobody.
func TestFileLockerHandoffUnderCancelRace(t *testing.T) {
	l := NewFileLocker(t.TempDir())
	target := filepath.Join(t.TempDir(), "compose.yml")
	for i := 0; i < 50; i++ {
		holder, err := l.Acquire(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Acquire(ctx, target) // will usually time out while queued
		}()
		time.Sleep(time.Millisecond) // let the waiter queue up
		cancel()
		holder.Release()
		wg.Wait()

		next, err := l.Acquire(context.Background(), target)
		if err != nil {
			t.Fatalf("iteration %d: lock stuck after cancel race: %v", i, err)
		}
		next.Release()
	}
}
