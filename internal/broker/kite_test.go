package broker

import (
	"context"
	"net/url"
	"strings"
	"testing"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"tradeguardian/internal/domain"
)

func TestLoginURLCarriesStateInRedirectParams(t *testing.T) {
	kite, err := NewKite("key", "secret", ModeProduction)
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

func TestAutoslicePartialFailureIsReturned(t *testing.T) {
	response := kiteconnect.OrderResponse{OrderID: "parent", Children: []kiteconnect.OrderChild{
		{OrderID: "child-1"},
		{Error: &kiteconnect.OrderChildError{ErrorType: "MarginException", Message: "insufficient margin"}},
	}}
	if err := autosliceError(response); err == nil || !strings.Contains(err.Error(), "insufficient margin") {
		t.Fatalf("autosliceError() = %v", err)
	}
}

func TestSandboxRejectsUnsupportedMarketPlacementBeforeNetwork(t *testing.T) {
	kite, err := NewKite("sandboxdemo", "sandboxdemo-secret", ModeSandbox)
	if err != nil {
		t.Fatal(err)
	}
	_, err = kite.Place(context.Background(), domain.OrderRequest{OrderType: "MARKET"})
	if err == nil || !strings.Contains(err.Error(), "only supports LIMIT") {
		t.Fatalf("Place() error = %v", err)
	}
}
