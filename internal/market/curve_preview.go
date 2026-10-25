package market

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

// CurvePreview verifies indicative account charges using read-only cost
// requests. Prices remain in the API's raw units (bond percent of nominal,
// futures quote points). It never creates, reserves or submits an order.
func (p *Provider) CurvePreview(ctx context.Context, legs []curve.Leg, accountID string) ([]curve.LegPreview, error) {
	if strings.TrimSpace(accountID) == "" || len(legs) == 0 {
		return nil, fmt.Errorf("explicit account and nonempty curve legs required")
	}
	info, err := p.users.GetInfo(ctx, &pb.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("actual tariff: %w", err)
	}
	profile, err := curveActualProfile(info.GetTariff())
	if err != nil {
		return nil, err
	}
	results := make([]curve.LegPreview, 0, len(legs))
	for _, leg := range legs {
		instrument := leg.Instrument
		if instrument.UID == "" || leg.Lots <= 0 || instrument.Lot <= 0 || !leg.Limit.IsPositive() || !instrument.PriceFactor.IsPositive() || (leg.Side != "buy" && leg.Side != "sell") || (instrument.Kind != "bond" && instrument.Kind != "future") {
			return nil, fmt.Errorf("invalid curve preview leg")
		}
		if instrument.QualifiedOnly && !info.GetQualStatus() {
			return nil, fmt.Errorf("%s requires qualified investor status", instrument.Ticker)
		}
		direction := pb.OrderDirection_ORDER_DIRECTION_BUY
		if leg.Side == "sell" {
			direction = pb.OrderDirection_ORDER_DIRECTION_SELL
		}
		price, e := toQuotation(leg.Limit)
		if e != nil {
			return nil, e
		}
		request := &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: instrument.UID, Price: price, Direction: direction, Quantity: leg.Lots}
		response, e := p.prices.GetOrderPrice(ctx, request)
		if e != nil {
			return nil, fmt.Errorf("%s cost preview: %w", instrument.Ticker, e)
		}
		commission, e := previewCommission(response, leg.Lots)
		if e != nil {
			return nil, fmt.Errorf("%s cost preview: %w", instrument.Ticker, e)
		}
		units := decimal.NewFromInt(leg.Lots).Mul(decimal.NewFromInt(instrument.Lot))
		clean := leg.Limit.Mul(instrument.PriceFactor).Mul(units)
		if !curveAmountMatches(response.GetInitialOrderAmount(), clean) {
			return nil, fmt.Errorf("%s broker quote conversion differs from the current instrument multiplier", instrument.Ticker)
		}
		result := curve.LegPreview{UID: instrument.UID, Status: "matched", Commission: commission, Message: "Read-only published " + profile.Name + " tariff match at worst executable limit; no order or reservation"}
		if instrument.Kind == "bond" {
			if instrument.Bond == nil || instrument.Bond.Accrued.IsNegative() {
				return nil, fmt.Errorf("bond accrued interest metadata unavailable")
			}
			// A one-lot check disambiguates per-order/per-bond ACI responses.
			one := response
			if leg.Lots > 1 {
				oneRequest := &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: instrument.UID, Price: price, Direction: direction, Quantity: 1}
				one, e = p.prices.GetOrderPrice(ctx, oneRequest)
				if e != nil {
					return nil, fmt.Errorf("%s one-lot accrued-interest verification: %w", instrument.Ticker, e)
				}
			}
			if one == nil || one.GetLotsRequested() != 1 || !curveAmountMatches(one.GetInitialOrderAmount(), leg.Limit.Mul(instrument.PriceFactor).Mul(decimal.NewFromInt(instrument.Lot))) {
				return nil, fmt.Errorf("bond one-lot clean amount is unverified")
			}
			aci := one.GetExtraBond().GetAciValue()
			if !curveAmountMatches(aci, instrument.Bond.Accrued) && !curveAmountMatches(aci, instrument.Bond.Accrued.Mul(decimal.NewFromInt(instrument.Lot))) {
				return nil, fmt.Errorf("%s broker accrued interest differs from current bond metadata", instrument.Ticker)
			}
			if !curveCommissionMatches(commission, profile.StockFee(clean)) {
				return nil, fmt.Errorf("%s bond commission does not match published tariff on clean notional excluding accrued interest", instrument.Ticker)
			}
			result.Message += "; bond clean notional and accrued interest verified separately"
		} else {
			if !instrument.ExchangeFeeKnown || instrument.ExchangeFee.IsNegative() || instrument.ExchangeFeeSource == "" {
				return nil, fmt.Errorf("current futures exchange floor unavailable")
			}
			matched := false
			for _, prior := range []string{"0", "5000000.02", "10000000.02", "12000000.02", "17000000.02"} {
				profile.FutureTurnover = decimal.RequireFromString(prior)
				if curveCommissionMatches(commission, profile.FutureFee(clean, leg.Lots, instrument.ExchangeFee)) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("%s future commission does not match any published account tariff band/floor", instrument.Ticker)
			}
			result.InitialMargin, e = previewMargin(response, leg.Lots)
			if e != nil {
				return nil, e
			}
			if leg.Lots > 1 {
				oneRequest := &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: instrument.UID, Price: price, Direction: direction, Quantity: 1}
				one, e := p.prices.GetOrderPrice(ctx, oneRequest)
				if e != nil {
					return nil, fmt.Errorf("one-contract margin verification: %w", e)
				}
				unitMargin, e := previewMargin(one, 1)
				if e != nil {
					return nil, e
				}
				result.InitialMargin = decimal.Max(result.InitialMargin, unitMargin.Mul(decimal.NewFromInt(leg.Lots)))
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func curveActualProfile(tariff string) (scanner.Profile, error) {
	name := strings.ToLower(strings.TrimSpace(tariff))
	switch name {
	case "инвестор":
		name = "investor"
	case "трейдер":
		name = "trader"
	case "премиум":
		name = "premium"
	}
	for _, profile := range scanner.Profiles() {
		if profile.Name == name {
			return profile, nil
		}
	}
	return scanner.Profile{}, fmt.Errorf("unknown actual account tariff %q", tariff)
}
func curveAmountMatches(value *pb.MoneyValue, expected decimal.Decimal) bool {
	return value != nil && strings.EqualFold(value.GetCurrency(), "rub") && !money(value).IsNegative() && money(value).Sub(expected).Abs().LessThanOrEqual(decimal.RequireFromString("0.02"))
}
func curveCommissionMatches(actual, expected decimal.Decimal) bool {
	return actual.Sub(expected).Abs().LessThanOrEqual(decimal.RequireFromString("0.02"))
}
