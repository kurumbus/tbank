package curve

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

const (
	PositionOpen               = "open"
	PositionAwaitingSettlement = "awaiting_settlement"
	PositionClosed             = "closed"
)

// ErrAwaitingSettlement requires a persisted lifecycle transition, not an
// invented close fill or tender submission. Derivatives settlement is outside
// this version; cash offers require explicit acceptance and payment evidence.
var ErrAwaitingSettlement = errors.New("settlement boundary reached; verified payment or settlement is required")

// NewPosition freezes the candidate, entry fees and zero-prior-turnover exit
// schedules. It creates a paper position and never submits broker orders.
func NewPosition(entry Candidate, source string) (Position, error) {
	if strings.TrimSpace(source) == "" {
		return Position{}, fmt.Errorf("position source is required")
	}
	if err := ValidatePolicy(entry.Policy); err != nil {
		return Position{}, err
	}
	if err := validateEntry(entry); err != nil {
		return Position{}, err
	}
	for _, leg := range entry.Legs {
		if _, err := positionCoupons(leg, entry.Snapshot.ObservedAt, entry.Snapshot.ObservedAt); err != nil {
			return Position{}, err
		}
	}
	frozen, err := clonePositionJSON(entry)
	if err != nil {
		return Position{}, err
	}
	profiles := scanner.Profiles()
	for i := range profiles {
		profiles[i].Family = "frozen"
		profiles[i].FutureTurnover = decimal.Zero
	}
	return Position{Source: source, Status: PositionOpen, Entry: frozen, Policy: frozen.Policy, ExitProfiles: profiles, OpenedAt: frozen.Snapshot.ObservedAt}, nil
}

// LastTrade is the first frozen contract's last trading instant. Positions are
// closed before this instant; expiry never substitutes for executable quotes.
func LastTrade(p Position) time.Time {
	var result time.Time
	for _, leg := range p.Entry.Legs {
		if leg.Instrument.Kind == "future" && !leg.Instrument.Perpetual && !leg.Instrument.LastTrade.IsZero() && (result.IsZero() || leg.Instrument.LastTrade.Before(result)) {
			result = leg.Instrument.LastTrade
		}
	}
	return result
}

// MarkPosition prices full opposite-side paper close fills using current depth.
// A target crossing requires the original minimum tariff still to meet both
// frozen profit hurdles. Time exits apply even at a loss, with valid depth.
func MarkPosition(p Position, s Snapshot) (Valuation, error) {
	if p.Status != PositionOpen && p.Status != PositionAwaitingSettlement {
		return Valuation{}, fmt.Errorf("only an open position can be marked")
	}
	if p.OpenedAt.IsZero() || !p.OpenedAt.Equal(p.Entry.Snapshot.ObservedAt) || s.ObservedAt.IsZero() || s.ObservedAt.Before(p.OpenedAt) || (p.Latest != nil && s.ObservedAt.Before(p.Latest.ObservedAt)) {
		return Valuation{}, fmt.Errorf("position observation is missing or out of order")
	}
	if err := validateEntry(p.Entry); err != nil {
		return Valuation{}, err
	}
	if err := ValidatePolicy(p.Policy); err != nil {
		return Valuation{}, err
	}
	lastTrade := LastTrade(p)
	settlementDue := SettlementDue(p)
	if p.Status == PositionAwaitingSettlement || (!settlementDue.IsZero() && !s.ObservedAt.Before(settlementDue)) {
		return Valuation{}, ErrAwaitingSettlement
	}
	profiles, err := positionExitProfiles(p)
	if err != nil {
		return Valuation{}, err
	}
	current := make(map[string]Instrument, len(s.Instruments))
	for _, instrument := range s.Instruments {
		if instrument.UID == "" {
			return Valuation{}, fmt.Errorf("missing instrument identity")
		}
		if _, exists := current[instrument.UID]; exists {
			return Valuation{}, fmt.Errorf("duplicate instrument identity %s", instrument.UID)
		}
		current[instrument.UID] = instrument
	}
	v := Valuation{ObservedAt: s.ObservedAt, Kind: "mark", ExecutionReserve: p.Entry.ExecutionReserve}
	observedFX := false
	for _, entry := range p.Entry.Legs {
		i, ok := current[entry.Instrument.UID]
		if !ok {
			return Valuation{}, fmt.Errorf("missing current instrument %s", entry.Instrument.UID)
		}
		if err := samePositionInstrument(entry.Instrument, i, s.ObservedAt, p.Policy.MaxReferenceAge); err != nil {
			return Valuation{}, err
		}
		previous := p.Entry.Snapshot.Books[i.UID].Time
		if p.Latest != nil {
			previous = p.Latest.Snapshot.Books[i.UID].Time
		}
		if s.Books[i.UID].Time.Before(previous) {
			return Valuation{}, fmt.Errorf("quote timestamp moved backwards for %s", i.UID)
		}
		closingInstrument := i
		if entry.Side == "buy" {
			// Selling the owned long closes it; short-opening eligibility is
			// irrelevant. Preserve the actual metadata in the recorded fill.
			closingInstrument.ShortEnabled = true
		}
		close, err := fill(s, p.Policy, closingInstrument, opposite(entry.Side), entry.Lots, decimal.Zero)
		if err != nil {
			return Valuation{}, fmt.Errorf("close %s: %w", i.UID, err)
		}
		close.Instrument = i
		v.Legs = append(v.Legs, close)
		profit, err := positionLegProfit(p, entry, close)
		if err != nil {
			return Valuation{}, err
		}
		v.LegProfits = append(v.LegProfits, profit)
		v.GrossProfit = v.GrossProfit.Add(profit)
		observedFX = observedFX || (entry.Instrument.Kind == "future" && strings.EqualFold(entry.Instrument.PriceCurrency, "usd"))
		income, err := positionCoupons(entry, p.OpenedAt, s.ObservedAt)
		if err != nil {
			return Valuation{}, err
		}
		v.CouponIncome = v.CouponIncome.Add(income)
	}
	if observedFX {
		v.Warnings = append(v.Warnings, "USD-quoted futures P&L uses observed quote increments translated at each observation's verified RUB factor; past accrual stays frozen. This is a paper approximation, not the exchange clearing variation-margin ledger.")
	}
	if len(p.Entry.Legs) == 2 {
		if err := checkSkew(s, p.Policy, p.Entry.Legs[0].Instrument.UID, p.Entry.Legs[1].Instrument.UID); err != nil {
			return Valuation{}, err
		}
	}
	v.Snapshot, err = clonePositionJSON(s)
	if err != nil {
		return Valuation{}, err
	}
	elapsed := years(s.ObservedAt.Sub(p.OpenedAt))
	v.CashBenchmark = p.Entry.CapitalRequired.Mul(p.Policy.CashRate).Mul(elapsed)
	v.CarryReserve = p.Entry.CapitalRequired.Mul(p.Policy.CarryReserveRate).Mul(elapsed)
	var cashCosts map[string][]scanner.FeeLine
	if IsRelativeStrategy(p.Entry.Strategy) {
		v.CashIncome, cashCosts, err = CashPositionAdjustments(p, s)
		if err != nil {
			return Valuation{}, err
		}
		var fundingWarnings []string
		v.FundingIncome, fundingWarnings, err = FundingPositionAdjustments(p, s)
		if err != nil {
			return Valuation{}, err
		}
		v.Warnings = append(v.Warnings, fundingWarnings...)
		borrowWarning := false
		for _, lines := range cashCosts {
			for _, fee := range lines {
				if strings.HasPrefix(fee.Name, "borrow_estimated") {
					v.Warnings = append(v.Warnings, "Cash-short carry is a conservative paper estimate from observed balances and the dated tariff schedule; previously accrued intervals remain frozen.")
					borrowWarning = true
					break
				}
			}
			if borrowWarning {
				break
			}
		}
	}
	for _, profile := range profiles {
		fees, err := positionEntryFees(p.Entry, profile.Name)
		if err != nil {
			return Valuation{}, err
		}
		for n, leg := range v.Legs {
			fees = append(fees, scanner.FeeLine{Name: fmt.Sprintf("exit_%d_%s", n, leg.Instrument.UID), Amount: legFee(profile, leg)})
		}
		fees = append(fees, cashCosts[profile.Name]...)
		total := decimal.Zero
		for _, fee := range fees {
			total = total.Add(fee.Amount)
		}
		net := v.GrossProfit.Add(v.CouponIncome).Add(v.CashIncome).Add(v.FundingIncome).Sub(total).Sub(v.ExecutionReserve).Sub(v.CarryReserve)
		excess := net.Sub(v.CashBenchmark)
		annualized := decimal.Zero
		if elapsed.IsPositive() {
			annualized = net.Div(p.Entry.CapitalRequired).Div(elapsed)
		}
		v.Scenarios = append(v.Scenarios, scanner.Scenario{Profile: profile.Name, StockRate: profile.StockRate, FutureRate: profile.FutureRate, FeeSource: profile.Source, Fees: fees, TradingFees: total, NetProfit: net, ExcessProfit: excess, AnnualizedReturn: annualized, Qualifies: !net.LessThan(p.Policy.MinProfit) && !excess.LessThan(p.Policy.MinExcessProfit)})
	}
	v.Metric, err = positionMetric(p, s, current)
	if err == nil && (v.Metric.Key != p.Entry.Metric.Key || v.Metric.Strategy != p.Entry.Metric.Strategy || v.Metric.Unit != p.Entry.Metric.Unit || v.Metric.ModelSource != p.Entry.Metric.ModelSource) {
		err = fmt.Errorf("current metric identity or model differs from the frozen target")
	}
	if err != nil {
		v.Warnings = append(v.Warnings, "Current convergence metric unavailable: "+err.Error()+"; target close is disabled, time exits remain active.")
	} else if crossedPositionTarget(p.Entry.Metric.Value, p.Entry.Target, v.Metric.Value) {
		for _, scenario := range v.Scenarios {
			if scenario.Profile == p.Entry.MinimumTariff && scenario.Qualifies {
				v.ShouldClose, v.CloseReason = true, "target_met"
			}
		}
	}
	if !s.ObservedAt.Before(p.OpenedAt.Add(p.Policy.MaxHolding)) {
		v.ShouldClose, v.CloseReason = true, "max_holding"
	}
	if !lastTrade.IsZero() && !s.ObservedAt.Before(lastTrade.Add(-p.Policy.ExpiryBuffer)) {
		v.ShouldClose, v.CloseReason = true, "expiry_buffer"
	}
	return v, nil
}

// A changed FX factor never retranslates prior observed USD-futures accrual.
// Each later mark adds only its raw close-quote change, using the current factor.
func positionLegProfit(p Position, entry, close Leg) (decimal.Decimal, error) {
	if entry.Instrument.Kind != "future" || !strings.EqualFold(entry.Instrument.PriceCurrency, "usd") || p.Latest == nil {
		return LegProfit(entry, close), nil
	}
	if len(p.Latest.LegProfits) != len(p.Latest.Legs) {
		return decimal.Zero, fmt.Errorf("prior per-leg USD-futures accrual is missing")
	}
	for n, prior := range p.Latest.Legs {
		if prior.Instrument.UID != entry.Instrument.UID {
			continue
		}
		if prior.Lots != entry.Lots || prior.Side != opposite(entry.Side) || !prior.Units.Equal(entry.Units) || close.Instrument.PriceFactorAt.Before(prior.Instrument.PriceFactorAt) {
			return decimal.Zero, fmt.Errorf("prior USD-futures quantity or current conversion timestamp is inconsistent for %s", entry.Instrument.UID)
		}
		baseline := entry
		baseline.Price = prior.Price
		return p.Latest.LegProfits[n].Add(LegProfit(baseline, close)), nil
	}
	return decimal.Zero, fmt.Errorf("prior USD-futures close observation is missing for %s", entry.Instrument.UID)
}

func validateEntry(c Candidate) error {
	if c.Snapshot.ObservedAt.IsZero() || len(c.Legs) < 1 || len(c.Legs) > 2 || !c.CapitalRequired.IsPositive() || c.CapitalRequired.GreaterThan(c.Policy.CapitalLimit) || c.ExecutionReserve.IsNegative() || c.Key == "" {
		return fmt.Errorf("invalid frozen position entry")
	}
	if c.Strategy != OFZRGBI && c.Strategy != KEYRATECalendar && c.Strategy != RUONIACalendar && c.Strategy != RGBICalendar && !IsRelativeStrategy(c.Strategy) {
		return fmt.Errorf("unsupported position strategy")
	}
	if c.Strategy == CashOffer {
		if len(c.Legs) != 1 || c.Legs[0].Side != "buy" {
			return fmt.Errorf("cash offer requires one owned long leg")
		}
	} else if len(c.Legs) != 2 {
		return fmt.Errorf("spread position requires two legs")
	}
	if IsRelativeStrategy(c.Strategy) && (c.Relationship == nil || c.Relationship.Strategy != c.Strategy || !c.Relationship.Verified || c.Relationship.Source == "") {
		return fmt.Errorf("verified frozen relationship is required")
	}
	if c.Strategy == CashOffer {
		if err := validateOffer(c.Snapshot, c.Policy, *c.Relationship); err != nil {
			return err
		}
	}
	if c.MinimumTariff != "investor" && c.MinimumTariff != "trader" && c.MinimumTariff != "premium" {
		return fmt.Errorf("qualified minimum tariff is required")
	}
	if len(c.Scenarios) != 3 {
		return fmt.Errorf("exactly three entry tariff scenarios are required")
	}
	minimum := ""
	for _, name := range []string{"investor", "trader", "premium"} {
		if _, err := positionEntryFees(c, name); err != nil {
			return err
		}
		for _, scenario := range c.Scenarios {
			if scenario.Profile == name && (!scenario.StockRate.IsPositive() || !scenario.FutureRate.IsPositive() || strings.TrimSpace(scenario.FeeSource) == "" || (name == c.MinimumTariff && (!scenario.Qualifies || scenario.NetProfit.LessThan(c.Policy.MinProfit) || scenario.ExcessProfit.LessThan(c.Policy.MinExcessProfit)))) {
				return fmt.Errorf("invalid or ineligible frozen tariff %s", name)
			}
			if scenario.Profile == name && scenario.Qualifies && minimum == "" {
				minimum = name
			}
		}
	}
	if minimum != c.MinimumTariff {
		return fmt.Errorf("entry minimum tariff differs from least eligible scenario")
	}
	for n, l := range c.Legs {
		if l.Instrument.UID == "" || l.Lots <= 0 || !l.Units.Equal(decimal.NewFromInt(l.Lots).Mul(decimal.NewFromInt(l.Instrument.Lot))) || !l.Value.IsPositive() || !l.FeeValue.IsPositive() || (l.Side != "buy" && l.Side != "sell") || (n > 0 && l.Instrument.UID == c.Legs[0].Instrument.UID) {
			return fmt.Errorf("invalid entry leg")
		}
		if err := validInstrument(l.Instrument, c.Snapshot.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}

func positionEntryFees(c Candidate, name string) ([]scanner.FeeLine, error) {
	var result []scanner.FeeLine
	matches := 0
	for _, scenario := range c.Scenarios {
		if scenario.Profile != name {
			continue
		}
		matches++
		for n, leg := range c.Legs {
			wanted := fmt.Sprintf("entry_%d_%s", n, leg.Instrument.UID)
			found := false
			for _, fee := range scenario.Fees {
				if fee.Name == wanted {
					if found || !fee.Amount.IsPositive() {
						return nil, fmt.Errorf("invalid frozen %s fee for %s", wanted, name)
					}
					found = true
					result = append(result, fee)
				}
			}
			if !found {
				return nil, fmt.Errorf("missing frozen %s fee for %s", wanted, name)
			}
		}
	}
	if matches != 1 || len(result) != len(c.Legs) {
		return nil, fmt.Errorf("entry scenario %s is missing or duplicated", name)
	}
	return result, nil
}

func positionExitProfiles(p Position) ([]scanner.Profile, error) {
	if len(p.ExitProfiles) != 3 {
		return nil, fmt.Errorf("three frozen exit schedules required")
	}
	var result []scanner.Profile
	for _, name := range []string{"investor", "trader", "premium"} {
		count := 0
		for _, profile := range p.ExitProfiles {
			if profile.Name != name {
				continue
			}
			count++
			if !profile.StockRate.IsPositive() || !profile.FutureRate.IsPositive() || !profile.FutureTurnover.IsZero() || strings.TrimSpace(profile.Source) == "" {
				return nil, fmt.Errorf("invalid frozen exit schedule %s", name)
			}
			profile.Family = "frozen"
			result = append(result, profile)
		}
		if count != 1 {
			return nil, fmt.Errorf("exit schedule %s is missing or duplicated", name)
		}
	}
	return result, nil
}

func samePositionInstrument(a, b Instrument, now time.Time, maxReferenceAge time.Duration) error {
	if a.UID != b.UID || a.Ticker != b.Ticker || a.ClassCode != b.ClassCode || a.Kind != b.Kind || a.Family != b.Family || a.Currency != b.Currency || a.Lot != b.Lot || !a.UnitSize.Equal(b.UnitSize) || !strings.EqualFold(a.PriceCurrency, b.PriceCurrency) || a.Perpetual != b.Perpetual || a.SettlementReference != b.SettlementReference || a.UnderlyingUID != b.UnderlyingUID || a.SettlementType != b.SettlementType || !a.Expiry.Equal(b.Expiry) || !a.LastTrade.Equal(b.LastTrade) {
		return fmt.Errorf("observation does not match frozen contract %s", a.UID)
	}
	if a.Kind == "future" && strings.EqualFold(a.PriceCurrency, "usd") {
		if !b.PriceFactorKnown || !b.PriceFactor.IsPositive() || b.PriceFactorAt.IsZero() || b.PriceFactorAt.Before(a.PriceFactorAt) || b.PriceFactorAt.After(now) || now.Sub(b.PriceFactorAt) > maxReferenceAge || strings.TrimSpace(b.PriceFactorSource) == "" {
			return fmt.Errorf("current verified quote conversion is required for %s", a.UID)
		}
	} else if !a.PriceFactor.Equal(b.PriceFactor) {
		return fmt.Errorf("observation changes frozen contract multiplier %s", a.UID)
	}
	if a.Kind == "bond" && (a.Bond == nil || b.Bond == nil || !a.Bond.Nominal.Equal(b.Bond.Nominal) || !a.Bond.Maturity.Equal(b.Bond.Maturity) || a.Bond.FixedCoupon != b.Bond.FixedCoupon || a.Bond.Floating != b.Bond.Floating || a.Bond.Indexed != b.Bond.Indexed || a.Bond.Amortizing != b.Bond.Amortizing) {
		return fmt.Errorf("observation changes frozen bond terms %s", a.UID)
	}
	if a.Kind == "bond" {
		for _, frozen := range a.Bond.Cashflows {
			if positionCouponDate(frozen.Date).Before(positionCouponDate(b.Bond.AccruedAt)) {
				continue // Providers may omit already-paid historical flows.
			}
			found := false
			for _, current := range b.Bond.Cashflows {
				if current.Kind == frozen.Kind && positionCouponDate(current.Date).Equal(positionCouponDate(frozen.Date)) && current.Amount.Equal(frozen.Amount) && (frozen.RecordDate.IsZero() || positionCouponDate(current.RecordDate).Equal(positionCouponDate(frozen.RecordDate))) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("observation changes frozen bond cashflow %s on %s", a.UID, frozen.Date.Format("2006-01-02"))
			}
		}
	}
	return nil
}

func positionCoupons(entry Leg, openedAt, now time.Time) (decimal.Decimal, error) {
	result := decimal.Zero
	if entry.Instrument.Kind != "bond" {
		return result, nil
	}
	openedDate, nowDate := positionCouponDate(openedAt), positionCouponDate(now)
	for _, flow := range entry.Instrument.Bond.Cashflows {
		if flow.Kind != "coupon" || positionCouponDate(flow.Date).Before(openedDate) {
			continue
		}
		paymentDate := positionCouponDate(flow.Date)
		if !flow.RecordDate.IsZero() && positionCouponDate(flow.RecordDate).Equal(openedDate) && !nowDate.Before(openedDate) {
			return decimal.Zero, fmt.Errorf("same-record-date coupon entitlement is ambiguous for %s; verified settlement eligibility is required", entry.Instrument.UID)
		}
		if paymentDate.After(nowDate) {
			if !flow.RecordDate.IsZero() {
				recordDate := positionCouponDate(flow.RecordDate)
				if !recordDate.Before(openedDate) && recordDate.Before(nowDate) {
					return decimal.Zero, fmt.Errorf("earned unpaid coupon receivable for %s until %s; paper marking waits for payment", entry.Instrument.UID, paymentDate.Format("2006-01-02"))
				}
			}
			continue
		}
		if flow.RecordDate.IsZero() {
			return decimal.Zero, fmt.Errorf("coupon entitlement is unknown for %s payment %s", entry.Instrument.UID, flow.Date.Format(time.RFC3339))
		}
		if positionCouponDate(flow.RecordDate).Before(openedDate) {
			continue
		}
		amount := flow.Amount.Mul(entry.Units)
		if entry.Side == "sell" {
			amount = amount.Neg()
		}
		result = result.Add(amount)
	}
	return result, nil
}

// API payment and record dates are calendar dates. The record-day transition
// applies on the following Moscow business-calendar date, not at UTC midnight
// or the first intraday mark on the record date itself.
func positionCouponDate(at time.Time) time.Time {
	local := at.In(time.FixedZone("MSK", 3*60*60))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func positionMetric(p Position, s Snapshot, instruments map[string]Instrument) (Metric, error) {
	if IsRelativeStrategy(p.Entry.Strategy) {
		if p.Entry.Relationship == nil {
			return Metric{}, fmt.Errorf("missing frozen relationship")
		}
		return RelativeMetric(s, p.Policy, *p.Entry.Relationship)
	}
	a, b := instruments[p.Entry.Legs[0].Instrument.UID], instruments[p.Entry.Legs[1].Instrument.UID]
	if p.Entry.Strategy == OFZRGBI {
		if a.Kind != "bond" {
			a, b = b, a
		}
		_, spread, _, err := fitObservedBond(s, p.Policy, a)
		if err != nil {
			return Metric{}, err
		}
		return Metric{Key: p.Entry.Metric.Key, Strategy: p.Entry.Strategy, Unit: p.Entry.Metric.Unit, ModelSource: p.Entry.Metric.ModelSource, InstrumentUIDs: []string{a.UID, b.UID}, ObservedAt: s.ObservedAt, QuoteTimes: []time.Time{s.Books[a.UID].Time, s.Books[b.UID].Time}, Value: spread.Mul(bps)}, nil
	}
	if p.Entry.Strategy != KEYRATECalendar && p.Entry.Strategy != RUONIACalendar && p.Entry.Strategy != RGBICalendar {
		return Metric{}, fmt.Errorf("unsupported position strategy")
	}
	if b.Expiry.Before(a.Expiry) {
		a, b = b, a
	}
	return calendarMetric(s, p.Policy, a, b)
}

func crossedPositionTarget(initial, target, current decimal.Decimal) bool {
	if initial.GreaterThan(target) {
		return !current.GreaterThan(target)
	}
	if initial.LessThan(target) {
		return !current.LessThan(target)
	}
	return false
}

func clonePositionJSON[T any](value T) (T, error) {
	var result T
	data, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}
