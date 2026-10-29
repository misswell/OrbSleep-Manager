package engine

import (
	"context"
	"errors"
	"time"
)

// beginOperation registers a transactional goroutine so shutdown can cancel it
// and wait for its rollback before closing the database and gateway.
func (e *Engine) beginOperation(timeout time.Duration) (context.Context, func(), error) {
	e.txnMu.Lock()
	defer e.txnMu.Unlock()
	if e.closing {
		return nil, nil, errors.New("后台助手正在关闭，请稍后重试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	id := newID("run")
	e.txnCancels[id] = cancel
	e.txnWG.Add(1)
	done := func() {
		cancel()
		e.txnMu.Lock()
		delete(e.txnCancels, id)
		e.txnMu.Unlock()
		e.txnWG.Done()
	}
	return ctx, done, nil
}

// waitTransactions waits for registered operations, cancelling them on close.
func (e *Engine) waitTransactions(grace time.Duration) bool {
	e.txnMu.Lock()
	e.closing = true
	for _, cancel := range e.txnCancels {
		cancel()
	}
	e.txnMu.Unlock()
	done := make(chan struct{})
	go func() {
		e.txnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}
