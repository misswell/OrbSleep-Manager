package engine

import (
	"fmt"
	"sync"
	"time"

	"orbsleep/internal/model"
)

// maxOperationLogEntries bounds one operation's log. Every add() re-serializes
// the whole log into a single UPDATE; without a cap a long batch attach would
// rewrite an ever-growing JSON blob on every step.
const maxOperationLogEntries = 400

type opLogger struct {
	e  *Engine
	mu sync.Mutex
	op model.Operation
}

func (e *Engine) newOpLogger(op model.Operation) *opLogger {
	l := &opLogger{e: e, op: op}
	l.persist()
	return l
}

func (l *opLogger) persist() {
	cp := l.snapshot()
	_ = l.e.store.UpdateOperation(cp)
}

func (l *opLogger) snapshot() model.Operation {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := l.op
	cp.Log = append([]model.LogEntry(nil), l.op.Log...)
	return cp
}

func (l *opLogger) add(level, msg string) {
	l.mu.Lock()
	l.op.Log = append(l.op.Log, model.LogEntry{
		At:      time.Now().UTC().Format(time.RFC3339),
		Level:   level,
		Message: msg,
	})
	if len(l.op.Log) > maxOperationLogEntries {
		dropped := len(l.op.Log) - maxOperationLogEntries
		l.op.Log = append([]model.LogEntry{{
			At:      time.Now().UTC().Format(time.RFC3339),
			Level:   "warn",
			Message: fmt.Sprintf("…（日志过长，已省略最早的 %d 条）", dropped),
		}}, l.op.Log[dropped:]...)
	}
	cp := l.op
	l.mu.Unlock()
	l.e.logger.Printf(level, "%s: %s", cp.ID, msg)
	_ = l.e.store.UpdateOperation(cp)
}

func (l *opLogger) finish(status, errMsg string) {
	l.mu.Lock()
	l.op.Status = status
	l.op.Error = errMsg
	l.op.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	cp := l.op
	l.mu.Unlock()
	_ = l.e.store.UpdateOperation(cp)
	if status == "succeeded" {
		l.e.logger.Printf("info", "%s 完成", cp.ID)
	} else {
		l.e.logger.Printf("error", "%s %s: %s", cp.ID, status, errMsg)
	}
	l.e.triggerReconcile()
}

func (l *opLogger) setAffected(ids []string) {
	l.mu.Lock()
	l.op.AffectedServiceIDs = ids
	cp := l.op
	l.mu.Unlock()
	_ = l.e.store.UpdateOperation(cp)
}

// Operation returns one recorded operation.
func (e *Engine) Operation(id string) (model.Operation, bool, error) {
	return e.store.GetOperation(id)
}

// Operations lists recent operations.
func (e *Engine) Operations(limit int) ([]model.Operation, error) {
	ops, err := e.store.ListOperations(limit)
	if ops == nil {
		ops = []model.Operation{}
	}
	return ops, err
}
