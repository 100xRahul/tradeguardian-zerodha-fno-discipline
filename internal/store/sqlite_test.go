package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tradeguardian/internal/domain"
)

func TestLockPersistsWithAuditAndUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tradeguardian.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 7, 21, 5, 0, 0, 0, time.UTC)
	record := domain.LockRecord{Status: domain.TradingLocked, LockedOn: "2026-07-21", TriggerMTMPaise: domain.LossLimitPaise, TriggeredAt: now, UnlockAt: now.Add(24 * time.Hour), LiquidationState: "IN_PROGRESS"}
	if err := database.Lock(ctx, record, domain.AuditEvent{CreatedAt: now, Type: "DAILY_LOSS_LOCK", Code: domain.CodeTradingLocked, Message: domain.LockMessage}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	got, err := database.CurrentLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TradingLocked || got.TriggerMTMPaise != domain.LossLimitPaise || !got.UnlockAt.Equal(record.UnlockAt) {
		t.Fatalf("persisted lock = %#v", got)
	}
	events, err := database.ListAudit(ctx, 10)
	if err != nil || len(events) != 1 || events[0].Type != "DAILY_LOSS_LOCK" {
		t.Fatalf("audit events = %#v, error = %v", events, err)
	}
	if err := database.Unlock(ctx, now.Add(24*time.Hour), domain.AuditEvent{Type: "AUTOMATIC_UNLOCK", Message: "unlocked"}); err != nil {
		t.Fatal(err)
	}
	got, err = database.CurrentLock(ctx)
	if err != nil || got.Status != domain.TradingActive {
		t.Fatalf("unlocked state = %#v, error = %v", got, err)
	}
}

func TestIdempotencyReservationIsDurableAndCompletesConditionally(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "tradeguardian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if result, found, err := database.LookupIdempotency(ctx, "request-123"); err != nil || found || result != "" {
		t.Fatalf("initial lookup result=%q found=%v error=%v", result, found, err)
	}
	if existing, reserved, err := database.ReserveIdempotency(ctx, "request-123", "pending-order:abc", time.Now()); err != nil || !reserved || existing != "" {
		t.Fatalf("first reservation existing=%q reserved=%v error=%v", existing, reserved, err)
	}
	if existing, reserved, err := database.ReserveIdempotency(ctx, "request-123", "pending-order:def", time.Now()); err != nil || reserved || existing != "pending-order:abc" {
		t.Fatalf("replay reservation existing=%q reserved=%v error=%v", existing, reserved, err)
	}
	if err := database.CompleteIdempotency(ctx, "request-123", "pending-order:def", "order-wrong"); err == nil {
		t.Fatal("completion with wrong reservation succeeded")
	}
	if err := database.CompleteIdempotency(ctx, "request-123", "pending-order:abc", "order-1"); err != nil {
		t.Fatal(err)
	}
	if result, found, err := database.LookupIdempotency(ctx, "request-123"); err != nil || !found || result != "order-1" {
		t.Fatalf("completed lookup result=%q found=%v error=%v", result, found, err)
	}
	if existing, reserved, err := database.ReserveIdempotency(ctx, "request-123", "pending-order:ghi", time.Now()); err != nil || reserved || existing != "order-1" {
		t.Fatalf("completed replay existing=%q reserved=%v error=%v", existing, reserved, err)
	}
}
