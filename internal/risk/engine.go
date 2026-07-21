package risk

import (
	"fmt"
	"math"
	"strings"
	"time"

	"tradeguardian/internal/domain"
)

type Engine struct {
	now func() time.Time
}

func New(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{now: now}
}

func (e *Engine) Evaluate(request domain.OrderRequest, trading domain.TradingStatus, runtime domain.RuntimeStatus, mtm int64) domain.RiskDecision {
	decision := domain.RiskDecision{EvaluatedMTM: mtm, TradingStatus: trading, Timestamp: e.now()}
	reject := func(code domain.DecisionCode, message string) domain.RiskDecision {
		decision.Code, decision.Message = code, message
		return decision
	}
	if trading == domain.TradingLocked {
		return reject(domain.CodeTradingLocked, domain.LockedMessage)
	}
	if trading != domain.TradingActive {
		return reject(domain.CodeMonitoringDegraded, "Trading state is unavailable; new orders are blocked.")
	}
	if runtime == domain.RuntimeAuthRequired {
		return reject(domain.CodeAuthRequired, "Connect your Kite account before trading.")
	}
	if runtime != domain.RuntimeReady {
		return reject(domain.CodeMonitoringDegraded, "New orders are blocked because risk monitoring is unavailable.")
	}
	request.Exchange = strings.ToUpper(request.Exchange)
	request.Variety = strings.ToLower(request.Variety)
	request.OrderType = strings.ToUpper(request.OrderType)
	request.TransactionType = strings.ToUpper(request.TransactionType)
	request.Validity = strings.ToUpper(request.Validity)
	request.InstrumentType = strings.ToUpper(request.InstrumentType)
	if !domain.IsFNOExchange(request.Exchange) {
		return reject(domain.CodeUnsupportedSegment, "Only NFO and BFO orders are supported.")
	}
	if request.Variety != "regular" {
		return reject(domain.CodeUnsupportedVariety, "Only regular orders are supported.")
	}
	if request.TradingSymbol == "" || request.Quantity <= 0 {
		return reject(domain.CodeInvalidOrder, "Trading symbol and positive quantity are required.")
	}
	if math.IsNaN(request.Price) || math.IsInf(request.Price, 0) || math.IsNaN(request.TriggerPrice) || math.IsInf(request.TriggerPrice, 0) {
		return reject(domain.CodeInvalidOrder, "Price and trigger price must be finite numbers.")
	}
	if request.Product != "MIS" && request.Product != "NRML" {
		return reject(domain.CodeInvalidOrder, "Product must be MIS or NRML.")
	}
	if request.TransactionType != "BUY" && request.TransactionType != "SELL" {
		return reject(domain.CodeInvalidOrder, "Transaction type must be BUY or SELL.")
	}
	switch request.OrderType {
	case "MARKET", "LIMIT", "SL", "SL-M":
	default:
		return reject(domain.CodeInvalidOrder, "Order type must be MARKET, LIMIT, SL, or SL-M.")
	}
	if request.Validity != "DAY" && request.Validity != "IOC" {
		return reject(domain.CodeInvalidOrder, "Validity must be DAY or IOC.")
	}
	if (request.OrderType == "LIMIT" || request.OrderType == "SL") && request.Price <= 0 {
		return reject(domain.CodeInvalidOrder, "A positive price is required for LIMIT and SL orders.")
	}
	if (request.OrderType == "SL" || request.OrderType == "SL-M") && request.TriggerPrice <= 0 {
		return reject(domain.CodeInvalidOrder, "A positive trigger price is required for stop-loss orders.")
	}
	if domain.IsOptionType(request.InstrumentType) && request.TransactionType == "BUY" {
		return reject(domain.CodeOptionBuyForbidden, "Standalone CE/PE BUY orders are forbidden. Use the validated hedge-basket workflow.")
	}
	if request.InstrumentType != "FUT" && !domain.IsOptionType(request.InstrumentType) {
		return reject(domain.CodeInvalidOrder, fmt.Sprintf("Unsupported derivative instrument type %q.", request.InstrumentType))
	}
	decision.Allowed = true
	decision.Code = domain.CodeApproved
	decision.Message = "Order approved by TradeGuardian."
	return decision
}

func DailyFNOMTM(positions []domain.Position) int64 {
	var total float64
	for _, position := range positions {
		if domain.IsFNOExchange(position.Exchange) {
			total += position.M2M
		}
	}
	return domain.RupeesToPaise(total)
}
