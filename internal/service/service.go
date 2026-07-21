package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"tradeguardian/internal/calendar"
	"tradeguardian/internal/domain"
	"tradeguardian/internal/risk"
)

const (
	forcedExitTag       = domain.ForcedExitTag
	pendingOrderPrefix  = "pending-order:"
	pendingModifyPrefix = "pending-modify:"
)

type Service struct {
	gate          sync.Mutex
	operation     sync.Mutex
	broker        domain.Broker
	store         domain.StateStore
	calendar      *calendar.Calendar
	risk          *risk.Engine
	now           func() time.Time
	log           *log.Logger
	notify        func()
	trading       domain.TradingStatus
	runtime       domain.RuntimeStatus
	authed        bool
	mtm           int64
	lastUpdate    time.Time
	positions     []domain.Position
	orders        []domain.Order
	instruments   map[string]domain.Instrument
	lockRecord    domain.LockRecord
	attention     map[string]int
	forcedExits   map[string]string
	forcedExitAt  map[string]time.Time
	hardAttention bool
	lockPersisted bool
}

func New(ctx context.Context, broker domain.Broker, store domain.StateStore, cal *calendar.Calendar, logger *log.Logger, now func() time.Time) (*Service, error) {
	if broker == nil || store == nil || cal == nil {
		return nil, fmt.Errorf("broker, store, and calendar are required")
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = log.Default()
	}
	record, err := store.CurrentLock(ctx)
	if err != nil {
		return nil, err
	}
	service := &Service{
		broker: broker, store: store, calendar: cal, risk: risk.New(now), now: now,
		log: logger, notify: func() {}, trading: record.Status, runtime: domain.RuntimeAuthRequired,
		instruments: make(map[string]domain.Instrument), lockRecord: record,
		attention: make(map[string]int), forcedExits: make(map[string]string), forcedExitAt: make(map[string]time.Time), lockPersisted: true,
	}
	if err := service.maybeUnlockLocked(ctx); err != nil {
		return nil, err
	}
	intents, err := store.ListLiquidationIntents(ctx)
	if err != nil {
		return nil, err
	}
	for _, intent := range intents {
		if intent.PositionKey != "" && intent.OrderID != "" {
			service.forcedExits[intent.PositionKey] = intent.OrderID
			service.forcedExitAt[intent.PositionKey] = intent.CreatedAt
		}
	}
	return service, nil
}

func (s *Service) SetNotifier(notify func()) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if notify == nil {
		s.notify = func() {}
		return
	}
	s.notify = notify
}

func (s *Service) LoginURL(state string) string { return s.broker.LoginURL(state) }

func (s *Service) Authenticate(ctx context.Context, requestToken string) error {
	if _, err := s.broker.GenerateSession(ctx, requestToken); err != nil {
		return err
	}
	s.gate.Lock()
	s.authed = true
	s.runtime = domain.RuntimeDegraded
	s.lockRecord.LastError = "Kite connected; fresh risk state is initializing"
	s.gate.Unlock()
	if err := s.refreshInstruments(ctx); err != nil {
		s.gate.Lock()
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired; reconnect to resume monitoring")
		} else {
			s.runtime = domain.RuntimeDegraded
			s.lockRecord.LastError = "instrument catalogue unavailable"
		}
		s.gate.Unlock()
		s.signal()
		return err
	}
	if err := s.PollOnce(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Service) refreshInstruments(ctx context.Context) error {
	index := make(map[string]domain.Instrument)
	for _, exchange := range []string{"NFO", "BFO"} {
		instruments, err := s.broker.Instruments(ctx, exchange)
		if err != nil {
			return err
		}
		for _, instrument := range instruments {
			if instrument.Exchange == exchange {
				index[instrumentKey(exchange, instrument.TradingSymbol)] = instrument
			}
		}
	}
	s.gate.Lock()
	s.instruments = index
	s.gate.Unlock()
	return nil
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.PollOnce(ctx); err != nil && !errors.Is(err, domain.ErrNotAuthenticated) && !errors.Is(err, context.Canceled) {
				s.log.Printf("level=warn message=%q error=%q", "risk monitor poll failed", err)
			}
		}
	}
}

func (s *Service) PollOnce(ctx context.Context) error {
	if err := s.MaybeUnlock(ctx); err != nil {
		return err
	}
	s.gate.Lock()
	authed := s.authed
	needsInstruments := len(s.instruments) == 0
	s.gate.Unlock()
	if !authed {
		s.gate.Lock()
		s.runtime = domain.RuntimeAuthRequired
		s.gate.Unlock()
		return domain.ErrNotAuthenticated
	}
	if needsInstruments {
		if err := s.refreshInstruments(ctx); err != nil {
			s.gate.Lock()
			if errors.Is(err, domain.ErrNotAuthenticated) {
				s.setAuthRequiredLocked("Kite session expired; reconnect to resume monitoring")
			} else {
				s.runtime = domain.RuntimeDegraded
				s.lockRecord.LastError = "instrument catalogue unavailable"
			}
			s.gate.Unlock()
			s.signal()
			return err
		}
	}
	s.gate.Lock()
	positions, err := s.broker.Positions(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.authed = false
			s.runtime = domain.RuntimeAuthRequired
			s.lockRecord.LastError = "Kite session expired; reconnect to resume monitoring"
			s.gate.Unlock()
			s.signal()
			return err
		}
		s.runtime = domain.RuntimeDegraded
		s.lockRecord.LastError = "position monitoring unavailable"
		s.gate.Unlock()
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "MONITOR_ERROR", Code: domain.CodeMonitoringDegraded, Message: "Position monitoring unavailable."})
		s.signal()
		return err
	}
	mtm, err := risk.DailyFNOMTM(positions)
	if err != nil {
		s.runtime = domain.RuntimeDegraded
		s.lockRecord.LastError = "position MTM data is invalid"
		s.gate.Unlock()
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "MONITOR_ERROR", Code: domain.CodeMonitoringDegraded, Message: "Position MTM data was invalid."})
		s.signal()
		return err
	}
	s.positions = filterPositions(positions)
	s.mtm = mtm
	s.lastUpdate = s.now()
	intentCleanupErr := s.purgeExpiredExitIntentsLocked(ctx)
	if s.trading == domain.TradingLocked && !s.lockPersisted {
		s.retryLockPersistenceLocked(ctx)
	}
	if s.trading == domain.TradingActive && s.mtm <= domain.LossLimitPaise {
		s.enterDailyLossLockLocked(ctx)
		s.gate.Unlock()
		s.signal()
		return errors.Join(intentCleanupErr, s.liquidate(ctx))
	}
	if intentCleanupErr != nil {
		if s.trading == domain.TradingLocked {
			s.runtime = domain.RuntimeLiquidating
		} else {
			s.runtime = domain.RuntimeDegraded
		}
		s.lockRecord.LastError = "durable liquidation intent cleanup failed"
		s.gate.Unlock()
		s.signal()
		return intentCleanupErr
	}
	orders, orderErr := s.broker.Orders(ctx)
	if orderErr != nil {
		if errors.Is(orderErr, domain.ErrNotAuthenticated) {
			s.authed = false
			s.runtime = domain.RuntimeAuthRequired
			s.lockRecord.LastError = "Kite session expired; reconnect to resume monitoring"
			s.gate.Unlock()
			s.signal()
			return orderErr
		}
		locked := s.trading == domain.TradingLocked
		if locked {
			s.runtime = domain.RuntimeLiquidating
			s.lockRecord.LastError = "order monitoring unavailable during liquidation"
		} else {
			s.runtime = domain.RuntimeDegraded
			s.lockRecord.LastError = "order monitoring unavailable"
		}
		s.gate.Unlock()
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "MONITOR_ERROR", Code: domain.CodeMonitoringDegraded, Message: "Order monitoring unavailable."})
		s.signal()
		if locked {
			return s.liquidate(ctx)
		}
		return orderErr
	}
	s.orders = filterOrders(orders)
	s.decorateOrdersLocked()
	if s.trading == domain.TradingLocked && !portfolioFlat(s.positions, s.orders) && s.lockRecord.LiquidationState == "COMPLETED" {
		s.lockRecord.LiquidationState = "IN_PROGRESS"
		s.lockRecord.LastError = "new F&O exposure detected while locked"
		s.runtime = domain.RuntimeLiquidating
		if updateErr := s.store.UpdateLiquidation(ctx, "IN_PROGRESS", s.lockRecord.LastError); updateErr != nil {
			s.log.Printf("level=error message=%q error=%q", "failed to persist resumed liquidation", updateErr)
		}
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "LIQUIDATION_RESUMED", Code: domain.CodeTradingLocked, Message: "New F&O exposure was detected while trading remained locked."})
	}
	if s.hardAttention && portfolioFlat(s.positions, s.orders) {
		s.hardAttention = false
	}
	s.reconcileAttentionLocked(orders)
	shouldLiquidate := s.trading == domain.TradingLocked && s.lockRecord.LiquidationState != "COMPLETED"
	if s.trading == domain.TradingActive && s.runtime != domain.RuntimeBasket && !s.hardAttention && len(s.attention) == 0 {
		s.lockRecord.LastError = ""
		s.runtime = domain.RuntimeReady
	} else if s.trading == domain.TradingActive && (s.hardAttention || len(s.attention) > 0) {
		s.runtime = domain.RuntimeDegraded
		s.lockRecord.LastError = "basket rollback requires attention"
	}
	s.gate.Unlock()
	s.signal()
	if shouldLiquidate {
		return s.liquidate(ctx)
	}
	return nil
}

func (s *Service) enterDailyLossLockLocked(ctx context.Context) {
	now := s.now()
	record := domain.LockRecord{
		Status: domain.TradingLocked, LockedOn: s.calendar.TradingDate(now),
		TriggerMTMPaise: s.mtm, TriggeredAt: now, UnlockAt: s.calendar.NextUnlock(now),
		LiquidationState: "IN_PROGRESS",
	}
	event := domain.AuditEvent{CreatedAt: now, Type: "DAILY_LOSS_LOCK", Code: domain.CodeTradingLocked, Message: domain.LockMessage, Metadata: map[string]any{"mtm_paise": s.mtm}}
	if persistErr := s.store.Lock(ctx, record, event); persistErr != nil {
		s.log.Printf("level=error message=%q error=%q", "failed to persist daily loss lock; retaining in-memory lock", persistErr)
		record.LastError = "lock persistence failed"
		s.lockPersisted = false
	} else {
		s.lockPersisted = true
	}
	s.trading = domain.TradingLocked
	s.runtime = domain.RuntimeLiquidating
	s.lockRecord = record
}

func (s *Service) retryLockPersistenceLocked(ctx context.Context) {
	event := domain.AuditEvent{CreatedAt: s.lockRecord.TriggeredAt, Type: "DAILY_LOSS_LOCK", Code: domain.CodeTradingLocked, Message: domain.LockMessage, Metadata: map[string]any{"mtm_paise": s.lockRecord.TriggerMTMPaise}}
	if persistErr := s.store.Lock(ctx, s.lockRecord, event); persistErr == nil {
		s.lockPersisted = true
		if s.lockRecord.LastError == "lock persistence failed" {
			s.lockRecord.LastError = ""
		}
	} else {
		s.log.Printf("level=error message=%q error=%q", "daily loss lock persistence retry failed", persistErr)
	}
}

func (s *Service) Place(ctx context.Context, request domain.OrderRequest) (domain.RiskDecision, string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	normalizeOrder(&request)
	if decision, blocked := s.preTradeStateDecisionLocked(); blocked {
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	instrument, found := s.instruments[instrumentKey(request.Exchange, request.TradingSymbol)]
	if found {
		request.InstrumentType = instrument.InstrumentType
	}
	if !found {
		decision := s.reject(domain.CodeInvalidOrder, "Instrument was not found in the current NFO/BFO catalogue.")
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	if instrument.LotSize <= 0 || (domain.IsOptionType(instrument.InstrumentType) && (strings.TrimSpace(instrument.Name) == "" || instrument.Expiry.IsZero() || instrument.Strike <= 0)) {
		decision := s.reject(domain.CodeInvalidOrder, "Instrument metadata is incomplete; order blocked until the catalogue is refreshed.")
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	decision := s.risk.Evaluate(request, s.trading, s.runtime, s.mtm)
	if !decision.Allowed {
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	if err := validateOrderTicks(request.Price, request.TriggerPrice, instrument.TickSize); err != nil {
		decision = s.reject(domain.CodeInvalidOrder, err.Error())
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	if instrument.LotSize > 0 && request.Quantity%instrument.LotSize != 0 {
		decision = s.reject(domain.CodeInvalidOrder, fmt.Sprintf("Quantity must be a multiple of lot size %d.", instrument.LotSize))
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	if domain.IsOptionType(instrument.InstrumentType) && request.TransactionType == "SELL" {
		if err := s.validateOptionCoverageLocked(instrument, request.Product, -request.Quantity, nil, nil); err != nil {
			decision = s.reject(domain.CodeUnhedgedExposure, err.Error())
			s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
			return decision, "", nil
		}
	}
	if !validIdempotencyKey(request.IdempotencyKey) {
		decision = s.reject(domain.CodeInvalidOrder, "A valid idempotency key is required.")
		s.auditDecision(ctx, "ORDER_REJECTED", decision, request, "")
		return decision, "", nil
	}
	reference, err := pendingReference(pendingOrderPrefix)
	if err != nil {
		return s.reject(domain.CodeBrokerError, "Order state could not be reserved."), "", err
	}
	existing, reserved, err := s.store.ReserveIdempotency(ctx, request.IdempotencyKey, reference, s.now())
	if err != nil {
		return s.reject(domain.CodeBrokerError, "Order state could not be reserved."), "", err
	}
	if !reserved {
		return s.idempotentDecision(existing, "order")
	}
	request.Tag = "tradeguardian"
	orderID, err := s.broker.Place(ctx, request)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while placing an order")
			decision = s.reject(domain.CodeAuthRequired, "Kite session expired. Reconnect before trading.")
		} else {
			decision = s.reject(domain.CodeBrokerError, "Zerodha did not accept the order request.")
		}
		s.auditDecision(ctx, "ORDER_ERROR", decision, request, "")
		return decision, "", err
	}
	if err := s.store.CompleteIdempotency(ctx, request.IdempotencyKey, reference, orderID); err != nil {
		s.log.Printf("level=error message=%q order_id=%q error=%q", "order placed but idempotency record failed", orderID, err)
	}
	s.cacheSubmittedOrderLocked(orderID, request, instrument.Token)
	s.auditDecision(ctx, "ORDER_SUBMITTED", decision, request, orderID)
	s.signalAsync()
	return decision, orderID, nil
}

func (s *Service) Modify(ctx context.Context, orderID string, request domain.ModifyRequest) (domain.RiskDecision, string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if decision, blocked := s.preTradeStateDecisionLocked(); blocked {
		s.auditModifyDecision(ctx, "MODIFY_REJECTED", decision, orderID)
		return decision, "", nil
	}
	if !validIdempotencyKey(request.IdempotencyKey) {
		decision := s.reject(domain.CodeInvalidOrder, "A valid idempotency key is required.")
		s.auditModifyDecision(ctx, "MODIFY_REJECTED", decision, orderID)
		return decision, "", nil
	}
	existing, found, err := s.store.LookupIdempotency(ctx, request.IdempotencyKey)
	if err != nil {
		return s.reject(domain.CodeBrokerError, "Modification state could not be read."), "", err
	}
	if found {
		return s.idempotentDecision(existing, "modification")
	}
	orders, err := s.broker.Orders(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while reading orders")
		}
		return s.reject(domain.CodeBrokerError, "Zerodha orders could not be read."), "", err
	}
	current, ok := findOrder(orders, orderID)
	if !ok || !domain.IsFNOExchange(current.Exchange) || current.Variety != "regular" || !current.Cancellable() {
		return s.reject(domain.CodeInvalidOrder, "A modifiable regular NFO/BFO order was not found."), "", nil
	}
	if orderHasTagPrefix(current, "tgb") {
		return s.reject(domain.CodeInvalidOrder, "Basket legs cannot be modified individually."), "", nil
	}
	instrument, ok := s.instruments[instrumentKey(current.Exchange, current.TradingSymbol)]
	if !ok {
		return s.reject(domain.CodeInvalidOrder, "Order instrument was not found in the current catalogue."), "", nil
	}
	if request.Quantity == 0 {
		request.Quantity = current.Quantity
	}
	if request.OrderType == "" {
		request.OrderType = current.OrderType
	}
	if request.Validity == "" {
		request.Validity = current.Validity
	}
	request.OrderType = strings.ToUpper(request.OrderType)
	request.Validity = strings.ToUpper(request.Validity)
	candidate := domain.OrderRequest{
		Variety: current.Variety, Exchange: current.Exchange, TradingSymbol: current.TradingSymbol,
		Product: current.Product, OrderType: request.OrderType, TransactionType: current.TransactionType,
		Validity: request.Validity, Quantity: request.Quantity, Price: request.Price,
		TriggerPrice: request.TriggerPrice, InstrumentType: instrument.InstrumentType,
	}
	decision := s.risk.Evaluate(candidate, s.trading, s.runtime, s.mtm)
	if !decision.Allowed {
		s.auditDecision(ctx, "MODIFY_REJECTED", decision, candidate, orderID)
		return decision, "", nil
	}
	if err := validateOrderTicks(request.Price, request.TriggerPrice, instrument.TickSize); err != nil {
		return s.reject(domain.CodeInvalidOrder, err.Error()), "", nil
	}
	if instrument.LotSize > 0 && request.Quantity%instrument.LotSize != 0 {
		return s.reject(domain.CodeInvalidOrder, fmt.Sprintf("Quantity must be a multiple of lot size %d.", instrument.LotSize)), "", nil
	}
	if domain.IsOptionType(instrument.InstrumentType) && current.TransactionType == "SELL" {
		delta := -(request.Quantity - current.Quantity)
		if err := s.validateOptionCoverageLocked(instrument, current.Product, delta, nil, orders); err != nil {
			return s.reject(domain.CodeUnhedgedExposure, err.Error()), "", nil
		}
	}
	reference, err := pendingReference(pendingModifyPrefix)
	if err != nil {
		return s.reject(domain.CodeBrokerError, "Modification state could not be reserved."), "", err
	}
	existing, reserved, err := s.store.ReserveIdempotency(ctx, request.IdempotencyKey, reference, s.now())
	if err != nil {
		return s.reject(domain.CodeBrokerError, "Modification state could not be reserved."), "", err
	}
	if !reserved {
		return s.idempotentDecision(existing, "modification")
	}
	modifiedID, err := s.broker.Modify(ctx, orderID, request)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while modifying an order")
			decision = s.reject(domain.CodeAuthRequired, "Kite session expired. Reconnect before trading.")
		} else {
			decision = s.reject(domain.CodeBrokerError, "Zerodha did not accept the modification.")
		}
		s.auditDecision(ctx, "MODIFY_ERROR", decision, candidate, orderID)
		return decision, "", err
	}
	if err := s.store.CompleteIdempotency(ctx, request.IdempotencyKey, reference, modifiedID); err != nil {
		s.log.Printf("level=error message=%q order_id=%q error=%q", "modification submitted but idempotency record failed", modifiedID, err)
	}
	s.cacheModifiedOrderLocked(orderID, modifiedID, request, current)
	s.auditDecision(ctx, "ORDER_MODIFIED", decision, candidate, modifiedID)
	s.signalAsync()
	return decision, modifiedID, nil
}

func (s *Service) Cancel(ctx context.Context, orderID string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if !s.authed {
		return domain.ErrNotAuthenticated
	}
	orders, err := s.broker.Orders(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while reading orders")
		}
		return err
	}
	order, ok := findOrder(orders, orderID)
	if !ok || !domain.IsFNOExchange(order.Exchange) || order.Variety != "regular" || !order.Cancellable() {
		return fmt.Errorf("cancellable regular NFO/BFO order not found")
	}
	if err := s.broker.Cancel(ctx, "regular", orderID); err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while cancelling an order")
		}
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "CANCEL_ERROR", Code: domain.CodeBrokerError, Message: "Order cancellation failed.", OrderID: orderID})
		return err
	}
	s.auditBestEffort(ctx, domain.AuditEvent{Type: "ORDER_CANCELLED", Code: domain.CodeApproved, Message: "Cancellation submitted.", OrderID: orderID})
	s.signalAsync()
	return nil
}

func (s *Service) PlaceBasket(ctx context.Context, request domain.BasketRequest) (domain.BasketResult, error) {
	if !validIdempotencyKey(request.IdempotencyKey) {
		return domain.BasketResult{}, fmt.Errorf("a valid idempotency key is required")
	}
	s.gate.Lock()
	if s.trading == domain.TradingLocked {
		s.gate.Unlock()
		return domain.BasketResult{}, fmt.Errorf("%s", domain.LockedMessage)
	}
	if s.trading != domain.TradingActive {
		s.gate.Unlock()
		return domain.BasketResult{}, fmt.Errorf("trading state is unavailable; basket blocked")
	}
	if !s.authed {
		s.gate.Unlock()
		return domain.BasketResult{}, domain.ErrNotAuthenticated
	}
	if s.runtime != domain.RuntimeReady {
		s.gate.Unlock()
		return domain.BasketResult{}, fmt.Errorf("risk monitoring must be ready before deploying a basket")
	}
	instrumentIndex := make(map[string]domain.Instrument, len(s.instruments))
	for key, instrument := range s.instruments {
		instrumentIndex[key] = instrument
	}
	validated, err := risk.ValidateBasket(request, instrumentIndex)
	if err != nil {
		s.gate.Unlock()
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "BASKET_REJECTED", Code: domain.CodeInvalidOrder, Message: err.Error()})
		return domain.BasketResult{}, err
	}
	if err := s.validateBasketPortfolioCoverageLocked(validated); err != nil {
		s.gate.Unlock()
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "BASKET_REJECTED", Code: domain.CodeUnhedgedExposure, Message: err.Error()})
		return domain.BasketResult{}, err
	}
	basketID, err := RandomID()
	if err != nil {
		s.gate.Unlock()
		return domain.BasketResult{}, err
	}
	existing, reserved, err := s.store.ReserveIdempotency(ctx, request.IdempotencyKey, basketID, s.now())
	if err != nil {
		s.gate.Unlock()
		return domain.BasketResult{}, err
	}
	if !reserved {
		s.gate.Unlock()
		return domain.BasketResult{BasketID: existing, Status: "REPLAY", Message: "This basket request was already started.", MaxLossPaise: validated.MaxLossPaise}, nil
	}
	s.runtime = domain.RuntimeBasket
	s.lockRecord.LastError = ""
	s.gate.Unlock()
	s.signal()

	s.operation.Lock()
	defer s.operation.Unlock()
	result := domain.BasketResult{BasketID: basketID, Status: "DEPLOYING", Message: "Basket deployment started.", MaxLossPaise: validated.MaxLossPaise}
	tag := "tgb" + basketID[:12]
	s.auditBestEffort(ctx, domain.AuditEvent{Type: "BASKET_STARTED", Code: domain.CodeApproved, Message: "Validated hedge basket deployment started.", Metadata: map[string]any{"basket_id": basketID, "max_loss_paise": validated.MaxLossPaise, "legs": len(validated.Request.Legs)}})

	buyOrders, err := s.placeBasketPhase(ctx, validated.Request.Legs, "BUY", tag)
	result.OrderIDs = append(result.OrderIDs, phaseIDs(buyOrders)...)
	if err != nil {
		result.Status, result.Message = "ATTENTION_REQUIRED", "Protective BUY phase failed and final fills could not be assumed. Any confirmed long fills are retained until reconciliation."
		buyFills, captureErr := s.captureFills(ctx, buyOrders)
		if captureErr == nil {
			result.Status, result.Message = "ROLLED_BACK", "Protective BUY phase failed; confirmed long fills were rolled back."
			result.RollbackOrders, _ = s.applyBasketRollback(ctx, &result, buyFills, nil, tag)
		} else {
			s.markBasketAttention("protective BUY fills could not be confirmed")
			err = errors.Join(err, captureErr)
		}
		s.finishBasket(ctx, result, err)
		return result, err
	}
	buyFills, buyComplete, err := s.awaitPhase(ctx, buyOrders, 3*time.Second)
	if err != nil || !buyComplete {
		s.cancelPhase(ctx, buyOrders)
		if err != nil {
			result.Status, result.Message = "ATTENTION_REQUIRED", "Protective BUY fills could not be confirmed; possible long fills are retained for safety."
			s.markBasketAttention("protective BUY fills could not be confirmed")
		} else {
			result.Status, result.Message = "ROLLED_BACK", "Protective BUY legs did not fully fill as IOC orders; confirmed long fills were rolled back."
			result.RollbackOrders, _ = s.applyBasketRollback(ctx, &result, buyFills, nil, tag)
			err = fmt.Errorf("protective BUY phase did not fill completely")
		}
		s.finishBasket(ctx, result, err)
		return result, err
	}
	if err := s.ensureBasketMayContinue(); err != nil {
		result.Status, result.Message = "ROLLED_BACK", "Risk state changed during deployment; protective BUY quantities were rolled back."
		result.RollbackOrders, _ = s.applyBasketRollback(ctx, &result, buyFills, nil, tag)
		s.finishBasket(ctx, result, err)
		return result, err
	}

	sellOrders, err := s.placeBasketPhase(ctx, validated.Request.Legs, "SELL", tag)
	result.OrderIDs = append(result.OrderIDs, phaseIDs(sellOrders)...)
	if err != nil {
		result.Status, result.Message = "ATTENTION_REQUIRED", "SELL phase failed and final short fills could not be assumed. Protective long fills are retained until reconciliation."
		sellFills, captureErr := s.captureFills(ctx, sellOrders)
		if captureErr == nil {
			result.Status, result.Message = "ROLLED_BACK", "SELL phase failed; confirmed short fills were closed before protective longs were unwound."
			result.RollbackOrders, _ = s.applyBasketRollback(ctx, &result, buyFills, sellFills, tag)
		} else {
			s.markBasketAttention("SELL fills could not be confirmed; protective longs retained")
			err = errors.Join(err, captureErr)
		}
		s.finishBasket(ctx, result, err)
		return result, err
	}
	sellFills, sellComplete, err := s.awaitPhase(ctx, sellOrders, 3*time.Second)
	if err != nil || !sellComplete {
		s.cancelPhase(ctx, sellOrders)
		if err != nil {
			result.Status, result.Message = "ATTENTION_REQUIRED", "SELL fills could not be confirmed; protective long fills are retained for safety."
			s.markBasketAttention("SELL fills could not be confirmed; protective longs retained")
		} else {
			result.Status, result.Message = "ROLLED_BACK", "SELL legs did not fully fill as IOC orders; confirmed shorts were closed before protective longs were unwound."
			result.RollbackOrders, _ = s.applyBasketRollback(ctx, &result, buyFills, sellFills, tag)
			err = fmt.Errorf("SELL phase did not fill completely")
		}
		s.finishBasket(ctx, result, err)
		return result, err
	}
	result.Status, result.Message = "COMPLETE", "Hedged option basket deployed and broker fills confirmed."
	s.finishBasket(ctx, result, nil)
	return result, nil
}

type phaseOrder struct {
	OrderID string
	Leg     domain.BasketLeg
	Filled  int
}

func (s *Service) placeBasketPhase(ctx context.Context, legs []domain.BasketLeg, side, tag string) ([]phaseOrder, error) {
	orders := make([]phaseOrder, 0, len(legs))
	for _, leg := range legs {
		if leg.TransactionType != side {
			continue
		}
		orderID, err := s.placeBasketLeg(ctx, domain.OrderRequest{
			Variety: "regular", Exchange: leg.Exchange, TradingSymbol: leg.TradingSymbol,
			Product: leg.Product, OrderType: "LIMIT", TransactionType: side,
			Validity: "IOC", Quantity: leg.Quantity, Price: leg.LimitPrice, Tag: tag,
		})
		if err != nil {
			s.cancelPhase(ctx, orders)
			return orders, err
		}
		orders = append(orders, phaseOrder{OrderID: orderID, Leg: leg})
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "BASKET_LEG_SUBMITTED", Code: domain.CodeApproved, Message: side + " basket leg submitted.", OrderID: orderID, Metadata: map[string]any{"tradingsymbol": leg.TradingSymbol, "quantity": leg.Quantity}})
	}
	return orders, nil
}

func (s *Service) placeBasketLeg(ctx context.Context, request domain.OrderRequest) (string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.trading == domain.TradingLocked {
		return "", fmt.Errorf("%s", domain.LockedMessage)
	}
	if s.trading != domain.TradingActive {
		return "", fmt.Errorf("basket deployment stopped because trading state is unavailable")
	}
	if s.runtime != domain.RuntimeBasket {
		return "", fmt.Errorf("basket deployment stopped because risk state changed")
	}
	orderID, err := s.broker.Place(ctx, request)
	if errors.Is(err, domain.ErrNotAuthenticated) {
		s.setAuthRequiredLocked("Kite session expired during basket deployment")
	}
	return orderID, err
}

func (s *Service) awaitPhase(ctx context.Context, phase []phaseOrder, timeout time.Duration) ([]phaseOrder, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		orders, err := s.broker.Orders(ctx)
		if err != nil {
			if errors.Is(err, domain.ErrNotAuthenticated) {
				s.markAuthenticationRequired("Kite session expired during basket reconciliation")
			}
			return phase, false, err
		}
		allTerminal, allFilled := true, true
		for index := range phase {
			order, found := findOrder(orders, phase[index].OrderID)
			if !found {
				allTerminal, allFilled = false, false
				continue
			}
			phase[index].Filled = order.FilledQuantity
			if order.Cancellable() || (order.Status != "COMPLETE" && order.Status != "CANCELLED" && order.Status != "REJECTED") {
				allTerminal = false
			}
			if order.Status != "COMPLETE" || order.FilledQuantity != phase[index].Leg.Quantity {
				allFilled = false
			}
		}
		if allTerminal {
			return phase, allFilled, nil
		}
		select {
		case <-ctx.Done():
			return phase, false, ctx.Err()
		case <-deadline.C:
			return phase, false, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) cancelPhase(ctx context.Context, phase []phaseOrder) {
	orders, err := s.broker.Orders(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.markAuthenticationRequired("Kite session expired during basket cancellation")
		}
		return
	}
	for _, submitted := range phase {
		if order, ok := findOrder(orders, submitted.OrderID); ok && order.Cancellable() {
			if err := s.broker.Cancel(ctx, "regular", submitted.OrderID); errors.Is(err, domain.ErrNotAuthenticated) {
				s.markAuthenticationRequired("Kite session expired during basket cancellation")
				return
			}
		}
	}
}

func (s *Service) captureFills(ctx context.Context, phase []phaseOrder) ([]phaseOrder, error) {
	orders, err := s.broker.Orders(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.markAuthenticationRequired("Kite session expired during basket reconciliation")
		}
		return phase, err
	}
	for index := range phase {
		if order, ok := findOrder(orders, phase[index].OrderID); ok {
			phase[index].Filled = order.FilledQuantity
		}
	}
	return phase, nil
}

func (s *Service) rollbackBasket(ctx context.Context, buys, sells []phaseOrder, tag string) ([]string, bool) {
	rollback := make([]string, 0, len(buys)+len(sells))
	shortCloseIDs, shortsClosed := s.rollbackPhase(ctx, sells, tag)
	rollback = append(rollback, shortCloseIDs...)
	if !shortsClosed {
		// Never remove protection while a short-closing order is failed or uncertain.
		return rollback, false
	}
	longCloseIDs, longsClosed := s.rollbackPhase(ctx, buys, tag)
	rollback = append(rollback, longCloseIDs...)
	return rollback, longsClosed
}

func (s *Service) applyBasketRollback(ctx context.Context, result *domain.BasketResult, buys, sells []phaseOrder, tag string) ([]string, bool) {
	ids, safe := s.rollbackBasket(ctx, buys, sells, tag)
	if !safe {
		result.Status = "ATTENTION_REQUIRED"
		result.Message = "Basket rollback is incomplete. Protective long fills are retained whenever short closure is not confirmed."
	}
	return ids, safe
}

func (s *Service) rollbackPhase(ctx context.Context, fills []phaseOrder, tag string) ([]string, bool) {
	ids := make([]string, 0, len(fills))
	orders := make([]phaseOrder, 0, len(fills))
	submissionFailed := false
	for _, submitted := range fills {
		if submitted.Filled <= 0 {
			continue
		}
		side := "SELL"
		if submitted.Leg.TransactionType == "SELL" {
			side = "BUY"
		}
		orderID, err := s.broker.Place(ctx, domain.OrderRequest{
			Variety: "regular", Exchange: submitted.Leg.Exchange, TradingSymbol: submitted.Leg.TradingSymbol,
			Product: submitted.Leg.Product, OrderType: "MARKET", TransactionType: side,
			Validity: "IOC", Quantity: submitted.Filled, Tag: tag,
		})
		if err != nil {
			if errors.Is(err, domain.ErrNotAuthenticated) {
				s.markAuthenticationRequired("Kite session expired during basket rollback")
			}
			submissionFailed = true
			continue
		}
		ids = append(ids, orderID)
		orders = append(orders, phaseOrder{OrderID: orderID, Leg: domain.BasketLeg{Exchange: submitted.Leg.Exchange, TradingSymbol: submitted.Leg.TradingSymbol, Product: submitted.Leg.Product, TransactionType: side, Quantity: submitted.Filled}})
		s.auditBestEffort(ctx, domain.AuditEvent{Type: "BASKET_ROLLBACK", Code: domain.CodeApproved, Message: "Basket fill rollback submitted.", OrderID: orderID})
	}
	complete := true
	if len(orders) > 0 {
		_, confirmed, err := s.awaitPhase(ctx, orders, 3*time.Second)
		complete = err == nil && confirmed
	}
	if submissionFailed || !complete {
		s.gate.Lock()
		for _, order := range orders {
			s.attention[order.OrderID] = order.Leg.Quantity
		}
		s.gate.Unlock()
		s.markBasketAttention("basket rollback could not be confirmed; protective longs retained")
		return ids, false
	}
	return ids, true
}

func (s *Service) markBasketAttention(message string) {
	s.gate.Lock()
	if !s.authed {
		s.runtime = domain.RuntimeAuthRequired
	} else {
		s.runtime = domain.RuntimeDegraded
	}
	s.hardAttention = true
	s.lockRecord.LastError = message
	s.gate.Unlock()
}

func (s *Service) ensureBasketMayContinue() error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.trading == domain.TradingLocked {
		return fmt.Errorf("daily loss lock triggered during basket deployment")
	}
	if s.runtime != domain.RuntimeBasket {
		return fmt.Errorf("risk monitor changed state during basket deployment")
	}
	return nil
}

func (s *Service) finishBasket(ctx context.Context, result domain.BasketResult, basketErr error) {
	s.gate.Lock()
	if s.trading == domain.TradingActive && s.runtime == domain.RuntimeBasket {
		if basketErr == nil || result.Status == "ROLLED_BACK" {
			s.runtime = domain.RuntimeDegraded
			s.lockRecord.LastError = "basket state is awaiting a fresh broker reconciliation"
		} else {
			s.runtime = domain.RuntimeDegraded
			s.lockRecord.LastError = "basket deployment requires attention"
		}
	}
	s.gate.Unlock()
	code := domain.CodeApproved
	eventType := "BASKET_COMPLETED"
	if basketErr != nil {
		code, eventType = domain.CodeBrokerError, "BASKET_FAILED"
	}
	s.auditBestEffort(ctx, domain.AuditEvent{Type: eventType, Code: code, Message: result.Message, Metadata: map[string]any{"basket_id": result.BasketID, "status": result.Status}})
	s.signal()
}

func phaseIDs(phase []phaseOrder) []string {
	ids := make([]string, 0, len(phase))
	for _, order := range phase {
		ids = append(ids, order.OrderID)
	}
	return ids
}

func (s *Service) ExitPosition(ctx context.Context, token uint32, product string) (string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if !s.authed {
		return "", domain.ErrNotAuthenticated
	}
	product = strings.ToUpper(strings.TrimSpace(product))
	if product != "MIS" && product != "NRML" {
		return "", fmt.Errorf("position product must be MIS or NRML")
	}
	positions, err := s.broker.Positions(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.setAuthRequiredLocked("Kite session expired while reading positions")
		}
		return "", err
	}
	for _, position := range positions {
		if position.InstrumentToken == token && position.Product == product && domain.IsFNOExchange(position.Exchange) && position.Quantity != 0 {
			key := positionKeyFor(position.Exchange, position.TradingSymbol, position.Product)
			if existingID, submitted := s.forcedExits[key]; submitted {
				found, active := intentOrderState(s.orders, existingID)
				if !found || active {
					return existingID, fmt.Errorf("a previous exit submission is still awaiting broker reconciliation")
				}
				delete(s.forcedExits, key)
				delete(s.forcedExitAt, key)
			}
			if instrument, ok := s.instrumentByTokenLocked(token); ok && domain.IsOptionType(instrument.InstrumentType) {
				if err := s.validateOptionCoverageLocked(instrument, position.Product, -position.Quantity, positions, nil); err != nil {
					return "", err
				}
			}
			if err := s.reserveForcedExitIntentLocked(ctx, key); err != nil {
				return "", err
			}
			orderID, err := s.broker.ExitPosition(ctx, position)
			if orderID != "" {
				s.forcedExits[key] = orderID
				if persistErr := s.persistForcedExitIntentLocked(ctx, key, orderID); persistErr != nil {
					err = errors.Join(err, persistErr)
				}
				s.auditBestEffort(ctx, domain.AuditEvent{Type: "POSITION_EXIT", Code: domain.CodeApproved, Message: "Risk-reducing position exit submitted.", OrderID: orderID, Metadata: map[string]any{"instrument_token": token, "exchange": position.Exchange, "tradingsymbol": position.TradingSymbol, "product": position.Product}})
				if s.trading == domain.TradingActive {
					s.runtime = domain.RuntimeDegraded
					s.lockRecord.LastError = "position exit is awaiting a fresh broker reconciliation"
				}
				s.signalAsync()
			}
			if err != nil {
				if errors.Is(err, domain.ErrNotAuthenticated) {
					s.setAuthRequiredLocked("Kite session expired while exiting a position")
				}
				return orderID, err
			}
			return orderID, nil
		}
	}
	return "", fmt.Errorf("open NFO/BFO position and product not found")
}

func (s *Service) liquidate(ctx context.Context) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		complete, err := s.liquidationPass(ctx)
		if err == nil && complete {
			s.gate.Lock()
			s.runtime = domain.RuntimeReady
			s.lockRecord.LiquidationState = "COMPLETED"
			s.lockRecord.LastError = ""
			s.gate.Unlock()
			if updateErr := s.store.UpdateLiquidation(ctx, "COMPLETED", ""); updateErr != nil {
				return updateErr
			}
			s.auditBestEffort(ctx, domain.AuditEvent{Type: "LIQUIDATION_COMPLETED", Code: domain.CodeApproved, Message: "All NFO/BFO orders cancelled and positions reconciled flat."})
			s.signal()
			return nil
		}
		if errors.Is(err, domain.ErrNotAuthenticated) {
			s.gate.Lock()
			s.setAuthRequiredLocked("Kite session expired during liquidation; reconnect to continue")
			s.lockRecord.LiquidationState = "RETRYING"
			s.gate.Unlock()
			_ = s.store.UpdateLiquidation(ctx, "RETRYING", "Kite session expired during liquidation; reconnect to continue")
			s.auditBestEffort(ctx, domain.AuditEvent{Type: "LIQUIDATION_AUTH_REQUIRED", Code: domain.CodeAuthRequired, Message: "Liquidation paused until Kite is reconnected."})
			s.signal()
			return err
		}
		if err != nil {
			lastErr = err
		}
		delay := time.Duration(1<<attempt) * 250 * time.Millisecond
		if delay > 4*time.Second {
			delay = 4 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("positions or pending orders remain after reconciliation")
	}
	s.gate.Lock()
	s.runtime = domain.RuntimeLiquidating
	s.lockRecord.LiquidationState = "RETRYING"
	s.lockRecord.LastError = lastErr.Error()
	s.gate.Unlock()
	_ = s.store.UpdateLiquidation(ctx, "RETRYING", lastErr.Error())
	s.auditBestEffort(ctx, domain.AuditEvent{Type: "LIQUIDATION_INCOMPLETE", Code: domain.CodeBrokerError, Message: "Liquidation remains incomplete and will retry."})
	s.signal()
	return lastErr
}

func (s *Service) liquidationPass(ctx context.Context) (bool, error) {
	s.gate.Lock()
	intents := copyStringMap(s.forcedExits)
	s.gate.Unlock()
	orders, err := s.broker.Orders(ctx)
	if err != nil {
		return false, err
	}
	var passErr error
	for _, order := range orders {
		if domain.IsFNOExchange(order.Exchange) && order.Cancellable() && !isForcedExitOrder(order, intents) {
			if err := s.broker.Cancel(ctx, order.Variety, order.OrderID); err != nil {
				passErr = errors.Join(passErr, fmt.Errorf("cancel %s: %w", order.OrderID, err))
			} else {
				s.auditBestEffort(ctx, domain.AuditEvent{Type: "FORCED_CANCEL", Code: domain.CodeApproved, Message: "Pending F&O order cancellation submitted.", OrderID: order.OrderID})
			}
		}
	}
	orders, err = s.broker.Orders(ctx)
	if err != nil {
		return false, errors.Join(passErr, err)
	}
	activeExits := make(map[string][]domain.Order)
	seenIntents := make(map[string]bool)
	for _, order := range orders {
		if domain.IsFNOExchange(order.Exchange) && order.Cancellable() && !isForcedExitOrder(order, intents) {
			return false, passErr
		}
	}
	for _, order := range orders {
		if isForcedExitOrder(order, intents) {
			key := positionKeyFor(order.Exchange, order.TradingSymbol, order.Product)
			if parentID, ok := intents[key]; ok && orderMatchesIntent(order, parentID) {
				seenIntents[key] = true
			}
			if order.Cancellable() {
				activeExits[key] = append(activeExits[key], order)
			}
		}
	}
	positions, err := s.broker.Positions(ctx)
	if err != nil {
		return false, errors.Join(passErr, err)
	}
	open := 0
	openKeys := make(map[string]bool)
	for _, position := range positions {
		if domain.IsFNOExchange(position.Exchange) && position.Quantity != 0 {
			openKeys[positionKeyFor(position.Exchange, position.TradingSymbol, position.Product)] = true
		}
	}
	for key, activeOrders := range activeExits {
		if openKeys[key] {
			continue
		}
		for _, order := range activeOrders {
			if err := s.broker.Cancel(ctx, order.Variety, order.OrderID); err != nil {
				passErr = errors.Join(passErr, fmt.Errorf("cancel orphan forced exit %s: %w", order.OrderID, err))
			} else {
				s.auditBestEffort(ctx, domain.AuditEvent{Type: "FORCED_EXIT_CANCEL", Code: domain.CodeApproved, Message: "Forced exit cancelled because its position was already flat.", OrderID: order.OrderID})
			}
		}
	}
	for _, position := range positions {
		if !domain.IsFNOExchange(position.Exchange) || position.Quantity == 0 {
			continue
		}
		open++
		key := positionKeyFor(position.Exchange, position.TradingSymbol, position.Product)
		if len(activeExits[key]) > 0 {
			continue
		}
		if _, submitted := intents[key]; submitted && !seenIntents[key] {
			passErr = errors.Join(passErr, fmt.Errorf("forced exit %s is not yet visible in the order book", intents[key]))
			continue
		}
		if err := s.reserveForcedExitIntent(ctx, key); err != nil {
			passErr = errors.Join(passErr, err)
			continue
		}
		intents[key] = s.forcedExitIntent(key)
		orderID, err := s.broker.ExitPosition(ctx, position)
		if orderID != "" {
			s.gate.Lock()
			s.forcedExits[key] = orderID
			s.gate.Unlock()
			intents[key] = orderID
			if persistErr := s.store.PutLiquidationIntent(ctx, domain.LiquidationIntent{PositionKey: key, OrderID: orderID, CreatedAt: s.now()}); persistErr != nil {
				passErr = errors.Join(passErr, persistErr)
			}
			s.auditBestEffort(ctx, domain.AuditEvent{Type: "FORCED_EXIT", Code: domain.CodeApproved, Message: "Forced F&O position exit submitted.", OrderID: orderID, Metadata: map[string]any{"exchange": position.Exchange, "tradingsymbol": position.TradingSymbol, "product": position.Product}})
		}
		if err != nil {
			passErr = errors.Join(passErr, fmt.Errorf("exit %s: %w", position.TradingSymbol, err))
			continue
		}
	}
	if open > 0 {
		return false, passErr
	}
	for key, orderID := range intents {
		if !seenIntents[key] {
			passErr = errors.Join(passErr, fmt.Errorf("forced exit %s is not yet visible in the order book", orderID))
		}
	}
	s.gate.Lock()
	for key := range s.forcedExits {
		if !openKeys[key] && len(activeExits[key]) == 0 && seenIntents[key] {
			if err := s.store.DeleteLiquidationIntent(ctx, key); err != nil {
				passErr = errors.Join(passErr, err)
			} else {
				delete(s.forcedExits, key)
				delete(s.forcedExitAt, key)
			}
		}
	}
	s.gate.Unlock()
	for _, order := range orders {
		if domain.IsFNOExchange(order.Exchange) && order.Cancellable() {
			return false, passErr
		}
	}
	return passErr == nil, passErr
}

func intentOrderState(orders []domain.Order, parentID string) (found, active bool) {
	for _, order := range orders {
		if orderMatchesIntent(order, parentID) {
			found = true
			active = active || order.Cancellable()
		}
	}
	return found, active
}

func (s *Service) MaybeUnlock(ctx context.Context) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.maybeUnlockLocked(ctx)
}

func (s *Service) maybeUnlockLocked(ctx context.Context) error {
	if s.trading != domain.TradingLocked || s.lockRecord.UnlockAt.IsZero() || s.now().Before(s.lockRecord.UnlockAt) {
		return nil
	}
	if s.lockRecord.LiquidationState != "COMPLETED" {
		s.lockRecord.LastError = "scheduled unlock withheld until liquidation is reconciled complete"
		if s.authed {
			s.runtime = domain.RuntimeLiquidating
		} else {
			s.runtime = domain.RuntimeAuthRequired
		}
		return nil
	}
	now := s.now()
	if err := s.store.Unlock(ctx, now, domain.AuditEvent{Type: "AUTOMATIC_UNLOCK", Code: domain.CodeApproved, Message: "Trading automatically unlocked for the next trading day."}); err != nil {
		return err
	}
	s.trading = domain.TradingActive
	s.lockRecord = domain.LockRecord{Status: domain.TradingActive}
	if s.authed {
		s.runtime = domain.RuntimeDegraded
		s.lockRecord.LastError = "automatic unlock completed; fresh risk state is initializing"
	} else {
		s.runtime = domain.RuntimeAuthRequired
	}
	s.signalAsync()
	return nil
}

func (s *Service) Snapshot() domain.Snapshot {
	s.gate.Lock()
	defer s.gate.Unlock()
	message := "Risk controls active."
	if s.trading == domain.TradingLocked {
		message = domain.LockedMessage
		if s.lockRecord.TriggeredAt.IsZero() || s.calendar.TradingDate(s.lockRecord.TriggeredAt) == s.calendar.TradingDate(s.now()) {
			message = domain.LockMessage
		}
	} else if s.runtime == domain.RuntimeAuthRequired {
		message = "Connect your Kite account before trading."
	} else if s.runtime == domain.RuntimeDegraded {
		message = "Risk monitoring is degraded. New orders are blocked."
	}
	openQty, pending := 0, 0
	for _, position := range s.positions {
		if position.Quantity < 0 {
			openQty -= position.Quantity
		} else {
			openQty += position.Quantity
		}
	}
	for _, order := range s.orders {
		if order.Cancellable() {
			pending++
		}
	}
	return domain.Snapshot{
		TradingStatus: s.trading, RuntimeStatus: s.runtime, Message: message,
		MTMPaise: s.mtm, LossLimitPaise: domain.LossLimitPaise, LastRefresh: optionalTime(s.lastUpdate),
		Authenticated: s.authed, Liquidation: s.lockRecord.LiquidationState,
		LastError: s.lockRecord.LastError, NextUnlock: optionalTime(s.lockRecord.UnlockAt),
		OpenPositionQty: openQty, PendingOrders: pending,
	}
}

func (s *Service) Positions() []domain.Position {
	s.gate.Lock()
	defer s.gate.Unlock()
	return append(make([]domain.Position, 0, len(s.positions)), s.positions...)
}

func (s *Service) Orders() []domain.Order {
	s.gate.Lock()
	defer s.gate.Unlock()
	return append(make([]domain.Order, 0, len(s.orders)), s.orders...)
}

func (s *Service) SearchInstruments(query, exchange, kind string, limit int) []domain.Instrument {
	s.gate.Lock()
	defer s.gate.Unlock()
	query = strings.ToUpper(strings.TrimSpace(query))
	exchange = strings.ToUpper(strings.TrimSpace(exchange))
	kind = strings.ToUpper(strings.TrimSpace(kind))
	if exchange != "NFO" && exchange != "BFO" {
		return []domain.Instrument{}
	}
	if kind != "" && kind != "OPTION" && kind != "FUTURE" {
		return []domain.Instrument{}
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	result := make([]domain.Instrument, 0, limit)
	for _, instrument := range s.instruments {
		kindMatches := kind == "" || (kind == "OPTION" && (instrument.InstrumentType == "CE" || instrument.InstrumentType == "PE")) || (kind == "FUTURE" && instrument.InstrumentType == "FUT")
		if instrument.Exchange == exchange && kindMatches && (query == "" || strings.Contains(strings.ToUpper(instrument.TradingSymbol), query) || strings.Contains(strings.ToUpper(instrument.Name), query)) {
			result = append(result, instrument)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Expiry.Equal(result[j].Expiry) {
			return result[i].TradingSymbol < result[j].TradingSymbol
		}
		return result[i].Expiry.Before(result[j].Expiry)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func (s *Service) Audit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	return s.store.ListAudit(ctx, limit)
}

func (s *Service) reject(code domain.DecisionCode, message string) domain.RiskDecision {
	return domain.RiskDecision{Allowed: false, Code: code, Message: message, EvaluatedMTM: s.mtm, TradingStatus: s.trading, Timestamp: s.now()}
}

func (s *Service) preTradeStateDecisionLocked() (domain.RiskDecision, bool) {
	if s.trading == domain.TradingLocked {
		return s.reject(domain.CodeTradingLocked, domain.LockedMessage), true
	}
	if s.trading != domain.TradingActive {
		return s.reject(domain.CodeMonitoringDegraded, "Trading state is unavailable; new orders are blocked."), true
	}
	if !s.authed || s.runtime == domain.RuntimeAuthRequired {
		return s.reject(domain.CodeAuthRequired, "Connect your Kite account before trading."), true
	}
	if s.runtime != domain.RuntimeReady {
		return s.reject(domain.CodeMonitoringDegraded, "New orders are blocked because risk monitoring is unavailable."), true
	}
	return domain.RiskDecision{}, false
}

func (s *Service) setAuthRequiredLocked(message string) {
	s.authed = false
	s.runtime = domain.RuntimeAuthRequired
	s.lockRecord.LastError = message
}

func (s *Service) markAuthenticationRequired(message string) {
	s.gate.Lock()
	s.setAuthRequiredLocked(message)
	s.gate.Unlock()
	s.signal()
}

func (s *Service) approve(code domain.DecisionCode, message string) domain.RiskDecision {
	return domain.RiskDecision{Allowed: true, Code: code, Message: message, EvaluatedMTM: s.mtm, TradingStatus: s.trading, Timestamp: s.now()}
}

func (s *Service) auditDecision(ctx context.Context, kind string, decision domain.RiskDecision, request domain.OrderRequest, orderID string) {
	s.auditBestEffort(ctx, domain.AuditEvent{Type: kind, Code: decision.Code, Message: decision.Message, OrderID: orderID, Metadata: map[string]any{
		"exchange": request.Exchange, "tradingsymbol": request.TradingSymbol, "transaction_type": request.TransactionType,
		"order_type": request.OrderType, "quantity": request.Quantity,
	}})
}

func (s *Service) auditModifyDecision(ctx context.Context, kind string, decision domain.RiskDecision, orderID string) {
	s.auditBestEffort(ctx, domain.AuditEvent{Type: kind, Code: decision.Code, Message: decision.Message, OrderID: orderID})
}

func (s *Service) auditBestEffort(ctx context.Context, event domain.AuditEvent) {
	event.CreatedAt = s.now()
	if err := s.store.AppendAudit(ctx, event); err != nil {
		s.log.Printf("level=error message=%q event_type=%q error=%q", "audit append failed", event.Type, err)
	}
}

func (s *Service) decorateOrdersLocked() {
	for index := range s.orders {
		if instrument, ok := s.instruments[instrumentKey(s.orders[index].Exchange, s.orders[index].TradingSymbol)]; ok {
			s.orders[index].InstrumentType = instrument.InstrumentType
		}
	}
}

func (s *Service) cacheSubmittedOrderLocked(orderID string, request domain.OrderRequest, instrumentToken uint32) {
	s.orders = append(s.orders, domain.Order{
		OrderID: orderID, Variety: request.Variety, Status: "SUBMITTED", Exchange: request.Exchange,
		TradingSymbol: request.TradingSymbol, InstrumentToken: instrumentToken, InstrumentType: request.InstrumentType,
		Product: request.Product, OrderType: request.OrderType, TransactionType: request.TransactionType,
		Validity: request.Validity, Quantity: request.Quantity, PendingQuantity: request.Quantity,
		Price: request.Price, TriggerPrice: request.TriggerPrice, Tag: request.Tag,
	})
}

func (s *Service) cacheModifiedOrderLocked(originalID, modifiedID string, request domain.ModifyRequest, current domain.Order) {
	updated := current
	if modifiedID != "" {
		updated.OrderID = modifiedID
	}
	updated.Quantity = request.Quantity
	updated.PendingQuantity = request.Quantity - current.FilledQuantity
	if updated.PendingQuantity < 0 {
		updated.PendingQuantity = 0
	}
	updated.OrderType = request.OrderType
	updated.Validity = request.Validity
	updated.Price = request.Price
	updated.TriggerPrice = request.TriggerPrice
	updated.Status = "SUBMITTED"
	for index := range s.orders {
		if s.orders[index].OrderID == originalID {
			s.orders[index] = updated
			return
		}
	}
	s.orders = append(s.orders, updated)
}

type positionKey struct {
	token   uint32
	product string
}

type optionGroup struct {
	instrument domain.Instrument
	product    string
}

func (s *Service) validateOptionCoverageLocked(target domain.Instrument, targetProduct string, delta int, positions []domain.Position, orders []domain.Order) error {
	if positions == nil {
		positions = s.positions
	}
	if orders == nil {
		orders = s.orders
	}
	quantities := make(map[positionKey]int)
	for _, position := range positions {
		if !domain.IsFNOExchange(position.Exchange) || position.Quantity == 0 {
			continue
		}
		instrument, ok := s.instrumentByTokenLocked(position.InstrumentToken)
		if !ok {
			instrument, ok = s.instruments[instrumentKey(position.Exchange, position.TradingSymbol)]
		}
		if !ok {
			return fmt.Errorf("existing F&O position %s:%s has no current instrument metadata; option exposure cannot be verified", position.Exchange, position.TradingSymbol)
		}
		quantities[positionKey{token: instrument.Token, product: position.Product}] += position.Quantity
	}
	for _, order := range orders {
		// A pending BUY is not protection: the new SELL could fill first. A
		// pending SELL is counted as exposure because it may fill at any time.
		if !order.Cancellable() || order.TransactionType != "SELL" {
			continue
		}
		instrument, ok := s.instrumentByTokenLocked(order.InstrumentToken)
		if !ok {
			instrument, ok = s.instruments[instrumentKey(order.Exchange, order.TradingSymbol)]
		}
		if !ok {
			return fmt.Errorf("pending SELL order %s has no current instrument metadata; option exposure cannot be verified", order.OrderID)
		}
		if !domain.IsOptionType(instrument.InstrumentType) {
			continue
		}
		quantities[positionKey{token: instrument.Token, product: order.Product}] -= order.RemainingQuantity()
	}
	quantities[positionKey{token: target.Token, product: targetProduct}] += delta
	longQty, shortQty := 0, 0
	for key, quantity := range quantities {
		instrument, ok := s.instrumentByTokenLocked(key.token)
		if !ok || key.product != targetProduct || instrument.Exchange != target.Exchange || instrument.Name != target.Name || !instrument.Expiry.Equal(target.Expiry) || instrument.InstrumentType != target.InstrumentType {
			continue
		}
		if quantity > 0 {
			longQty += quantity
		} else {
			shortQty -= quantity
		}
	}
	if shortQty > longQty {
		return fmt.Errorf("resulting %s exposure would have %d unprotected short quantity; use a validated hedge basket", target.InstrumentType, shortQty-longQty)
	}
	return nil
}

func (s *Service) validateBasketPortfolioCoverageLocked(validated risk.ValidatedBasket) error {
	quantities := make(map[positionKey]int)
	for _, position := range s.positions {
		if position.Quantity == 0 {
			continue
		}
		instrument, ok := s.instrumentByTokenLocked(position.InstrumentToken)
		if !ok {
			instrument, ok = s.instruments[instrumentKey(position.Exchange, position.TradingSymbol)]
		}
		if !ok {
			return fmt.Errorf("existing F&O position %s:%s has no current instrument metadata; basket exposure cannot be verified", position.Exchange, position.TradingSymbol)
		}
		quantities[positionKey{token: instrument.Token, product: position.Product}] += position.Quantity
	}
	for _, order := range s.orders {
		// Only pending SELL quantity is included. Pending BUY quantity cannot
		// safely cover a short until the broker reports its fill in positions.
		if !order.Cancellable() || order.TransactionType != "SELL" {
			continue
		}
		instrument, ok := s.instrumentByTokenLocked(order.InstrumentToken)
		if !ok {
			instrument, ok = s.instruments[instrumentKey(order.Exchange, order.TradingSymbol)]
		}
		if !ok {
			return fmt.Errorf("pending SELL order %s has no current instrument metadata; basket exposure cannot be verified", order.OrderID)
		}
		if !domain.IsOptionType(instrument.InstrumentType) {
			continue
		}
		quantities[positionKey{token: instrument.Token, product: order.Product}] -= order.RemainingQuantity()
	}
	groups := make(map[string]optionGroup)
	for _, leg := range validated.Request.Legs {
		instrument := validated.Instruments[instrumentKey(leg.Exchange, leg.TradingSymbol)]
		delta := leg.Quantity
		if leg.TransactionType == "SELL" {
			delta = -delta
		}
		key := positionKey{token: instrument.Token, product: leg.Product}
		quantities[key] += delta
		group := optionGroup{instrument: instrument, product: leg.Product}
		groups[optionGroupKey(instrument, leg.Product)] = group
	}
	for _, target := range groups {
		longQty, shortQty := 0, 0
		for key, quantity := range quantities {
			instrument, ok := s.instrumentByTokenLocked(key.token)
			if !ok || optionGroupKey(instrument, key.product) != optionGroupKey(target.instrument, target.product) {
				continue
			}
			if quantity > 0 {
				longQty += quantity
			} else {
				shortQty -= quantity
			}
		}
		if shortQty > longQty {
			return fmt.Errorf("resulting portfolio would retain %d uncovered %s quantity for %s %s %s; basket rejected", shortQty-longQty, target.instrument.InstrumentType, target.instrument.Name, target.instrument.Expiry.Format("2006-01-02"), target.product)
		}
	}
	return nil
}

func optionGroupKey(instrument domain.Instrument, product string) string {
	return instrument.Exchange + "|" + instrument.Name + "|" + instrument.Expiry.UTC().Format(time.RFC3339Nano) + "|" + instrument.InstrumentType + "|" + product
}

func validateOrderTicks(price, trigger, tick float64) error {
	if tick <= 0 {
		return nil
	}
	for name, value := range map[string]float64{"price": price, "trigger price": trigger} {
		if value <= 0 {
			continue
		}
		units := value / tick
		if math.Abs(units-math.Round(units)) >= 1e-7 {
			return fmt.Errorf("%s must use the instrument tick size %.4g", name, tick)
		}
	}
	return nil
}

func (s *Service) instrumentByTokenLocked(token uint32) (domain.Instrument, bool) {
	for _, instrument := range s.instruments {
		if instrument.Token == token {
			return instrument, true
		}
	}
	return domain.Instrument{}, false
}

func (s *Service) reconcileAttentionLocked(orders []domain.Order) {
	if len(s.attention) == 0 {
		return
	}
	for orderID, expected := range s.attention {
		order, ok := findOrder(orders, orderID)
		if ok && order.Status == "COMPLETE" && order.FilledQuantity >= expected {
			delete(s.attention, orderID)
		}
	}
}

func (s *Service) signal() {
	s.gate.Lock()
	notify := s.notify
	s.gate.Unlock()
	notify()
}

func (s *Service) signalAsync() {
	go s.signal()
}

func normalizeOrder(request *domain.OrderRequest) {
	request.Exchange = strings.ToUpper(strings.TrimSpace(request.Exchange))
	request.TradingSymbol = strings.ToUpper(strings.TrimSpace(request.TradingSymbol))
	request.Product = strings.ToUpper(strings.TrimSpace(request.Product))
	request.OrderType = strings.ToUpper(strings.TrimSpace(request.OrderType))
	request.TransactionType = strings.ToUpper(strings.TrimSpace(request.TransactionType))
	request.Validity = strings.ToUpper(strings.TrimSpace(request.Validity))
	request.Variety = strings.ToLower(strings.TrimSpace(request.Variety))
}

func instrumentKey(exchange, symbol string) string {
	return strings.ToUpper(exchange) + ":" + strings.ToUpper(symbol)
}

func filterPositions(all []domain.Position) []domain.Position {
	result := make([]domain.Position, 0, len(all))
	for _, position := range all {
		if domain.IsFNOExchange(position.Exchange) {
			result = append(result, position)
		}
	}
	return result
}

func filterOrders(all []domain.Order) []domain.Order {
	result := make([]domain.Order, 0, len(all))
	for _, order := range all {
		if domain.IsFNOExchange(order.Exchange) {
			result = append(result, order)
		}
	}
	return result
}

func portfolioFlat(positions []domain.Position, orders []domain.Order) bool {
	for _, position := range positions {
		if position.Quantity != 0 {
			return false
		}
	}
	for _, order := range orders {
		if order.Cancellable() {
			return false
		}
	}
	return true
}

func isForcedExitOrder(order domain.Order, intents map[string]string) bool {
	if order.Tag == forcedExitTag {
		return true
	}
	for _, tag := range order.Tags {
		if tag == forcedExitTag {
			return true
		}
	}
	for _, parentID := range intents {
		if orderMatchesIntent(order, parentID) {
			return true
		}
	}
	return false
}

func orderHasTagPrefix(order domain.Order, prefix string) bool {
	if strings.HasPrefix(order.Tag, prefix) {
		return true
	}
	for _, tag := range order.Tags {
		if strings.HasPrefix(tag, prefix) {
			return true
		}
	}
	return false
}

func orderMatchesIntent(order domain.Order, parentID string) bool {
	return parentID != "" && (order.OrderID == parentID || order.ParentOrderID == parentID)
}

func positionKeyFor(exchange, symbol, product string) string {
	return instrumentKey(exchange, symbol) + "|" + strings.ToUpper(product)
}

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (s *Service) reserveForcedExitIntent(ctx context.Context, key string) error {
	reference, err := pendingReference("pending-exit:")
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.store.PutLiquidationIntent(ctx, domain.LiquidationIntent{PositionKey: key, OrderID: reference, CreatedAt: now}); err != nil {
		return err
	}
	s.gate.Lock()
	s.forcedExits[key] = reference
	s.forcedExitAt[key] = now
	s.gate.Unlock()
	return nil
}

func (s *Service) reserveForcedExitIntentLocked(ctx context.Context, key string) error {
	reference, err := pendingReference("pending-exit:")
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.store.PutLiquidationIntent(ctx, domain.LiquidationIntent{PositionKey: key, OrderID: reference, CreatedAt: now}); err != nil {
		return err
	}
	s.forcedExits[key] = reference
	s.forcedExitAt[key] = now
	return nil
}

func (s *Service) persistForcedExitIntentLocked(ctx context.Context, key, orderID string) error {
	createdAt := s.forcedExitAt[key]
	if createdAt.IsZero() {
		createdAt = s.now()
		s.forcedExitAt[key] = createdAt
	}
	return s.store.PutLiquidationIntent(ctx, domain.LiquidationIntent{PositionKey: key, OrderID: orderID, CreatedAt: createdAt})
}

func (s *Service) forcedExitIntent(key string) string {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.forcedExits[key]
}

func (s *Service) purgeExpiredExitIntentsLocked(ctx context.Context) error {
	today := s.calendar.TradingDate(s.now())
	for key, createdAt := range s.forcedExitAt {
		if s.calendar.TradingDate(createdAt) == today {
			continue
		}
		if err := s.store.DeleteLiquidationIntent(ctx, key); err != nil {
			return err
		}
		delete(s.forcedExits, key)
		delete(s.forcedExitAt, key)
	}
	return nil
}

func findOrder(orders []domain.Order, id string) (domain.Order, bool) {
	for _, order := range orders {
		if order.OrderID == id {
			return order, true
		}
	}
	return domain.Order{}, false
}

func validIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func pendingReference(prefix string) (string, error) {
	id, err := RandomID()
	if err != nil {
		return "", err
	}
	return prefix + id, nil
}

func (s *Service) idempotentDecision(existing, operation string) (domain.RiskDecision, string, error) {
	if strings.HasPrefix(existing, pendingOrderPrefix) || strings.HasPrefix(existing, pendingModifyPrefix) {
		decision := s.reject(domain.CodeIdempotentReplay, "This "+operation+" request was already started, but its broker result is not confirmed. Reconcile the order book before retrying with a new request.")
		return decision, "", nil
	}
	decision := s.approve(domain.CodeIdempotentReplay, "This "+operation+" request was already submitted.")
	return decision, existing, nil
}

func RandomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate random id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}
