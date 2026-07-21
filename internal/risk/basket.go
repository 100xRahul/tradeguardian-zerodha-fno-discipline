package risk

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"tradeguardian/internal/domain"
)

type ValidatedBasket struct {
	Request      domain.BasketRequest
	Instruments  map[string]domain.Instrument
	MaxLossPaise int64
}

func ValidateBasket(request domain.BasketRequest, instruments map[string]domain.Instrument) (ValidatedBasket, error) {
	if len(request.Legs) < 2 || len(request.Legs) > 4 {
		return ValidatedBasket{}, fmt.Errorf("a basket must contain 2 to 4 option legs")
	}
	validated := ValidatedBasket{Request: request, Instruments: make(map[string]domain.Instrument)}
	var base domain.Instrument
	var product string
	longByType := map[string]int{}
	shortByType := map[string]int{}
	strikes := map[float64]struct{}{0: {}}
	buyCount, sellCount := 0, 0
	for index := range validated.Request.Legs {
		leg := &validated.Request.Legs[index]
		leg.Exchange = strings.ToUpper(strings.TrimSpace(leg.Exchange))
		leg.TradingSymbol = strings.ToUpper(strings.TrimSpace(leg.TradingSymbol))
		leg.Product = strings.ToUpper(strings.TrimSpace(leg.Product))
		leg.TransactionType = strings.ToUpper(strings.TrimSpace(leg.TransactionType))
		if !domain.IsFNOExchange(leg.Exchange) {
			return ValidatedBasket{}, fmt.Errorf("leg %d must be on NFO or BFO", index+1)
		}
		instrument, ok := instruments[leg.Exchange+":"+leg.TradingSymbol]
		if !ok || !domain.IsOptionType(instrument.InstrumentType) {
			return ValidatedBasket{}, fmt.Errorf("leg %d is not a recognised option contract", index+1)
		}
		if strings.TrimSpace(instrument.Name) == "" || instrument.Expiry.IsZero() || instrument.Strike <= 0 {
			return ValidatedBasket{}, fmt.Errorf("leg %d has incomplete underlying, expiry, or strike metadata", index+1)
		}
		if leg.Product != "MIS" && leg.Product != "NRML" {
			return ValidatedBasket{}, fmt.Errorf("leg %d product must be MIS or NRML", index+1)
		}
		if leg.TransactionType != "BUY" && leg.TransactionType != "SELL" {
			return ValidatedBasket{}, fmt.Errorf("leg %d transaction type must be BUY or SELL", index+1)
		}
		if leg.Quantity <= 0 || instrument.LotSize <= 0 || leg.Quantity%instrument.LotSize != 0 {
			return ValidatedBasket{}, fmt.Errorf("leg %d quantity must be a positive multiple of lot size %d", index+1, instrument.LotSize)
		}
		if leg.LimitPrice <= 0 || math.IsNaN(leg.LimitPrice) || math.IsInf(leg.LimitPrice, 0) {
			return ValidatedBasket{}, fmt.Errorf("leg %d requires a positive IOC limit price", index+1)
		}
		if instrument.TickSize > 0 && !tickAligned(leg.LimitPrice, instrument.TickSize) {
			return ValidatedBasket{}, fmt.Errorf("leg %d limit price must use tick size %.4g", index+1, instrument.TickSize)
		}
		if index == 0 {
			base, product = instrument, leg.Product
		} else if instrument.Exchange != base.Exchange || instrument.Name != base.Name || !instrument.Expiry.Equal(base.Expiry) || leg.Product != product {
			return ValidatedBasket{}, fmt.Errorf("all legs must use the same exchange, underlying, expiry, and product")
		}
		validated.Instruments[leg.Exchange+":"+leg.TradingSymbol] = instrument
		strikes[instrument.Strike] = struct{}{}
		if leg.TransactionType == "BUY" {
			buyCount++
			longByType[instrument.InstrumentType] += leg.Quantity
		} else {
			sellCount++
			shortByType[instrument.InstrumentType] += leg.Quantity
		}
	}
	if buyCount == 0 || sellCount == 0 {
		return ValidatedBasket{}, fmt.Errorf("a hedge basket must contain both BUY and SELL option legs")
	}
	for _, optionType := range []string{"CE", "PE"} {
		longQty, shortQty := longByType[optionType], shortByType[optionType]
		if longQty > 0 && shortQty == 0 {
			return ValidatedBasket{}, fmt.Errorf("every %s BUY must be paired with a %s SELL", optionType, optionType)
		}
		if shortQty > longQty {
			return ValidatedBasket{}, fmt.Errorf("%s short quantity exceeds its protective long quantity", optionType)
		}
	}
	levels := make([]float64, 0, len(strikes))
	for strike := range strikes {
		levels = append(levels, strike)
	}
	sort.Float64s(levels)
	minimum := math.Inf(1)
	for _, spot := range levels {
		payoff := basketPayoff(validated.Request.Legs, validated.Instruments, spot)
		if payoff < minimum {
			minimum = payoff
		}
	}
	if math.IsInf(minimum, 0) || math.IsNaN(minimum) {
		return ValidatedBasket{}, fmt.Errorf("basket maximum loss could not be calculated")
	}
	if minimum < 0 {
		validated.MaxLossPaise = domain.RupeesToPaise(-minimum)
	}
	return validated, nil
}

func tickAligned(value, tick float64) bool {
	units := value / tick
	return math.Abs(units-math.Round(units)) < 1e-7
}

func basketPayoff(legs []domain.BasketLeg, instruments map[string]domain.Instrument, spot float64) float64 {
	var payoff float64
	for _, leg := range legs {
		instrument := instruments[leg.Exchange+":"+leg.TradingSymbol]
		direction := 1.0
		if leg.TransactionType == "SELL" {
			direction = -1
		}
		intrinsic := math.Max(spot-instrument.Strike, 0)
		if instrument.InstrumentType == "PE" {
			intrinsic = math.Max(instrument.Strike-spot, 0)
		}
		payoff += direction * (intrinsic - leg.LimitPrice) * float64(leg.Quantity)
	}
	return payoff
}
