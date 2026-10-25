package scanner

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Evaluate prices a fully funded purchase of shares and sale of a matching
// physically delivered future, held through delivery. It does not place orders.
func Evaluate(s Snapshot, a Assumptions, profiles []Profile) (Result, error) {
	if err := validate(s, a, profiles); err != nil {
		return Result{}, err
	}
	stock, future := s.Pair.Stock, s.Pair.Future
	shareCount := future.AssetSize.Mul(decimal.NewFromInt(a.Contracts))
	if !shareCount.Equal(shareCount.Truncate(0)) || shareCount.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return Result{}, fmt.Errorf("underlying share quantity is not a supported integer")
	}
	shares := shareCount.IntPart()
	if shares <= 0 || shares%stock.Lot != 0 {
		return Result{}, fmt.Errorf("%d shares cannot be purchased in stock lots of %d", shares, stock.Lot)
	}
	stockLots := shares / stock.Lot
	stockCost, stockLimit, err := bookValue(s.StockBook.Asks, stockLots, stock.PriceFactor.Mul(decimal.NewFromInt(stock.Lot)), true)
	if err != nil {
		return Result{}, fmt.Errorf("stock ask book: %w", err)
	}
	futureValue, futureLimit, err := bookValue(s.FutureBook.Bids, a.Contracts, future.PriceFactor, false)
	if err != nil {
		return Result{}, fmt.Errorf("future bid book: %w", err)
	}
	days := decimal.NewFromInt(future.Expiry.Sub(s.ObservedAt).Nanoseconds()).Div(decimal.NewFromInt(int64(24 * time.Hour)))
	margin := future.InitialMargin.Mul(decimal.NewFromInt(a.Contracts))
	entryFees := decimal.Zero
	for _, p := range profiles {
		entryFees = decimal.Max(entryFees, p.StockFee(stockCost).Add(p.FutureFee(futureValue, a.Contracts, future.ExchangeFee)))
	}
	capital := stockCost.Add(margin.Mul(decimal.NewFromInt(1).Add(a.MarginBufferRate))).Add(entryFees)
	executionBuffer := stockCost.Add(futureValue).Mul(a.ExecutionBufferBPS).Div(decimal.NewFromInt(10000))
	benchmark := capital.Mul(a.CashRate).Mul(days).Div(decimal.NewFromInt(365))
	dividends := a.DividendPerShare.Mul(shareCount)
	// A scenario assumption for commission estimation, not a bound on the actual
	// stock price at expiry. Use the larger current leg value before the buffer.
	deliveryNotional := decimal.Max(stockCost, futureValue).Mul(decimal.NewFromInt(1).Add(a.SettlementPriceBufferRate))
	r := Result{
		Key:      stock.UID + ":" + future.UID + fmt.Sprintf(":%d", a.Contracts),
		Snapshot: s, Assumptions: a, Contracts: a.Contracts, Shares: shares, StockLots: stockLots,
		StockCost: stockCost, FutureValue: futureValue, StockLimit: stockLimit, FutureLimit: futureLimit,
		GrossBasis: futureValue.Sub(stockCost), Dividends: dividends, InitialMargin: margin,
		CapitalRequired: capital, CashBenchmark: benchmark, ExecutionBuffer: executionBuffer, Days: days,
		Warnings: []string{
			"Delivery stock commission is an estimate using the configured settlement-price buffer; actual settlement prices can exceed this assumption.",
			"Commission estimates treat each leg as one transaction; split executions and daily turnover changes can affect the broker's final charges.",
			"Capital includes the largest entry commission across profiles and the configured margin buffer; future margin calls can require additional cash.",
		},
	}
	if !a.DividendPerShare.IsZero() {
		r.Warnings = append(r.Warnings, "Dividend income is a user-supplied assumption, not a confirmed entitlement or net-of-tax amount.")
	}
	if capital.GreaterThan(a.CapitalLimit) {
		r.Warnings = append(r.Warnings, "Required funded capital exceeds the configured capital limit.")
	}
	for _, p := range profiles {
		fees := []FeeLine{
			{Name: "stock_entry", Amount: p.StockFee(stockCost)},
			{Name: "future_entry_including_exchange_floor", Amount: p.FutureFee(futureValue, a.Contracts, future.ExchangeFee)},
			{Name: "stock_delivery_estimate", Amount: p.StockFee(deliveryNotional)},
		}
		totalFees := decimal.Zero
		for _, line := range fees {
			totalFees = totalFees.Add(line.Amount)
		}
		net := r.GrossBasis.Add(dividends).Sub(totalFees).Sub(executionBuffer)
		excess := net.Sub(benchmark)
		r.Scenarios = append(r.Scenarios, Scenario{
			StockRate: p.StockRate, FutureRate: p.EffectiveFutureRate(), FeeSource: p.Source,
			Profile: p.Name, Fees: fees, TradingFees: totalFees,
			NetProfit: net, ExcessProfit: excess,
			AnnualizedReturn: net.Div(capital).Mul(decimal.NewFromInt(365)).Div(days),
			Qualifies:        !capital.GreaterThan(a.CapitalLimit) && !net.LessThan(a.MinProfit) && !excess.LessThan(a.MinExcessProfit),
		})
	}
	return r, nil
}

func validate(s Snapshot, a Assumptions, profiles []Profile) error {
	stock, future := s.Pair.Stock, s.Pair.Future
	if s.ObservedAt.IsZero() || a.Contracts <= 0 || !a.CapitalLimit.IsPositive() || a.MaxDays <= 0 || a.MaxQuoteAge <= 0 || a.MaxQuoteSkew < 0 {
		return fmt.Errorf("invalid observation time, quantity, capital limit, or time limits")
	}
	for name, value := range map[string]decimal.Decimal{"cash rate": a.CashRate, "margin buffer": a.MarginBufferRate, "execution buffer": a.ExecutionBufferBPS, "settlement buffer": a.SettlementPriceBufferRate, "dividend": a.DividendPerShare, "minimum profit": a.MinProfit, "minimum excess profit": a.MinExcessProfit} {
		if value.IsNegative() {
			return fmt.Errorf("%s cannot be negative", name)
		}
	}
	if stock.Kind != "share" || (future.Kind != "future" && future.Kind != "futures") {
		return fmt.Errorf("only a share and its dated future are supported")
	}
	if stock.UID == "" || future.UID == "" || stock.UID == future.UID || stock.PositionUID == "" || stock.PositionUID != future.UnderlyingPositionUID {
		return fmt.Errorf("stock and future underlying position identities are not verified")
	}
	if !strings.EqualFold(stock.Currency, "rub") || !strings.EqualFold(future.Currency, "rub") {
		return fmt.Errorf("only RUB shares and RUB futures are supported")
	}
	if future.Settlement != "physical" || future.Expiry.IsZero() || future.LastTrade.IsZero() || !future.LastTrade.After(s.ObservedAt) || future.LastTrade.After(future.Expiry) {
		return fmt.Errorf("future must have verified physical delivery and future trading/expiry dates")
	}
	duration := future.Expiry.Sub(s.ObservedAt)
	if duration <= 0 || decimal.NewFromInt(duration.Nanoseconds()).GreaterThan(decimal.NewFromInt(int64(24*time.Hour)).Mul(decimal.NewFromInt(int64(a.MaxDays)))) {
		return fmt.Errorf("future expiry is outside the configured horizon")
	}
	if stock.Lot <= 0 || future.Lot != 1 || !future.AssetSize.IsPositive() || !future.AssetSize.Equal(future.AssetSize.Truncate(0)) || !stock.PriceFactor.IsPositive() || !future.PriceFactor.IsPositive() || !future.InitialMargin.IsPositive() {
		return fmt.Errorf("invalid lot size, asset size, price conversion, or short initial margin")
	}
	if !future.ExchangeFeeKnown || future.ExchangeFee.IsNegative() || future.ExchangeFeeSource == "" {
		return fmt.Errorf("future exchange fee floor is not verified")
	}
	if !stock.APIAvailable || !stock.BuyAvailable || !future.APIAvailable || !future.SellAvailable {
		return fmt.Errorf("required API buy/sell direction is unavailable")
	}
	for _, b := range []struct {
		book Book
		uid  string
	}{{s.StockBook, stock.UID}, {s.FutureBook, future.UID}} {
		if b.book.UID != b.uid || !b.book.Consistent || !b.book.TradeAvailable || b.book.Time.IsZero() {
			return fmt.Errorf("book identity, consistency, trading status, or timestamp is invalid")
		}
		age := s.ObservedAt.Sub(b.book.Time)
		if age < 0 || age > a.MaxQuoteAge {
			return fmt.Errorf("book timestamp is stale or in the future")
		}
	}
	skew := s.StockBook.Time.Sub(s.FutureBook.Time)
	if skew < 0 {
		skew = -skew
	}
	if skew > a.MaxQuoteSkew {
		return fmt.Errorf("stock/future quote timestamp skew exceeds the configured limit")
	}
	if len(profiles) == 0 {
		return fmt.Errorf("at least one commission profile is required")
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		if p.Name == "" || seen[p.Name] || !p.StockRate.IsPositive() || !p.FutureRate.IsPositive() || !p.EffectiveFutureRate().IsPositive() || p.FutureTurnover.IsNegative() {
			return fmt.Errorf("invalid or duplicate commission profile %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// bookValue consumes the requested lots in the executable side of the book.
// factor converts one quoted price for one order lot into RUB.
func bookValue(levels []Level, lots int64, factor decimal.Decimal, asks bool) (decimal.Decimal, decimal.Decimal, error) {
	remaining, value, limit := lots, decimal.Zero, decimal.Zero
	for i, level := range levels {
		if !level.Price.IsPositive() || level.Lots <= 0 {
			return decimal.Zero, decimal.Zero, fmt.Errorf("invalid price or volume at level %d", i)
		}
		if i > 0 && ((asks && level.Price.LessThan(levels[i-1].Price)) || (!asks && level.Price.GreaterThan(levels[i-1].Price))) {
			return decimal.Zero, decimal.Zero, fmt.Errorf("book levels are not ordered")
		}
		take := level.Lots
		if take > remaining {
			take = remaining
		}
		value = value.Add(level.Price.Mul(factor).Mul(decimal.NewFromInt(take)))
		limit = level.Price
		remaining -= take
		if remaining == 0 {
			return value, limit, nil
		}
	}
	return decimal.Zero, decimal.Zero, fmt.Errorf("insufficient depth: missing %d lots", remaining)
}
