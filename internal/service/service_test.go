package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tradeguardian/internal/calendar"
	"tradeguardian/internal/domain"
)

func TestOptionBuyRejectedAndFutureBuyPlaced(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	ctx := context.Background()
	if err := app.Authenticate(ctx, "request"); err != nil {
		t.Fatal(err)
	}
	option := validRequest("NIFTY26JUL25000CE", "BUY", 50)
	decision, _, err := app.Place(ctx, option)
	if err != nil || decision.Code != domain.CodeOptionBuyForbidden || decision.Allowed {
		t.Fatalf("option decision = %#v, error = %v", decision, err)
	}
	if broker.placeCalls != 0 {
		t.Fatalf("broker Place called %d times for rejected option", broker.placeCalls)
	}
	future := validRequest("NIFTY26JULFUT", "BUY", 50)
	future.IdempotencyKey = "future-order-123"
	decision, orderID, err := app.Place(ctx, future)
	if err != nil || !decision.Allowed || orderID == "" || broker.placeCalls != 1 {
		t.Fatalf("future result decision=%#v orderID=%q calls=%d error=%v", decision, orderID, broker.placeCalls, err)
	}
	decision, replayID, err := app.Place(ctx, future)
	if err != nil || decision.Code != domain.CodeIdempotentReplay || replayID != orderID || broker.placeCalls != 1 {
		t.Fatalf("replay result decision=%#v orderID=%q calls=%d error=%v", decision, replayID, broker.placeCalls, err)
	}
}

func TestInstrumentSearchFiltersExchangeAndContractKind(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.instruments = append(broker.instruments, domain.Instrument{
		Token: 4, Exchange: "BFO", TradingSymbol: "SENSEX26JUL80000CE", Name: "SENSEX",
		InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 80_000, LotSize: 20,
	})
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}

	options := app.SearchInstruments("NIFTY", "NFO", "OPTION", 20)
	if len(options) != 2 || options[0].InstrumentType != "CE" || options[1].InstrumentType != "CE" {
		t.Fatalf("NFO option search = %#v", options)
	}
	futures := app.SearchInstruments("NIFTY", "NFO", "FUTURE", 20)
	if len(futures) != 1 || futures[0].InstrumentType != "FUT" {
		t.Fatalf("NFO future search = %#v", futures)
	}
	bfo := app.SearchInstruments("SENSEX", "BFO", "OPTION", 20)
	if len(bfo) != 1 || bfo[0].Exchange != "BFO" {
		t.Fatalf("BFO option search = %#v", bfo)
	}
	if invalid := app.SearchInstruments("NIFTY", "NSE", "", 20); len(invalid) != 0 {
		t.Fatalf("invalid exchange search = %#v", invalid)
	}
}

func TestOrderPriceMustMatchInstrumentTickSize(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.instruments[0].TickSize = 0.05
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := validRequest("NIFTY26JULFUT", "BUY", 50)
	request.OrderType = "LIMIT"
	request.Price = 100.03
	decision, _, err := app.Place(context.Background(), request)
	if err != nil || decision.Code != domain.CodeInvalidOrder || broker.placeCalls != 0 {
		t.Fatalf("decision=%#v calls=%d error=%v", decision, broker.placeCalls, err)
	}
}

func TestStandaloneUncoveredOptionSellRejected(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	decision, _, err := app.Place(context.Background(), request)
	if err != nil || decision.Code != domain.CodeUnhedgedExposure || decision.Allowed {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
	if broker.placeCalls != 0 {
		t.Fatalf("broker Place called %d times", broker.placeCalls)
	}
}

func TestPendingOrdersIncludedInOptionCoverage(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", Quantity: 50}}
	broker.orders = []domain.Order{{OrderID: "sell-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", TransactionType: "SELL", PendingQuantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	decision, _, err := app.Place(context.Background(), validRequest("NIFTY26JUL25000CE", "SELL", 50))
	if err != nil || decision.Code != domain.CodeUnhedgedExposure {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
}

func TestPendingBuyDoesNotCountAsOptionProtection(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.orders = []domain.Order{{OrderID: "buy-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", TransactionType: "BUY", PendingQuantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	decision, _, err := app.Place(context.Background(), validRequest("NIFTY26JUL25000CE", "SELL", 50))
	if err != nil || decision.Code != domain.CodeUnhedgedExposure || decision.Allowed {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
}

func TestRapidSellOrdersCannotReuseTheSameLongCoverage(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", Quantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	first := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	if decision, _, err := app.Place(context.Background(), first); err != nil || !decision.Allowed {
		t.Fatalf("first decision=%#v error=%v", decision, err)
	}
	second := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	second.IdempotencyKey = "second-sell-key-123"
	decision, _, err := app.Place(context.Background(), second)
	if err != nil || decision.Code != domain.CodeUnhedgedExposure || decision.Allowed || broker.placeCalls != 1 {
		t.Fatalf("second decision=%#v calls=%d error=%v", decision, broker.placeCalls, err)
	}
}

func TestOptionProtectionMustUseSameProduct(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", Quantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	request.Product = "NRML"
	decision, _, err := app.Place(context.Background(), request)
	if err != nil || decision.Code != domain.CodeUnhedgedExposure || decision.Allowed {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
}

func TestExplicitPositionExitMatchesInstrumentAndProduct(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{
		{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50},
		{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "NRML", Quantity: 50},
	}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ExitPosition(context.Background(), 1, "NRML"); err != nil {
		t.Fatal(err)
	}
	if len(broker.exitedProducts) != 1 || broker.exitedProducts[0] != "NRML" || broker.positions[0].Quantity != 50 || broker.positions[1].Quantity != 0 {
		t.Fatalf("exited=%v positions=%#v", broker.exitedProducts, broker.positions)
	}
}

func TestPartiallyAcceptedExitFailsClosedUntilReconciliation(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.exitErrAfterID = true
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	orderID, err := app.ExitPosition(context.Background(), 1, "MIS")
	if err == nil || orderID == "" || app.Snapshot().RuntimeStatus != domain.RuntimeDegraded {
		t.Fatalf("orderID=%q snapshot=%#v error=%v", orderID, app.Snapshot(), err)
	}
}

func TestExactLossLimitLocksCancelsAndLiquidates(t *testing.T) {
	app, broker, store := newTestService(t, -30_000)
	broker.orders = []domain.Order{{OrderID: "pending-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", PendingQuantity: 50}}
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: -30_000}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	snapshot := app.Snapshot()
	if snapshot.TradingStatus != domain.TradingLocked || snapshot.Liquidation != "COMPLETED" || snapshot.MTMPaise != domain.LossLimitPaise {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if broker.cancelCalls == 0 || broker.exitCalls == 0 {
		t.Fatalf("cancel calls=%d exit calls=%d", broker.cancelCalls, broker.exitCalls)
	}
	if store.record.Status != domain.TradingLocked || store.record.TriggerMTMPaise != domain.LossLimitPaise {
		t.Fatalf("stored lock = %#v", store.record)
	}
	decision, _, err := app.Place(context.Background(), validRequest("NIFTY26JULFUT", "SELL", 50))
	if err != nil || decision.Code != domain.CodeTradingLocked {
		t.Fatalf("locked decision = %#v, error = %v", decision, err)
	}
	unknown := validRequest("NOT-A-CONTRACT", "BUY", 1)
	unknown.IdempotencyKey = "locked-unknown-123"
	decision, _, err = app.Place(context.Background(), unknown)
	if err != nil || decision.Code != domain.CodeTradingLocked || decision.Message != domain.LockedMessage {
		t.Fatalf("locked malformed decision = %#v, error = %v", decision, err)
	}
}

func TestPositionThresholdLocksEvenWhenOrderBookIsUnavailable(t *testing.T) {
	app, broker, store := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: -30_000}}
	broker.ordersErr = errors.New("orders unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := app.Authenticate(ctx, "request"); err == nil {
		t.Fatal("Authenticate() error = nil with unavailable liquidation order book")
	}
	snapshot := app.Snapshot()
	if snapshot.TradingStatus != domain.TradingLocked || store.record.Status != domain.TradingLocked || store.record.TriggerMTMPaise != domain.LossLimitPaise {
		t.Fatalf("snapshot=%#v store=%#v", snapshot, store.record)
	}
}

func TestMonitoringFailureFailsClosedButCancellationWorks(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	ctx := context.Background()
	if err := app.Authenticate(ctx, "request"); err != nil {
		t.Fatal(err)
	}
	broker.orders = []domain.Order{{OrderID: "pending-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", PendingQuantity: 50}}
	broker.failPositions = true
	if err := app.PollOnce(ctx); err == nil {
		t.Fatal("PollOnce() error = nil, want monitoring failure")
	}
	decision, _, _ := app.Place(ctx, validRequest("NIFTY26JULFUT", "SELL", 50))
	if decision.Code != domain.CodeMonitoringDegraded {
		t.Fatalf("decision code = %s", decision.Code)
	}
	if err := app.Cancel(ctx, "pending-1"); err != nil {
		t.Fatalf("Cancel() while degraded: %v", err)
	}
}

func TestExpiredSessionWhileReadingOrdersRequiresAuthentication(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.ordersErr = domain.ErrNotAuthenticated
	if err := app.PollOnce(context.Background()); !errors.Is(err, domain.ErrNotAuthenticated) {
		t.Fatalf("PollOnce() error = %v", err)
	}
	snapshot := app.Snapshot()
	if snapshot.Authenticated || snapshot.RuntimeStatus != domain.RuntimeAuthRequired {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestValidatedBasketDeploysBuysBeforeSells(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	result, err := app.PlaceBasket(context.Background(), domain.BasketRequest{
		IdempotencyKey: "basket-key-123", Name: "Bull call spread",
		Legs: []domain.BasketLeg{
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
		},
	})
	if err != nil || result.Status != "COMPLETE" || result.MaxLossPaise != 250_000 {
		t.Fatalf("basket result = %#v, error = %v", result, err)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.placedSides) < 2 || broker.placedSides[0] != "BUY" || broker.placedSides[1] != "SELL" {
		t.Fatalf("placed sides = %#v, want BUY then SELL", broker.placedSides)
	}
}

func TestBasketRollbackNeverRemovesLongProtectionWhenShortCloseFails(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.partialSell = true
	broker.failShortClose = true
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	result, err := app.PlaceBasket(context.Background(), domain.BasketRequest{
		IdempotencyKey: "basket-rollback-123", Name: "partial spread",
		Legs: []domain.BasketLeg{
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
		},
	})
	if err == nil || result.Status != "ATTENTION_REQUIRED" || app.Snapshot().RuntimeStatus != domain.RuntimeDegraded {
		t.Fatalf("result=%#v snapshot=%#v error=%v", result, app.Snapshot(), err)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, request := range broker.placedRequests {
		if request.OrderType == "MARKET" && request.TransactionType == "SELL" {
			t.Fatalf("protective BUY was unwound after short close failed: %#v", broker.placedRequests)
		}
	}
}

func TestBasketRejectsResultingUncoveredPortfolio(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", InstrumentToken: 3, Product: "MIS", Quantity: -50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	_, err := app.PlaceBasket(context.Background(), domain.BasketRequest{
		IdempotencyKey: "basket-portfolio-123", Name: "would remain uncovered",
		Legs: []domain.BasketLeg{
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
		},
	})
	if err == nil || broker.placeCalls != 0 {
		t.Fatalf("basket error=%v placeCalls=%d", err, broker.placeCalls)
	}
}

func TestUncertainOrderIsNotResubmittedWithSameIdempotencyKey(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.failPlace = true
	request := validRequest("NIFTY26JULFUT", "BUY", 50)
	request.IdempotencyKey = "uncertain-order-123"
	if _, _, err := app.Place(context.Background(), request); err == nil {
		t.Fatal("first placement error = nil")
	}
	decision, orderID, err := app.Place(context.Background(), request)
	if err != nil || decision.Code != domain.CodeIdempotentReplay || decision.Allowed || orderID != "" || broker.placeCalls != 1 {
		t.Fatalf("decision=%#v orderID=%q calls=%d error=%v", decision, orderID, broker.placeCalls, err)
	}
}

func TestInvalidModificationDoesNotConsumeIdempotencyKey(t *testing.T) {
	app, broker, store := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := domain.ModifyRequest{IdempotencyKey: "modify-key-123", Quantity: 50, OrderType: "MARKET", Validity: "DAY"}
	decision, _, err := app.Modify(context.Background(), "missing", request)
	if err != nil || decision.Code != domain.CodeInvalidOrder {
		t.Fatalf("invalid modification decision=%#v error=%v", decision, err)
	}
	if _, exists := store.idempotency[request.IdempotencyKey]; exists {
		t.Fatal("invalid modification consumed its idempotency key")
	}
	broker.orders = []domain.Order{{OrderID: "order-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", OrderType: "MARKET", TransactionType: "SELL", Validity: "DAY", Quantity: 50, PendingQuantity: 50}}
	decision, orderID, err := app.Modify(context.Background(), "order-1", request)
	if err != nil || !decision.Allowed || orderID != "modified-1" || broker.modifyCalls != 1 {
		t.Fatalf("valid modification decision=%#v orderID=%q calls=%d error=%v", decision, orderID, broker.modifyCalls, err)
	}
}

func TestLiquidationDoesNotTreatOldCompletedExitAsCurrent(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: -30_000}}
	broker.orders = []domain.Order{{OrderID: "old-exit", Variety: "regular", Status: "COMPLETE", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", Tag: forcedExitTag}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	if broker.exitCalls != 1 || app.Snapshot().Liquidation != "COMPLETED" {
		t.Fatalf("exitCalls=%d snapshot=%#v", broker.exitCalls, app.Snapshot())
	}
}

func TestLockedAccountResumesLiquidationForNewExposure(t *testing.T) {
	app, broker, store := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.gate.Lock()
	app.trading = domain.TradingLocked
	app.lockRecord = domain.LockRecord{Status: domain.TradingLocked, UnlockAt: app.now().Add(time.Hour), LiquidationState: "COMPLETED"}
	store.record = app.lockRecord
	app.gate.Unlock()
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	if err := app.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.exitCalls != 1 || app.Snapshot().Liquidation != "COMPLETED" {
		t.Fatalf("exitCalls=%d snapshot=%#v", broker.exitCalls, app.Snapshot())
	}
}

func TestScheduledUnlockWaitsForCompletedLiquidation(t *testing.T) {
	app, _, store := newTestService(t, 0)
	app.gate.Lock()
	app.trading = domain.TradingLocked
	app.lockRecord = domain.LockRecord{Status: domain.TradingLocked, UnlockAt: app.now().Add(-time.Minute), LiquidationState: "RETRYING"}
	store.record = app.lockRecord
	app.gate.Unlock()
	if err := app.MaybeUnlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.Snapshot().TradingStatus != domain.TradingLocked || store.record.Status != domain.TradingLocked {
		t.Fatalf("incomplete liquidation unlocked: snapshot=%#v store=%#v", app.Snapshot(), store.record)
	}
	app.gate.Lock()
	app.lockRecord.LiquidationState = "COMPLETED"
	app.gate.Unlock()
	if err := app.MaybeUnlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.Snapshot().TradingStatus != domain.TradingActive || store.record.Status != domain.TradingActive {
		t.Fatalf("completed liquidation remained locked: snapshot=%#v store=%#v", app.Snapshot(), store.record)
	}
}

func newTestService(t *testing.T, mtm float64) (*Service, *fakeBroker, *fakeStore) {
	t.Helper()
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, mustLocation(t))
	path := filepath.Join(t.TempDir(), "calendar.json")
	if err := os.WriteFile(path, []byte(`{"year":2026,"holidays":[],"special_trading_days":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cal, err := calendar.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	broker := &fakeBroker{mtm: mtm, instruments: []domain.Instrument{
		{Token: 1, Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Name: "NIFTY", InstrumentType: "FUT", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), LotSize: 50},
		{Token: 2, Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Name: "NIFTY", InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 25_000, LotSize: 50},
		{Token: 3, Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Name: "NIFTY", InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 25_100, LotSize: 50},
	}}
	store := &fakeStore{record: domain.LockRecord{Status: domain.TradingActive}, idempotency: map[string]string{}}
	app, err := New(context.Background(), broker, store, cal, log.New(io.Discard, "", 0), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return app, broker, store
}

func validRequest(symbol, side string, quantity int) domain.OrderRequest {
	return domain.OrderRequest{IdempotencyKey: "order-key-123", Variety: "regular", Exchange: "NFO", TradingSymbol: symbol, Product: "MIS", OrderType: "MARKET", TransactionType: side, Validity: "DAY", Quantity: quantity}
}

func mustLocation(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	return location
}

type fakeBroker struct {
	mu             sync.Mutex
	mtm            float64
	positions      []domain.Position
	orders         []domain.Order
	instruments    []domain.Instrument
	placeCalls     int
	cancelCalls    int
	exitCalls      int
	exitedProducts []string
	exitErrAfterID bool
	modifyCalls    int
	failPositions  bool
	placedSides    []string
	placedRequests []domain.OrderRequest
	failPlace      bool
	partialSell    bool
	failShortClose bool
	ordersErr      error
}

func (f *fakeBroker) LoginURL(string) string { return "https://example.invalid/login" }
func (f *fakeBroker) GenerateSession(context.Context, string) (domain.Session, error) {
	return domain.Session{UserID: "test", AccessToken: "not-persisted"}, nil
}
func (f *fakeBroker) Positions(context.Context) ([]domain.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPositions {
		return nil, errors.New("positions unavailable")
	}
	if len(f.positions) == 0 && f.mtm != 0 {
		return []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: f.mtm}}, nil
	}
	return append([]domain.Position(nil), f.positions...), nil
}
func (f *fakeBroker) Orders(context.Context) ([]domain.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ordersErr != nil {
		return nil, f.ordersErr
	}
	return append([]domain.Order(nil), f.orders...), nil
}
func (f *fakeBroker) Instruments(_ context.Context, exchange string) ([]domain.Instrument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []domain.Instrument
	for _, instrument := range f.instruments {
		if instrument.Exchange == exchange {
			result = append(result, instrument)
		}
	}
	return result, nil
}
func (f *fakeBroker) Place(_ context.Context, request domain.OrderRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placeCalls++
	f.placedRequests = append(f.placedRequests, request)
	if f.failPlace || (f.failShortClose && request.OrderType == "MARKET" && request.TransactionType == "BUY") {
		return "", errors.New("placement failed")
	}
	orderID := fmt.Sprintf("placed-%d", f.placeCalls)
	f.placedSides = append(f.placedSides, request.TransactionType)
	status, filled := "COMPLETE", request.Quantity
	if f.partialSell && request.OrderType == "LIMIT" && request.TransactionType == "SELL" {
		status, filled = "CANCELLED", request.Quantity/2
	}
	f.orders = append(f.orders, domain.Order{
		OrderID: orderID, Variety: "regular", Status: status, Exchange: request.Exchange,
		TradingSymbol: request.TradingSymbol, Product: request.Product, OrderType: request.OrderType,
		TransactionType: request.TransactionType, Validity: request.Validity, Quantity: request.Quantity,
		FilledQuantity: filled, Tag: request.Tag,
	})
	return orderID, nil
}

func (f *fakeBroker) Modify(context.Context, string, domain.ModifyRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modifyCalls++
	return "modified-1", nil
}
func (f *fakeBroker) Cancel(_ context.Context, _ string, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	for index := range f.orders {
		if f.orders[index].OrderID == orderID {
			f.orders[index].PendingQuantity = 0
			f.orders[index].Status = "CANCELLED"
		}
	}
	return nil
}
func (f *fakeBroker) ExitPosition(_ context.Context, position domain.Position) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitCalls++
	f.exitedProducts = append(f.exitedProducts, position.Product)
	for index := range f.positions {
		if f.positions[index].InstrumentToken == position.InstrumentToken && f.positions[index].Product == position.Product {
			f.positions[index].Quantity = 0
		}
	}
	f.mtm = 0
	f.orders = append(f.orders, domain.Order{OrderID: "exit-1", Variety: "regular", Status: "COMPLETE", Exchange: position.Exchange, TradingSymbol: position.TradingSymbol, Product: position.Product, Tag: forcedExitTag})
	if f.exitErrAfterID {
		return "exit-1", errors.New("one autoslice child failed")
	}
	return "exit-1", nil
}

type fakeStore struct {
	mu          sync.Mutex
	record      domain.LockRecord
	events      []domain.AuditEvent
	idempotency map[string]string
}

func (f *fakeStore) CurrentLock(context.Context) (domain.LockRecord, error) { return f.record, nil }
func (f *fakeStore) Lock(_ context.Context, record domain.LockRecord, event domain.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = record
	f.events = append(f.events, event)
	return nil
}
func (f *fakeStore) Unlock(_ context.Context, _ time.Time, event domain.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = domain.LockRecord{Status: domain.TradingActive}
	f.events = append(f.events, event)
	return nil
}
func (f *fakeStore) UpdateLiquidation(_ context.Context, state, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record.LiquidationState, f.record.LastError = state, lastError
	return nil
}
func (f *fakeStore) AppendAudit(_ context.Context, event domain.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}
func (f *fakeStore) ListAudit(context.Context, int) ([]domain.AuditEvent, error) {
	return append([]domain.AuditEvent(nil), f.events...), nil
}
func (f *fakeStore) LookupIdempotency(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.idempotency[key]
	return value, ok, nil
}
func (f *fakeStore) ReserveIdempotency(_ context.Context, key, reference string, _ time.Time) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value, ok := f.idempotency[key]; ok {
		return value, false, nil
	}
	f.idempotency[key] = reference
	return "", true, nil
}
func (f *fakeStore) CompleteIdempotency(_ context.Context, key, reference, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idempotency[key] != reference {
		return errors.New("reservation changed")
	}
	f.idempotency[key] = result
	return nil
}
func (f *fakeStore) Close() error { return nil }
