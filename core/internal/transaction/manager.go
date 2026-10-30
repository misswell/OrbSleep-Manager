package transaction

import (
	"encoding/json"
	"fmt"
	"time"

	"orbsleep/internal/state"
)

// Manager centralizes durable transaction persistence and phase validation.
type Manager struct {
	store *state.Store
}

func NewManager(store *state.Store) *Manager { return &Manager{store: store} }

func (m *Manager) Start(opID, kind string, payload any, newID func() string) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := newID()
	txn := state.Transaction{ID: id, Kind: kind, OperationID: opID, Status: "running",
		Phase: string(PhasePlanned), StartedAt: now, UpdatedAt: now, Data: string(raw)}
	if err := m.store.SaveTransaction(txn); err != nil {
		return "", err
	}
	return id, nil
}

func (m *Manager) Update(id string, to Phase, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	txn, ok, err := m.store.GetTransaction(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("事务 %s 不存在", id)
	}
	from := Phase(txn.Phase)
	if err := ValidateTransition(from, to); err != nil {
		return err
	}
	raw, err := mutate([]byte(txn.Data))
	if err != nil {
		return err
	}
	txn.Phase = string(to)
	txn.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	txn.Data = string(raw)
	return m.store.SaveTransaction(txn)
}

func (m *Manager) Finish(id, status string, cause error, phase Phase) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if phase == "" {
		phase = Phase(status)
	}
	_ = m.store.FinishTransaction(id, status, msg, string(phase))
}

func (m *Manager) FinishWithOperation(id, opID, status string, cause error, phase Phase, opStatus string) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if phase == "" {
		phase = Phase(status)
	}
	return m.store.FinishTransactionWithOperation(id, opID, status, string(phase), msg, opStatus, msg)
}
