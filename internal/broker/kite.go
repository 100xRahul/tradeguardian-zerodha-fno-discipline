package broker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"

	"tradeguardian/internal/domain"
)

type Kite struct {
	mu                sync.RWMutex
	rateMu            sync.Mutex
	nextRequest       time.Time
	apiSecret         string
	trade             *kiteconnect.Client
	market            *kiteconnect.Client
	apiKey            string
	authed            bool
	accessToken       string
	streamMu          sync.Mutex
	streamCtx         context.Context
	streamCancel      context.CancelFunc
	streamTokens      []uint32
	streamCallbacks   domain.MarketStreamCallbacks
	streamAccessToken string
	streamGeneration  uint64
}

func NewKite(apiKey, apiSecret string) (*Kite, error) {
	if apiKey == "" || apiSecret == "" {
		return nil, fmt.Errorf("KITE_API_KEY and KITE_API_SECRET are required")
	}
	trade := kiteconnect.New(apiKey)
	market := kiteconnect.New(apiKey)
	trade.SetHTTPClient(newIPv4HTTPClient(10 * time.Second))
	market.SetHTTPClient(newIPv4HTTPClient(15 * time.Second))
	// The official ticker client uses Gorilla's package-level dialer and does
	// not expose a dialer hook. Configure it before any stream starts so Kite
	// REST and WebSocket traffic both leave through the whitelisted VPS IPv4.
	websocket.DefaultDialer.NetDialContext = newIPv4DialContext(5 * time.Second)
	return &Kite{apiSecret: apiSecret, trade: trade, market: market, apiKey: apiKey}, nil
}

func newIPv4HTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = newIPv4DialContext(5 * time.Second)
	return &http.Client{Timeout: timeout, Transport: transport}
}

func newIPv4DialContext(timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, _ string, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", address)
	}
}

func (k *Kite) LoginURL(state string) string {
	base := "https://kite.zerodha.com/connect/login"
	values := url.Values{"api_key": {k.apiKey}, "v": {"3"}}
	if state != "" {
		values.Set("redirect_params", "state="+state)
	}
	return base + "?" + values.Encode()
}

func (k *Kite) GenerateSession(ctx context.Context, requestToken string) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	if strings.TrimSpace(requestToken) == "" {
		return domain.Session{}, fmt.Errorf("request token is required")
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return domain.Session{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	session, err := k.trade.GenerateSession(requestToken, k.apiSecret)
	if err != nil {
		return domain.Session{}, fmt.Errorf("generate Kite session: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	k.trade.SetAccessToken(session.AccessToken)
	k.market.SetAccessToken(session.AccessToken)
	k.authed = true
	k.accessToken = session.AccessToken
	return domain.Session{UserID: session.UserID, AccessToken: session.AccessToken}, nil
}

func (k *Kite) RestoreSession(ctx context.Context, session domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(session.AccessToken) == "" {
		return fmt.Errorf("cached Kite access token is empty")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.trade.SetAccessToken(session.AccessToken)
	k.market.SetAccessToken(session.AccessToken)
	k.authed = true
	k.accessToken = session.AccessToken
	return nil
}

func (k *Kite) ensureAuth() error {
	if !k.authed {
		return domain.ErrNotAuthenticated
	}
	return nil
}

func (k *Kite) Positions(ctx context.Context) ([]domain.Position, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return nil, err
	}
	positions, err := k.trade.GetPositions()
	if err != nil {
		return nil, kiteError("get Kite positions", err)
	}
	result := make([]domain.Position, 0, len(positions.Net))
	for _, position := range positions.Net {
		if !domain.IsFNOExchange(position.Exchange) {
			continue
		}
		converted, err := convertKitePosition(position)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, ctx.Err()
}

func convertKitePosition(position kiteconnect.Position) (domain.Position, error) {
	if position.InstrumentToken == 0 || strings.TrimSpace(position.Exchange) == "" || strings.TrimSpace(position.Tradingsymbol) == "" || strings.TrimSpace(position.Product) == "" {
		return domain.Position{}, fmt.Errorf("Kite position contains incomplete instrument identity")
	}
	if position.Multiplier <= 0 || math.IsNaN(position.Multiplier) || math.IsInf(position.Multiplier, 0) {
		return domain.Position{}, fmt.Errorf("Kite position %s:%s contains an invalid multiplier", position.Exchange, position.Tradingsymbol)
	}
	if position.ClosePrice < 0 || math.IsNaN(position.ClosePrice) || math.IsInf(position.ClosePrice, 0) ||
		(position.OvernightQuantity != 0 && position.ClosePrice == 0) ||
		position.LastPrice < 0 || math.IsNaN(position.LastPrice) || math.IsInf(position.LastPrice, 0) ||
		math.IsNaN(position.M2M) || math.IsInf(position.M2M, 0) ||
		position.BuyM2MValue < 0 || math.IsNaN(position.BuyM2MValue) || math.IsInf(position.BuyM2MValue, 0) ||
		position.SellM2MValue < 0 || math.IsNaN(position.SellM2MValue) || math.IsInf(position.SellM2MValue, 0) {
		return domain.Position{}, fmt.Errorf("Kite position %s:%s contains invalid price or MTM data", position.Exchange, position.Tradingsymbol)
	}
	calculatedM2M := (position.SellM2MValue - position.BuyM2MValue) + (float64(position.Quantity) * position.LastPrice * position.Multiplier)
	if math.IsNaN(calculatedM2M) || math.IsInf(calculatedM2M, 0) || math.Round(calculatedM2M*100) != math.Round(position.M2M*100) {
		return domain.Position{}, fmt.Errorf("Kite position %s:%s contains inconsistent MTM components", position.Exchange, position.Tradingsymbol)
	}
	return domain.Position{
		Exchange: position.Exchange, TradingSymbol: position.Tradingsymbol,
		InstrumentToken: position.InstrumentToken, Product: position.Product,
		Quantity: position.Quantity, OvernightQty: position.OvernightQuantity,
		Multiplier: position.Multiplier, ClosePrice: position.ClosePrice,
		BuyM2M: position.BuyM2MValue, SellM2M: position.SellM2MValue,
		M2M: position.M2M, LastPrice: position.LastPrice,
	}, nil
}

func (k *Kite) Trades(ctx context.Context) ([]domain.Trade, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return nil, err
	}
	trades, err := k.trade.GetTrades()
	if err != nil {
		return nil, kiteError("get Kite trades", err)
	}
	result := make([]domain.Trade, 0, len(trades))
	for _, trade := range trades {
		if !domain.IsFNOExchange(trade.Exchange) {
			continue
		}
		converted, err := convertKiteTrade(trade)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, ctx.Err()
}

func convertKiteTrade(trade kiteconnect.Trade) (domain.Trade, error) {
	quantity, err := exactNonNegativeInt("quantity", trade.Quantity)
	if err != nil || quantity == 0 {
		return domain.Trade{}, fmt.Errorf("Kite trade %q has invalid quantity", trade.TradeID)
	}
	if strings.TrimSpace(trade.TradeID) == "" || strings.TrimSpace(trade.OrderID) == "" || trade.InstrumentToken == 0 ||
		strings.TrimSpace(trade.Exchange) == "" || strings.TrimSpace(trade.TradingSymbol) == "" || strings.TrimSpace(trade.Product) == "" ||
		(trade.TransactionType != "BUY" && trade.TransactionType != "SELL") ||
		trade.AveragePrice <= 0 || math.IsNaN(trade.AveragePrice) || math.IsInf(trade.AveragePrice, 0) {
		return domain.Trade{}, fmt.Errorf("Kite trade %q contains invalid execution data", trade.TradeID)
	}
	return domain.Trade{
		TradeID: trade.TradeID, OrderID: trade.OrderID,
		Exchange: trade.Exchange, TradingSymbol: trade.TradingSymbol,
		InstrumentToken: trade.InstrumentToken, Product: trade.Product,
		TransactionType: trade.TransactionType, Quantity: quantity, AveragePrice: trade.AveragePrice,
	}, nil
}

func (k *Kite) Orders(ctx context.Context) ([]domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return nil, err
	}
	orders, err := k.trade.GetOrders()
	if err != nil {
		return nil, kiteError("get Kite orders", err)
	}
	result := make([]domain.Order, 0, len(orders))
	for _, order := range orders {
		converted, err := convertKiteOrder(order)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, ctx.Err()
}

func (k *Kite) Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	instruments, err := k.market.GetInstrumentsByExchange(exchange)
	if err != nil {
		return nil, kiteError("get "+exchange+" instruments", err)
	}
	result := make([]domain.Instrument, 0, len(instruments))
	for _, instrument := range instruments {
		converted, err := convertKiteInstrument(instrument)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, ctx.Err()
}

func convertKiteOrder(order kiteconnect.Order) (domain.Order, error) {
	quantity, err := exactNonNegativeInt("quantity", order.Quantity)
	if err != nil {
		return domain.Order{}, fmt.Errorf("Kite order %q: %w", order.OrderID, err)
	}
	pending, err := exactNonNegativeInt("pending_quantity", order.PendingQuantity)
	if err != nil {
		return domain.Order{}, fmt.Errorf("Kite order %q: %w", order.OrderID, err)
	}
	filled, err := exactNonNegativeInt("filled_quantity", order.FilledQuantity)
	if err != nil {
		return domain.Order{}, fmt.Errorf("Kite order %q: %w", order.OrderID, err)
	}
	cancelled, err := exactNonNegativeInt("cancelled_quantity", order.CancelledQuantity)
	if err != nil {
		return domain.Order{}, fmt.Errorf("Kite order %q: %w", order.OrderID, err)
	}
	return domain.Order{
		OrderID: order.OrderID, ParentOrderID: order.ParentOrderID, Variety: order.Variety, Status: order.Status,
		Exchange: order.Exchange, TradingSymbol: order.TradingSymbol,
		InstrumentToken: order.InstrumentToken, Product: order.Product,
		OrderType: order.OrderType, TransactionType: order.TransactionType,
		Validity: order.Validity, Quantity: quantity,
		PendingQuantity: pending, FilledQuantity: filled, CancelledQty: cancelled, Price: order.Price,
		TriggerPrice: order.TriggerPrice, StatusMessage: order.StatusMessage,
		Tag: order.Tag, Tags: append([]string(nil), order.Tags...),
	}, nil
}

func convertKiteInstrument(instrument kiteconnect.Instrument) (domain.Instrument, error) {
	if instrument.InstrumentToken <= 0 || uint64(instrument.InstrumentToken) > uint64(^uint32(0)) {
		return domain.Instrument{}, fmt.Errorf("Kite instrument %q has invalid instrument_token", instrument.Tradingsymbol)
	}
	lotSize, err := exactNonNegativeInt("lot_size", instrument.LotSize)
	if err != nil || lotSize == 0 {
		return domain.Instrument{}, fmt.Errorf("Kite instrument %q has invalid lot_size", instrument.Tradingsymbol)
	}
	if instrument.TickSize <= 0 || math.IsNaN(instrument.TickSize) || math.IsInf(instrument.TickSize, 0) {
		return domain.Instrument{}, fmt.Errorf("Kite instrument %q has invalid tick_size", instrument.Tradingsymbol)
	}
	if instrument.StrikePrice < 0 || math.IsNaN(instrument.StrikePrice) || math.IsInf(instrument.StrikePrice, 0) {
		return domain.Instrument{}, fmt.Errorf("Kite instrument %q has invalid strike", instrument.Tradingsymbol)
	}
	return domain.Instrument{
		Token: uint32(instrument.InstrumentToken), Exchange: instrument.Exchange,
		TradingSymbol: instrument.Tradingsymbol, Name: instrument.Name,
		InstrumentType: strings.ToUpper(instrument.InstrumentType), Expiry: instrument.Expiry.Time,
		Strike: instrument.StrikePrice, TickSize: instrument.TickSize, LotSize: lotSize,
	}, nil
}

func exactNonNegativeInt(field string, value float64) (int, error) {
	maxValue := float64(int(^uint(0) >> 1))
	if maxValue > float64(1<<53-1) {
		maxValue = float64(1<<53 - 1)
	}
	if value < 0 || value > maxValue || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, fmt.Errorf("%s is not an exact non-negative integer", field)
	}
	return int(value), nil
}

func (k *Kite) Place(ctx context.Context, request domain.OrderRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return "", err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return "", err
	}
	params := toOrderParams(request)
	response, err := k.trade.PlaceOrder("regular", params)
	if err != nil {
		return "", kiteError("place Kite order", err)
	}
	if response.OrderID == "" {
		return "", fmt.Errorf("Kite accepted order without an order id")
	}
	return response.OrderID, ctx.Err()
}

func (k *Kite) Modify(ctx context.Context, orderID string, request domain.ModifyRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return "", err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return "", err
	}
	response, err := k.trade.ModifyOrder("regular", orderID, kiteconnect.OrderParams{
		Quantity: request.Quantity, OrderType: request.OrderType, Validity: request.Validity,
		Price: request.Price, TriggerPrice: request.TriggerPrice,
	})
	if err != nil {
		return "", kiteError("modify Kite order", err)
	}
	return response.OrderID, ctx.Err()
}

func (k *Kite) Cancel(ctx context.Context, variety, orderID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return err
	}
	if _, err := k.trade.CancelOrder(variety, orderID, nil); err != nil {
		return kiteError("cancel Kite order", err)
	}
	return ctx.Err()
}

func (k *Kite) ExitPosition(ctx context.Context, position domain.Position) (string, error) {
	transaction := "SELL"
	quantity := position.Quantity
	if quantity < 0 {
		transaction = "BUY"
		quantity = -quantity
	}
	request := domain.OrderRequest{
		Variety: "regular", Exchange: position.Exchange, TradingSymbol: position.TradingSymbol,
		Product: position.Product, OrderType: "MARKET", TransactionType: transaction,
		Validity: "DAY", Quantity: quantity, Tag: domain.ForcedExitTag,
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := k.waitRateLimit(ctx); err != nil {
		return "", err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.ensureAuth(); err != nil {
		return "", err
	}
	params := toOrderParams(request)
	params.MarketProtection = -1
	params.Autoslice = true
	response, err := k.trade.PlaceOrder("regular", params)
	if err != nil {
		return "", kiteError("exit Kite position", err)
	}
	if response.OrderID == "" {
		return "", fmt.Errorf("Kite accepted exit without an order id")
	}
	if err := autosliceError(response); err != nil {
		return response.OrderID, err
	}
	return response.OrderID, ctx.Err()
}

func (k *Kite) StartMarketStream(ctx context.Context, tokens []uint32, callbacks domain.MarketStreamCallbacks) error {
	if ctx == nil {
		return fmt.Errorf("market stream context is required")
	}
	normalized, err := normalizeMarketTokens(tokens)
	if err != nil {
		return err
	}
	k.mu.RLock()
	authed, accessToken := k.authed, k.accessToken
	k.mu.RUnlock()
	if !authed || accessToken == "" {
		return domain.ErrNotAuthenticated
	}
	callbacks = normalizeMarketCallbacks(callbacks)

	k.streamMu.Lock()
	defer k.streamMu.Unlock()
	if k.streamCancel != nil {
		k.streamCancel()
	}
	k.streamCtx = ctx
	k.streamTokens = normalized
	k.streamCallbacks = callbacks
	k.streamAccessToken = accessToken
	callbacks.OnStatus(false)
	k.startMarketStreamLocked()
	return nil
}

func (k *Kite) SetMarketSubscriptions(tokens []uint32) error {
	normalized, err := normalizeMarketTokens(tokens)
	if err != nil {
		return err
	}
	k.streamMu.Lock()
	defer k.streamMu.Unlock()
	if equalMarketTokens(k.streamTokens, normalized) {
		return nil
	}
	k.streamTokens = normalized
	if k.streamCtx == nil || k.streamAccessToken == "" {
		return nil
	}
	k.streamCallbacks.OnStatus(false)
	if k.streamCancel != nil {
		k.streamCancel()
	}
	k.startMarketStreamLocked()
	return nil
}

func (k *Kite) startMarketStreamLocked() {
	streamCtx, cancel := context.WithCancel(k.streamCtx)
	k.streamCancel = cancel
	k.streamGeneration++
	generation := k.streamGeneration
	tokens := append([]uint32(nil), k.streamTokens...)
	callbacks := k.streamCallbacks
	accessToken := k.streamAccessToken
	go k.runMarketStream(streamCtx, generation, accessToken, tokens, callbacks)
}

func (k *Kite) runMarketStream(ctx context.Context, generation uint64, accessToken string, tokens []uint32, callbacks domain.MarketStreamCallbacks) {
	setStatus := func(connected bool) {
		if k.currentMarketStream(generation) {
			callbacks.OnStatus(connected)
		}
	}
	for ctx.Err() == nil {
		stream := kiteticker.New(k.apiKey, accessToken)
		stream.SetAutoReconnect(true)
		stream.SetReconnectMaxRetries(20)
		stream.OnConnect(func() {
			if !k.currentMarketStream(generation) {
				return
			}
			if len(tokens) > 0 {
				if err := stream.Subscribe(tokens); err != nil {
					setStatus(false)
					if k.currentMarketStream(generation) {
						callbacks.OnError(redactMarketStreamError(fmt.Errorf("subscribe Kite market tokens: %w", err)))
					}
					_ = stream.Close()
					return
				}
				if err := stream.SetMode(kiteticker.ModeLTP, tokens); err != nil {
					setStatus(false)
					if k.currentMarketStream(generation) {
						callbacks.OnError(redactMarketStreamError(fmt.Errorf("set Kite market mode: %w", err)))
					}
					_ = stream.Close()
					return
				}
			}
			setStatus(true)
		})
		stream.OnTick(func(tick models.Tick) {
			if k.currentMarketStream(generation) {
				callbacks.OnTick(domain.MarketTick{InstrumentToken: tick.InstrumentToken, LastPrice: tick.LastPrice, ReceivedAt: time.Now()})
			}
		})
		stream.OnOrderUpdate(func(_ kiteconnect.Order) {
			if k.currentMarketStream(generation) {
				callbacks.OnOrderUpdate()
			}
		})
		stream.OnReconnect(func(_ int, _ time.Duration) { setStatus(false) })
		stream.OnNoReconnect(func(_ int) { setStatus(false) })
		stream.OnClose(func(_ int, _ string) { setStatus(false) })
		stream.OnError(func(err error) {
			if k.currentMarketStream(generation) {
				callbacks.OnError(redactMarketStreamError(err))
			}
		})
		stream.ServeWithContext(ctx)
		setStatus(false)
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (k *Kite) currentMarketStream(generation uint64) bool {
	k.streamMu.Lock()
	defer k.streamMu.Unlock()
	return k.streamGeneration == generation
}

func normalizeMarketTokens(tokens []uint32) ([]uint32, error) {
	seen := make(map[uint32]struct{}, len(tokens))
	for _, token := range tokens {
		if token == 0 {
			return nil, fmt.Errorf("market subscription contains an invalid instrument token")
		}
		seen[token] = struct{}{}
	}
	if len(seen) > 3000 {
		return nil, fmt.Errorf("market subscription exceeds Kite's 3000-instrument limit")
	}
	result := make([]uint32, 0, len(seen))
	for token := range seen {
		result = append(result, token)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func equalMarketTokens(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func normalizeMarketCallbacks(callbacks domain.MarketStreamCallbacks) domain.MarketStreamCallbacks {
	if callbacks.OnTick == nil {
		callbacks.OnTick = func(domain.MarketTick) {}
	}
	if callbacks.OnOrderUpdate == nil {
		callbacks.OnOrderUpdate = func() {}
	}
	if callbacks.OnStatus == nil {
		callbacks.OnStatus = func(bool) {}
	}
	if callbacks.OnError == nil {
		callbacks.OnError = func(string) {}
	}
	return callbacks
}

func redactMarketStreamError(err error) string {
	if err == nil {
		return "unknown market stream error"
	}
	message := err.Error()
	for _, key := range []string{"access_token=", "api_key="} {
		searchFrom := 0
		for {
			relativeStart := strings.Index(message[searchFrom:], key)
			if relativeStart < 0 {
				break
			}
			start := searchFrom + relativeStart
			valueStart := start + len(key)
			valueEnd := len(message)
			for index := valueStart; index < len(message); index++ {
				if message[index] == '&' || message[index] == ' ' || message[index] == '"' {
					valueEnd = index
					break
				}
			}
			const redacted = "[REDACTED]"
			message = message[:valueStart] + redacted + message[valueEnd:]
			searchFrom = valueStart + len(redacted)
		}
	}
	return message
}

func (k *Kite) waitRateLimit(ctx context.Context) error {
	k.rateMu.Lock()
	defer k.rateMu.Unlock()
	delay := time.Until(k.nextRequest)
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	// Stay below Kite's documented 10 requests/second aggregate limit.
	k.nextRequest = time.Now().Add(125 * time.Millisecond)
	return nil
}

func autosliceError(response kiteconnect.OrderResponse) error {
	var result error
	for _, child := range response.Children {
		if child.Error != nil {
			result = errors.Join(result, fmt.Errorf("autoslice child failed: %s", child.Error.Message))
		}
	}
	return result
}

func toOrderParams(request domain.OrderRequest) kiteconnect.OrderParams {
	params := kiteconnect.OrderParams{
		Exchange: request.Exchange, Tradingsymbol: request.TradingSymbol,
		Product: request.Product, OrderType: request.OrderType,
		TransactionType: request.TransactionType, Validity: request.Validity,
		Quantity: request.Quantity, Price: request.Price, TriggerPrice: request.TriggerPrice,
		Tag: request.Tag,
	}
	if request.OrderType == "MARKET" || request.OrderType == "SL-M" {
		params.MarketProtection = -1
	}
	return params
}

func kiteError(operation string, err error) error {
	var apiError kiteconnect.Error
	if errors.As(err, &apiError) && apiError.ErrorType == kiteconnect.TokenError {
		return fmt.Errorf("%s: %w", operation, domain.ErrNotAuthenticated)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
