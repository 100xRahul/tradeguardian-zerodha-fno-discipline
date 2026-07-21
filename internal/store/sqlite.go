package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"tradeguardian/internal/domain"
)

type SQLite struct {
	db *sql.DB
}

func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLite{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS trading_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'LOCKED')),
			locked_on TEXT NOT NULL DEFAULT '',
			trigger_mtm_paise INTEGER NOT NULL DEFAULT 0,
			triggered_at TEXT NOT NULL DEFAULT '',
			unlock_at TEXT NOT NULL DEFAULT '',
			liquidation_state TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO trading_state (id, status) VALUES (1, 'ACTIVE')`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			type TEXT NOT NULL,
			code TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL,
			order_id TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS audit_events_created_at ON audit_events(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
			key TEXT PRIMARY KEY,
			order_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS liquidation_intents (
			position_key TEXT PRIMARY KEY,
			order_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

func (s *SQLite) CurrentLock(ctx context.Context) (domain.LockRecord, error) {
	var record domain.LockRecord
	var triggeredAt, unlockAt string
	err := s.db.QueryRowContext(ctx, `SELECT status, locked_on, trigger_mtm_paise, triggered_at, unlock_at, liquidation_state, last_error FROM trading_state WHERE id = 1`).
		Scan(&record.Status, &record.LockedOn, &record.TriggerMTMPaise, &triggeredAt, &unlockAt, &record.LiquidationState, &record.LastError)
	if err != nil {
		return record, fmt.Errorf("read trading state: %w", err)
	}
	record.TriggeredAt, err = parseOptionalTime(triggeredAt)
	if err != nil {
		return record, fmt.Errorf("parse trigger time: %w", err)
	}
	record.UnlockAt, err = parseOptionalTime(unlockAt)
	if err != nil {
		return record, fmt.Errorf("parse unlock time: %w", err)
	}
	return record, nil
}

func (s *SQLite) Lock(ctx context.Context, record domain.LockRecord, event domain.AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin lock transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE trading_state SET status = 'LOCKED', locked_on = ?, trigger_mtm_paise = ?, triggered_at = ?, unlock_at = ?, liquidation_state = ?, last_error = ? WHERE id = 1 AND status = 'ACTIVE'`,
		record.LockedOn, record.TriggerMTMPaise, formatTime(record.TriggeredAt), formatTime(record.UnlockAt), record.LiquidationState, record.LastError)
	if err != nil {
		return fmt.Errorf("persist lock: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read lock result: %w", err)
	}
	if changed > 0 {
		if err := insertAudit(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lock: %w", err)
	}
	return nil
}

func (s *SQLite) Unlock(ctx context.Context, at time.Time, event domain.AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unlock transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE trading_state SET status = 'ACTIVE', locked_on = '', trigger_mtm_paise = 0, triggered_at = '', unlock_at = '', liquidation_state = '', last_error = '' WHERE id = 1`); err != nil {
		return fmt.Errorf("persist unlock: %w", err)
	}
	event.CreatedAt = at
	if err := insertAudit(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unlock: %w", err)
	}
	return nil
}

func (s *SQLite) UpdateLiquidation(ctx context.Context, state, lastError string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE trading_state SET liquidation_state = ?, last_error = ? WHERE id = 1`, state, lastError); err != nil {
		return fmt.Errorf("update liquidation: %w", err)
	}
	return nil
}

func (s *SQLite) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	return insertAudit(ctx, s.db, event)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertAudit(ctx context.Context, target execer, event domain.AuditEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	_, err = target.ExecContext(ctx, `INSERT INTO audit_events (created_at, type, code, message, order_id, metadata_json) VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(event.CreatedAt), event.Type, event.Code, event.Message, event.OrderID, string(metadata))
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func (s *SQLite) ListAudit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, type, code, message, order_id, metadata_json FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	events := make([]domain.AuditEvent, 0, limit)
	for rows.Next() {
		var event domain.AuditEvent
		var createdAt, metadata string
		if err := rows.Scan(&event.ID, &createdAt, &event.Type, &event.Code, &event.Message, &event.OrderID, &metadata); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse audit timestamp: %w", err)
		}
		if err := json.Unmarshal([]byte(metadata), &event.Metadata); err != nil {
			return nil, fmt.Errorf("decode audit metadata: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return events, nil
}

func (s *SQLite) LookupIdempotency(ctx context.Context, key string) (string, bool, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `SELECT order_id FROM idempotency_keys WHERE key = ?`, key).Scan(&result)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read idempotency key: %w", err)
	}
	return result, true, nil
}

func (s *SQLite) ReserveIdempotency(ctx context.Context, key, reference string, at time.Time) (string, bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_keys (key, order_id, created_at) VALUES (?, ?, ?) ON CONFLICT(key) DO NOTHING`, key, reference, formatTime(at))
	if err != nil {
		return "", false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("read idempotency reservation: %w", err)
	}
	if changed == 1 {
		return "", true, nil
	}
	var existing string
	if err := s.db.QueryRowContext(ctx, `SELECT order_id FROM idempotency_keys WHERE key = ?`, key).Scan(&existing); err != nil {
		return "", false, fmt.Errorf("read idempotency key: %w", err)
	}
	return existing, false, nil
}

func (s *SQLite) CompleteIdempotency(ctx context.Context, key, reference, result string) error {
	updated, err := s.db.ExecContext(ctx, `UPDATE idempotency_keys SET order_id = ? WHERE key = ? AND order_id = ?`, result, key, reference)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("read idempotency completion: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("idempotency reservation changed before completion")
	}
	return nil
}

func (s *SQLite) ListLiquidationIntents(ctx context.Context) ([]domain.LiquidationIntent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT position_key, order_id, created_at FROM liquidation_intents ORDER BY position_key`)
	if err != nil {
		return nil, fmt.Errorf("list liquidation intents: %w", err)
	}
	defer rows.Close()
	intents := make([]domain.LiquidationIntent, 0)
	for rows.Next() {
		var intent domain.LiquidationIntent
		var createdAt string
		if err := rows.Scan(&intent.PositionKey, &intent.OrderID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan liquidation intent: %w", err)
		}
		intent.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse liquidation intent timestamp: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate liquidation intents: %w", err)
	}
	return intents, nil
}

func (s *SQLite) PutLiquidationIntent(ctx context.Context, intent domain.LiquidationIntent) error {
	if intent.PositionKey == "" || intent.OrderID == "" || intent.CreatedAt.IsZero() {
		return fmt.Errorf("liquidation intent requires position key, order id, and timestamp")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO liquidation_intents (position_key, order_id, created_at) VALUES (?, ?, ?) ON CONFLICT(position_key) DO UPDATE SET order_id = excluded.order_id, created_at = excluded.created_at`,
		intent.PositionKey, intent.OrderID, formatTime(intent.CreatedAt))
	if err != nil {
		return fmt.Errorf("persist liquidation intent: %w", err)
	}
	return nil
}

func (s *SQLite) DeleteLiquidationIntent(ctx context.Context, positionKey string) error {
	if positionKey == "" {
		return fmt.Errorf("liquidation position key is required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM liquidation_intents WHERE position_key = ?`, positionKey); err != nil {
		return fmt.Errorf("delete liquidation intent: %w", err)
	}
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}
