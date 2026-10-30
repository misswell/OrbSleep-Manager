// Package state persists managed services, operations and settings in SQLite.
package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"orbsleep/internal/model"
)

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// Keep the database (and its WAL sidecars) readable by the current user only.
	_ = os.Chmod(path, 0o600)
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS services (
			id TEXT PRIMARY KEY,
			container_id TEXT NOT NULL DEFAULT '',
			container_name TEXT NOT NULL DEFAULT '',
			compose_project TEXT NOT NULL DEFAULT '',
			compose_service TEXT NOT NULL DEFAULT '',
			compose_file TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			bindings TEXT NOT NULL DEFAULT '[]',
			idle_timeout INTEGER NOT NULL DEFAULT 0,
			start_timeout INTEGER NOT NULL DEFAULT 0,
			active_conns INTEGER NOT NULL DEFAULT 0,
			last_active_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			excluded_from_sleep INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS operations (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			backup_dir TEXT NOT NULL DEFAULT '',
			affected TEXT NOT NULL DEFAULT '[]',
			log TEXT NOT NULL DEFAULT '[]'
		)`,
		`CREATE TABLE IF NOT EXISTS transactions (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL DEFAULT '',
			operation_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			phase TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`,
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Older databases predate operation_id. Only the expected duplicate-column
	// error may be ignored; anything else (read-only DB, I/O error) must abort.
	if _, err := s.db.Exec(`ALTER TABLE transactions ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''`); err != nil {
		if !isDuplicateColumn(err) {
			return fmt.Errorf("migrate transactions.operation_id: %w", err)
		}
	}
	return nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// UpsertService inserts or replaces a managed service.
func (s *Store) UpsertService(svc model.Service) error {
	bindings, err := json.Marshal(svc.Bindings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO services
		(id, container_id, container_name, compose_project, compose_service, compose_file, status, bindings,
		 idle_timeout, start_timeout, active_conns, last_active_at, created_at, last_error, excluded_from_sleep)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		 container_id=excluded.container_id, container_name=excluded.container_name,
		 compose_project=excluded.compose_project, compose_service=excluded.compose_service,
		 compose_file=excluded.compose_file, status=excluded.status, bindings=excluded.bindings,
		 idle_timeout=excluded.idle_timeout, start_timeout=excluded.start_timeout,
		 active_conns=excluded.active_conns, last_active_at=excluded.last_active_at,
		 last_error=excluded.last_error, excluded_from_sleep=excluded.excluded_from_sleep`,
		svc.ID, svc.ContainerID, svc.ContainerName, svc.ComposeProject, svc.ComposeService,
		svc.ComposeFile, svc.Status, string(bindings), svc.IdleTimeoutSecs, svc.StartTimeoutSecs,
		svc.ActiveConnections, svc.LastActiveAt, svc.CreatedAt, svc.LastError, boolInt(svc.ExcludedFromSleep))
	return err
}

// ListServices returns all managed services ordered by creation time.
func (s *Store) ListServices() ([]model.Service, error) {
	rows, err := s.db.Query(`SELECT id, container_id, container_name, compose_project, compose_service,
		compose_file, status, bindings, idle_timeout, start_timeout, active_conns, last_active_at,
		created_at, last_error, excluded_from_sleep FROM services ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Service
	for rows.Next() {
		var svc model.Service
		var bindings string
		var excl int
		if err := rows.Scan(&svc.ID, &svc.ContainerID, &svc.ContainerName, &svc.ComposeProject,
			&svc.ComposeService, &svc.ComposeFile, &svc.Status, &bindings, &svc.IdleTimeoutSecs,
			&svc.StartTimeoutSecs, &svc.ActiveConnections, &svc.LastActiveAt, &svc.CreatedAt,
			&svc.LastError, &excl); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(bindings), &svc.Bindings)
		svc.ExcludedFromSleep = excl != 0
		out = append(out, svc)
	}
	return out, rows.Err()
}

// GetService returns one service.
func (s *Store) GetService(id string) (model.Service, bool, error) {
	list, err := s.ListServices()
	if err != nil {
		return model.Service{}, false, err
	}
	for _, svc := range list {
		if svc.ID == id {
			return svc, true, nil
		}
	}
	return model.Service{}, false, nil
}

// DeleteService removes a managed service record.
func (s *Store) DeleteService(id string) error {
	_, err := s.db.Exec(`DELETE FROM services WHERE id=?`, id)
	return err
}

// SetServiceStatus updates status, last error and container id.
func (s *Store) SetServiceStatus(id, status, lastError, containerID string) error {
	_, err := s.db.Exec(`UPDATE services SET status=?, last_error=?, container_id=CASE WHEN ?='' THEN container_id ELSE ? END WHERE id=?`,
		status, lastError, containerID, containerID, id)
	return err
}

// SetServiceRuntime updates volatile runtime fields.
func (s *Store) SetServiceRuntime(id string, active int, lastActive string) error {
	_, err := s.db.Exec(`UPDATE services SET active_conns=?, last_active_at=? WHERE id=?`, active, lastActive, id)
	return err
}

// InsertOperation records an operation.
func (s *Store) InsertOperation(op model.Operation) error {
	logJSON, _ := json.Marshal(op.Log)
	affected, _ := json.Marshal(op.AffectedServiceIDs)
	_, err := s.db.Exec(`INSERT INTO operations (id, kind, status, started_at, finished_at, error, backup_dir, affected, log)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		op.ID, op.Kind, op.Status, op.StartedAt, op.FinishedAt, op.Error, op.BackupDir, string(affected), string(logJSON))
	return err
}

// UpdateOperation updates an existing operation.
func (s *Store) UpdateOperation(op model.Operation) error {
	logJSON, _ := json.Marshal(op.Log)
	affected, _ := json.Marshal(op.AffectedServiceIDs)
	_, err := s.db.Exec(`UPDATE operations SET status=?, finished_at=?, error=?, backup_dir=?, affected=?, log=? WHERE id=?`,
		op.Status, op.FinishedAt, op.Error, op.BackupDir, string(affected), string(logJSON), op.ID)
	return err
}

// ListRunningOperations returns operations that never reached a terminal state.
func (s *Store) ListRunningOperations() ([]model.Operation, error) {
	rows, err := s.db.Query(`SELECT id, kind, status, started_at, finished_at, error, backup_dir, affected, log
		FROM operations WHERE status='running' ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// GetOperation returns one operation.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperation(row rowScanner) (model.Operation, error) {
	var op model.Operation
	var affected, logJSON string
	if err := row.Scan(&op.ID, &op.Kind, &op.Status, &op.StartedAt, &op.FinishedAt, &op.Error, &op.BackupDir, &affected, &logJSON); err != nil {
		return op, err
	}
	_ = json.Unmarshal([]byte(affected), &op.AffectedServiceIDs)
	_ = json.Unmarshal([]byte(logJSON), &op.Log)
	if op.Log == nil {
		op.Log = []model.LogEntry{}
	}
	return op, nil
}

func (s *Store) GetOperation(id string) (model.Operation, bool, error) {
	row := s.db.QueryRow(`SELECT id, kind, status, started_at, finished_at, error, backup_dir, affected, log FROM operations WHERE id=?`, id)
	op, err := scanOperation(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return model.Operation{}, false, nil
		}
		return model.Operation{}, false, err
	}
	return op, true, nil
}

// ListOperations returns recent operations, newest first.
func (s *Store) ListOperations(limit int) ([]model.Operation, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id FROM operations ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]model.Operation, 0, len(ids))
	for _, id := range ids {
		if op, ok, err := s.GetOperation(id); err == nil && ok {
			out = append(out, op)
		}
	}
	return out, nil
}

// PruneOperations deletes operations older than cutoff.
func (s *Store) PruneOperations(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM operations WHERE started_at < ?`, cutoff.UTC().Format(time.RFC3339))
	return err
}

// Transaction is a durable record of an in-flight or recovered operation.
type Transaction struct {
	ID          string
	Kind        string
	OperationID string
	Status      string // running | committed | rolled_back | rollback_failed
	Phase       string
	StartedAt   string
	UpdatedAt   string
	Error       string
	Data        string
}

// SaveTransaction inserts or updates a transaction record.
func (s *Store) SaveTransaction(t Transaction) error {
	_, err := s.db.Exec(`INSERT INTO transactions (id, kind, operation_id, status, phase, started_at, updated_at, error, data)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET operation_id=excluded.operation_id, status=excluded.status,
		 phase=excluded.phase, updated_at=excluded.updated_at, error=excluded.error, data=excluded.data`,
		t.ID, t.Kind, t.OperationID, t.Status, t.Phase, t.StartedAt, t.UpdatedAt, t.Error, t.Data)
	return err
}

// GetTransaction returns one transaction record.
func (s *Store) GetTransaction(id string) (Transaction, bool, error) {
	row := s.db.QueryRow(`SELECT id, kind, operation_id, status, phase, started_at, updated_at, error, data FROM transactions WHERE id=?`, id)
	var t Transaction
	if err := row.Scan(&t.ID, &t.Kind, &t.OperationID, &t.Status, &t.Phase, &t.StartedAt, &t.UpdatedAt, &t.Error, &t.Data); err != nil {
		if err == sql.ErrNoRows {
			return Transaction{}, false, nil
		}
		return Transaction{}, false, err
	}
	return t, true, nil
}

// ListRunningTransactions returns transactions that were not finished.
func (s *Store) ListRunningTransactions() ([]Transaction, error) {
	rows, err := s.db.Query(`SELECT id, kind, operation_id, status, phase, started_at, updated_at, error, data
		FROM transactions WHERE status='running' ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.Kind, &t.OperationID, &t.Status, &t.Phase, &t.StartedAt, &t.UpdatedAt, &t.Error, &t.Data); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FinishTransaction marks a transaction terminal.
func (s *Store) FinishTransaction(id, status, errMsg, phase string) error {
	_, err := s.db.Exec(`UPDATE transactions SET status=?, phase=?, updated_at=?, error=? WHERE id=?`,
		status, phase, time.Now().UTC().Format(time.RFC3339), errMsg, id)
	return err
}

// ListTransactions returns all transaction records (for orphan reconciliation).
func (s *Store) ListTransactions() ([]Transaction, error) {
	rows, err := s.db.Query(`SELECT id, kind, operation_id, status, phase, started_at, updated_at, error, data
		FROM transactions ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.Kind, &t.OperationID, &t.Status, &t.Phase, &t.StartedAt, &t.UpdatedAt, &t.Error, &t.Data); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FinishTransactionWithOperation commits the transaction terminal state and the
// operation terminal state atomically.
func (s *Store) FinishTransactionWithOperation(txnID, opID, txnStatus, phase, txnErr, opStatus, opErr string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`UPDATE transactions SET status=?, phase=?, updated_at=?, error=? WHERE id=?`,
		txnStatus, phase, now, txnErr, txnID); err != nil {
		return err
	}
	if opID != "" {
		if _, err := tx.Exec(`UPDATE operations SET status=?, finished_at=?, error=? WHERE id=?`,
			opStatus, now, opErr, opID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneTransactions deletes old terminal transactions.
func (s *Store) PruneTransactions(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM transactions WHERE status<>'running' AND updated_at < ?`,
		cutoff.UTC().Format(time.RFC3339))
	return err
}

// GetSetting reads a settings key.
func (s *Store) GetSetting(key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// SetSetting writes a settings key.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// GetMeta reads an internal metadata value.
func (s *Store) GetMeta(key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// SetMeta writes an internal metadata value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// DeleteMeta removes an internal metadata value.
func (s *Store) DeleteMeta(key string) error {
	_, err := s.db.Exec(`DELETE FROM meta WHERE key=?`, key)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BackupTo writes a consistent snapshot of the database to path using
// SQLite's VACUUM INTO. The destination must not already exist.
func (s *Store) BackupTo(path string) error {
	_ = os.Remove(path)
	stmt := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(path, "'", "''"))
	_, err := s.db.Exec(stmt)
	return err
}
