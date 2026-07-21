package broker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"tradeguardian/internal/domain"
)

type Mode string

const (
	ModeProduction Mode = "production"
	ModeSandbox    Mode = "sandbox"
)

type Kite struct {
	mu          sync.RWMutex
	rateMu      sync.Mutex
	nextRequest time.Time
	apiSecret   string
	mode        Mode
	trade       *kiteconnect.Client
	market      *kiteconnect.Client
	apiKey      string
	authed      bool
}

func NewKite(apiKey, apiSecret string, mode Mode) (*Kite, error) {
	if apiKey == "" || apiSecret == "" {
		return nil, fmt.Errorf("KITE_API_KEY and KITE_API_SECRET are required")
	}
	if mode != ModeProduction && mode != ModeSandbox {
		return nil, fmt.Errorf("unsupported broker mode %q", mode)
	}
	trade := kiteconnect.New(apiKey)
	market := kiteconnect.New(apiKey)
	trade.SetTimeout(10 * time.Second)
	market.SetTimeout(15 * time.Second)
	if mode == ModeSandbox {
		trade.SetBaseURI("https://sandbox.kite.trade/oms")
		market.SetBaseURI("https://sandbox.kite.trade")
	}
	return &Kite{apiSecret: apiSecret, mode: mode, trade: trade, market: market, apiKey: apiKey}, nil
}

func (k *Kite) LoginURL(state string) string {
	base := "https://kite.zerodha.com/connect/login"
	if k.mode == ModeSandbox {
		base = "https://sandbox.kite.trade/connect/login"
	}
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
	return domain.Session{UserID: session.UserID, AccessToken: session.AccessToken}, nil
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
		result = append(result, domain.Position{
			Exchange: position.Exchange, TradingSymbol: position.Tradingsymbol,
			InstrumentToken: position.InstrumentToken, Product: position.Product,
			Quantity: position.Quantity, M2M: position.M2M, LastPrice: position.LastPrice,
		})
	}
	return result, ctx.Err()
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
		result = append(result, domain.Order{
			OrderID: order.OrderID, ParentOrderID: order.ParentOrderID, Variety: order.Variety, Status: order.Status,
			Exchange: order.Exchange, TradingSymbol: order.TradingSymbol,
			InstrumentToken: order.InstrumentToken, Product: order.Product,
			OrderType: order.OrderType, TransactionType: order.TransactionType,
			Validity: order.Validity, Quantity: int(order.Quantity),
			PendingQuantity: int(order.PendingQuantity), FilledQuantity: int(order.FilledQuantity), Price: order.Price,
			TriggerPrice: order.TriggerPrice, StatusMessage: order.StatusMessage,
			Tag: order.Tag, Tags: append([]string(nil), order.Tags...),
		})
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
		return nil, fmt.Errorf("get %s instruments: %w", exchange, err)
	}
	result := make([]domain.Instrument, 0, len(instruments))
	for _, instrument := range instruments {
		result = append(result, domain.Instrument{
			Token: uint32(instrument.InstrumentToken), Exchange: instrument.Exchange,
			TradingSymbol: instrument.Tradingsymbol, Name: instrument.Name,
			InstrumentType: strings.ToUpper(instrument.InstrumentType), Expiry: instrument.Expiry.Time,
			Strike: instrument.StrikePrice, TickSize: instrument.TickSize, LotSize: int(instrument.LotSize),
		})
	}
	return result, ctx.Err()
}

func (k *Kite) Place(ctx context.Context, request domain.OrderRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if k.mode == ModeSandbox && request.OrderType != "LIMIT" {
		return "", fmt.Errorf("Kite sandbox only supports LIMIT order placement")
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
	if k.mode == ModeSandbox && request.OrderType != "" && request.OrderType != "LIMIT" {
		return "", fmt.Errorf("Kite sandbox only supports LIMIT order modification")
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
	if k.mode == ModeSandbox {
		return "", fmt.Errorf("Kite sandbox does not support MARKET liquidation orders")
	}
	transaction := "SELL"
	quantity := position.Quantity
	if quantity < 0 {
		transaction = "BUY"
		quantity = -quantity
	}
	request := domain.OrderRequest{
		Variety: "regular", Exchange: position.Exchange, TradingSymbol: position.TradingSymbol,
		Product: position.Product, OrderType: "MARKET", TransactionType: transaction,
		Validity: "DAY", Quantity: quantity, Tag: "tg-force-exit",
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
