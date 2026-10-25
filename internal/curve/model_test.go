package curve

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func modelDecimal(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func modelPolicy() Policy {
	p := DefaultPolicy()
	p.FutureContracts = 10
	p.MinHistory = 2
	p.MinHistorySpan = time.Hour
	p.MinProfit = decimal.Zero
	p.MinExcessProfit = decimal.Zero
	p.CashRate = decimal.Zero
	p.ExecutionBufferBPS = decimal.Zero
	p.CarryReserveRate = decimal.Zero
	p.MinDeviationBPS = one
	return p
}

func modelCalendar(family string) Snapshot {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	factor, near, far := modelDecimal("1000"), modelDecimal("10"), modelDecimal("11")
	if family == "RUONIA" {
		factor, near, far = modelDecimal("10000"), modelDecimal("4"), modelDecimal("4.2")
	}
	if family == "RGBI" {
		factor, near, far = one, modelDecimal("10000"), modelDecimal("10200")
	}
	s := Snapshot{ObservedAt: now, Books: map[string]scanner.Book{}}
	for n, price := range []decimal.Decimal{near, far} {
		uid := fmt.Sprintf("%s-%d", family, n)
		i := Instrument{UID: uid, Ticker: uid, Kind: "future", Family: family, Currency: "rub", Lot: 1, PriceFactor: factor, Expiry: now.Add(time.Duration(60+n*30) * 24 * time.Hour), LastTrade: now.Add(time.Duration(59+n*30) * 24 * time.Hour), APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginBuy: modelDecimal("1000"), InitialMarginSell: modelDecimal("1000"), ExchangeFee: one, ExchangeFeeKnown: true, ExchangeFeeSource: "synthetic test schedule"}
		s.Instruments = append(s.Instruments, i)
		spread := modelDecimal("0.01")
		if family == "RUONIA" {
			spread = modelDecimal("0.0001")
		}
		if family == "RGBI" {
			spread = one
		}
		s.Books[uid] = scanner.Book{UID: uid, Time: now.Add(-time.Second), Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: price.Sub(spread), Lots: 1000}}, Asks: []scanner.Level{{Price: price.Add(spread), Lots: 1000}}}
	}
	return s
}

func modelHistory(m Metric, target decimal.Decimal) []Metric {
	var result []Metric
	for _, hours := range []int{2, 1} {
		past := m
		past.ObservedAt = m.ObservedAt.Add(-time.Duration(hours) * time.Hour)
		past.QuoteTimes = []time.Time{past.ObservedAt.Add(-time.Second), past.ObservedAt.Add(-time.Second)}
		past.Value = target
		result = append(result, past)
	}
	return result
}

func firstMetric(t *testing.T, s Snapshot, p Policy) Metric {
	t.Helper()
	out, err := Evaluate(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Metrics) == 0 {
		t.Fatalf("no metric: %v", out.Skips)
	}
	return out.Metrics[0]
}

func modelFindCandidate(t *testing.T, out Scan, strategy, direction string) Candidate {
	t.Helper()
	for _, c := range out.Candidates {
		if c.Strategy == strategy && c.Direction == direction {
			return c
		}
	}
	t.Fatalf("candidate missing: %s %s; skips=%v", strategy, direction, out.Skips)
	return Candidate{}
}

func TestCalendarRequiresPriorIndependentHistoryAndUsesNonzeroMedian(t *testing.T) {
	s, p := modelCalendar("KEYRATE"), modelPolicy()
	first, err := Evaluate(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Metrics) != 1 || len(first.Candidates) != 0 || !strings.Contains(strings.Join(first.Skips, " "), "warmup") {
		t.Fatal("warmup should retain metric but create no candidates")
	}
	m := first.Metrics[0]
	if !m.Value.Equal(modelDecimal("100")) {
		t.Fatal("KEYRATE direct pp-to-bps conversion", m.Value)
	}
	history := modelHistory(m, modelDecimal("50"))
	out, err := Evaluate(s, p, history)
	if err != nil {
		t.Fatal(err)
	}
	short := modelFindCandidate(t, out, KEYRATECalendar, "short_far_long_near")
	if !short.Target.Equal(modelDecimal("50")) || short.MinimumTariff == "" || !short.ExpectedGross.IsPositive() {
		t.Fatalf("nonzero median target or eligible direction wrong: %+v", short)
	}
	long := modelFindCandidate(t, out, KEYRATECalendar, "long_far_short_near")
	if long.MinimumTariff != "" || !long.ExpectedGross.IsNegative() {
		t.Fatal("wrong direction must lose under the same target scenario")
	}
	if short.Legs[0].Lots != short.Legs[1].Lots || !short.ResidualDV01.IsZero() {
		t.Fatal("KEYRATE hedge must match equal contract sensitivity")
	}
	if len(short.Scenarios) != 3 || len(short.Scenarios[0].Fees) != 4 {
		t.Fatal("expected all three tariffs and four round-trip fees")
	}
	// Repeated copies of the same exchange quote are not independent history.
	history[1].QuoteTimes = append([]time.Time(nil), history[0].QuoteTimes...)
	out, _ = Evaluate(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("repeated quote counted as new historical information")
	}
	history = modelHistory(m, decimal.Zero)
	history[0].ObservedAt = s.ObservedAt
	out, _ = Evaluate(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("current observation leaked into its own baseline")
	}
	history = modelHistory(m, decimal.Zero)
	history[0].ModelSource = "other definition"
	out, _ = Evaluate(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("incompatible historical metric definition accepted")
	}
	history = modelHistory(m, decimal.Zero)
	history[1].ObservedAt = m.ObservedAt.Add(-500 * time.Millisecond)
	history[1].QuoteTimes = append([]time.Time(nil), m.QuoteTimes...)
	out, _ = Evaluate(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("manual rerun reused current quotes in their own historical baseline")
	}
}

func TestRUONIAUsesAccumulatedIndexForwardMetricAndHorizonHedge(t *testing.T) {
	s, p := modelCalendar("RUONIA"), modelPolicy()
	m := firstMetric(t, s, p)
	want, _ := logRatioBPS(modelDecimal("4.2"), modelDecimal("4"), years(30*24*time.Hour))
	if !m.Value.Equal(want) {
		t.Fatal("wrong RUONIA log-forward units")
	}
	out, err := Evaluate(s, p, modelHistory(m, m.Value.Sub(modelDecimal("100"))))
	if err != nil {
		t.Fatal(err)
	}
	c := modelFindCandidate(t, out, RUONIACalendar, "short_far_long_near")
	if c.Legs[0].Lots != 16 || c.Legs[1].Lots != 10 {
		t.Fatalf("wanted horizon-weighted16:10 hedge, got %d:%d", c.Legs[0].Lots, c.Legs[1].Lots)
	}
	if c.ResidualDV01Ratio.GreaterThan(p.MaxResidualDV01Ratio) || !c.DV01Known {
		t.Fatal("invalid RUONIA hedge risk audit")
	}
	s.Instruments[0].PriceFactor = one
	out, _ = Evaluate(s, p, modelHistory(m, decimal.Zero))
	if len(out.Candidates) != 0 {
		t.Fatal("incorrect accumulated-index multiplier accepted")
	}
}

func TestRGBICalendarWorksWithoutDurationReferenceAndMatchesNotional(t *testing.T) {
	s, p := modelCalendar("RGBI"), modelPolicy()
	m := firstMetric(t, s, p)
	out, err := Evaluate(s, p, modelHistory(m, m.Value.Sub(modelDecimal("100"))))
	if err != nil {
		t.Fatal(err)
	}
	c := modelFindCandidate(t, out, RGBICalendar, "short_far_long_near")
	if c.DV01Known || c.HedgeMeasure != "index_notional" || c.ResidualDV01Ratio.GreaterThan(p.MaxResidualDV01Ratio) {
		t.Fatal("missing reference should leave notional hedge auditable, absolute DV01 unknown")
	}
	if !strings.Contains(strings.Join(c.Warnings, " "), "not a spread between two OFZ") {
		t.Fatal("funding basis must not be described as bond-tenor slope")
	}
}

func modelBondUniverse(t *testing.T) Snapshot {
	t.Helper()
	known := bondTestCurve()
	s := Snapshot{ObservedAt: known.AsOf, Books: map[string]scanner.Book{}, RGBIReference: RGBIReference{ModifiedDuration: modelDecimal("5"), AsOf: known.AsOf, Source: "synthetic exact test", Method: "synthetic_test_duration", Known: true}}
	for _, maturity := range []int{2, 3, 4, 5, 6, 7, 8} {
		id := fmt.Sprintf("OFZ-%d", maturity)
		flows := bondTestFlows(known.AsOf, maturity, modelDecimal("80"), modelDecimal("1000"))
		fair, err := CurvePrice(known, flows, decimal.Zero)
		if err != nil {
			t.Fatal(err)
		}
		mid := fair.Sub(modelDecimal("20"))
		if maturity == 5 {
			mid = mid.Sub(modelDecimal("30"))
		}
		i := Instrument{UID: id, Ticker: id, ClassCode: "TQOB", Kind: "bond", Family: "OFZ", Currency: "rub", Lot: 1, PriceFactor: modelDecimal("10"), APIAvailable: true, BuyAvailable: true, SellAvailable: true, Bond: &BondTerms{Nominal: modelDecimal("1000"), Accrued: modelDecimal("20"), AccruedAt: known.AsOf, Maturity: known.AsOf.AddDate(maturity, 0, 0), FixedCoupon: true, Cashflows: flows, Source: "synthetic cashflows"}}
		s.Instruments = append(s.Instruments, i)
		q := mid.Div(i.PriceFactor)
		s.Books[id] = scanner.Book{UID: id, Time: known.AsOf, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: q.Sub(modelDecimal("0.001")), Lots: 10000}}, Asks: []scanner.Level{{Price: q.Add(modelDecimal("0.001")), Lots: 10000}}}
	}
	f := Instrument{UID: "RB", Ticker: "RB", Kind: "future", Family: "RGBI", Currency: "rub", Lot: 1, PriceFactor: one, Expiry: known.AsOf.Add(90 * 24 * time.Hour), LastTrade: known.AsOf.Add(89 * 24 * time.Hour), APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginBuy: modelDecimal("1000"), InitialMarginSell: modelDecimal("1000"), ExchangeFee: one, ExchangeFeeKnown: true, ExchangeFeeSource: "synthetic"}
	s.Instruments = append(s.Instruments, f)
	s.Books[f.UID] = scanner.Book{UID: f.UID, Time: known.AsOf, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: modelDecimal("9999"), Lots: 1000}}, Asks: []scanner.Level{{Price: modelDecimal("10001"), Lots: 1000}}}
	return s
}

func TestOFZLeaveOneOutDirtyCapitalCleanFeesAndProxyGate(t *testing.T) {
	s, p := modelBondUniverse(t), modelPolicy()
	p.BondLots = 1000
	p.CapitalLimit = modelDecimal("10000000")
	out, err := Evaluate(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	var candidate Candidate
	found := false
	for _, c := range out.Candidates {
		if c.Strategy == OFZRGBI && c.Legs[0].Instrument.UID == "OFZ-5" {
			candidate = c
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("OFZ candidate missing: %v", out.Skips)
	}
	if candidate.MinimumTariff == "" || !candidate.Metric.Value.IsPositive() || !candidate.Target.IsZero() {
		t.Fatal("cheap OFZ target scenario not eligible")
	}
	for _, uid := range candidate.ReferenceUIDs {
		if uid == "OFZ-5" {
			t.Fatal("candidate included in its own reference curve")
		}
	}
	if len(candidate.ReferenceUIDs) < 4 {
		t.Fatal("missing audit peer identities")
	}
	if !candidate.Legs[0].Value.Sub(candidate.Legs[0].FeeValue).Equal(modelDecimal("20000")) {
		t.Fatal("dirty funding and clean commission bases were not separated")
	}
	wantFee := scanner.Profiles()[0].StockFee(candidate.Legs[0].FeeValue)
	if !candidate.Scenarios[0].Fees[0].Amount.Equal(wantFee) {
		t.Fatal("bond commission included accrued interest")
	}
	s.RGBIReference.Method = "aggregate_proxy"
	out, err = Evaluate(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Candidates {
		if c.Strategy == OFZRGBI && c.MinimumTariff != "" {
			t.Fatal("proxy OFZ entry enabled without explicit opt-in")
		}
	}
	p.AllowDurationProxy = true
	out, _ = Evaluate(s, p, nil)
	found = false
	for _, c := range out.Candidates {
		if c.Strategy == OFZRGBI && c.Legs[0].Instrument.UID == "OFZ-5" && c.MinimumTariff != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit proxy opt-in did not permit an otherwise eligible candidate")
	}
}

func TestModelQuoteAndPolicyGates(t *testing.T) {
	s, p := modelCalendar("KEYRATE"), modelPolicy()
	m := firstMetric(t, s, p)
	history := modelHistory(m, decimal.Zero)
	bad := s.Books[s.Instruments[0].UID]
	bad.Time = s.ObservedAt.Add(time.Second)
	s.Books[bad.UID] = bad
	out, _ := Evaluate(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("future-dated order book accepted")
	}
	for _, mutate := range []func(*Policy){func(p *Policy) { p.MaxQuoteAge = 301 * time.Second }, func(p *Policy) { p.MaxQuoteSkew = p.MaxQuoteAge + time.Second }, func(p *Policy) { p.MinHistory = 1001 }} {
		q := p
		mutate(&q)
		if ValidatePolicy(q) == nil {
			t.Fatal("unsafe policy accepted")
		}
	}
}
