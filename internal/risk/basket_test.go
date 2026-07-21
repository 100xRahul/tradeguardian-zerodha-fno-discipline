package risk

import (
	"testing"
	"time"

	"tradeguardian/internal/domain"
)

func TestValidateBasketBullCallSpread(t *testing.T) {
	expiry := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	instruments := map[string]domain.Instrument{
		"NFO:NIFTY25000CE": {Exchange: "NFO", TradingSymbol: "NIFTY25000CE", Name: "NIFTY", InstrumentType: "CE", Expiry: expiry, Strike: 25_000, LotSize: 50},
		"NFO:NIFTY25100CE": {Exchange: "NFO", TradingSymbol: "NIFTY25100CE", Name: "NIFTY", InstrumentType: "CE", Expiry: expiry, Strike: 25_100, LotSize: 50},
	}
	request := domain.BasketRequest{Legs: []domain.BasketLeg{
		{Exchange: "NFO", TradingSymbol: "NIFTY25000CE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
		{Exchange: "NFO", TradingSymbol: "NIFTY25100CE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
	}}
	validated, err := ValidateBasket(request, instruments)
	if err != nil {
		t.Fatal(err)
	}
	if validated.MaxLossPaise != 250_000 {
		t.Fatalf("MaxLossPaise = %d, want 250000", validated.MaxLossPaise)
	}
}

func TestValidateBasketRejectsUnpairedOrRatioExposure(t *testing.T) {
	expiry := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	instruments := map[string]domain.Instrument{
		"NFO:LOWCE":  {Exchange: "NFO", TradingSymbol: "LOWCE", Name: "NIFTY", InstrumentType: "CE", Expiry: expiry, Strike: 25_000, LotSize: 50},
		"NFO:HIGHCE": {Exchange: "NFO", TradingSymbol: "HIGHCE", Name: "NIFTY", InstrumentType: "CE", Expiry: expiry, Strike: 25_100, LotSize: 50},
	}
	request := domain.BasketRequest{Legs: []domain.BasketLeg{
		{Exchange: "NFO", TradingSymbol: "LOWCE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
		{Exchange: "NFO", TradingSymbol: "HIGHCE", Product: "MIS", TransactionType: "SELL", Quantity: 100, LimitPrice: 50},
	}}
	if _, err := ValidateBasket(request, instruments); err == nil {
		t.Fatal("ValidateBasket() error = nil, want uncovered ratio rejection")
	}
}

func TestValidateBasketRejectsMissingUnderlyingMetadata(t *testing.T) {
	expiry := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	instruments := map[string]domain.Instrument{
		"NFO:LOWCE":  {Exchange: "NFO", TradingSymbol: "LOWCE", InstrumentType: "CE", Expiry: expiry, Strike: 25_000, LotSize: 50},
		"NFO:HIGHCE": {Exchange: "NFO", TradingSymbol: "HIGHCE", InstrumentType: "CE", Expiry: expiry, Strike: 25_100, LotSize: 50},
	}
	request := domain.BasketRequest{Legs: []domain.BasketLeg{
		{Exchange: "NFO", TradingSymbol: "LOWCE", Product: "MIS", TransactionType: "BUY", Quantity: 50, LimitPrice: 100},
		{Exchange: "NFO", TradingSymbol: "HIGHCE", Product: "MIS", TransactionType: "SELL", Quantity: 50, LimitPrice: 50},
	}}
	if _, err := ValidateBasket(request, instruments); err == nil {
		t.Fatal("ValidateBasket() error = nil for missing underlying metadata")
	}
}
