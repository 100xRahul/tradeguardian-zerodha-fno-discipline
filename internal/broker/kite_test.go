package broker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"tradeguardian/internal/domain"
)

func TestKiteHTTPClientUsesIPv4Transport(t *testing.T) {
	client := newIPv4HTTPClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatalf("transport = %#v", client.Transport)
	}
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()
	connection, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if connection != nil {
		connection.Close()
	}
	if err == nil {
		t.Fatal("IPv4-only transport unexpectedly connected to an IPv6-only listener")
	}
}

func TestKiteWebSocketDialerUsesIPv4Transport(t *testing.T) {
	if _, err := NewKite("key", "secret"); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()
	connection, err := websocket.DefaultDialer.NetDialContext(context.Background(), "tcp", listener.Addr().String())
	if connection != nil {
		connection.Close()
	}
	if err == nil {
		t.Fatal("Kite WebSocket dialer unexpectedly connected to an IPv6-only listener")
	}
}

func TestLoginURLCarriesStateInRedirectParams(t *testing.T) {
	kite, err := NewKite("key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(kite.LoginURL("state-value"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "kite.zerodha.com" || parsed.Query().Get("api_key") != "key" || parsed.Query().Get("redirect_params") != "state=state-value" {
		t.Fatalf("login URL = %s", parsed.String())
	}
}

func TestOrderParamsApplyProtectionOnlyToMarketTypes(t *testing.T) {
	market := toOrderParams(domain.OrderRequest{OrderType: "MARKET"})
	stopMarket := toOrderParams(domain.OrderRequest{OrderType: "SL-M"})
	limit := toOrderParams(domain.OrderRequest{OrderType: "LIMIT"})
	if market.MarketProtection != -1 || stopMarket.MarketProtection != -1 || limit.MarketProtection != 0 {
		t.Fatalf("protection market=%v sl-m=%v limit=%v", market.MarketProtection, stopMarket.MarketProtection, limit.MarketProtection)
	}
}

func TestExitNeedsAutosliceDetectsFreezeLimitErrors(t *testing.T) {
	if exitNeedsAutoslice(errors.New("quantity should be greater than or equal to 1755 for order slicing")) {
		t.Fatal("autoslice minimum error must not trigger autoslice retry")
	}
	if !exitNeedsAutoslice(errors.New("order exceeds freeze quantity")) {
		t.Fatal("expected freeze-limit error to request autoslice retry")
	}
}

func TestAutoslicePartialFailureIsReturned(t *testing.T) {
	response := kiteconnect.OrderResponse{OrderID: "parent", Children: []kiteconnect.OrderChild{
		{OrderID: "child-1"},
		{Error: &kiteconnect.OrderChildError{ErrorType: "MarginException", Message: "insufficient margin"}},
	}}
	if err := autosliceError(response); err == nil || !strings.Contains(err.Error(), "insufficient margin") {
		t.Fatalf("autosliceError() = %v", err)
	}
}

func TestKiteOrderQuantitiesMustBeExactIntegers(t *testing.T) {
	if _, err := convertKiteOrder(kiteconnect.Order{OrderID: "order-1", Quantity: 50.5}); err == nil {
		t.Fatal("convertKiteOrder() accepted a fractional quantity")
	}
	order, err := convertKiteOrder(kiteconnect.Order{OrderID: "order-1", Quantity: 50, PendingQuantity: 30, FilledQuantity: 20})
	if err != nil || order.Quantity != 50 || order.PendingQuantity != 30 || order.FilledQuantity != 20 {
		t.Fatalf("order=%#v error=%v", order, err)
	}
}

func TestKiteInstrumentMetadataMustBeExact(t *testing.T) {
	base := kiteconnect.Instrument{InstrumentToken: 1, Tradingsymbol: "NIFTYFUT", LotSize: 50, TickSize: 0.05}
	if _, err := convertKiteInstrument(base); err != nil {
		t.Fatalf("valid instrument rejected: %v", err)
	}
	base.LotSize = 50.5
	if _, err := convertKiteInstrument(base); err == nil {
		t.Fatal("convertKiteInstrument() accepted fractional lot size")
	}
	base.LotSize, base.TickSize = 50, 0
	if _, err := convertKiteInstrument(base); err == nil {
		t.Fatal("convertKiteInstrument() accepted missing tick size")
	}
}

func TestKitePositionPreservesPnLMultiplier(t *testing.T) {
	position, err := convertKitePosition(kiteconnect.Position{
		InstrumentToken: 1, Exchange: "NFO", Tradingsymbol: "NIFTY26JULFUT", Product: "MIS",
		Quantity: 50, Multiplier: 1, LastPrice: 25000, M2M: -125.50,
		BuyM2MValue: 1_250_125.50, SellM2MValue: 0,
	})
	if err != nil || position.Multiplier != 1 || position.M2M != -125.50 || position.BuyM2M != 1_250_125.50 || position.SellM2M != 0 {
		t.Fatalf("position=%#v error=%v", position, err)
	}
	if _, err := convertKitePosition(kiteconnect.Position{InstrumentToken: 1, Exchange: "NFO", Tradingsymbol: "NIFTY26JULFUT", Product: "MIS", Multiplier: 0}); err == nil {
		t.Fatal("convertKitePosition() accepted a missing P&L multiplier")
	}
	if _, err := convertKitePosition(kiteconnect.Position{
		InstrumentToken: 1, Exchange: "NFO", Tradingsymbol: "NIFTY26JULFUT", Product: "MIS",
		Quantity: 50, Multiplier: 1, LastPrice: 25_000, M2M: -125.50,
		BuyM2MValue: 1_250_000,
	}); err == nil {
		t.Fatal("convertKitePosition() accepted inconsistent MTM components")
	}
}

func TestKiteTradeRequiresExactExecutionData(t *testing.T) {
	trade, err := convertKiteTrade(kiteconnect.Trade{
		TradeID: "trade-1", OrderID: "order-1", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT",
		InstrumentToken: 1, Product: "NRML", TransactionType: "BUY", Quantity: 50, AveragePrice: 25_000.25,
	})
	if err != nil || trade.Quantity != 50 || trade.AveragePrice != 25_000.25 {
		t.Fatalf("trade=%#v error=%v", trade, err)
	}
	if _, err := convertKiteTrade(kiteconnect.Trade{
		TradeID: "trade-2", OrderID: "order-2", Exchange: "NFO", TradingSymbol: "NIFTY26JULFUT",
		InstrumentToken: 1, Product: "NRML", TransactionType: "SELL", Quantity: 50.5, AveragePrice: 25_000,
	}); err == nil {
		t.Fatal("convertKiteTrade() accepted fractional quantity")
	}
}

func TestNormalizeMarketTokensIsStableAndExact(t *testing.T) {
	tokens, err := normalizeMarketTokens([]uint32{42, 7, 42})
	if err != nil || len(tokens) != 2 || tokens[0] != 7 || tokens[1] != 42 {
		t.Fatalf("tokens=%v error=%v", tokens, err)
	}
	if _, err := normalizeMarketTokens([]uint32{7, 0}); err == nil {
		t.Fatal("normalizeMarketTokens() accepted token zero")
	}
}

func TestMarketStreamErrorsRedactCredentials(t *testing.T) {
	message := redactMarketStreamError(errors.New("dial wss://ws.kite.trade?api_key=public&access_token=private: bad handshake"))
	if strings.Contains(message, "public") || strings.Contains(message, "private") || !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("redacted error = %q", message)
	}
}
