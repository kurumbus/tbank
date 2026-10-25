package scanner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	PositionOpen               = "open"
	PositionAwaitingSettlement = "awaiting_settlement"
	PositionClosed             = "closed"
)

// ErrAwaitingSettlement means executable trading is over; callers should persist
// the state transition without inventing an exit fill or a settlement price.
var ErrAwaitingSettlement = errors.New("last trading time reached; verified settlement inputs required")

// Position is a persistent paper position. Entry, MinimumTariff and CaptureTarget
// are fixed at opening. ID and lifecycle persistence belong to the state layer.
type Position struct {
	ID            int64           `json:"id"`
	Source        string          `json:"source"`
	Status        string          `json:"status"`
	Entry         Result          `json:"entry"`
	ExitProfiles  []Profile       `json:"exit_profiles"`
	MinimumTariff string          `json:"minimum_tariff"`
	OpenedAt      time.Time       `json:"opened_at"`
	CaptureTarget decimal.Decimal `json:"capture_target"`
	Latest        *Valuation      `json:"latest,omitempty"`
	ClosedAt      *time.Time      `json:"closed_at,omitempty"`
	CloseReason   string          `json:"close_reason,omitempty"`
}

// SettlementInput contains operator-verified official settlement observations.
// Prices use the same raw quote units as the frozen instruments, not necessarily
// RUB per contract. Dividends are cumulative cash receipts, not projections.
type SettlementInput struct {
	StockPrice  decimal.Decimal `json:"stock_price"`
	FuturePrice decimal.Decimal `json:"future_price"`
	ObservedAt  time.Time       `json:"observed_at"`
	Reference   string          `json:"reference"`
	Dividends   decimal.Decimal `json:"dividends_rub"`
	Verified    bool            `json:"verified"`
}

// Valuation records the prices and frozen tariff calculations behind one mark.
// GrossProfit is price-only P&L; verified dividends are added separately to net.
// ExecutionReserve is the opening buffer, charged once in every lifetime P&L
// view rather than deducted again on each hourly observation.
type Valuation struct {
	ObservedAt       time.Time        `json:"observed_at"`
	Kind             string           `json:"kind"`
	Snapshot         *Snapshot        `json:"snapshot,omitempty"`
	Settlement       *SettlementInput `json:"settlement,omitempty"`
	StockValue       decimal.Decimal  `json:"stock_value_rub"`
	FutureValue      decimal.Decimal  `json:"future_value_rub"`
	StockLimit       decimal.Decimal  `json:"stock_limit_price"`
	FutureLimit      decimal.Decimal  `json:"future_limit_price"`
	GrossProfit      decimal.Decimal  `json:"gross_profit_rub"`
	ResidualBasis    decimal.Decimal  `json:"residual_basis_rub"`
	CapturedFraction decimal.Decimal  `json:"captured_fraction"`
	Dividends        decimal.Decimal  `json:"dividends_rub"`
	ExecutionReserve decimal.Decimal  `json:"execution_reserve_rub"`
	CashBenchmark    decimal.Decimal  `json:"cash_benchmark_rub"`
	Days             decimal.Decimal  `json:"days"`
	Scenarios        []Scenario       `json:"scenarios"`
	ShouldClose      bool             `json:"should_close"`
	CloseReason      string           `json:"close_reason,omitempty"`
}

// MinimumEligibleTariff chooses in product order, independent of slice order.
// The selected opening classification is not recomputed as the position moves.
func MinimumEligibleTariff(r Result) string {
	if !r.GrossBasis.IsPositive() || !r.CapitalRequired.IsPositive() || r.CapitalRequired.GreaterThan(r.Assumptions.CapitalLimit) {
		return ""
	}
	for _, name := range []string{"investor", "trader", "premium"} {
		for _, s := range r.Scenarios {
			if s.Profile == name && s.Qualifies && !s.NetProfit.LessThan(r.Assumptions.MinProfit) && !s.ExcessProfit.LessThan(r.Assumptions.MinExcessProfit) {
				return name
			}
		}
	}
	return ""
}

func NewPosition(entry Result, source string, captureTarget decimal.Decimal) (Position, error) {
	if strings.TrimSpace(source) == "" {
		return Position{}, fmt.Errorf("position source is required")
	}
	if !captureTarget.IsPositive() || captureTarget.GreaterThan(decimal.NewFromInt(1)) {
		return Position{}, fmt.Errorf("capture target must be greater than zero and at most one")
	}
	minimum := MinimumEligibleTariff(entry)
	if minimum == "" {
		return Position{}, fmt.Errorf("no tariff meets the opening profit and capital hurdles")
	}
	frozen, err := frozenProfiles(entry)
	if err != nil {
		return Position{}, err
	}
	if err = validate(entry.Snapshot, entry.Assumptions, frozen); err != nil {
		return Position{}, fmt.Errorf("invalid entry: %w", err)
	}
	if entry.Contracts <= 0 || entry.Contracts != entry.Assumptions.Contracts || entry.Shares <= 0 || entry.StockLots <= 0 || !entry.StockCost.IsPositive() || !entry.FutureValue.IsPositive() || !entry.GrossBasis.Equal(entry.FutureValue.Sub(entry.StockCost)) || entry.ExecutionBuffer.IsNegative() {
		return Position{}, fmt.Errorf("entry sizing, amounts, or execution reserve is inconsistent")
	}
	if !entry.Snapshot.Pair.Future.AssetSize.Mul(decimal.NewFromInt(entry.Contracts)).Equal(decimal.NewFromInt(entry.Shares)) || entry.Shares%entry.Snapshot.Pair.Stock.Lot != 0 || entry.StockLots != entry.Shares/entry.Snapshot.Pair.Stock.Lot {
		return Position{}, fmt.Errorf("entry share and contract quantities do not match")
	}
	// Deep copy ensures later mutations of the scan result cannot change the
	// opening execution, fee schedule or hurdle snapshot retained by the position.
	copyEntry, err := cloneJSON(entry)
	if err != nil {
		return Position{}, err
	}
	return Position{Source: source, Status: PositionOpen, Entry: copyEntry, ExitProfiles: Profiles(), MinimumTariff: minimum, OpenedAt: entry.Snapshot.ObservedAt, CaptureTarget: captureTarget}, nil
}

func MarkPosition(p Position, s Snapshot) (Valuation, error) {
	if p.Status == PositionAwaitingSettlement {
		return Valuation{}, ErrAwaitingSettlement
	}
	if p.Status != PositionOpen {
		return Valuation{}, fmt.Errorf("only an open position can be marked")
	}
	if err := validatePosition(p, s.ObservedAt); err != nil {
		return Valuation{}, err
	}
	if err := sameEconomicPair(p.Entry.Snapshot.Pair, s.Pair); err != nil {
		return Valuation{}, err
	}
	if !s.ObservedAt.Before(p.Entry.Snapshot.Pair.Future.LastTrade) {
		return Valuation{}, ErrAwaitingSettlement
	}
	profiles, err := frozenExitProfiles(p)
	if err != nil {
		return Valuation{}, err
	}
	// Reuse common quote and instrument validation but validate the closing
	// directions: sell owned shares and buy back the sold futures.
	check := s
	check.Pair.Stock.BuyAvailable = s.Pair.Stock.SellAvailable
	check.Pair.Future.SellAvailable = s.Pair.Future.BuyAvailable
	if err = validate(check, p.Entry.Assumptions, profiles); err != nil {
		return Valuation{}, fmt.Errorf("invalid close snapshot: %w", err)
	}
	previous := p.Entry.Snapshot
	if p.Latest != nil && p.Latest.Snapshot != nil {
		previous = *p.Latest.Snapshot
	}
	if s.StockBook.Time.Before(previous.StockBook.Time) || s.FutureBook.Time.Before(previous.FutureBook.Time) {
		return Valuation{}, fmt.Errorf("order book timestamps moved backwards")
	}
	stockValue, stockLimit, err := bookValue(s.StockBook.Bids, p.Entry.StockLots, s.Pair.Stock.PriceFactor.Mul(decimal.NewFromInt(s.Pair.Stock.Lot)), false)
	if err != nil {
		return Valuation{}, fmt.Errorf("stock exit bid book: %w", err)
	}
	futureValue, futureLimit, err := bookValue(s.FutureBook.Asks, p.Entry.Contracts, s.Pair.Future.PriceFactor, true)
	if err != nil {
		return Valuation{}, fmt.Errorf("future close ask book: %w", err)
	}
	v := baseValuation(p, s.ObservedAt, stockValue, futureValue)
	v.Kind, v.StockLimit, v.FutureLimit = "mark", stockLimit, futureLimit
	saved, err := cloneJSON(s)
	if err != nil {
		return Valuation{}, err
	}
	v.Snapshot = &saved
	for _, profile := range profiles {
		fees, err := frozenEntryFees(p.Entry, profile.Name)
		if err != nil {
			return Valuation{}, err
		}
		fees = append(fees, FeeLine{Name: "stock_exit", Amount: profile.StockFee(stockValue)}, FeeLine{Name: "future_exit_including_exchange_floor", Amount: profile.FutureFee(futureValue, p.Entry.Contracts, s.Pair.Future.ExchangeFee)})
		v.Scenarios = append(v.Scenarios, valueScenario(p, v, profile, fees))
	}
	for _, scenario := range v.Scenarios {
		if scenario.Profile == p.MinimumTariff && scenario.Qualifies && !v.CapturedFraction.LessThan(p.CaptureTarget) {
			v.ShouldClose, v.CloseReason = true, "capture_target_met"
		}
	}
	return v, nil
}

// SettlePosition requires explicit verified observations. It never substitutes
// the entry future quote or a missing quote for the actual settlement inputs.
func SettlePosition(p Position, input SettlementInput) (Valuation, error) {
	if p.Status != PositionOpen && p.Status != PositionAwaitingSettlement {
		return Valuation{}, fmt.Errorf("only an open or awaiting position can settle")
	}
	if err := validatePosition(p, input.ObservedAt); err != nil {
		return Valuation{}, err
	}
	if input.ObservedAt.Before(p.Entry.Snapshot.Pair.Future.Expiry) {
		return Valuation{}, fmt.Errorf("settlement observation precedes contract expiry")
	}
	if !input.Verified || strings.TrimSpace(input.Reference) == "" || !input.StockPrice.IsPositive() || !input.FuturePrice.IsPositive() || input.Dividends.IsNegative() {
		return Valuation{}, fmt.Errorf("positive verified settlement prices, reference, and nonnegative received dividends are required")
	}
	profiles, err := frozenExitProfiles(p)
	if err != nil {
		return Valuation{}, err
	}
	stockValue := input.StockPrice.Mul(p.Entry.Snapshot.Pair.Stock.PriceFactor).Mul(decimal.NewFromInt(p.Entry.Shares))
	futureValue := input.FuturePrice.Mul(p.Entry.Snapshot.Pair.Future.PriceFactor).Mul(decimal.NewFromInt(p.Entry.Contracts))
	v := baseValuation(p, input.ObservedAt, stockValue, futureValue)
	v.Kind, v.Settlement, v.StockLimit, v.FutureLimit, v.Dividends = "settlement", &input, input.StockPrice, input.FuturePrice, input.Dividends
	v.ShouldClose, v.CloseReason = true, "verified_settlement"
	for _, profile := range profiles {
		fees, err := frozenEntryFees(p.Entry, profile.Name)
		if err != nil {
			return Valuation{}, err
		}
		// Physical delivery has a securities sale commission. There is no
		// fictitious futures buyback or additional futures expiry commission.
		fees = append(fees, FeeLine{Name: "stock_delivery", Amount: profile.StockFee(stockValue)})
		v.Scenarios = append(v.Scenarios, valueScenario(p, v, profile, fees))
	}
	return v, nil
}

func baseValuation(p Position, at time.Time, stockValue, futureValue decimal.Decimal) Valuation {
	days := decimal.NewFromInt(at.Sub(p.OpenedAt).Nanoseconds()).Div(decimal.NewFromInt(int64(24 * time.Hour)))
	gross := stockValue.Sub(p.Entry.StockCost).Add(p.Entry.FutureValue.Sub(futureValue))
	return Valuation{ObservedAt: at, StockValue: stockValue, FutureValue: futureValue, GrossProfit: gross, ResidualBasis: futureValue.Sub(stockValue), CapturedFraction: gross.Div(p.Entry.GrossBasis), ExecutionReserve: p.Entry.ExecutionBuffer, Days: days, CashBenchmark: p.Entry.CapitalRequired.Mul(p.Entry.Assumptions.CashRate).Mul(days).Div(decimal.NewFromInt(365))}
}

func valueScenario(p Position, v Valuation, profile Profile, fees []FeeLine) Scenario {
	total := decimal.Zero
	for _, fee := range fees {
		total = total.Add(fee.Amount)
	}
	net := v.GrossProfit.Add(v.Dividends).Sub(total).Sub(v.ExecutionReserve)
	excess := net.Sub(v.CashBenchmark)
	annualized := decimal.Zero
	if v.Days.IsPositive() {
		annualized = net.Div(p.Entry.CapitalRequired).Mul(decimal.NewFromInt(365)).Div(v.Days)
	}
	return Scenario{Profile: profile.Name, StockRate: profile.StockRate, FutureRate: profile.FutureRate, FeeSource: profile.Source, Fees: fees, TradingFees: total, NetProfit: net, ExcessProfit: excess, AnnualizedReturn: annualized, Qualifies: !net.LessThan(p.Entry.Assumptions.MinProfit) && !excess.LessThan(p.Entry.Assumptions.MinExcessProfit)}
}

func frozenProfiles(entry Result) ([]Profile, error) {
	if len(entry.Scenarios) != 3 {
		return nil, fmt.Errorf("position requires exactly investor, trader and premium entry scenarios")
	}
	profiles := make([]Profile, 0, 3)
	for _, name := range []string{"investor", "trader", "premium"} {
		found := false
		for _, s := range entry.Scenarios {
			if s.Profile != name {
				continue
			}
			if found || !s.StockRate.IsPositive() || !s.FutureRate.IsPositive() || s.FeeSource == "" {
				return nil, fmt.Errorf("invalid frozen rates for %s", name)
			}
			found = true
			if _, err := frozenEntryFees(entry, name); err != nil {
				return nil, err
			}
			// No tariff-family lookup here: these are the actual rates retained
			// from opening, including its explicitly assumed turnover tier.
			profiles = append(profiles, Profile{Name: name, Family: "frozen", StockRate: s.StockRate, FutureRate: s.FutureRate, Source: s.FeeSource})
		}
		if !found {
			return nil, fmt.Errorf("missing frozen %s scenario", name)
		}
	}
	return profiles, nil
}

// Exit schedules are frozen independently at opening with zero assumed prior
// exit-day turnover. An entry-day discount never becomes a permanent fee rate.
func frozenExitProfiles(p Position) ([]Profile, error) {
	if len(p.ExitProfiles) != 3 {
		return nil, fmt.Errorf("position requires three frozen exit tariff schedules")
	}
	profiles := make([]Profile, 0, 3)
	for _, name := range []string{"investor", "trader", "premium"} {
		found := false
		for _, profile := range p.ExitProfiles {
			if profile.Name != name {
				continue
			}
			if found || !profile.StockRate.IsPositive() || !profile.FutureRate.IsPositive() || !profile.FutureTurnover.IsZero() || profile.Source == "" {
				return nil, fmt.Errorf("invalid frozen exit schedule for %s", name)
			}
			found = true
			profile.Family = "frozen"
			profiles = append(profiles, profile)
		}
		if !found {
			return nil, fmt.Errorf("missing frozen exit schedule for %s", name)
		}
	}
	return profiles, nil
}

func frozenEntryFees(entry Result, name string) ([]FeeLine, error) {
	fees := make([]FeeLine, 0, 2)
	for _, scenario := range entry.Scenarios {
		if scenario.Profile != name {
			continue
		}
		for _, wanted := range []string{"stock_entry", "future_entry_including_exchange_floor"} {
			found := false
			for _, line := range scenario.Fees {
				if line.Name != wanted {
					continue
				}
				if found || !line.Amount.IsPositive() {
					return nil, fmt.Errorf("invalid frozen %s fee for %s", wanted, name)
				}
				found = true
				fees = append(fees, line)
			}
			if !found {
				return nil, fmt.Errorf("missing frozen %s fee for %s", wanted, name)
			}
		}
	}
	if len(fees) != 2 {
		return nil, fmt.Errorf("entry fee scenario %s is missing or duplicated", name)
	}
	return fees, nil
}

func validatePosition(p Position, observedAt time.Time) error {
	if p.OpenedAt.IsZero() || !p.OpenedAt.Equal(p.Entry.Snapshot.ObservedAt) || observedAt.IsZero() || observedAt.Before(p.OpenedAt) || (p.Latest != nil && observedAt.Before(p.Latest.ObservedAt)) {
		return fmt.Errorf("position observation is missing or out of order")
	}
	if !p.CaptureTarget.IsPositive() || p.CaptureTarget.GreaterThan(decimal.NewFromInt(1)) || !p.Entry.GrossBasis.IsPositive() || !p.Entry.CapitalRequired.IsPositive() || p.Entry.ExecutionBuffer.IsNegative() {
		return fmt.Errorf("invalid frozen position economics")
	}
	if p.MinimumTariff != "investor" && p.MinimumTariff != "trader" && p.MinimumTariff != "premium" {
		return fmt.Errorf("invalid frozen minimum tariff")
	}
	return nil
}

func sameEconomicPair(a, b Pair) error {
	for _, instruments := range [][2]Instrument{{a.Stock, b.Stock}, {a.Future, b.Future}} {
		x, y := instruments[0], instruments[1]
		if x.UID != y.UID || x.PositionUID != y.PositionUID || x.UnderlyingPositionUID != y.UnderlyingPositionUID || x.Lot != y.Lot || x.Currency != y.Currency || !x.AssetSize.Equal(y.AssetSize) || !x.PriceFactor.Equal(y.PriceFactor) || !x.Expiry.Equal(y.Expiry) || !x.LastTrade.Equal(y.LastTrade) || x.Settlement != y.Settlement {
			return fmt.Errorf("observation does not match the frozen instrument contract")
		}
	}
	return nil
}

func cloneJSON[T any](value T) (T, error) {
	var copy T
	data, err := json.Marshal(value)
	if err != nil {
		return copy, err
	}
	err = json.Unmarshal(data, &copy)
	return copy, err
}
