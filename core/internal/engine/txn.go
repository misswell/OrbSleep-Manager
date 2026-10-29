package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/state"
)

// txnPayload is the durable state of an attach transaction. It contains enough
// information to roll the service back after an abrupt process termination.
type txnPayload struct {
	OperationID string       `json:"operationId"`
	Baseline    proxy.Config `json:"baseline"`
	Items       []txnItem    `json:"items"`
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

func (e *Engine) startTxn(opID, kind string, payload txnPayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	txn := state.Transaction{
		ID: newID("txn"), Kind: kind, Status: "running", Phase: "planned",
		StartedAt: now, UpdatedAt: now, Data: string(raw),
	}
	if err := e.store.SaveTransaction(txn); err != nil {
		return "", err
	}
	return txn.ID, nil
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

// updateTxn mutates and persists the transaction payload.
func (e *Engine) updateTxn(id, phase string, mutate func(*txnPayload)) error {
	t, p, err := e.loadTxn(id)
	if err != nil {
		return err
	}
	if mutate != nil {
		mutate(&p)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	t.Phase = phase
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	t.Data = string(raw)
	return e.store.SaveTransaction(t)
}

func (e *Engine) finishTxn(id, status string, cause error, phase string) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if phase == "" {
		phase = status
	}
	_ = e.store.FinishTransaction(id, status, msg, phase)
}
