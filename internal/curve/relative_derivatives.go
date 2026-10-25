package curve

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

func relativeFutureFamily(family string) bool {
	switch strings.ToUpper(family) {
	case "MX", "MM", "IMOEX", "MXI", "SI", "UM", "USDRUB", "USDM", "GD", "GN", "GOLD", "GOLDM", "SR", "SP", "SBRF", "SBPR", "SBER", "SBERP", "TT", "TP", "TATN", "TATNP", "TATP", "CNYRUBF", "CR", "CNY", "CNYRUB", "GLDRUBF", "GL", "GLDRUB", "GOLDRUB":
		return true
	}
	return false
}

// EvaluateRelationships evaluates only the six explicitly sourced relationships.
// It shares the curve engine's costs, quotes, history and persistent lifecycle.
func EvaluateRelationships(s Snapshot, p Policy, history []Metric) (Scan, error) {
	out := Scan{ObservedAt: s.ObservedAt}
	if err := ValidatePolicy(p); err != nil {
		return out, err
	}
	if s.ObservedAt.IsZero() {
		return out, fmt.Errorf("snapshot observation time is required")
	}
	seen := map[string]bool{}
	for _, i := range s.Instruments {
		if i.UID == "" || seen[i.UID] {
			return out, fmt.Errorf("missing or duplicated instrument UID")
		}
		seen[i.UID] = true
	}
	relationshipIDs := map[string]bool{}
	for _, r := range s.Relationships {
		if r.ID == "" || relationshipIDs[r.ID] {
			return out, fmt.Errorf("missing or duplicated relationship ID")
		}
		relationshipIDs[r.ID] = true
	}
	for _, r := range s.Relationships {
		if r.Strategy != StandardMini && r.Strategy != ShareClass && r.Strategy != PerpetualDated {
			continue
		}
		m, err := derivativeRelationshipMetric(s, p, r)
		if err != nil {
			out.Skips = append(out.Skips, r.ID+": "+err.Error())
			continue
		}
		out.Metrics = append(out.Metrics, m)
		target, source := decimal.Zero, "shared settlement fixing and equal underlying units; normalized parity target"
		if r.Strategy == ShareClass {
			target, source, err = historyTarget(m, history, p)
			if err != nil {
				out.Skips = append(out.Skips, r.ID+": "+err.Error())
				continue
			}
		}
		if r.Strategy == PerpetualDated {
			source = "zero dated/perpetual price basis scenario at dated horizon; unknown future funding is not projected"
		}
		for _, longPrimary := range []bool{true, false} {
			c, err := derivativeRelationshipCandidate(s, p, r, m, target, source, longPrimary)
			if err != nil {
				out.Skips = append(out.Skips, r.ID+": "+err.Error())
				continue
			}
			out.Candidates = append(out.Candidates, c)
		}
	}
	cash := evaluateCashRelationships(s, p, history)
	out.Metrics = append(out.Metrics, cash.Metrics...)
	out.Candidates = append(out.Candidates, cash.Candidates...)
	out.Skips = append(out.Skips, cash.Skips...)
	return out, nil
}

// RelativeMetric uses current reference data but rejects changed instrument or
// relationship identities. A changed economic definition needs fresh history.
func RelativeMetric(s Snapshot, p Policy, frozen Relationship) (Metric, error) {
	r, err := currentRelationship(s, frozen)
	if err != nil {
		return Metric{}, err
	}
	if r.Strategy == StandardMini || r.Strategy == ShareClass || r.Strategy == PerpetualDated {
		return derivativeRelationshipMetric(s, p, r)
	}
	if r.Strategy == FundNAV {
		if err = validateFundNAVAlignment(s, p, r); err != nil {
			return Metric{}, err
		}
	}
	return cashRelationshipMetric(s, p, r)
}

// ValidateRelativeEntryAt rechecks time-dependent facts after an account preview.
// It keeps the original prices and reference timestamps for audit; it does not
// refresh, retime or substitute market data.
func ValidateRelativeEntryAt(c Candidate, now time.Time) error {
	if !IsRelativeStrategy(c.Strategy) {
		return nil
	}
	if c.Relationship == nil || now.IsZero() || now.Before(c.Snapshot.ObservedAt) {
		return fmt.Errorf("relative entry needs its original relationship and a monotonic observation time")
	}
	s := c.Snapshot
	s.ObservedAt = now
	r, err := currentRelationship(s, *c.Relationship)
	if err != nil {
		return err
	}
	for _, leg := range c.Legs {
		if _, err := validatedBook(s, c.Policy, leg.Instrument); err != nil {
			return err
		}
		if leg.Instrument.Kind == "future" && !leg.Instrument.Perpetual && !leg.Instrument.LastTrade.After(now.Add(c.Policy.ExpiryBuffer)) {
			return fmt.Errorf("future reached its entry expiry buffer during preview")
		}
	}
	if _, err = RelativeMetric(s, c.Policy, r); err != nil {
		return err
	}
	if r.Strategy == HoldingStub || r.Strategy == FundNAV || r.Strategy == CashOffer {
		return validateCashEntryAt(c, now)
	}
	return nil
}

func currentRelationship(s Snapshot, frozen Relationship) (Relationship, error) {
	var found *Relationship
	for _, r := range s.Relationships {
		if r.ID != frozen.ID {
			continue
		}
		if found != nil {
			return Relationship{}, fmt.Errorf("duplicate relationship reference %s", r.ID)
		}
		copy := r
		found = &copy
	}
	if found == nil {
		return Relationship{}, fmt.Errorf("current relationship reference %s is missing", frozen.ID)
	}
	r := *found
	if r.Strategy != frozen.Strategy || r.PrimaryUID != frozen.PrimaryUID || r.HedgeUID != frozen.HedgeUID {
		return Relationship{}, fmt.Errorf("relationship instrument identity changed")
	}
	if r.Strategy == StandardMini || r.Strategy == ShareClass || r.Strategy == PerpetualDated {
		if !r.HedgeLotsPerPrimaryLot.Equal(frozen.HedgeLotsPerPrimaryLot) || !r.PrimaryPriceScale.Equal(frozen.PrimaryPriceScale) || !r.HedgePriceScale.Equal(frozen.HedgePriceScale) {
			return Relationship{}, fmt.Errorf("frozen derivative units or normalizations changed")
		}
	}
	return r, nil
}

func relationshipLegs(s Snapshot, r Relationship) (Instrument, Instrument, error) {
	var a, b Instrument
	for _, i := range s.Instruments {
		if i.UID == r.PrimaryUID {
			a = i
		}
		if i.UID == r.HedgeUID {
			b = i
		}
	}
	if a.UID == "" || b.UID == "" || a.UID == b.UID {
		return a, b, fmt.Errorf("relationship requires two distinct resolved instruments")
	}
	return a, b, nil
}

func validateDerivativeRelationship(s Snapshot, p Policy, r Relationship) (Instrument, Instrument, error) {
	a, b, err := relationshipLegs(s, r)
	if err != nil {
		return a, b, err
	}
	if r.ID == "" || !r.Verified || strings.TrimSpace(r.Source) == "" || r.AsOf.IsZero() || r.AsOf.After(s.ObservedAt) || r.ValidUntil.IsZero() || !r.ValidUntil.After(s.ObservedAt) || r.PrimaryLots <= 0 || !r.HedgeLotsPerPrimaryLot.IsPositive() || !r.PrimaryPriceScale.IsPositive() || !r.HedgePriceScale.IsPositive() {
		return a, b, fmt.Errorf("verified, current relationship terms and exact quantities are required")
	}
	if a.Kind != "future" || b.Kind != "future" || !a.UnitSize.IsPositive() || !b.UnitSize.IsPositive() {
		return a, b, fmt.Errorf("verified futures contract unit sizes required")
	}
	if _, err = mid(s, p, a); err != nil {
		return a, b, err
	}
	if _, err = mid(s, p, b); err != nil {
		return a, b, err
	}
	if err = checkSkew(s, p, a.UID, b.UID); err != nil {
		return a, b, err
	}
	if !strings.EqualFold(a.PriceCurrency, b.PriceCurrency) {
		return a, b, fmt.Errorf("relationship quote currencies differ")
	}
	if !strings.EqualFold(a.PriceCurrency, "rub") && !strings.EqualFold(a.PriceCurrency, "usd") {
		return a, b, fmt.Errorf("verified RUB or USD quote currency required")
	}
	if !a.UnitSize.Equal(b.UnitSize.Mul(r.HedgeLotsPerPrimaryLot)) {
		return a, b, fmt.Errorf("contract ratio does not match underlying unit sizes")
	}
	if r.Strategy == PerpetualDated {
		if !a.Perpetual || b.Perpetual || a.UnderlyingUID == "" || a.UnderlyingUID != b.UnderlyingUID {
			return a, b, fmt.Errorf("perpetual and dated contracts require the same verified underlying")
		}
		if err = validateFundingReference(r.Funding, a.UID, s.ObservedAt, s.ObservedAt, false); err != nil {
			return a, b, err
		}
	} else {
		if a.Perpetual || b.Perpetual || !a.Expiry.Equal(b.Expiry) || !a.LastTrade.Equal(b.LastTrade) {
			return a, b, fmt.Errorf("dated relationship contracts must have matching expiry and last trading time")
		}
		if r.Strategy == StandardMini && (a.UnderlyingUID == "" || a.UnderlyingUID != b.UnderlyingUID || a.SettlementReference == "" || a.SettlementReference != b.SettlementReference || a.SettlementType != b.SettlementType) {
			return a, b, fmt.Errorf("standard/mini contracts lack a common verified settlement fixing")
		}
	}
	// Contract units and quote normalization must describe equal P&L exposure.
	left := a.PriceFactor.Div(r.PrimaryPriceScale)
	right := b.PriceFactor.Div(r.HedgePriceScale).Mul(r.HedgeLotsPerPrimaryLot)
	if left.Sub(right).Abs().Div(decimal.Max(left, right)).GreaterThan(decimal.RequireFromString("0.000001")) {
		return a, b, fmt.Errorf("quote conversion and hedge units are inconsistent")
	}
	return a, b, nil
}

func derivativeRelationshipMetric(s Snapshot, p Policy, r Relationship) (Metric, error) {
	a, b, err := validateDerivativeRelationship(s, p, r)
	if err != nil {
		return Metric{}, err
	}
	pa, _ := mid(s, p, a)
	pb, _ := mid(s, p, b)
	pa = pa.Mul(r.PrimaryPriceScale)
	pb = pb.Mul(r.HedgePriceScale)
	value, err := logRatioBPS(pa, pb, one)
	if err != nil {
		return Metric{}, err
	}
	unit, definition := "log_ratio_bps", "normalized_primary_over_hedge_log_v1"
	if r.Strategy == PerpetualDated {
		value, err = logRatioBPS(pb, pa, years(b.Expiry.Sub(s.ObservedAt)))
		if err != nil {
			return Metric{}, err
		}
		unit, definition = "annualized_basis_bps", "dated_over_perpetual_log_act365_v1"
	}
	source := fmt.Sprintf("%s:%s:%s:%s:%s", definition, r.ID, r.PrimaryPriceScale, r.HedgePriceScale, r.HedgeLotsPerPrimaryLot)
	return Metric{Key: r.Strategy + ":" + r.ID + ":" + a.UID + ":" + b.UID, Strategy: r.Strategy, Unit: unit, ModelSource: source, InstrumentUIDs: []string{a.UID, b.UID}, ObservedAt: s.ObservedAt, QuoteTimes: []time.Time{s.Books[a.UID].Time, s.Books[b.UID].Time}, Value: value}, nil
}

func derivativeRelationshipCandidate(s Snapshot, p Policy, r Relationship, m Metric, target decimal.Decimal, source string, longPrimary bool) (Candidate, error) {
	a, b, err := validateDerivativeRelationship(s, p, r)
	if err != nil {
		return Candidate{}, err
	}
	q := decimal.NewFromInt(r.PrimaryLots).Mul(r.HedgeLotsPerPrimaryLot)
	if !q.Equal(q.Truncate(0)) || q.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return Candidate{}, fmt.Errorf("hedge quantity must be an exact positive integer")
	}
	qa, qb := r.PrimaryLots, q.IntPart()
	sa, sb, direction := "buy", "sell", "long_primary_short_hedge"
	if !longPrimary {
		sa, sb, direction = "sell", "buy", "short_primary_long_hedge"
	}
	l1, err := fill(s, p, a, sa, qa, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	l2, err := fill(s, p, b, sb, qb, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	pa, _ := mid(s, p, a)
	pb, _ := mid(s, p, b)
	shiftA, shiftB := decimal.Zero, decimal.Zero
	if r.Strategy == PerpetualDated {
		fair, err := expTarget(pa.Mul(r.PrimaryPriceScale), target, years(b.Expiry.Sub(s.ObservedAt)))
		if err != nil {
			return Candidate{}, err
		}
		shiftB = fair.Div(r.HedgePriceScale).Sub(pb)
	} else {
		fair, err := expTarget(pb.Mul(r.HedgePriceScale), target, one)
		if err != nil {
			return Candidate{}, err
		}
		shiftA = fair.Div(r.PrimaryPriceScale).Sub(pa)
	}
	e1, err := closeFill(s, p, l1, shiftA)
	if err != nil {
		return Candidate{}, err
	}
	e2, err := closeFill(s, p, l2, shiftB)
	if err != nil {
		return Candidate{}, err
	}
	rCopy := r
	c := Candidate{Key: m.Key, Strategy: r.Strategy, Direction: direction, Snapshot: s, Policy: p, Legs: []Leg{l1, l2}, Metric: m, Target: target, TargetSource: source, Relationship: &rCopy, HedgeMeasure: "matched_underlying_units", DV01Known: false, Warnings: []string{"Expected result is a target-price scenario with the other normalized leg and current book shape unchanged; current exit liquidity is not guaranteed.", "Futures funding and margin cash demands remain separate from matched underlying units."}}
	if strings.EqualFold(a.PriceCurrency, "usd") || strings.EqualFold(b.PriceCurrency, "usd") {
		c.Warnings = append(c.Warnings, "USD-quoted target P&L uses the current RUB conversion. Later paper marks accrue between observations at each observation's conversion; this approximation is not an official clearing variation-margin ledger.")
	}
	switch r.Strategy {
	case StandardMini:
		c.Warnings = append(c.Warnings, "The contracts share a documented fixing, expiry and unit exposure. Paper exit still occurs before expiry; this version does not fabricate final settlement.")
	case ShareClass:
		c.Warnings = append(c.Warnings, "Ordinary and preferred shares have different rights. Historical share-class ratios have no compulsory parity or convergence at futures delivery.")
	case PerpetualDated:
		c.Warnings = append(c.Warnings, "Future perpetual funding is unknown and is not extrapolated from today's funding. The projected result excludes unknown funding, with a separate uncertainty reserve; realized funding uses the verified ledger.")
	}
	return priceCandidate(c, []Leg{e1, e2})
}

func validateFundingReference(f *FundingReference, uid string, from, to time.Time, full bool) error {
	if f == nil || !f.Known || f.InstrumentUID != uid || strings.TrimSpace(f.Source) == "" || f.CoverageStart.IsZero() || f.CoverageEnd.IsZero() || f.CoverageEnd.Before(f.CoverageStart) || f.CoverageEnd.After(to) {
		return fmt.Errorf("verified perpetual funding coverage and source are required")
	}
	if !full && to.Sub(f.CoverageEnd) > 72*time.Hour {
		return fmt.Errorf("funding reference is stale")
	}
	if full && (f.CoverageStart.After(from) || f.CoverageEnd.Before(to)) {
		return fmt.Errorf("funding coverage has a missing held interval")
	}
	seen := map[string]bool{}
	for _, event := range f.Events {
		key := event.At.UTC().Format(time.RFC3339Nano)
		if event.At.IsZero() || event.Reference == "" || seen[key] || event.At.Before(f.CoverageStart) || event.At.After(f.CoverageEnd) {
			return fmt.Errorf("invalid or duplicate funding event")
		}
		seen[key] = true
	}
	return nil
}

func FundingPositionIncome(p Position, s Snapshot) (decimal.Decimal, error) {
	income, _, err := FundingPositionAdjustments(p, s)
	return income, err
}

// FundingPositionAdjustments never extrapolates the latest rate. Date-only rows
// are booked only for complete Moscow days held throughout; boundary-day cash
// is explicitly unresolved, rather than invented at an arbitrary clearing time.
func FundingPositionAdjustments(p Position, s Snapshot) (decimal.Decimal, []string, error) {
	income := decimal.Zero
	var warnings []string
	if p.Entry.Strategy != PerpetualDated {
		return income, warnings, nil
	}
	if p.Entry.Relationship == nil {
		return income, warnings, fmt.Errorf("frozen perpetual relationship missing")
	}
	r, err := currentRelationship(s, *p.Entry.Relationship)
	if err != nil {
		return income, warnings, err
	}
	var leg *Leg
	for n := range p.Entry.Legs {
		if p.Entry.Legs[n].Instrument.Perpetual {
			leg = &p.Entry.Legs[n]
			break
		}
	}
	if leg == nil {
		return income, warnings, fmt.Errorf("perpetual position leg missing")
	}
	f := r.Funding
	if err = validateFundingReference(f, leg.Instrument.UID, p.OpenedAt, s.ObservedAt, false); err != nil {
		return income, warnings, err
	}
	if p.Latest != nil {
		previous, err := currentRelationship(p.Latest.Snapshot, *p.Entry.Relationship)
		if err != nil || previous.Funding == nil {
			return income, warnings, fmt.Errorf("previously observed funding ledger is missing")
		}
		currentEvents := map[string]FundingEvent{}
		for _, event := range f.Events {
			currentEvents[event.At.UTC().Format(time.RFC3339Nano)] = event
		}
		for _, event := range previous.Funding.Events {
			if !fundingEventBooked(event, p.OpenedAt, p.Latest.ObservedAt) {
				continue
			}
			current, ok := currentEvents[event.At.UTC().Format(time.RFC3339Nano)]
			if !ok || event.DateOnly != current.DateOnly || !event.AmountPerLongContract.Equal(current.AmountPerLongContract) {
				return income, warnings, fmt.Errorf("previously accrued funding event was removed or revised; cash reconciliation required")
			}
		}
	}
	location := time.FixedZone("MSK", 3*60*60)
	day := func(t time.Time) time.Time {
		t = t.In(location)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, location)
	}
	startDay, endDay := day(p.OpenedAt), day(s.ObservedAt)
	firstFull := startDay.AddDate(0, 0, 1)
	if !p.OpenedAt.After(startDay) {
		firstFull = startDay
	}
	if endDay.After(firstFull) && (f.CoverageStart.After(firstFull) || f.CoverageEnd.Before(endDay.Add(-time.Nanosecond))) {
		return income, warnings, fmt.Errorf("verified funding ledger does not cover every complete held day")
	}
	if f.CoverageStart.After(p.OpenedAt) || f.CoverageEnd.Before(s.ObservedAt) {
		warnings = append(warnings, "Funding on partial boundary days is not fully resolved by the available ledger; unbooked amounts are not assumed to be zero.")
	}
	events := append([]FundingEvent(nil), f.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	for _, event := range events {
		if event.DateOnly {
			ed := day(event.At)
			if ed.Equal(startDay) && p.OpenedAt.After(startDay) || ed.Equal(endDay) {
				warnings = append(warnings, "Date-only funding for "+ed.Format("2006-01-02")+" is unresolved at the position boundary; it is not booked as cash.")
				continue
			}
			if ed.Before(firstFull) || !ed.Before(endDay) {
				continue
			}
		} else if !event.At.After(p.OpenedAt) || event.At.After(s.ObservedAt) {
			continue
		}
		amount := event.AmountPerLongContract.Mul(decimal.NewFromInt(leg.Lots))
		if leg.Side == "sell" {
			amount = amount.Neg()
		}
		income = income.Add(amount)
	}
	return income, warnings, nil
}

func fundingEventBooked(event FundingEvent, opened, observed time.Time) bool {
	if !event.DateOnly {
		return event.At.After(opened) && !event.At.After(observed)
	}
	location := time.FixedZone("MSK", 3*60*60)
	d := event.At.In(location)
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, location)
	return !opened.After(start) && !observed.Before(start.AddDate(0, 0, 1))
}
