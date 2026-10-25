package curve

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func derivativeTestSnapshot(at time.Time, anomaly bool) Snapshot {
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	expiry, lastTrade := base.Add(90*24*time.Hour), base.Add(89*24*time.Hour)
	s := Snapshot{ObservedAt: at, Books: map[string]scanner.Book{}}
	type spec struct {
		uid, family, underlying, unit, factor, price, spread string
		perpetual                                            bool
	}
	items := []spec{
		{"MX", "MX", "index-mx", "100", "1", "20000", "1", false},
		{"MM", "MM", "index-mx", "10", "10", "200", "0.01", false},
		{"SR", "SR", "ordinary-share", "100", "1", "10000", "1", false},
		{"SP", "SP", "preferred-share", "100", "1", "10000", "1", false},
		{"GLDRUBF", "GLDRUBF", "gold-gram", "1", "1", "10000", "1", true},
		{"GL", "GL", "gold-gram", "1", "1", "10000", "1", false},
	}
	for _, item := range items {
		price := modelDecimal(item.price)
		if anomaly {
			switch item.uid {
			case "MX":
				price = price.Add(modelDecimal("500"))
			case "SR":
				price = price.Add(modelDecimal("1000"))
			case "GL":
				price = price.Add(modelDecimal("400"))
			}
		}
		i := Instrument{UID: item.uid, Ticker: item.uid, Kind: "future", Family: item.family, Currency: "rub", Lot: 1, PriceFactor: modelDecimal(item.factor), UnitSize: modelDecimal(item.unit), PriceCurrency: "rub", PriceFactorKnown: true, PriceFactorAt: at, PriceFactorSource: "synthetic conversion, not market data", UnderlyingUID: item.underlying, SettlementReference: "synthetic fixing " + item.underlying, SettlementType: "cash", Perpetual: item.perpetual, Expiry: expiry, LastTrade: lastTrade, APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginBuy: modelDecimal("1000"), InitialMarginSell: modelDecimal("1000"), ExchangeFee: one, ExchangeFeeKnown: true, ExchangeFeeSource: "synthetic floor, not market data"}
		if i.Perpetual {
			i.Expiry, i.LastTrade = time.Time{}, time.Time{}
		}
		s.Instruments = append(s.Instruments, i)
		spread := modelDecimal(item.spread)
		s.Books[i.UID] = scanner.Book{UID: i.UID, Time: at.Add(-time.Second), Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: price.Sub(spread), Lots: 1000}}, Asks: []scanner.Level{{Price: price.Add(spread), Lots: 1000}}}
	}
	for _, item := range []struct{ id, strategy, primary, hedge, ratio, primaryScale, hedgeScale string }{
		{"fixture-mini", StandardMini, "MX", "MM", "10", "0.01", "1"},
		{"fixture-classes", ShareClass, "SR", "SP", "1", "0.01", "0.01"},
		{"fixture-perpetual", PerpetualDated, "GLDRUBF", "GL", "1", "1", "1"},
	} {
		r := Relationship{ID: item.id, Strategy: item.strategy, PrimarySelector: item.primary, HedgeSelector: item.hedge, PrimaryUID: item.primary, HedgeUID: item.hedge, PrimaryLots: 1, HedgeLotsPerPrimaryLot: modelDecimal(item.ratio), PrimaryPriceScale: modelDecimal(item.primaryScale), HedgePriceScale: modelDecimal(item.hedgeScale), Verified: true, Source: "synthetic fixture relationship, not market data", AsOf: base, ValidUntil: lastTrade}
		if r.Strategy == PerpetualDated {
			r.Funding = &FundingReference{InstrumentUID: item.primary, Known: true, Source: "synthetic complete exact-time funding ledger, not exchange history", CoverageStart: base, CoverageEnd: at}
			payment := base.Add(42 * time.Hour) // Twelve hours after the anomaly entry.
			if !at.Before(payment) {
				r.Funding.Events = []FundingEvent{{At: payment, AmountPerLongContract: modelDecimal("-5"), Reference: "synthetic-exact-payment-1"}}
			}
		}
		s.Relationships = append(s.Relationships, r)
	}
	return s
}

func derivativeSingle(s Snapshot, strategy string) Snapshot {
	for _, r := range s.Relationships {
		if r.Strategy == strategy {
			s.Relationships = []Relationship{r}
			return s
		}
	}
	panic("fixture relationship missing")
}

func derivativeNow() time.Time { return time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC) }

func derivativeFixtureSnapshots() []Snapshot {
	base := derivativeNow().Add(-30 * time.Hour)
	var snapshots []Snapshot
	for n := 0; n < 30; n++ {
		snapshots = append(snapshots, derivativeTestSnapshot(base.Add(time.Duration(n)*time.Hour), false))
	}
	return append(snapshots, derivativeTestSnapshot(derivativeNow(), true), derivativeTestSnapshot(derivativeNow().Add(24*time.Hour), false))
}

func TestDerivativeChronologicalFixtureWarmupEntryAndExit(t *testing.T) {
	p := DefaultPolicy()
	var history []Metric
	var positions []Position
	snapshots := derivativeFixtureSnapshots()
	for n, s := range snapshots {
		out, err := EvaluateRelationships(s, p, history)
		if err != nil {
			t.Fatal(err)
		}
		if n < 30 {
			for _, c := range out.Candidates {
				if c.MinimumTariff != "" {
					t.Fatal("normal baseline unexpectedly opened a position")
				}
			}
		}
		if n == 30 {
			for _, c := range out.Candidates {
				if c.MinimumTariff == "" {
					continue
				}
				pos, err := NewPosition(c, "synthetic fixture")
				if err != nil {
					t.Fatal(err)
				}
				positions = append(positions, pos)
			}
			if len(positions) != 3 {
				t.Fatalf("default-policy fixture should open all three derivative strategies, got%d: %v", len(positions), out.Skips)
			}
		}
		history = append(history, out.Metrics...)
	}
	for _, pos := range positions {
		v, err := MarkPosition(pos, snapshots[len(snapshots)-1])
		if err != nil {
			t.Fatal(err)
		}
		if !v.ShouldClose || len(v.Scenarios) != 3 {
			t.Fatalf("fixture should close %s with three tariff scenarios: %s %v", pos.Entry.Strategy, v.CloseReason, v.Warnings)
		}
	}
}

func derivativeHistory(t *testing.T, s Snapshot, p Policy, strategy string, target decimal.Decimal) []Metric {
	t.Helper()
	out, err := EvaluateRelationships(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Metrics {
		if m.Strategy == strategy {
			return modelHistory(m, target)
		}
	}
	t.Fatalf("missing %s metric: %v", strategy, out.Skips)
	return nil
}

func TestStandardMiniNormalizationExactUnitsAndRoundTripCosts(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), StandardMini), modelPolicy()
	out, err := EvaluateRelationships(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := modelFindCandidate(t, out, StandardMini, "short_primary_long_hedge")
	if c.Legs[0].Lots != 1 || c.Legs[1].Lots != 10 || !c.Target.IsZero() || !c.ExpectedGross.Equal(modelDecimal("496")) {
		t.Fatalf("wrong normalized contract pairing or executable scenario: lots=%d/%d, gross=%s", c.Legs[0].Lots, c.Legs[1].Lots, c.ExpectedGross)
	}
	if c.MinimumTariff != "investor" || len(c.Scenarios) != 3 || len(c.Scenarios[0].Fees) != 4 {
		t.Fatal("missing tariff or round-trip fee scenarios")
	}
	for _, scenario := range c.Scenarios {
		if !scenario.NetProfit.Equal(c.ExpectedGross.Sub(scenario.TradingFees)) {
			t.Fatal("all four fees must reduce target profit")
		}
	}
	opposite := modelFindCandidate(t, out, StandardMini, "long_primary_short_hedge")
	if opposite.MinimumTariff != "" || !opposite.ExpectedGross.IsNegative() {
		t.Fatal("wrong spread direction became eligible")
	}
	baseline, _ := EvaluateRelationships(derivativeSingle(derivativeTestSnapshot(derivativeNow(), false), StandardMini), p, nil)
	if len(baseline.Metrics) != 1 || !baseline.Metrics[0].Value.IsZero() {
		t.Fatal("different quote scales created fictitious basis")
	}
}

func TestStandardMiniRejectsUnprovenFixingExpiryRatioAndDepth(t *testing.T) {
	for name, mutate := range map[string]func(*Snapshot){
		"fixing":        func(s *Snapshot) { s.Instruments[1].SettlementReference = "different fixing" },
		"expiry":        func(s *Snapshot) { s.Instruments[1].Expiry = s.Instruments[1].Expiry.Add(time.Hour) },
		"underlying":    func(s *Snapshot) { s.Instruments[1].UnderlyingUID = "different" },
		"ratio":         func(s *Snapshot) { s.Relationships[0].HedgeLotsPerPrimaryLot = one },
		"normalization": func(s *Snapshot) { s.Relationships[0].PrimaryPriceScale = one },
		"depth":         func(s *Snapshot) { b := s.Books["MM"]; b.Bids[0].Lots, b.Asks[0].Lots = 9, 9; s.Books["MM"] = b },
		"terms":         func(s *Snapshot) { s.Relationships[0].ValidUntil = s.ObservedAt },
	} {
		t.Run(name, func(t *testing.T) {
			s := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), StandardMini)
			mutate(&s)
			out, err := EvaluateRelationships(s, modelPolicy(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Candidates) != 0 {
				t.Fatalf("accepted invalid %s: %+v", name, out.Candidates)
			}
		})
	}
}

func TestShareClassUsesNonzeroPriorMedianAndTatneftContractRatio(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), ShareClass), modelPolicy()
	out, err := EvaluateRelationships(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Metrics) != 1 || len(out.Candidates) != 0 || !strings.Contains(strings.Join(out.Skips, " "), "warmup") {
		t.Fatal("share class must warm up rather than force parity")
	}
	history := modelHistory(out.Metrics[0], modelDecimal("200"))
	out, _ = EvaluateRelationships(s, p, history)
	c := modelFindCandidate(t, out, ShareClass, "short_primary_long_hedge")
	if !c.Target.Equal(modelDecimal("200")) || c.MinimumTariff == "" {
		t.Fatal("nonzero historical target lost")
	}
	// TT exposes100 ordinary shares; TP exposes10 preferred shares. Both
	// quotes normalize to a RUB share price, with ten TP contracts per TT.
	s.Instruments[2].Family, s.Instruments[3].Family = "TT", "TP"
	s.Instruments[3].UnitSize = modelDecimal("10")
	s.Relationships[0].HedgeLotsPerPrimaryLot = modelDecimal("10")
	s.Relationships[0].HedgePriceScale = modelDecimal("0.1")
	b := s.Books["SP"]
	b.Bids[0].Price, b.Asks[0].Price = modelDecimal("999.9"), modelDecimal("1000.1")
	s.Books["SP"] = b
	history = derivativeHistory(t, s, p, ShareClass, modelDecimal("200"))
	out, _ = EvaluateRelationships(s, p, history)
	c = modelFindCandidate(t, out, ShareClass, "short_primary_long_hedge")
	if c.Legs[1].Lots != 10 {
		t.Fatal("Tatneft hedge silently used1:1 contracts")
	}
	history[0].ModelSource = "old contract ratio"
	out, _ = EvaluateRelationships(s, p, history)
	if len(out.Candidates) != 0 {
		t.Fatal("changed normalization reused incompatible baseline")
	}
}

func TestPerpetualBasisAndVerifiedFundingDoNotExtrapolate(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), PerpetualDated), modelPolicy()
	out, err := EvaluateRelationships(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := modelFindCandidate(t, out, PerpetualDated, "long_primary_short_hedge")
	want, _ := logRatioBPS(modelDecimal("10400"), modelDecimal("10000"), years(s.Instruments[5].Expiry.Sub(s.ObservedAt)))
	if !c.Metric.Value.Equal(want) || !c.ExpectedGross.Equal(modelDecimal("396")) || !c.ExpectedIncome.IsZero() {
		t.Fatal("perpetual basis or projected future funding is wrong")
	}
	pos, err := NewPosition(c, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	mark := derivativeSingle(derivativeTestSnapshot(s.ObservedAt.Add(24*time.Hour), false), PerpetualDated)
	income, warnings, err := FundingPositionAdjustments(pos, mark)
	if err != nil || !income.Equal(modelDecimal("-5")) || len(warnings) != 0 {
		t.Fatalf("exact signed funding ledger: income=%s warnings=%v err=%v", income, warnings, err)
	}
	mark.Relationships[0].Funding.Known = false
	if _, _, err = FundingPositionAdjustments(pos, mark); err == nil {
		t.Fatal("unknown funding silently treated as zero")
	}
}

func TestDateOnlyFundingBooksOnlyCompleteHeldMoscowDays(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), PerpetualDated), modelPolicy()
	out, _ := EvaluateRelationships(s, p, nil)
	c := modelFindCandidate(t, out, PerpetualDated, "long_primary_short_hedge")
	pos, err := NewPosition(c, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	mark := derivativeSingle(derivativeTestSnapshot(s.ObservedAt.Add(48*time.Hour), false), PerpetualDated)
	msk := time.FixedZone("MSK", 3*3600)
	f := mark.Relationships[0].Funding
	f.CoverageStart, f.CoverageEnd = time.Date(2026, 10, 2, 0, 0, 0, 0, msk), time.Date(2026, 10, 4, 0, 0, 0, 0, msk).Add(-time.Nanosecond)
	f.Events = nil
	for day := 2; day <= 3; day++ {
		f.Events = append(f.Events, FundingEvent{At: time.Date(2026, 10, day, 0, 0, 0, 0, msk), DateOnly: true, AmountPerLongContract: modelDecimal("-7"), Reference: fmt.Sprintf("synthetic-day-%d", day)})
	}
	income, warnings, err := FundingPositionAdjustments(pos, mark)
	if err != nil || !income.Equal(modelDecimal("-7")) || len(warnings) == 0 {
		t.Fatalf("must book Oct3 only; got%s %v %v", income, warnings, err)
	}
	f.CoverageStart = f.CoverageStart.Add(48 * time.Hour)
	if _, _, err = FundingPositionAdjustments(pos, mark); err == nil {
		t.Fatal("missing complete day accepted")
	}
}

func TestPreviouslyAccruedFundingCannotDisappearFromLedger(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), PerpetualDated), modelPolicy()
	out, _ := EvaluateRelationships(s, p, nil)
	c := modelFindCandidate(t, out, PerpetualDated, "long_primary_short_hedge")
	pos, err := NewPosition(c, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	first := derivativeSingle(derivativeTestSnapshot(s.ObservedAt.Add(24*time.Hour), false), PerpetualDated)
	income, _, err := FundingPositionAdjustments(pos, first)
	if err != nil {
		t.Fatal(err)
	}
	pos.Latest = &Valuation{ObservedAt: first.ObservedAt, Snapshot: first, FundingIncome: income}
	next := derivativeSingle(derivativeTestSnapshot(first.ObservedAt.Add(time.Hour), false), PerpetualDated)
	next.Relationships[0].Funding.Events = nil
	if _, _, err = FundingPositionAdjustments(pos, next); err == nil || !strings.Contains(err.Error(), "reconciliation") {
		t.Fatal("previously paid funding silently disappeared", err)
	}
}

func TestUSDQuotedFutureScenarioUsesQuoteDeltaAndRequiresFreshFactor(t *testing.T) {
	entry := Leg{Instrument: Instrument{Kind: "future", Lot: 1, PriceCurrency: "usd", PriceFactor: modelDecimal("80")}, Side: "buy", Lots: 10, Price: modelDecimal("2000"), Value: modelDecimal("1600000")}
	exit := entry
	exit.Instrument.PriceFactor, exit.Value = modelDecimal("100"), modelDecimal("2000000")
	if !LegProfit(entry, exit).IsZero() {
		t.Fatal("unchanged USD futures quote manufactured FX notional profit")
	}
	exit.Price = modelDecimal("2001")
	if !LegProfit(entry, exit).Equal(modelDecimal("1000")) {
		t.Fatal("scenario quote delta multiplied by wrong conversion or lots")
	}
	s := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), StandardMini)
	for n := 0; n < 2; n++ {
		s.Instruments[n].PriceCurrency = "usd"
		s.Instruments[n].PriceFactorAt = s.ObservedAt.Add(-time.Minute)
	}
	out, _ := EvaluateRelationships(s, modelPolicy(), nil)
	if len(out.Candidates) != 0 {
		t.Fatal("stale USD conversion accepted")
	}
}

func TestRelativeFinalEntryFreshnessAndEvaluatorSeparation(t *testing.T) {
	s, p := derivativeSingle(derivativeTestSnapshot(derivativeNow(), true), StandardMini), modelPolicy()
	out, _ := EvaluateRelationships(s, p, nil)
	c := modelFindCandidate(t, out, StandardMini, "short_primary_long_hedge")
	if err := ValidateRelativeEntryAt(c, s.ObservedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRelativeEntryAt(c, s.ObservedAt.Add(p.MaxQuoteAge)); err == nil {
		t.Fatal("preview delay expired original books without blocking entry")
	}
	curveOnly, err := Evaluate(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range curveOnly.Metrics {
		if IsRelativeStrategy(m.Strategy) {
			t.Fatal("curve-only evaluator leaked relative strategies")
		}
	}
}
