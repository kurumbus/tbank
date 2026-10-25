package curve

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

var one = decimal.NewFromInt(1)
var bps = decimal.NewFromInt(10000)

func ValidatePolicy(p Policy) error {
	if p.BondLots <= 0 || p.FutureContracts <= 0 || !p.CapitalLimit.IsPositive() || p.MinPeers < 4 || p.MinPeers > 8 || p.MinHistory < 2 || p.MinHistory > 1000 || p.MinHistorySpan <= 0 || p.HistoryMaxAge < p.MinHistorySpan || p.HistoryMaxAge > 365*24*time.Hour || p.MaxQuoteAge <= 0 || p.MaxQuoteAge > 300*time.Second || p.MaxQuoteSkew < 0 || p.MaxQuoteSkew > p.MaxQuoteAge || p.MaxReferenceAge <= 0 || p.MaxHolding <= 0 || p.MaxHolding > 365*24*time.Hour || p.ExpiryBuffer <= 0 || p.ExpiryBuffer > 365*24*time.Hour {
		return fmt.Errorf("invalid sizing, history, quote age, peer count or holding policy")
	}
	for name, v := range map[string]decimal.Decimal{"cash rate": p.CashRate, "margin buffer": p.MarginBufferRate, "execution buffer": p.ExecutionBufferBPS, "carry reserve": p.CarryReserveRate, "turnover": p.FutureTurnover, "minimum profit": p.MinProfit, "minimum excess": p.MinExcessProfit, "deviation": p.MinDeviationBPS, "residual DV01": p.MaxResidualDV01Ratio, "curve RMSE": p.MaxCurveRMSE, "curve error": p.MaxCurveError} {
		if v.IsNegative() {
			return fmt.Errorf("%s cannot be negative", name)
		}
	}
	if p.MaxResidualDV01Ratio.GreaterThan(one) || !p.MaxCurveRMSE.IsPositive() || !p.MaxCurveError.IsPositive() {
		return fmt.Errorf("invalid model error bounds")
	}
	return nil
}

// Evaluate always publishes valid current metrics, even during history warmup.
// History is a prior-observation reference, not evidence of a guaranteed payoff.
func Evaluate(s Snapshot, p Policy, history []Metric) (Scan, error) {
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
			return out, fmt.Errorf("missing or duplicate instrument UID")
		}
		seen[i.UID] = true
	}
	for _, family := range []string{"KEYRATE", "RUONIA", "RGBI"} {
		futures := familyFutures(s, family, p.ExpiryBuffer)
		for n := 0; n+1 < len(futures); n++ {
			near, far := futures[n], futures[n+1]
			m, err := calendarMetric(s, p, near, far)
			if err != nil {
				out.Skips = append(out.Skips, near.Ticker+"/"+far.Ticker+": "+err.Error())
				continue
			}
			out.Metrics = append(out.Metrics, m)
			target, source, err := historyTarget(m, history, p)
			if err != nil {
				out.Skips = append(out.Skips, m.Key+": "+err.Error())
				continue
			}
			for _, longFar := range []bool{true, false} {
				candidate, err := calendarCandidate(s, p, near, far, m, target, source, longFar)
				if err != nil {
					out.Skips = append(out.Skips, m.Key+": "+err.Error())
					continue
				}
				out.Candidates = append(out.Candidates, candidate)
			}
		}
	}
	hedges := familyFutures(s, "RGBI", p.ExpiryBuffer)
	if len(hedges) > 0 {
		for _, bond := range s.Instruments {
			if bond.Kind != "bond" || bond.Family != "OFZ" {
				continue
			}
			candidate, m, err := bondCandidate(s, p, bond, hedges[0])
			if m.Key != "" {
				out.Metrics = append(out.Metrics, m)
			}
			if err != nil {
				out.Skips = append(out.Skips, bond.Ticker+": "+err.Error())
				continue
			}
			out.Candidates = append(out.Candidates, candidate)
		}
	}
	return out, nil
}

func familyFutures(s Snapshot, family string, buffer time.Duration) []Instrument {
	var result []Instrument
	for _, i := range s.Instruments {
		if i.Kind == "future" && i.Family == family && i.LastTrade.After(s.ObservedAt.Add(buffer)) {
			result = append(result, i)
		}
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Expiry.Before(result[b].Expiry) })
	return result
}

func calendarMetric(s Snapshot, p Policy, near, far Instrument) (Metric, error) {
	nearMid, err := mid(s, p, near)
	if err != nil {
		return Metric{}, err
	}
	farMid, err := mid(s, p, far)
	if err != nil {
		return Metric{}, err
	}
	if near.Family != far.Family || !far.Expiry.After(near.Expiry) {
		return Metric{}, fmt.Errorf("calendar legs must be adjacent dated contracts of one family")
	}
	if err = checkSkew(s, p, near.UID, far.UID); err != nil {
		return Metric{}, err
	}
	strategy, unit, source := "", "bps", ""
	var metric decimal.Decimal
	switch near.Family {
	case "KEYRATE":
		strategy = KEYRATECalendar
		source = "keyrate_direct_percentage_point_difference_v1"
		metric = farMid.Sub(nearMid).Mul(decimal.NewFromInt(100))
	case "RUONIA", "RGBI":
		strategy = RUONIACalendar
		source = "ruonia_accumulated_index_log_forward_act365_v1"
		if near.Family == "RGBI" {
			strategy = RGBICalendar
			source = "rgbi_log_calendar_funding_basis_act365_v1"
		}
		metric, err = logRatioBPS(farMid, nearMid, years(far.Expiry.Sub(near.Expiry)))
		if err != nil {
			return Metric{}, err
		}
	default:
		return Metric{}, fmt.Errorf("unsupported futures family")
	}
	return Metric{Key: strategy + ":" + near.UID + ":" + far.UID, Strategy: strategy, Unit: unit, ModelSource: source, InstrumentUIDs: []string{near.UID, far.UID}, ObservedAt: s.ObservedAt, QuoteTimes: []time.Time{s.Books[near.UID].Time, s.Books[far.UID].Time}, Value: metric}, nil
}

func historyTarget(current Metric, history []Metric, p Policy) (decimal.Decimal, string, error) {
	var compatible []Metric
	seen := map[string]bool{}
	for _, m := range history {
		if m.Key != current.Key || m.Strategy != current.Strategy || m.Unit != current.Unit || m.ModelSource != current.ModelSource || !m.ObservedAt.Before(current.ObservedAt) || m.ObservedAt.Before(current.ObservedAt.Add(-p.HistoryMaxAge)) || len(m.QuoteTimes) != 2 || len(m.InstrumentUIDs) != 2 {
			continue
		}
		if m.InstrumentUIDs[0] != current.InstrumentUIDs[0] || m.InstrumentUIDs[1] != current.InstrumentUIDs[1] {
			continue
		}
		if len(current.QuoteTimes) == 2 && m.QuoteTimes[0].Equal(current.QuoteTimes[0]) && m.QuoteTimes[1].Equal(current.QuoteTimes[1]) {
			// Re-running a scan against the same exchange quotes cannot make
			// those quotes part of their own historical reference distribution.
			continue
		}
		if m.QuoteTimes[0].IsZero() || m.QuoteTimes[1].IsZero() || m.QuoteTimes[0].After(m.ObservedAt) || m.QuoteTimes[1].After(m.ObservedAt) {
			continue
		}
		key := m.QuoteTimes[0].UTC().Format(time.RFC3339Nano) + "/" + m.QuoteTimes[1].UTC().Format(time.RFC3339Nano)
		if seen[key] {
			continue
		}
		seen[key] = true
		compatible = append(compatible, m)
	}
	if len(compatible) < p.MinHistory {
		return decimal.Zero, "", fmt.Errorf("history warmup: %d/%d independent observations", len(compatible), p.MinHistory)
	}
	sort.Slice(compatible, func(i, j int) bool { return compatible[i].ObservedAt.Before(compatible[j].ObservedAt) })
	if compatible[len(compatible)-1].ObservedAt.Sub(compatible[0].ObservedAt) < p.MinHistorySpan {
		return decimal.Zero, "", fmt.Errorf("history warmup: time span below %s", p.MinHistorySpan)
	}
	values := make([]decimal.Decimal, len(compatible))
	for i, m := range compatible {
		values[i] = m.Value
	}
	sort.Slice(values, func(i, j int) bool { return values[i].LessThan(values[j]) })
	target := values[len(values)/2]
	if len(values)%2 == 0 {
		target = target.Add(values[len(values)/2-1]).Div(decimal.NewFromInt(2))
	}
	return target, fmt.Sprintf("median of %d prior independent observations, %s through %s; %s", len(values), compatible[0].ObservedAt.UTC().Format(time.RFC3339), compatible[len(compatible)-1].ObservedAt.UTC().Format(time.RFC3339), current.ModelSource), nil
}

func calendarCandidate(s Snapshot, p Policy, near, far Instrument, m Metric, target decimal.Decimal, targetSource string, longFar bool) (Candidate, error) {
	nearMid, _ := mid(s, p, near)
	farMid, _ := mid(s, p, far)
	nearQty, farQty := p.FutureContracts, p.FutureContracts
	nearDV, farDV := decimal.Zero, decimal.Zero
	nearValue, farValue := nearMid.Mul(near.PriceFactor), farMid.Mul(far.PriceFactor)
	switch near.Family {
	case "KEYRATE":
		nearDV = near.PriceFactor.Div(decimal.NewFromInt(100))
		farDV = far.PriceFactor.Div(decimal.NewFromInt(100))
	case "RUONIA":
		nearDV = nearValue.Mul(years(near.Expiry.Sub(s.ObservedAt))).Div(bps)
		farDV = farValue.Mul(years(far.Expiry.Sub(s.ObservedAt))).Div(bps)
	case "RGBI":
		// Matching the same index's notionals does not require an external
		// duration estimate; the common duration factor cancels in the ratio.
		nearDV = nearValue
		farDV = farValue
	}
	if near.Family != "KEYRATE" {
		q, err := integerQuantity(farDV.Mul(decimal.NewFromInt(farQty)).Div(nearDV))
		if err != nil {
			return Candidate{}, err
		}
		nearQty = q
	}
	nearSide, farSide, direction := "sell", "buy", "long_far_short_near"
	if !longFar {
		nearSide, farSide, direction = "buy", "sell", "short_far_long_near"
	}
	l1, err := fill(s, p, near, nearSide, nearQty, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	l2, err := fill(s, p, far, farSide, farQty, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	l1.DV01 = nearDV.Mul(decimal.NewFromInt(nearQty))
	l2.DV01 = farDV.Mul(decimal.NewFromInt(farQty))
	residual := l1.DV01.Sub(l2.DV01).Abs()
	ratio := residual.Div(decimal.Max(l1.DV01, l2.DV01))
	if ratio.GreaterThan(p.MaxResidualDV01Ratio) {
		return Candidate{}, fmt.Errorf("integer hedge residual %s exceeds %s", ratio, p.MaxResidualDV01Ratio)
	}
	farTarget := nearMid.Add(target.Div(decimal.NewFromInt(100)))
	if near.Family != "KEYRATE" {
		var err error
		farTarget, err = expTarget(nearMid, target, years(far.Expiry.Sub(near.Expiry)))
		if err != nil {
			return Candidate{}, err
		}
	}
	e1, err := closeFill(s, p, l1, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	e2, err := closeFill(s, p, l2, farTarget.Sub(farMid))
	if err != nil {
		return Candidate{}, err
	}
	measure, known := "rate_dv01", true
	if near.Family == "RGBI" {
		measure = "index_notional"
		if validRGBIReference(s, p) == nil {
			factor := s.RGBIReference.ModifiedDuration.Div(bps)
			l1.DV01 = l1.DV01.Mul(factor)
			l2.DV01 = l2.DV01.Mul(factor)
			residual = residual.Mul(factor)
		} else {
			known = false
			l1.DV01 = decimal.Zero
			l2.DV01 = decimal.Zero
			residual = decimal.Zero
		}
	}
	c := Candidate{Key: m.Key, Strategy: m.Strategy, Direction: direction, Snapshot: s, Policy: p, Legs: []Leg{l1, l2}, Metric: m, Target: target, TargetSource: targetSource, ResidualDV01: residual, ResidualDV01Ratio: ratio, DV01Known: known, HedgeMeasure: measure, Warnings: []string{"Target is a historical-reference scenario, not mandatory convergence. Near-contract price and order-book shape are assumed unchanged while the far leg moves to target.", "Fees and full entry/exit spreads are included; unobserved carry and funding changes have a separate reserve."}}
	if near.Family == "RUONIA" {
		c.Warnings = append(c.Warnings, "Hedge quantities use each accumulated-index contract's remaining horizon and price sensitivity; they are not one-for-one.")
	}
	if near.Family == "RGBI" {
		c.Warnings = append(c.Warnings, "This is the futures funding/basis curve on one bond index, not a spread between two OFZ bond maturities. Absolute DV01 uses the recorded index-duration method.")
		if !known {
			c.Warnings = append(c.Warnings, "Absolute DV01 is unavailable without a current duration reference; the reported hedge ratio error is index-notional mismatch, not measured rate risk.")
		}
	}
	return priceCandidate(c, []Leg{e1, e2})
}

func bondCandidate(s Snapshot, p Policy, bond, hedge Instrument) (Candidate, Metric, error) {
	curve, spread, dirtyMid, err := fitObservedBond(s, p, bond)
	if err != nil {
		return Candidate{}, Metric{}, err
	}
	m := Metric{Key: OFZRGBI + ":" + bond.UID + ":" + hedge.UID, Strategy: OFZRGBI, Unit: "bps", ModelSource: "local_leave_one_out_continuous_zero_spread_act365_v1", InstrumentUIDs: []string{bond.UID, hedge.UID}, ObservedAt: s.ObservedAt, QuoteTimes: []time.Time{s.Books[bond.UID].Time, s.Books[hedge.UID].Time}, Value: spread.Mul(bps)}
	if err = validRGBIReference(s, p); err != nil {
		return Candidate{}, m, err
	}
	hedgeMid, err := mid(s, p, hedge)
	if err != nil {
		return Candidate{}, m, err
	}
	if err = checkSkew(s, p, bond.UID, hedge.UID); err != nil {
		return Candidate{}, m, err
	}
	bondDV, err := BondDV01(curve, bond.Bond.Cashflows, spread)
	if err != nil {
		return Candidate{}, m, err
	}
	bondDV = bondDV.Mul(decimal.NewFromInt(bond.Lot)).Mul(decimal.NewFromInt(p.BondLots))
	futureDV := hedgeMid.Mul(hedge.PriceFactor).Mul(s.RGBIReference.ModifiedDuration).Div(bps)
	hedgeQty, err := integerQuantity(bondDV.Div(futureDV))
	if err != nil {
		return Candidate{}, m, err
	}
	l1, err := fill(s, p, bond, "buy", p.BondLots, decimal.Zero)
	if err != nil {
		return Candidate{}, m, err
	}
	l2, err := fill(s, p, hedge, "sell", hedgeQty, decimal.Zero)
	if err != nil {
		return Candidate{}, m, err
	}
	l1.DV01 = bondDV
	l2.DV01 = futureDV.Mul(decimal.NewFromInt(hedgeQty))
	residual := l1.DV01.Sub(l2.DV01).Abs()
	ratio := residual.Div(l1.DV01)
	if ratio.GreaterThan(p.MaxResidualDV01Ratio) {
		return Candidate{}, m, fmt.Errorf("rounded OFZ/index hedge residual %s exceeds limit", ratio)
	}
	fair, err := CurvePrice(curve, bond.Bond.Cashflows, decimal.Zero)
	if err != nil {
		return Candidate{}, m, err
	}
	e1, err := closeFill(s, p, l1, fair.Sub(dirtyMid).Div(bond.PriceFactor))
	if err != nil {
		return Candidate{}, m, err
	}
	e2, err := closeFill(s, p, l2, decimal.Zero)
	if err != nil {
		return Candidate{}, m, err
	}
	c := Candidate{Key: m.Key, Strategy: OFZRGBI, Direction: "long_ofz_short_rgbi", Snapshot: s, Policy: p, Legs: []Leg{l1, l2}, Metric: m, Target: decimal.Zero, TargetSource: fmt.Sprintf("leave-one-out zero curve from peers %s; RMS=%s max=%s; candidate excluded", strings.Join(curve.Peers, ","), curve.RMSEPriceFraction, curve.MaxPriceErrorFraction), ResidualDV01: residual, ResidualDV01Ratio: ratio, DV01Known: true, HedgeMeasure: "rate_dv01", Warnings: []string{"Zero target means normalization to the fitted peer curve; no contract forces this bond residual to vanish.", "Target assumes the peer curve and hedge price unchanged. Coupon/rolldown and funding uncertainty are covered by a reserve, not counted as free expected profit.", "Cash cost and P&L use dirty bond prices. Bond commissions use clean value under documented broker regulation; live account preview must confirm it.", "A parallel-duration hedge leaves curve shape, convexity and index-composition risk."}}
	c.ReferenceUIDs = append([]string(nil), curve.Peers...)
	c, err = priceCandidate(c, []Leg{e1, e2})
	if err != nil {
		return Candidate{}, m, err
	}
	if s.RGBIReference.Method == "aggregate_proxy" {
		c.Warnings = append(c.Warnings, "Index DV01 is an aggregate proxy derived from dirty-weighted Macaulay duration/yield applied to a clean-price futures notional; it is not exact.")
		if !p.AllowDurationProxy {
			c.MinimumTariff = ""
			for i := range c.Scenarios {
				c.Scenarios[i].Qualifies = false
			}
			c.Warnings = append(c.Warnings, "Automatic paper opening blocked: AllowDurationProxy is false.")
		}
	}
	return c, m, nil
}

func fitObservedBond(s Snapshot, p Policy, bond Instrument) (BondCurve, decimal.Decimal, decimal.Decimal, error) {
	if bond.Bond == nil {
		return BondCurve{}, decimal.Zero, decimal.Zero, fmt.Errorf("bond cashflows missing")
	}
	if err := ValidateBondTerms(*bond.Bond, s.ObservedAt); err != nil {
		return BondCurve{}, decimal.Zero, decimal.Zero, err
	}
	quote, err := mid(s, p, bond)
	if err != nil {
		return BondCurve{}, decimal.Zero, decimal.Zero, err
	}
	dirty := quote.Mul(bond.PriceFactor).Add(bond.Bond.Accrued)
	var peers []BondObservation
	for _, other := range s.Instruments {
		if other.Kind != "bond" || other.Family != "OFZ" || other.UID == bond.UID || other.Bond == nil {
			continue
		}
		if ValidateBondTerms(*other.Bond, s.ObservedAt) != nil {
			continue
		}
		q, e := mid(s, p, other)
		if e != nil || checkSkew(s, p, bond.UID, other.UID) != nil {
			continue
		}
		peers = append(peers, BondObservation{ID: other.UID, Cashflows: other.Bond.Cashflows, DirtyPrice: q.Mul(other.PriceFactor).Add(other.Bond.Accrued), Maturity: other.Bond.Maturity})
	}
	curve, err := FitBondCurve(s.ObservedAt, bond.UID, bond.Bond.Maturity, peers, p.MinPeers)
	if err != nil {
		return curve, decimal.Zero, dirty, err
	}
	if curve.RMSEPriceFraction.GreaterThan(p.MaxCurveRMSE) || curve.MaxPriceErrorFraction.GreaterThan(p.MaxCurveError) {
		return curve, decimal.Zero, dirty, fmt.Errorf("peer zero-curve fit exceeds configured error gates")
	}
	spread, err := CurveSpread(curve, bond.Bond.Cashflows, dirty)
	return curve, spread, dirty, err
}

func priceCandidate(c Candidate, exits []Leg) (Candidate, error) {
	if len(c.Legs) < 1 || len(c.Legs) > 2 || len(exits) != len(c.Legs) || (len(c.Legs) == 1 && c.Strategy != CashOffer) {
		return c, fmt.Errorf("one verified cash-offer leg or two matched legs required")
	}
	horizon := c.Horizon
	if horizon <= 0 {
		horizon = c.Policy.MaxHolding
	}
	for _, leg := range c.Legs {
		if leg.Instrument.Kind == "future" && !leg.Instrument.Perpetual {
			remaining := leg.Instrument.LastTrade.Sub(c.Snapshot.ObservedAt) - c.Policy.ExpiryBuffer
			if remaining < horizon {
				horizon = remaining
			}
		}
	}
	if horizon <= 0 {
		return c, fmt.Errorf("insufficient time before exit buffer")
	}
	c.Horizon = horizon
	margin, cash, turnover := decimal.Zero, decimal.Zero, decimal.Zero
	for n, leg := range c.Legs {
		turnover = turnover.Add(leg.Value)
		if leg.Instrument.Kind == "bond" || leg.Instrument.Kind == "share" || leg.Instrument.Kind == "etf" {
			if leg.Side == "buy" {
				cash = cash.Add(leg.Value)
			} else {
				if !leg.Instrument.InitialMarginSell.IsPositive() {
					return c, fmt.Errorf("verified cash-short reserve is missing")
				}
				margin = margin.Add(leg.Instrument.InitialMarginSell.Mul(leg.Units))
			}
		} else {
			per := leg.Instrument.InitialMarginBuy
			if leg.Side == "sell" {
				per = leg.Instrument.InitialMarginSell
			}
			margin = margin.Add(per.Mul(decimal.NewFromInt(leg.Lots)))
		}
		c.ExpectedGross = c.ExpectedGross.Add(LegProfit(leg, exits[n]))
	}
	profiles := scanner.Profiles()
	base := scanner.Profiles()
	maxEntryFee := decimal.Zero
	for n := range profiles {
		profiles[n].FutureTurnover = c.Policy.FutureTurnover
		fees := decimal.Zero
		for _, leg := range c.Legs {
			fees = fees.Add(legFee(profiles[n], leg))
		}
		maxEntryFee = decimal.Max(maxEntryFee, fees)
	}
	c.CapitalRequired = cash.Add(margin.Mul(one.Add(c.Policy.MarginBufferRate))).Add(maxEntryFee)
	if !c.CapitalRequired.IsPositive() {
		return c, fmt.Errorf("nonpositive funded capital")
	}
	c.CashBenchmark = c.CapitalRequired.Mul(c.Policy.CashRate).Mul(years(horizon))
	c.CarryReserve = c.CapitalRequired.Mul(c.Policy.CarryReserveRate).Mul(years(horizon))
	c.ExecutionReserve = turnover.Mul(c.Policy.ExecutionBufferBPS).Div(bps)
	for n, profile := range profiles {
		var fees []scanner.FeeLine
		total := decimal.Zero
		for j, leg := range c.Legs {
			f := legFee(profile, leg)
			fees = append(fees, scanner.FeeLine{Name: fmt.Sprintf("entry_%d_%s", j, leg.Instrument.UID), Amount: f})
			total = total.Add(f)
		}
		for j, leg := range exits {
			f := legFee(base[n], leg)
			fees = append(fees, scanner.FeeLine{Name: fmt.Sprintf("exit_%d_%s", j, leg.Instrument.UID), Amount: f})
			total = total.Add(f)
		}
		for _, cost := range c.ExtraCosts[profile.Name] {
			if cost.Amount.IsNegative() {
				return c, fmt.Errorf("negative projected cash cost")
			}
			fees = append(fees, cost)
			total = total.Add(cost.Amount)
		}
		net := c.ExpectedGross.Add(c.ExpectedIncome).Sub(total).Sub(c.ExecutionReserve).Sub(c.CarryReserve)
		excess := net.Sub(c.CashBenchmark)
		deviation := c.Metric.Value.Sub(c.Target).Abs()
		qualifies := !c.CapitalRequired.GreaterThan(c.Policy.CapitalLimit) && !net.LessThan(c.Policy.MinProfit) && !excess.LessThan(c.Policy.MinExcessProfit) && !deviation.LessThan(c.Policy.MinDeviationBPS)
		c.Scenarios = append(c.Scenarios, scanner.Scenario{Profile: profile.Name, StockRate: profile.StockRate, FutureRate: profile.EffectiveFutureRate(), FeeSource: profile.Source, Fees: fees, TradingFees: total, NetProfit: net, ExcessProfit: excess, AnnualizedReturn: net.Div(c.CapitalRequired).Div(years(horizon)), Qualifies: qualifies})
		if qualifies && c.MinimumTariff == "" {
			c.MinimumTariff = profile.Name
		}
	}
	return c, nil
}

func legFee(p scanner.Profile, l Leg) decimal.Decimal {
	if l.Execution == "cash_offer" {
		return decimal.Zero
	}
	if l.Instrument.Kind == "bond" || l.Instrument.Kind == "share" || l.Instrument.Kind == "etf" {
		return p.StockFee(l.FeeValue)
	}
	return p.FutureFee(l.FeeValue, l.Lots, l.Instrument.ExchangeFee)
}

// LegProfit marks futures in raw quote units using the CURRENT conversion
// factor. Revaluing a historic USD futures notional at a new FX rate would
// manufacture profit even when its quoted price did not move.
func LegProfit(entry, exit Leg) decimal.Decimal {
	profit := exit.Value.Sub(entry.Value)
	if entry.Instrument.Kind == "future" {
		profit = exit.Price.Sub(entry.Price).Mul(exit.Instrument.PriceFactor).Mul(decimal.NewFromInt(entry.Instrument.Lot)).Mul(decimal.NewFromInt(entry.Lots))
	}
	if entry.Side == "sell" {
		profit = profit.Neg()
	}
	return profit
}

func mid(s Snapshot, p Policy, i Instrument) (decimal.Decimal, error) {
	b, err := validatedBook(s, p, i)
	if err != nil {
		return decimal.Zero, err
	}
	if len(b.Bids) == 0 || len(b.Asks) == 0 {
		return decimal.Zero, fmt.Errorf("two-sided quotes required for %s metric", i.Ticker)
	}
	if !b.Bids[0].Price.IsPositive() || !b.Asks[0].Price.IsPositive() || b.Bids[0].Price.GreaterThan(b.Asks[0].Price) {
		return decimal.Zero, fmt.Errorf("invalid/crossed book for %s", i.Ticker)
	}
	return b.Bids[0].Price.Add(b.Asks[0].Price).Div(decimal.NewFromInt(2)), nil
}

func validatedBook(s Snapshot, p Policy, i Instrument) (scanner.Book, error) {
	if err := validInstrument(i, s.ObservedAt); err != nil {
		return scanner.Book{}, err
	}
	if strings.EqualFold(i.PriceCurrency, "usd") && (!i.PriceFactorKnown || i.PriceFactorAt.IsZero() || i.PriceFactorAt.After(s.ObservedAt) || s.ObservedAt.Sub(i.PriceFactorAt) > p.MaxQuoteAge || i.PriceFactorSource == "") {
		return scanner.Book{}, fmt.Errorf("current verified USD quote conversion is required for %s", i.Ticker)
	}
	b, ok := s.Books[i.UID]
	if !ok || b.UID != i.UID || !b.Consistent || !b.TradeAvailable || b.Time.IsZero() || b.Time.After(s.ObservedAt) || s.ObservedAt.Sub(b.Time) > p.MaxQuoteAge {
		return scanner.Book{}, fmt.Errorf("missing, stale, inconsistent or unavailable book for %s", i.Ticker)
	}
	if len(b.Bids) > 0 && len(b.Asks) > 0 && b.Bids[0].Price.GreaterThan(b.Asks[0].Price) {
		return scanner.Book{}, fmt.Errorf("crossed book for %s", i.Ticker)
	}
	return b, nil
}

func validInstrument(i Instrument, now time.Time) error {
	if i.UID == "" || !strings.EqualFold(i.Currency, "rub") || !i.APIAvailable || i.Lot <= 0 || !i.PriceFactor.IsPositive() {
		return fmt.Errorf("invalid RUB instrument metadata for %s", i.Ticker)
	}
	if i.Kind == "bond" {
		if i.Family != "OFZ" || i.Bond == nil {
			return fmt.Errorf("only conventional OFZ supported")
		}
		return ValidateBondTerms(*i.Bond, now)
	}
	if i.Kind == "share" || i.Kind == "etf" {
		return nil
	}
	if i.Kind != "future" || i.Lot != 1 || (!i.Perpetual && (i.Expiry.IsZero() || i.LastTrade.IsZero() || !i.LastTrade.After(now) || i.LastTrade.After(i.Expiry))) || !i.InitialMarginBuy.IsPositive() || !i.InitialMarginSell.IsPositive() || !i.ExchangeFeeKnown || i.ExchangeFee.IsNegative() || i.ExchangeFeeSource == "" {
		return fmt.Errorf("unverified dated future metadata for %s", i.Ticker)
	}
	expected := decimal.Zero
	switch i.Family {
	case "KEYRATE":
		expected = decimal.NewFromInt(1000)
	case "RUONIA":
		expected = decimal.NewFromInt(10000)
	case "RGBI":
		expected = one
	default:
		if !relativeFutureFamily(i.Family) || !i.UnitSize.IsPositive() || !i.PriceFactorKnown || i.PriceFactorSource == "" || i.PriceFactorAt.IsZero() || i.PriceFactorAt.After(now) || i.SettlementReference == "" {
			return fmt.Errorf("unsupported or unverified relative-value future family")
		}
		return nil
	}
	if !i.PriceFactor.Equal(expected) {
		return fmt.Errorf("unexpected %s contract quote multiplier", i.Family)
	}
	return nil
}

func fill(s Snapshot, p Policy, i Instrument, side string, lots int64, shift decimal.Decimal) (Leg, error) {
	book, err := validatedBook(s, p, i)
	if err != nil {
		return Leg{}, err
	}
	if lots <= 0 || (side != "buy" && side != "sell") || side == "buy" && !i.BuyAvailable || side == "sell" && !i.SellAvailable || side == "sell" && (i.Kind == "future" || i.Kind == "share" || i.Kind == "etf") && !i.ShortEnabled {
		return Leg{}, fmt.Errorf("unavailable %s direction or quantity for %s", side, i.Ticker)
	}
	levels := book.Asks
	if side == "sell" {
		levels = book.Bids
	}
	remaining := lots
	raw := decimal.Zero
	limit := decimal.Zero
	for n, level := range levels {
		price := level.Price.Add(shift)
		if level.Lots <= 0 || !price.IsPositive() {
			return Leg{}, fmt.Errorf("invalid executable level for %s", i.Ticker)
		}
		if n > 0 && ((side == "buy" && level.Price.LessThan(levels[n-1].Price)) || (side == "sell" && level.Price.GreaterThan(levels[n-1].Price))) {
			return Leg{}, fmt.Errorf("unordered depth")
		}
		take := level.Lots
		if take > remaining {
			take = remaining
		}
		raw = raw.Add(price.Mul(decimal.NewFromInt(take)))
		limit = price
		remaining -= take
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		return Leg{}, fmt.Errorf("insufficient %s depth for %s", side, i.Ticker)
	}
	units := decimal.NewFromInt(lots).Mul(decimal.NewFromInt(i.Lot))
	clean := raw.Mul(i.PriceFactor).Mul(decimal.NewFromInt(i.Lot))
	value := clean
	if i.Kind == "bond" {
		value = value.Add(i.Bond.Accrued.Mul(units))
	}
	return Leg{Instrument: i, Side: side, Lots: lots, Units: units, Price: raw.Div(decimal.NewFromInt(lots)), Limit: limit, Value: value, FeeValue: clean, Execution: "exchange"}, nil
}

// Selling an owned futures long is a reducing trade, not an opening short.
func closeFill(s Snapshot, p Policy, entry Leg, shift decimal.Decimal) (Leg, error) {
	i := entry.Instrument
	if entry.Side == "buy" && (i.Kind == "future" || i.Kind == "share" || i.Kind == "etf") {
		i.ShortEnabled = true
	}
	l, err := fill(s, p, i, opposite(entry.Side), entry.Lots, shift)
	l.Instrument = entry.Instrument
	return l, err
}

func checkSkew(s Snapshot, p Policy, a, b string) error {
	d := s.Books[a].Time.Sub(s.Books[b].Time)
	if d < 0 {
		d = -d
	}
	if d > p.MaxQuoteSkew {
		return fmt.Errorf("quote timestamps exceed permitted skew")
	}
	return nil
}
func validRGBIReference(s Snapshot, p Policy) error {
	r := s.RGBIReference
	if !r.Known || !r.ModifiedDuration.IsPositive() || r.Source == "" || r.Method == "" || r.AsOf.IsZero() || r.AsOf.After(s.ObservedAt) || s.ObservedAt.Sub(r.AsOf) > p.MaxReferenceAge {
		return fmt.Errorf("current sourced RGBI duration reference is required")
	}
	return nil
}
func opposite(side string) string {
	if side == "buy" {
		return "sell"
	}
	return "buy"
}
func years(d time.Duration) decimal.Decimal {
	return decimal.NewFromInt(d.Nanoseconds()).Div(decimal.NewFromInt(int64(365 * 24 * time.Hour)))
}
func integerQuantity(q decimal.Decimal) (int64, error) {
	rounded := q.Round(0)
	if !rounded.IsPositive() || rounded.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, fmt.Errorf("integer hedge cannot be sized at this scale")
	}
	return rounded.IntPart(), nil
}
func logRatioBPS(a, b, t decimal.Decimal) (decimal.Decimal, error) {
	af, _ := a.Float64()
	bf, _ := b.Float64()
	tf, _ := t.Float64()
	if af <= 0 || bf <= 0 || tf <= 0 {
		return decimal.Zero, fmt.Errorf("invalid forward metric inputs")
	}
	v := math.Log(af/bf) / tf * 10000
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return decimal.Zero, fmt.Errorf("nonfinite forward metric")
	}
	return decimal.NewFromFloat(v), nil
}
func expTarget(near, target, t decimal.Decimal) (decimal.Decimal, error) {
	nf, _ := near.Float64()
	yf, _ := target.Div(bps).Mul(t).Float64()
	v := nf * math.Exp(yf)
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return decimal.Zero, fmt.Errorf("invalid target price")
	}
	return decimal.NewFromFloat(v), nil
}
