package engine

import (
	"encoding/json"
	"fmt"

	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/state"
	"orbsleep/internal/transaction"
)

// txnPayload is the durable state of an attach transaction. It contains enough
// information to roll the service back after an abrupt process termination.
type txnPayload struct {
	OperationID string       `json:"operationId"`
	Baseline    proxy.Config `json:"baseline"`
	Items       []txnItem    `json:"items"`
}

// detachPayload is the durable state for restore/remove transactions.
type detachPayload struct {
	OperationID      string        `json:"operationId"`
	Service          model.Service `json:"service"`
	WorkingDir       string        `json:"workingDir"`
	ManagedContent   []byte        `json:"managedContent"`
	ExpectedRestored []byte        `json:"expectedRestored"`
	Phase            string        `json:"phase"`
}

type txnItem struct {
	ContainerID    string               `json:"containerId"`
	ContainerName  string               `json:"containerName"`
	ComposeProject string               `json:"composeProject"`
	ComposeService string               `json:"composeService"`
	ComposeFile    string               `json:"composeFile"`
	WorkingDir     string               `json:"workingDir"`
	Content        []byte               `json:"content"`
	Removals       []model.PortBinding  `json:"removals"`
	ProxyMappings  []model.ProxyMapping `json:"proxyMappings"`
	Bindings       []model.Binding      `json:"bindings"`
	BackupPath     string               `json:"backupPath"`
	PendingContent []byte               `json:"pendingContent"`
	ManagedContent []byte               `json:"managedContent"`
	ServiceID      string               `json:"serviceId"`
	Phase          string               `json:"phase"`
}

func txnFromPlan(opID string, baseline proxy.Config, snaps []planSnapshot) txnPayload {
	p := txnPayload{OperationID: opID, Baseline: baseline.Clone(), Items: make([]txnItem, len(snaps))}
	for i, s := range snaps {
		p.Items[i] = txnItem{
			ContainerID: s.containerID, ContainerName: s.containerName,
			ComposeProject: s.composeProject, ComposeService: s.composeService,
			ComposeFile: s.composeFile, WorkingDir: s.workingDir,
			Content: append([]byte(nil), s.content...), Removals: s.removals,
			ProxyMappings: s.proxyMappings, Bindings: s.bindings, Phase: "planned",
		}
	}
	return p
}

func (p txnPayload) snapshot(i int) *planSnapshot {
	it := p.Items[i]
	return &planSnapshot{
		containerID: it.ContainerID, containerName: it.ContainerName,
		composeProject: it.ComposeProject, composeService: it.ComposeService,
		composeFile: it.ComposeFile, workingDir: it.WorkingDir,
		content: it.Content, removals: it.Removals, proxyMappings: it.ProxyMappings,
		bindings: it.Bindings,
	}
}

func (e *Engine) startTxn(opID, kind string, payload any) (string, error) {
	return e.txns.Start(opID, kind, payload, func() string { return newID("txn") })
}

func (e *Engine) loadTxn(id string) (state.Transaction, txnPayload, error) {
	t, ok, err := e.store.GetTransaction(id)
	if err != nil {
		return t, txnPayload{}, err
	}
	if !ok {
		return t, txnPayload{}, fmt.Errorf("事务 %s 不存在", id)
	}
	var p txnPayload
	if err := json.Unmarshal([]byte(t.Data), &p); err != nil {
		return t, p, err
	}
	return t, p, nil
}

// updateTxn mutates and persists the transaction payload through the
// transaction manager, which enforces legal phase transitions.
func (e *Engine) updateTxn(id, phase string, mutate func(*txnPayload)) error {
	return e.txns.Update(id, transaction.Phase(phase), func(raw json.RawMessage) (json.RawMessage, error) {
		var p txnPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if mutate != nil {
			mutate(&p)
		}
		return json.Marshal(p)
	})
}

func (e *Engine) finishTxn(id, status string, cause error, phase string) {
	e.txns.Finish(id, status, cause, transaction.Phase(phase))
}

// finishTxnAtomic commits the transaction and operation terminal states in one
// SQLite transaction, closing the window where one says committed and the other
// still says running.
func (e *Engine) finishTxnAtomic(id, opID, status string, cause error, phase string) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if phase == "" {
		phase = status
	}
	opStatus := status
	if status == "committed" {
		opStatus = "succeeded"
	}
	if err := e.txns.FinishWithOperation(id, opID, status, cause, transaction.Phase(phase), opStatus); err != nil {
		e.logger.Printf("warn", "原子提交事务终态失败，退回分步提交: %v", err)
		e.finishTxn(id, status, cause, phase)
		if opID != "" {
			e.updateOriginalOperation(opID, opStatus, msg, "")
		}
	}
}
