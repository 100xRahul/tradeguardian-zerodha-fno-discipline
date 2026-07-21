package broker

import (
	"context"
	"os"
	"testing"
	"time"

	"tradeguardian/internal/domain"
)

// TestKiteSandboxSmoke is deliberately excluded from ordinary test runs. It
// performs a simulated broker mutation and therefore requires both an explicit
// opt-in and a fresh one-time request token from the Kite sandbox login flow.
func TestKiteSandboxSmoke(t *testing.T) {
	if os.Getenv("KITE_SANDBOX_SMOKE") != "1" {
		t.Skip("set KITE_SANDBOX_SMOKE=1 to run the opt-in Kite sandbox smoke test")
	}
	apiKey := os.Getenv("KITE_API_KEY")
	apiSecret := os.Getenv("KITE_API_SECRET")
	requestToken := os.Getenv("KITE_SANDBOX_REQUEST_TOKEN")
	if apiKey == "" || apiSecret == "" || requestToken == "" {
		t.Fatal("KITE_API_KEY, KITE_API_SECRET, and KITE_SANDBOX_REQUEST_TOKEN are required")
	}
	kite, err := NewKite(apiKey, apiSecret, ModeSandbox)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := kite.GenerateSession(ctx, requestToken); err != nil {
		t.Fatalf("GenerateSession(): %v", err)
	}
	if _, err := kite.Positions(ctx); err != nil {
		t.Fatalf("Positions(): %v", err)
	}
	if _, err := kite.Orders(ctx); err != nil {
		t.Fatalf("Orders(): %v", err)
	}
	instruments, err := kite.Instruments(ctx, "NFO")
	if err != nil {
		t.Fatalf("Instruments(): %v", err)
	}
	var future domain.Instrument
	for _, instrument := range instruments {
		if instrument.InstrumentType == "FUT" && instrument.LotSize > 0 && instrument.TickSize > 0 {
			future = instrument
			break
		}
	}
	if future.TradingSymbol == "" {
		t.Fatal("sandbox NFO instrument dump contained no usable future")
	}
	orderID, err := kite.Place(ctx, domain.OrderRequest{
		Variety: "regular", Exchange: future.Exchange, TradingSymbol: future.TradingSymbol,
		Product: "NRML", OrderType: "LIMIT", TransactionType: "BUY", Validity: "DAY",
		Quantity: future.LotSize, Price: future.TickSize, Tag: "tg-sandbox-smoke",
	})
	if err != nil {
		t.Fatalf("Place(): %v", err)
	}
	orders, err := kite.Orders(ctx)
	if err != nil {
		t.Fatalf("Orders() after placement: %v", err)
	}
	for _, order := range orders {
		if order.OrderID == orderID && order.Cancellable() {
			if err := kite.Cancel(ctx, "regular", orderID); err != nil {
				t.Fatalf("Cancel(): %v", err)
			}
			return
		}
	}
	// A simulated order may already be terminal; seeing it in the order book is
	// sufficient to validate placement and route configuration in that case.
	for _, order := range orders {
		if order.OrderID == orderID {
			return
		}
	}
	t.Fatalf("placed sandbox order %q was absent from the order book", orderID)
}
