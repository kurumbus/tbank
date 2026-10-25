package scanner

import (
	"strings"

	"github.com/shopspring/decimal"
)

// Profile describes a hypothetical trading tariff. Subscription charges are
// excluded from this model. FutureTurnover is completed futures turnover
// BEFORE the proposed order, not the turnover this candidate would generate.
type Profile struct {
	Name           string          `json:"name"`
	Family         string          `json:"family"`
	StockRate      decimal.Decimal `json:"stock_rate"`
	FutureRate     decimal.Decimal `json:"future_rate"`
	FutureTurnover decimal.Decimal `json:"future_turnover_before_order_rub"`
	Source         string          `json:"source"`
}

// Profiles returns standard Russian exchange tariffs without promotions,
// negotiated discounts or exceptional futures. Premium membership levels share
// the same standard trading commission schedule.
func Profiles() []Profile {
	d := decimal.RequireFromString
	return []Profile{
		{Name: "investor", Family: "investor", StockRate: d("0.003"), FutureRate: d("0.001"), Source: "https://cdn.tbank.ru/static/documents/invest-tariff-fees.pdf (Т-ИНВЕСТ-260424)"},
		{Name: "trader", Family: "trader", StockRate: d("0.0005"), FutureRate: d("0.0004"), Source: "https://cdn.tbank.ru/static/documents/invest-tariff-trader.pdf (Т-ТРЕЙД-260522)"},
		{Name: "premium", Family: "premium", StockRate: d("0.0004"), FutureRate: d("0.00025"), Source: "https://cdn.tbank.ru/static/documents/invest-tariff-premium.pdf (Т-ПРЕМ-260603)"},
	}
}

// EffectiveFutureRate conservatively uses completed pre-order turnover. The
// PDFs leave a one-kopeck wording gap at the final upper boundary; retain the
// preceding, higher commission at exactly that boundary.
func (p Profile) EffectiveFutureRate() decimal.Decimal {
	d := decimal.RequireFromString
	switch strings.ToLower(p.Family) {
	case "trader":
		if p.FutureTurnover.GreaterThan(d("10000000.01")) {
			return d("0.00025")
		}
		if p.FutureTurnover.GreaterThan(d("5000000")) {
			return d("0.0003")
		}
	case "premium":
		if p.FutureTurnover.GreaterThan(d("17000000.01")) {
			return d("0.00015")
		}
		if p.FutureTurnover.GreaterThan(d("12000000")) {
			return d("0.0002")
		}
	}
	return p.FutureRate
}

func roundedCommission(amount decimal.Decimal) decimal.Decimal {
	return decimal.Max(amount.Round(2), decimal.RequireFromString("0.01"))
}

// StockFee is the standard commission for one estimated securities transaction.
// Evaluate rejects zero/negative configured commission rates.
func (p Profile) StockFee(notional decimal.Decimal) decimal.Decimal {
	return roundedCommission(notional.Mul(p.StockRate))
}

// FutureFee applies the exchange-organiser floor, rather than adding exchange
// fees a second time. The adapter must supply a verified per-contract floor.
func (p Profile) FutureFee(notional decimal.Decimal, contracts int64, exchangeFeePerContract decimal.Decimal) decimal.Decimal {
	percentage := notional.Mul(p.EffectiveFutureRate())
	floor := exchangeFeePerContract.Mul(decimal.NewFromInt(contracts))
	return roundedCommission(decimal.Max(percentage, floor))
}
