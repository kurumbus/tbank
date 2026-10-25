package market

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

// RelativePreview calibrates costs for the hypothetical legs without placing
// orders. Own-fund free-order allowances never reduce the modelled commission:
// a verified zero preview is reported as an account privilege, not extrapolated.
func (p *Provider) RelativePreview(ctx context.Context, legs []curve.Leg, accountID string) ([]curve.LegPreview, error) {
	if strings.TrimSpace(accountID) == "" || len(legs) == 0 {
		return nil, fmt.Errorf("explicit account and relative legs are required")
	}
	info, e := p.users.GetInfo(ctx, &pb.GetInfoRequest{})
	if e != nil {
		return nil, e
	}
	profile, e := curveActualProfile(info.GetTariff())
	if e != nil {
		return nil, e
	}
	results := make([]curve.LegPreview, 0, len(legs))
	for _, leg := range legs {
		i := leg.Instrument
		if i.UID == "" || leg.Lots <= 0 || i.Lot <= 0 || !leg.Limit.IsPositive() || !i.PriceFactor.IsPositive() || (leg.Side != "buy" && leg.Side != "sell") || (i.Kind != "future" && i.Kind != "share" && i.Kind != "etf") {
			return nil, fmt.Errorf("invalid relative cost leg")
		}
		if i.QualifiedOnly && !info.GetQualStatus() {
			return nil, fmt.Errorf("%s requires qualified investor status", i.Ticker)
		}
		direction := pb.OrderDirection_ORDER_DIRECTION_BUY
		if leg.Side == "sell" {
			direction = pb.OrderDirection_ORDER_DIRECTION_SELL
		}
		price, e := toQuotation(leg.Limit)
		if e != nil {
			return nil, e
		}
		request := &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: i.UID, Price: price, Direction: direction, Quantity: leg.Lots}
		r, e := p.prices.GetOrderPrice(ctx, request)
		if e != nil {
			return nil, fmt.Errorf("%s cost preview: %w", i.Ticker, e)
		}
		commission, e := previewCommission(r, leg.Lots)
		if e != nil {
			return nil, e
		}
		quantity := decimal.NewFromInt(leg.Lots).Mul(decimal.NewFromInt(i.Lot))
		rubValue := leg.Limit.Mul(i.PriceFactor).Mul(quantity)
		amountMatches := curveAmountMatches(r.GetInitialOrderAmount(), rubValue)
		if i.Kind == "future" && i.PriceCurrency == "usd" && i.UnitSize.IsPositive() {
			value := r.GetInitialOrderAmount()
			usdValue := leg.Limit.Mul(i.UnitSize).Mul(decimal.NewFromInt(leg.Lots))
			amountMatches = amountMatches || (value != nil && strings.EqualFold(value.GetCurrency(), "usd") && money(value).Sub(usdValue).Abs().LessThanOrEqual(decimal.RequireFromString("0.02")))
		}
		if !amountMatches {
			return nil, fmt.Errorf("%s broker notional does not match current contract/quote currency conversion", i.Ticker)
		}
		preview := curve.LegPreview{UID: i.UID, Status: "matched", Commission: commission, Message: "Read-only " + profile.Name + " published tariff match; no order or reservation"}
		if i.Kind != "future" {
			if commission.IsZero() && i.Kind == "etf" && i.Ticker == "TGLD@" && i.ClassCode == "SPBRU" {
				preview.Message += "; exact own-fund zero fee preview accepted under published first-15-orders/day privilege; scenarios conservatively retain standard commissions because the daily allowance counter is unknown"
			} else if !curveCommissionMatches(commission, profile.StockFee(rubValue)) {
				return nil, fmt.Errorf("%s securities commission does not match published tariff", i.Ticker)
			}
		} else {
			if !i.ExchangeFeeKnown || i.ExchangeFee.IsNegative() || i.ExchangeFeeSource == "" {
				return nil, fmt.Errorf("%s current exchange fee floor unavailable", i.Ticker)
			}
			matched := false
			for _, prior := range []string{"0", "5000000.02", "10000000.02", "12000000.02", "17000000.02"} {
				profile.FutureTurnover = decimal.RequireFromString(prior)
				if curveCommissionMatches(commission, profile.FutureFee(rubValue, leg.Lots, i.ExchangeFee)) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("%s future commission does not match a published tariff band/floor", i.Ticker)
			}
			preview.InitialMargin, e = previewMargin(r, leg.Lots)
			if e != nil {
				return nil, e
			}
			if leg.Lots > 1 {
				one, e := p.prices.GetOrderPrice(ctx, &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: i.UID, Price: price, Direction: direction, Quantity: 1})
				if e != nil {
					return nil, e
				}
				unit, e := previewMargin(one, 1)
				if e != nil {
					return nil, e
				}
				preview.InitialMargin = decimal.Max(preview.InitialMargin, unit.Mul(decimal.NewFromInt(leg.Lots)))
			}
		}
		results = append(results, preview)
	}
	return results, nil
}
