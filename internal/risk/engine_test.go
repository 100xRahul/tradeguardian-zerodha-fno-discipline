package risk

import (
	"math"
	"testing"
	"time"

	"tradeguardian/internal/domain"
)

func TestNonFiniteOrderPriceRejected(t *testing.T) {
	engine := New(nil)
	request := domain.OrderRequest{Variety: "regular", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", OrderType: "MARKET", TransactionType: "BUY", Validity: "DAY", Quantity: 50, InstrumentType: "FUT"}
	request.Price = math.NaN()
	decision := engine.Evaluate(request, domain.TradingActive, domain.RuntimeReady, 0)
	if decision.Allowed || decision.Code != domain.CodeInvalidOrder {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestEvaluateOrderPolicy(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	engine := New(func() time.Time { return now })
	base := domain.OrderRequest{Variety: "regular", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT", Product: "MIS", OrderType: "MARKET", TransactionType: "BUY", Validity: "DAY", Quantity: 50, InstrumentType: "FUT"}
	tests := []struct {
		name    string
		change  func(*domain.OrderRequest)
		trading domain.TradingStatus
		runtime domain.RuntimeStatus
		allowed bool
		code    domain.DecisionCode
	}{
		{name: "future buy", change: func(*domain.OrderRequest) {}, trading: domain.TradingActive, runtime: domain.RuntimeReady, allowed: true, code: domain.CodeApproved},
		{name: "option sell", change: func(o *domain.OrderRequest) { o.InstrumentType, o.TransactionType = "CE", "SELL" }, trading: domain.TradingActive, runtime: domain.RuntimeReady, allowed: true, code: domain.CodeApproved},
		{name: "option buy blocked", change: func(o *domain.OrderRequest) { o.InstrumentType = "PE" }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeOptionBuyForbidden},
		{name: "locked", change: func(*domain.OrderRequest) {}, trading: domain.TradingLocked, runtime: domain.RuntimeReady, code: domain.CodeTradingLocked},
		{name: "degraded", change: func(*domain.OrderRequest) {}, trading: domain.TradingActive, runtime: domain.RuntimeDegraded, code: domain.CodeMonitoringDegraded},
		{name: "unknown runtime fails closed", change: func(*domain.OrderRequest) {}, trading: domain.TradingActive, runtime: domain.RuntimeStatus("UNKNOWN"), code: domain.CodeMonitoringDegraded},
		{name: "unknown trading state fails closed", change: func(*domain.OrderRequest) {}, trading: domain.TradingStatus("UNKNOWN"), runtime: domain.RuntimeReady, code: domain.CodeMonitoringDegraded},
		{name: "unsupported exchange", change: func(o *domain.OrderRequest) { o.Exchange = "MCX" }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeUnsupportedSegment},
		{name: "unsupported variety", change: func(o *domain.OrderRequest) { o.Variety = "amo" }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeUnsupportedVariety},
		{name: "SL needs limit price", change: func(o *domain.OrderRequest) { o.OrderType, o.TriggerPrice, o.Price = "SL", 100, 0 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeInvalidOrder},
		{name: "MARKET rejects price", change: func(o *domain.OrderRequest) { o.Price = 100 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeInvalidOrder},
		{name: "MARKET rejects trigger", change: func(o *domain.OrderRequest) { o.TriggerPrice = 100 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeInvalidOrder},
		{name: "LIMIT rejects trigger", change: func(o *domain.OrderRequest) { o.OrderType, o.Price, o.TriggerPrice = "LIMIT", 100, 99 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeInvalidOrder},
		{name: "SL-M rejects price", change: func(o *domain.OrderRequest) { o.OrderType, o.Price, o.TriggerPrice = "SL-M", 100, 99 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, code: domain.CodeInvalidOrder},
		{name: "valid SL", change: func(o *domain.OrderRequest) { o.OrderType, o.Price, o.TriggerPrice = "SL", 100, 99 }, trading: domain.TradingActive, runtime: domain.RuntimeReady, allowed: true, code: domain.CodeApproved},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.change(&request)
			decision := engine.Evaluate(request, test.trading, test.runtime, 123)
			if decision.Allowed != test.allowed || decision.Code != test.code {
				t.Fatalf("decision = allowed %v code %s, want allowed %v code %s", decision.Allowed, decision.Code, test.allowed, test.code)
			}
			if !decision.Timestamp.Equal(now) {
				t.Fatalf("timestamp = %s, want %s", decision.Timestamp, now)
			}
		})
	}
}

func TestDailyFNOMTM(t *testing.T) {
	positions := []domain.Position{{Exchange: "NFO", M2M: -20_000.004}, {Exchange: "BFO", M2M: -9_999.996}, {Exchange: "MCX", M2M: -100_000}}
	got, err := DailyFNOMTM(positions)
	if err != nil || got != domain.LossLimitPaise {
		t.Fatalf("DailyFNOMTM() = %d, want %d", got, domain.LossLimitPaise)
	}
}

func TestDailyFNOMTMSumsBeforeRoundingToPaise(t *testing.T) {
	positions := []domain.Position{{Exchange: "NFO", M2M: -15_000.004}, {Exchange: "BFO", M2M: -15_000.004}}
	got, err := DailyFNOMTM(positions)
	if err != nil || got != domain.LossLimitPaise-1 {
		t.Fatalf("DailyFNOMTM() = %d, want %d", got, domain.LossLimitPaise-1)
	}
}

func TestDailyFNOMTMRejectsNonFiniteBrokerValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := DailyFNOMTM([]domain.Position{{Exchange: "NFO", TradingSymbol: "BAD", M2M: value}}); err == nil {
			t.Fatalf("DailyFNOMTM(%v) error = nil", value)
		}
	}
}
