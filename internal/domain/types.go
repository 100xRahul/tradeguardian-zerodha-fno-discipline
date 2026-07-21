package domain

import (
	"context"
	"errors"
	"math"
	"time"
)

type TradingStatus string

const (
	TradingActive TradingStatus = "ACTIVE"
	TradingLocked TradingStatus = "LOCKED"
)

type RuntimeStatus string

const (
	RuntimeAuthRequired RuntimeStatus = "AUTH_REQUIRED"
	RuntimeReady        RuntimeStatus = "READY"
	RuntimeDegraded     RuntimeStatus = "MONITORING_DEGRADED"
	RuntimeLiquidating  RuntimeStatus = "LIQUIDATING"
	RuntimeBasket       RuntimeStatus = "BASKET_DEPLOYING"
)

type DecisionCode string

const (
	CodeApproved           DecisionCode = "APPROVED"
	CodeTradingLocked      DecisionCode = "TRADING_LOCKED"
	CodeAuthRequired       DecisionCode = "AUTH_REQUIRED"
	CodeMonitoringDegraded DecisionCode = "MONITORING_DEGRADED"
	CodeUnsupportedSegment DecisionCode = "UNSUPPORTED_SEGMENT"
	CodeUnsupportedVariety DecisionCode = "UNSUPPORTED_VARIETY"
	CodeInvalidOrder       DecisionCode = "INVALID_ORDER"
	CodeOptionBuyForbidden DecisionCode = "OPTION_BUY_FORBIDDEN"
	CodeHedgePolicyPending DecisionCode = "HEDGE_POLICY_PENDING"
	CodeUnhedgedExposure   DecisionCode = "UNHEDGED_OPTION_EXPOSURE"
	CodeBrokerError        DecisionCode = "BROKER_ERROR"
	CodeIdempotentReplay   DecisionCode = "IDEMPOTENT_REPLAY"
)

const (
	LossLimitPaise int64 = -3_000_000
	LockMessage          = "Daily Loss Limit Reached. Trading Locked Until Tomorrow."
	LockedMessage        = "Trading is locked for today."
)

var ErrNotAuthenticated = errors.New("kite session is not authenticated")

type OrderRequest struct {
	IdempotencyKey  string  `json:"idempotency_key"`
	Variety         string  `json:"variety"`
	Exchange        string  `json:"exchange"`
	TradingSymbol   string  `json:"tradingsymbol"`
	Product         string  `json:"product"`
	OrderType       string  `json:"order_type"`
	TransactionType string  `json:"transaction_type"`
	Validity        string  `json:"validity"`
	Quantity        int     `json:"quantity"`
	Price           float64 `json:"price"`
	TriggerPrice    float64 `json:"trigger_price"`
	InstrumentType  string  `json:"-"`
	Tag             string  `json:"-"`
}

type ModifyRequest struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Quantity       int     `json:"quantity"`
	OrderType      string  `json:"order_type"`
	Validity       string  `json:"validity"`
	Price          float64 `json:"price"`
	TriggerPrice   float64 `json:"trigger_price"`
}

type BasketLeg struct {
	Exchange        string  `json:"exchange"`
	TradingSymbol   string  `json:"tradingsymbol"`
	Product         string  `json:"product"`
	TransactionType string  `json:"transaction_type"`
	Quantity        int     `json:"quantity"`
	LimitPrice      float64 `json:"limit_price"`
}

type BasketRequest struct {
	IdempotencyKey string      `json:"idempotency_key"`
	Name           string      `json:"name"`
	Legs           []BasketLeg `json:"legs"`
}

type BasketResult struct {
	BasketID       string   `json:"basket_id"`
	Status         string   `json:"status"`
	Message        string   `json:"message"`
	MaxLossPaise   int64    `json:"max_loss_paise"`
	OrderIDs       []string `json:"order_ids"`
	RollbackOrders []string `json:"rollback_order_ids,omitempty"`
}

type RiskDecision struct {
	Allowed       bool          `json:"allowed"`
	Code          DecisionCode  `json:"code"`
	Message       string        `json:"message"`
	EvaluatedMTM  int64         `json:"evaluated_mtm_paise"`
	TradingStatus TradingStatus `json:"trading_status"`
	Timestamp     time.Time     `json:"timestamp"`
}

type Position struct {
	Exchange        string  `json:"exchange"`
	TradingSymbol   string  `json:"tradingsymbol"`
	InstrumentToken uint32  `json:"instrument_token"`
	Product         string  `json:"product"`
	Quantity        int     `json:"quantity"`
	M2M             float64 `json:"m2m"`
	LastPrice       float64 `json:"last_price"`
}

type Order struct {
	OrderID         string   `json:"order_id"`
	ParentOrderID   string   `json:"parent_order_id,omitempty"`
	Variety         string   `json:"variety"`
	Status          string   `json:"status"`
	Exchange        string   `json:"exchange"`
	TradingSymbol   string   `json:"tradingsymbol"`
	InstrumentToken uint32   `json:"instrument_token"`
	InstrumentType  string   `json:"instrument_type,omitempty"`
	Product         string   `json:"product"`
	OrderType       string   `json:"order_type"`
	TransactionType string   `json:"transaction_type"`
	Validity        string   `json:"validity"`
	Quantity        int      `json:"quantity"`
	PendingQuantity int      `json:"pending_quantity"`
	FilledQuantity  int      `json:"filled_quantity"`
	Price           float64  `json:"price"`
	TriggerPrice    float64  `json:"trigger_price"`
	StatusMessage   string   `json:"status_message,omitempty"`
	Tag             string   `json:"tag,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

func (o Order) Cancellable() bool {
	if o.PendingQuantity <= 0 {
		return false
	}
	switch o.Status {
	case "COMPLETE", "CANCELLED", "REJECTED":
		return false
	default:
		return true
	}
}

type Instrument struct {
	Token          uint32    `json:"instrument_token"`
	Exchange       string    `json:"exchange"`
	TradingSymbol  string    `json:"tradingsymbol"`
	Name           string    `json:"name"`
	InstrumentType string    `json:"instrument_type"`
	Expiry         time.Time `json:"expiry"`
	Strike         float64   `json:"strike"`
	TickSize       float64   `json:"tick_size"`
	LotSize        int       `json:"lot_size"`
}

type Session struct {
	UserID      string
	AccessToken string
}

type Broker interface {
	LoginURL(state string) string
	GenerateSession(ctx context.Context, requestToken string) (Session, error)
	Positions(ctx context.Context) ([]Position, error)
	Orders(ctx context.Context) ([]Order, error)
	Instruments(ctx context.Context, exchange string) ([]Instrument, error)
	Place(ctx context.Context, request OrderRequest) (string, error)
	Modify(ctx context.Context, orderID string, request ModifyRequest) (string, error)
	Cancel(ctx context.Context, variety, orderID string) error
	ExitPosition(ctx context.Context, position Position) (string, error)
}

type LockRecord struct {
	Status           TradingStatus `json:"status"`
	LockedOn         string        `json:"locked_on,omitempty"`
	TriggerMTMPaise  int64         `json:"trigger_mtm_paise,omitempty"`
	TriggeredAt      time.Time     `json:"triggered_at,omitempty"`
	UnlockAt         time.Time     `json:"unlock_at,omitempty"`
	LiquidationState string        `json:"liquidation_state,omitempty"`
	LastError        string        `json:"last_error,omitempty"`
}

type AuditEvent struct {
	ID        int64          `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	Type      string         `json:"type"`
	Code      DecisionCode   `json:"code,omitempty"`
	Message   string         `json:"message"`
	OrderID   string         `json:"order_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type StateStore interface {
	CurrentLock(ctx context.Context) (LockRecord, error)
	Lock(ctx context.Context, record LockRecord, event AuditEvent) error
	Unlock(ctx context.Context, at time.Time, event AuditEvent) error
	UpdateLiquidation(ctx context.Context, state, lastError string) error
	AppendAudit(ctx context.Context, event AuditEvent) error
	ListAudit(ctx context.Context, limit int) ([]AuditEvent, error)
	LookupIdempotency(ctx context.Context, key string) (result string, found bool, err error)
	ReserveIdempotency(ctx context.Context, key, reference string, at time.Time) (existing string, reserved bool, err error)
	CompleteIdempotency(ctx context.Context, key, reference, result string) error
	Close() error
}

type Snapshot struct {
	TradingStatus   TradingStatus `json:"trading_status"`
	RuntimeStatus   RuntimeStatus `json:"runtime_status"`
	Message         string        `json:"message"`
	MTMPaise        int64         `json:"mtm_paise"`
	LossLimitPaise  int64         `json:"loss_limit_paise"`
	LastRefresh     *time.Time    `json:"last_refresh,omitempty"`
	Authenticated   bool          `json:"authenticated"`
	Liquidation     string        `json:"liquidation_state,omitempty"`
	LastError       string        `json:"last_error,omitempty"`
	NextUnlock      *time.Time    `json:"next_unlock,omitempty"`
	OpenPositionQty int           `json:"open_position_quantity"`
	PendingOrders   int           `json:"pending_orders"`
}

func RupeesToPaise(value float64) int64 { return int64(math.Round(value * 100)) }

func IsFNOExchange(exchange string) bool { return exchange == "NFO" || exchange == "BFO" }

func IsOptionType(instrumentType string) bool {
	return instrumentType == "CE" || instrumentType == "PE"
}
