package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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
	if err != nil || decision.Code != domain.CodeMonitoringDegraded || replayID != "" || broker.placeCalls != 1 {
		t.Fatalf("pre-reconciliation replay decision=%#v orderID=%q calls=%d error=%v", decision, replayID, broker.placeCalls, err)
	}
}

func TestInstrumentSearchFiltersExchangeAndContractKind(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.instruments = append(broker.instruments, domain.Instrument{
		Token: 4, Exchange: "BFO", TradingSymbol: "SENSEX26JUL80000CE", Name: "SENSEX",
		InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 80_000, TickSize: 0.05, LotSize: 20,
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

func TestAuthenticationPersistsAndRestartRestoresSameDaySession(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	cache := &fakeSessionCache{}
	app.SetSessionCache(cache)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	if cache.saved.AccessToken != "not-persisted" || cache.savedAt.IsZero() {
		t.Fatalf("saved session=%#v at=%v", cache.saved, cache.savedAt)
	}

	restored, restoredBroker, _ := newTestService(t, 0)
	restoredCache := &fakeSessionCache{loaded: cache.saved}
	restored.SetSessionCache(restoredCache)
	if err := restored.RestoreCachedSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restoredBroker.restoreCalls != 1 || !restored.Snapshot().Authenticated || restored.Snapshot().RuntimeStatus != domain.RuntimeReady {
		t.Fatalf("restoreCalls=%d snapshot=%#v", restoredBroker.restoreCalls, restored.Snapshot())
	}
	if broker.restoreCalls != 0 {
		t.Fatalf("fresh authentication unexpectedly restored a token: %d", broker.restoreCalls)
	}
}

func TestRejectedCachedSessionIsDeletedAndStartsAuthRequired(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positionsErr = domain.ErrNotAuthenticated
	cache := &fakeSessionCache{loaded: domain.Session{UserID: "test", AccessToken: "expired"}}
	app.SetSessionCache(cache)
	if err := app.RestoreCachedSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cache.deleteCalls != 1 || app.Snapshot().RuntimeStatus != domain.RuntimeAuthRequired || app.Snapshot().Authenticated {
		t.Fatalf("deleteCalls=%d snapshot=%#v", cache.deleteCalls, app.Snapshot())
	}
}

func TestCachedSessionNetworkFailureStartsDegradedAndRetriesLater(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positionsErr = errors.New("temporary network failure")
	cache := &fakeSessionCache{loaded: domain.Session{UserID: "test", AccessToken: "same-day-token"}}
	app.SetSessionCache(cache)
	if err := app.RestoreCachedSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cache.deleteCalls != 0 || !app.Snapshot().Authenticated || app.Snapshot().RuntimeStatus != domain.RuntimeDegraded {
		t.Fatalf("deleteCalls=%d snapshot=%#v", cache.deleteCalls, app.Snapshot())
	}
}

func TestDashboardStateContainsOneMonitorSnapshot(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: 125.25}}
	broker.orders = []domain.Order{{OrderID: "order-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "SELL", Quantity: 50, PendingQuantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}

	state := app.DashboardState()
	if state.Status.RuntimeStatus != domain.RuntimeReady || state.Status.MTMPaise != 12_525 {
		t.Fatalf("status = %#v", state.Status)
	}
	if len(state.Positions) != 1 || state.Positions[0].TradingSymbol != "NIFTY26JULFUT" {
		t.Fatalf("positions = %#v", state.Positions)
	}
	if len(state.Orders) != 1 || state.Orders[0].OrderID != "order-1" || state.Status.PendingOrders != 1 {
		t.Fatalf("orders = %#v, pending = %d", state.Orders, state.Status.PendingOrders)
	}
}

func TestDashboardAppliesLiveTicksWithoutChangingConfirmedRiskMTM(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS",
		Quantity: 50, Multiplier: 1, BuyM2M: 10_100, LastPrice: 200, M2M: -100,
	}}
	broker.trades = []domain.Trade{{TradeID: "trade-1", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "BUY", Quantity: 50, AveragePrice: 202}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.setMarketStatus(true)
	tickTime := app.now().Add(time.Second)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 199, ReceivedAt: tickTime})

	state := app.DashboardState()
	if state.Status.MTMPaise != -10_000 {
		t.Fatalf("confirmed MTM = %d, want -10000", state.Status.MTMPaise)
	}
	if state.Status.LiveMTMPaise == nil || *state.Status.LiveMTMPaise != -15_000 {
		t.Fatalf("live MTM = %v, want -15000", state.Status.LiveMTMPaise)
	}
	if state.Status.MarketData != "LIVE" || state.Status.MarketDataAt == nil || !state.Status.MarketDataAt.Equal(tickTime) {
		t.Fatalf("market status = %#v", state.Status)
	}
	if len(state.Positions) != 1 || state.Positions[0].LastPrice != 199 || state.Positions[0].M2M != -150 {
		t.Fatalf("live positions = %#v", state.Positions)
	}

	app.setMarketStatus(false)
	disconnected := app.DashboardState()
	if disconnected.Status.LiveMTMPaise != nil || disconnected.Status.MarketData != "DISCONNECTED" || disconnected.Positions[0].LastPrice != 200 {
		t.Fatalf("disconnected dashboard = %#v", disconnected)
	}
}

func TestDashboardRetainsLatestStreamLTPAcrossPositionPolls(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS",
		Quantity: 50, Multiplier: 1, BuyM2M: 10_100, LastPrice: 200, M2M: -100,
	}}
	broker.trades = []domain.Trade{{TradeID: "trade-1", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "BUY", Quantity: 50, AveragePrice: 202}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.setMarketStatus(true)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 199, ReceivedAt: app.now().Add(-time.Second)})

	state := app.DashboardState()
	if state.Status.LiveMTMPaise == nil || *state.Status.LiveMTMPaise != -15_000 || state.Status.MarketData != "LIVE" || state.Positions[0].LastPrice != 199 {
		t.Fatalf("latest streamed LTP was discarded after a position poll: %#v", state)
	}
}

func TestPaidMarketStreamIsRequiredBeforeNewExposure(t *testing.T) {
	app, _, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.gate.Lock()
	app.marketRequired = true
	app.gate.Unlock()
	app.setMarketStatus(false)

	decision, _, err := app.Place(context.Background(), validRequest("NIFTY26JULFUT", "BUY", 50))
	if err != nil || decision.Code != domain.CodeMonitoringDegraded || decision.Allowed {
		t.Fatalf("decision=%#v error=%v", decision, err)
	}
	if snapshot := app.Snapshot(); snapshot.RuntimeStatus != domain.RuntimeDegraded || snapshot.MarketData != "DISCONNECTED" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestOpenPositionRequiresCompleteLiveTicks(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS",
		Quantity: 50, Multiplier: 1, BuyM2M: 10_100, LastPrice: 200, M2M: -100,
	}}
	broker.trades = []domain.Trade{{TradeID: "trade-1", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "BUY", Quantity: 50, AveragePrice: 202}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.gate.Lock()
	app.marketRequired = true
	app.gate.Unlock()
	app.setMarketStatus(true)
	if err := app.evaluateLiveRisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := app.Snapshot(); snapshot.RuntimeStatus != domain.RuntimeDegraded || snapshot.MarketData != "AWAITING_TICKS" {
		t.Fatalf("snapshot before tick=%#v", snapshot)
	}

	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 199, ReceivedAt: app.now()})
	if err := app.evaluateLiveRisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := app.Snapshot(); snapshot.RuntimeStatus != domain.RuntimeReady || snapshot.LiveMTMPaise == nil || *snapshot.LiveMTMPaise != -15_000 {
		t.Fatalf("snapshot after tick=%#v", snapshot)
	}
}

func TestLiveTickAtDailyLossLimitLocksImmediately(t *testing.T) {
	app, broker, store := newTestService(t, 0)
	broker.positions = []domain.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS",
		Quantity: 50, Multiplier: 1, BuyM2M: 35_000, LastPrice: 110, M2M: -29_500,
	}}
	broker.trades = []domain.Trade{{TradeID: "trade-1", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "BUY", Quantity: 50, AveragePrice: 700}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.gate.Lock()
	app.marketRequired = true
	app.gate.Unlock()
	app.setMarketStatus(true)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 100, ReceivedAt: app.now()})

	snapshot := app.Snapshot()
	if snapshot.TradingStatus != domain.TradingLocked || store.record.TriggerMTMPaise != domain.LossLimitPaise {
		t.Fatalf("snapshot=%#v lock=%#v", snapshot, store.record)
	}
}

func TestLiveMTMUsesTradesForFlatRealizedPositions(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{
		{Exchange: "NFO", TradingSymbol: "NIFTY26JUL24100PE", InstrumentToken: 1, Product: "NRML", Multiplier: 1, M2M: -2_000, BuyM2M: 3_000, SellM2M: 1_000},
		{Exchange: "NFO", TradingSymbol: "NIFTY26JUL24100CE", InstrumentToken: 2, Product: "NRML", Quantity: -10, Multiplier: 1, LastPrice: 102, M2M: 68},
	}
	broker.trades = []domain.Trade{
		{TradeID: "flat-buy", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JUL24100PE", InstrumentToken: 1, Product: "NRML", TransactionType: "BUY", Quantity: 100, AveragePrice: 240},
		{TradeID: "flat-sell", OrderID: "order-2", Exchange: "NFO", TradingSymbol: "NIFTY26JUL24100PE", InstrumentToken: 1, Product: "NRML", TransactionType: "SELL", Quantity: 100, AveragePrice: 230},
		{TradeID: "open-sell", OrderID: "order-3", Exchange: "NFO", TradingSymbol: "NIFTY26JUL24100CE", InstrumentToken: 2, Product: "NRML", TransactionType: "SELL", Quantity: 10, AveragePrice: 110},
	}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.setMarketStatus(true)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 2, LastPrice: 102, ReceivedAt: app.now()})

	state := app.DashboardState()
	if state.Status.LiveMTMPaise == nil || *state.Status.LiveMTMPaise != -92_000 {
		t.Fatalf("state=%#v, want live MTM -92000 paise", state)
	}
	if len(state.Positions) != 2 || state.Positions[0].M2M != -1_000 || state.Positions[1].M2M != 80 {
		t.Fatalf("trade-priced positions=%#v", state.Positions)
	}
}

func TestLiveMTMIncludesOvernightOpeningMarkAndTodayTrades(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "NRML",
		Quantity: -5, OvernightQty: -10, Multiplier: 1, ClosePrice: 100, LastPrice: 90, M2M: 150,
	}}
	broker.trades = []domain.Trade{{TradeID: "cover", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "NRML", TransactionType: "BUY", Quantity: 5, AveragePrice: 80}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.setMarketStatus(true)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 90, ReceivedAt: app.now()})
	if snapshot := app.Snapshot(); snapshot.LiveMTMPaise == nil || *snapshot.LiveMTMPaise != 15_000 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestTradePositionMismatchFailsClosed(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, Multiplier: 1, LastPrice: 200, M2M: 0}}
	broker.trades = []domain.Trade{{TradeID: "wrong-quantity", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", TransactionType: "BUY", Quantity: 49, AveragePrice: 200}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	app.gate.Lock()
	app.marketRequired = true
	app.gate.Unlock()
	app.setMarketStatus(true)
	app.applyMarketTick(domain.MarketTick{InstrumentToken: 1, LastPrice: 200, ReceivedAt: app.now()})
	if snapshot := app.Snapshot(); snapshot.RuntimeStatus != domain.RuntimeDegraded || snapshot.LiveMTMPaise != nil {
		t.Fatalf("inconsistent tradebook did not fail closed: %#v", snapshot)
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

func TestOrderBlockedWhenKiteTickSizeIsUnavailable(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.instruments[0].TickSize = 0
	if err := app.Authenticate(context.Background(), "request"); err == nil {
		t.Fatal("Authenticate() error = nil for invalid catalogue tick size")
	}
	if snapshot := app.Snapshot(); snapshot.RuntimeStatus != domain.RuntimeDegraded || broker.placeCalls != 0 {
		t.Fatalf("snapshot=%#v calls=%d", snapshot, broker.placeCalls)
	}
}

func TestStandaloneNakedOptionSellAllowed(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	decision, _, err := app.Place(context.Background(), request)
	if err != nil || decision.Code != domain.CodeApproved || !decision.Allowed {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
	if broker.placeCalls != 1 {
		t.Fatalf("broker Place called %d times", broker.placeCalls)
	}
}

func TestPendingSellDoesNotRequireLongOptionCoverage(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.orders = []domain.Order{{OrderID: "sell-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", TransactionType: "SELL", Quantity: 50, PendingQuantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	request := validRequest("NIFTY26JUL25000CE", "SELL", 50)
	decision, _, err := app.Place(context.Background(), request)
	if err != nil || !decision.Allowed || broker.placeCalls != 1 {
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
	if err != nil || decision.Code != domain.CodeMonitoringDegraded || decision.Allowed || broker.placeCalls != 1 {
		t.Fatalf("second decision=%#v calls=%d error=%v", decision, broker.placeCalls, err)
	}
}

func TestBasketTagInTagsArrayCannotBeModified(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.orders = []domain.Order{{OrderID: "basket-leg", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", InstrumentToken: 2, Product: "MIS", OrderType: "LIMIT", TransactionType: "SELL", Validity: "IOC", Quantity: 50, PendingQuantity: 50, Tags: []string{"tgb123456"}}}
	decision, _, err := app.Modify(context.Background(), "basket-leg", domain.ModifyRequest{IdempotencyKey: "modify-basket-123", Quantity: 50, OrderType: "LIMIT", Validity: "IOC", Price: 100})
	if err != nil || decision.Code != domain.CodeInvalidOrder || decision.Allowed || broker.modifyCalls != 0 {
		t.Fatalf("decision=%#v calls=%d error=%v", decision, broker.modifyCalls, err)
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

func TestExplicitPositionExitRequiresExactInstrumentTokenMetadata(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 999, Product: "MIS", Quantity: 50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ExitPosition(context.Background(), 999, "MIS"); err == nil || broker.exitCalls != 0 {
		t.Fatalf("exitCalls=%d error=%v", broker.exitCalls, err)
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

func TestRepeatedExplicitExitDoesNotDuplicateUncertainSubmission(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.exitInvisible = true
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	firstID, firstErr := app.ExitPosition(context.Background(), 1, "MIS")
	secondID, secondErr := app.ExitPosition(context.Background(), 1, "MIS")
	if firstErr != nil || firstID == "" || secondErr == nil || secondID != firstID || broker.exitCalls != 1 {
		t.Fatalf("first=(%q,%v) second=(%q,%v) calls=%d", firstID, firstErr, secondID, secondErr, broker.exitCalls)
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

func TestInvalidBrokerMTMDegradesWithoutReplacingLastValidValue(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50, M2M: math.NaN()}}
	if err := app.PollOnce(context.Background()); err == nil {
		t.Fatal("PollOnce() error = nil")
	}
	snapshot := app.Snapshot()
	if snapshot.RuntimeStatus != domain.RuntimeDegraded || snapshot.MTMPaise != 0 || snapshot.TradingStatus != domain.TradingActive {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestExpiredSessionDuringPlacementReturnsAuthDecision(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.placeErr = domain.ErrNotAuthenticated
	decision, _, err := app.Place(context.Background(), validRequest("NIFTY26JULFUT", "BUY", 50))
	if !errors.Is(err, domain.ErrNotAuthenticated) || decision.Code != domain.CodeAuthRequired || decision.Allowed {
		t.Fatalf("decision=%#v error=%v", decision, err)
	}
	if snapshot := app.Snapshot(); snapshot.Authenticated || snapshot.RuntimeStatus != domain.RuntimeAuthRequired {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestLiquidationDoesNotDuplicateInvisibleSubmittedExit(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.exitInvisible = true
	if complete, err := app.liquidationPass(context.Background()); complete || err != nil {
		t.Fatalf("first pass complete=%v error=%v", complete, err)
	}
	if complete, err := app.liquidationPass(context.Background()); complete || err == nil {
		t.Fatalf("second pass complete=%v error=%v", complete, err)
	}
	if complete, err := app.liquidationPass(context.Background()); complete || err == nil {
		t.Fatalf("third pass complete=%v error=%v", complete, err)
	}
	if broker.exitCalls != 1 {
		t.Fatalf("exit calls=%d, want one uncertain submission", broker.exitCalls)
	}
}

func TestInvisibleExitIntentIsNotClearedOnlyBecausePositionLooksFlat(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.exitInvisible = true
	if complete, err := app.liquidationPass(context.Background()); complete || err != nil {
		t.Fatalf("first pass complete=%v error=%v", complete, err)
	}
	broker.positions = nil
	for attempt := 0; attempt < 2; attempt++ {
		if complete, err := app.liquidationPass(context.Background()); complete || err == nil {
			t.Fatalf("uncertain pass %d complete=%v error=%v", attempt+1, complete, err)
		}
	}
	if broker.exitCalls != 1 {
		t.Fatalf("exit calls=%d", broker.exitCalls)
	}
}

func TestUncertainExitIntentSurvivesServiceRestart(t *testing.T) {
	app, broker, store := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.exitInvisible = true
	if complete, err := app.liquidationPass(context.Background()); complete || err != nil {
		t.Fatalf("first pass complete=%v error=%v", complete, err)
	}
	restarted, err := New(context.Background(), broker, store, app.calendar, log.New(io.Discard, "", 0), app.now)
	if err != nil {
		t.Fatal(err)
	}
	if complete, err := restarted.liquidationPass(context.Background()); complete || err == nil {
		t.Fatalf("restarted pass complete=%v error=%v", complete, err)
	}
	if broker.exitCalls != 1 {
		t.Fatalf("exit calls=%d after restart", broker.exitCalls)
	}
}

func TestLiquidationRecognisesAutosliceChildByParentID(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	key := positionKeyFor("NFO", "NIFTY26JULFUT", "MIS")
	app.forcedExits[key] = "parent-1"
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", Quantity: 50}}
	broker.orders = []domain.Order{{OrderID: "child-1", ParentOrderID: "parent-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", Quantity: 50, PendingQuantity: 50}}
	if complete, err := app.liquidationPass(context.Background()); complete || err != nil {
		t.Fatalf("complete=%v error=%v", complete, err)
	}
	if broker.exitCalls != 0 || broker.cancelCalls != 0 {
		t.Fatalf("exit calls=%d cancel calls=%d", broker.exitCalls, broker.cancelCalls)
	}
}

func TestLiquidationCancelsForcedExitAfterPositionIsFlat(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.orders = []domain.Order{{OrderID: "orphan-exit", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", Quantity: 50, PendingQuantity: 50, Tag: forcedExitTag}}
	if complete, err := app.liquidationPass(context.Background()); complete || err != nil {
		t.Fatalf("first pass complete=%v error=%v", complete, err)
	}
	if broker.cancelCalls != 1 {
		t.Fatalf("cancel calls=%d", broker.cancelCalls)
	}
	if complete, err := app.liquidationPass(context.Background()); !complete || err != nil {
		t.Fatalf("second pass complete=%v error=%v", complete, err)
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

func TestBasketAllowsExistingNakedShortPortfolio(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	broker.positions = []domain.Position{{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", InstrumentToken: 3, Product: "MIS", Quantity: -50}}
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	result, err := app.PlaceBasket(context.Background(), domain.BasketRequest{
		IdempotencyKey: "basket-portfolio-123", Name: "would remain uncovered",
		Legs: []domain.BasketLeg{
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
		},
	})
	if err != nil || result.Status != "COMPLETE" || broker.placeCalls != 2 {
		t.Fatalf("result=%#v error=%v placeCalls=%d", result, err, broker.placeCalls)
	}
}

func TestMarketBasketUsesProtectedMarketIOCAndDoesNotClaimPlannedLoss(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	result, err := app.PlaceBasket(context.Background(), domain.BasketRequest{
		IdempotencyKey: "market-basket-123", Name: "market spread", OrderType: "MARKET",
		Legs: []domain.BasketLeg{
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50},
			{Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50},
		},
	})
	if err != nil || result.Status != "COMPLETE" || result.MaxLossKnown || result.MaxLossPaise != 0 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.placedRequests) != 2 {
		t.Fatalf("placed=%#v", broker.placedRequests)
	}
	for _, request := range broker.placedRequests {
		if request.OrderType != "MARKET" || request.Validity != "IOC" || request.Price != 0 {
			t.Fatalf("market basket request=%#v", request)
		}
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
	broker.orders = []domain.Order{{OrderID: "order-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", OrderType: "MARKET", TransactionType: "SELL", Validity: "DAY", Quantity: 50, PendingQuantity: 50}}
	decision, orderID, err := app.Modify(context.Background(), "order-1", request)
	if err != nil || !decision.Allowed || orderID != "modified-1" || broker.modifyCalls != 1 {
		t.Fatalf("valid modification decision=%#v orderID=%q calls=%d error=%v", decision, orderID, broker.modifyCalls, err)
	}
}

func TestModificationDoesNotInheritOmittedOrderFields(t *testing.T) {
	app, broker, _ := newTestService(t, 0)
	if err := app.Authenticate(context.Background(), "request"); err != nil {
		t.Fatal(err)
	}
	broker.orders = []domain.Order{{OrderID: "order-1", Variety: "regular", Status: "OPEN", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", InstrumentToken: 1, Product: "MIS", OrderType: "LIMIT", TransactionType: "SELL", Validity: "DAY", Quantity: 50, PendingQuantity: 50}}
	request := domain.ModifyRequest{IdempotencyKey: "modify-required-123", Quantity: 50, Price: 100}
	decision, _, err := app.Modify(context.Background(), "order-1", request)
	if err != nil || decision.Code != domain.CodeInvalidOrder || decision.Allowed || broker.modifyCalls != 0 {
		t.Fatalf("decision=%#v calls=%d error=%v", decision, broker.modifyCalls, err)
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
		{Token: 1, Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Name: "NIFTY", InstrumentType: "FUT", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), TickSize: 0.05, LotSize: 50},
		{Token: 2, Exchange: "NFO", TradingSymbol: "NIFTY26JUL25000CE", Name: "NIFTY", InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 25_000, TickSize: 0.05, LotSize: 50},
		{Token: 3, Exchange: "NFO", TradingSymbol: "NIFTY26JUL25100CE", Name: "NIFTY", InstrumentType: "CE", Expiry: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), Strike: 25_100, TickSize: 0.05, LotSize: 50},
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
	trades         []domain.Trade
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
	placeErr       error
	partialSell    bool
	failShortClose bool
	positionsErr   error
	ordersErr      error
	tradesErr      error
	exitInvisible  bool
	restoreCalls   int
}

func (f *fakeBroker) LoginURL(string) string { return "https://example.invalid/login" }
func (f *fakeBroker) GenerateSession(context.Context, string) (domain.Session, error) {
	return domain.Session{UserID: "test", AccessToken: "not-persisted"}, nil
}
func (f *fakeBroker) RestoreSession(_ context.Context, session domain.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if session.AccessToken == "" {
		return errors.New("empty session")
	}
	f.restoreCalls++
	return nil
}
func (f *fakeBroker) Positions(context.Context) ([]domain.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.positionsErr != nil {
		return nil, f.positionsErr
	}
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
func (f *fakeBroker) Trades(context.Context) ([]domain.Trade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tradesErr != nil {
		return nil, f.tradesErr
	}
	return append([]domain.Trade(nil), f.trades...), nil
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
	if f.placeErr != nil {
		return "", f.placeErr
	}
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
	if f.exitInvisible {
		return "exit-invisible", nil
	}
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
	intents     map[string]domain.LiquidationIntent
}

type fakeSessionCache struct {
	loaded      domain.Session
	loadErr     error
	saved       domain.Session
	savedAt     time.Time
	saveErr     error
	deleteCalls int
}

func (f *fakeSessionCache) Load(context.Context) (domain.Session, error) {
	if f.loadErr != nil {
		return domain.Session{}, f.loadErr
	}
	if f.loaded.AccessToken == "" {
		return domain.Session{}, domain.ErrNoCachedSession
	}
	return f.loaded, nil
}

func (f *fakeSessionCache) Save(_ context.Context, session domain.Session, issuedAt time.Time) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = session
	f.savedAt = issuedAt
	return nil
}

func (f *fakeSessionCache) Delete(context.Context) error {
	f.deleteCalls++
	f.loaded = domain.Session{}
	return nil
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
func (f *fakeStore) ListLiquidationIntents(context.Context) ([]domain.LiquidationIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]domain.LiquidationIntent, 0, len(f.intents))
	for _, intent := range f.intents {
		result = append(result, intent)
	}
	return result, nil
}
func (f *fakeStore) PutLiquidationIntent(_ context.Context, intent domain.LiquidationIntent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intents == nil {
		f.intents = make(map[string]domain.LiquidationIntent)
	}
	f.intents[intent.PositionKey] = intent
	return nil
}
func (f *fakeStore) DeleteLiquidationIntent(_ context.Context, positionKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.intents, positionKey)
	return nil
}
func (f *fakeStore) Close() error { return nil }
