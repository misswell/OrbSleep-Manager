package transaction

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileLockerSerializesSamePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compose.yml")
	locker := NewFileLocker(filepath.Join(root, "locks"))
	first, err := locker.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		second, err := locker.Acquire(ctx, path)
		if err != nil {
			order <- "timeout"
			return
		}
		order <- "acquired"
		second.Release()
	}()
	time.Sleep(100 * time.Millisecond)
	first.Release()
	wg.Wait()
	if got := <-order; got != "acquired" {
		t.Fatalf("second lock: %s", got)
	}
}

func TestFileLockerTimeout(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compose.yml")
	locker := NewFileLocker(filepath.Join(root, "locks"))
	first, err := locker.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := locker.Acquire(ctx, path); err == nil {
		t.Fatal("expected timeout")
	}
}
