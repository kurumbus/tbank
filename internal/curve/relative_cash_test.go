package curve

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func cashTestSnapshot() Snapshot {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s := Snapshot{ObservedAt: now, Books: map[string]scanner.Book{}}
	for _, v := range []struct {
		uid, kind, price string
		lot              int64
	}{{"ENPG", "share", "1000", 1}, {"RUAL", "share", "50", 10}, {"TGLD@", "etf", "10", 1}, {"GL", "future", "10000", 1}, {"UGLD", "share", "11", 1}} {
		i := Instrument{UID: v.uid, Ticker: v.uid, Kind: v.kind, Family: v.uid, Currency: "rub", Lot: v.lot, PriceFactor: one, APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginSell: modelDecimal(v.price)}
		if v.kind == "future" {
			i.UnitSize = one
			i.PriceCurrency = "rub"
			i.PriceFactorKnown = true
			i.PriceFactorAt = now
			i.PriceFactorSource = "synthetic GL multiplier"
			i.SettlementReference = "synthetic GL RUB/gram fixing"
			i.ClassCode = "SPBFUT"
			i.Expiry = now.Add(60 * 24 * time.Hour)
			i.LastTrade = now.Add(59 * 24 * time.Hour)
			i.InitialMarginBuy = modelDecimal("1000")
			i.InitialMarginSell = modelDecimal("1000")
			i.ExchangeFee = one
			i.ExchangeFeeKnown = true
			i.ExchangeFeeSource = "synthetic GL exchange fee"
		}
		if v.kind == "etf" {
			i.ShortEnabled = false
		}
		s.Instruments = append(s.Instruments, i)
		cashTestPrice(&s, v.uid, v.price)
	}
	borrow := PublishedBorrowReference(now)
	borrow.ExistingBalanceKnown = true
	borrow.ExistingBalanceAssumed = true
	borrow.FreeTierEligibleKnown = true
	borrow.FreeTierEligible = false
	s.Relationships = []Relationship{
		{ID: "ENPG-RUAL-test", Strategy: HoldingStub, PrimaryUID: "ENPG", HedgeUID: "RUAL", PrimaryLots: 100, Verified: true, AsOf: now, ValidUntil: now.Add(30 * 24 * time.Hour), Source: "synthetic verified ownership report", Holding: &HoldingReference{SharesPerParent: modelDecimal("13.5271"), RatioSource: "synthetic ownership and share counts", RatioAsOf: now, Borrow: &borrow, DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "synthetic announced calendar"}},
		{ID: "TGLD-GL-test", Strategy: FundNAV, PrimaryUID: "TGLD@", HedgeUID: "GL", PrimaryLots: 10000, Verified: true, AsOf: now, ValidUntil: now.Add(30 * 24 * time.Hour), Source: "synthetic verified fund facts", Fund: &FundReference{NAVPerUnitRUB: modelDecimal("10"), NAVAsOf: now, NAVSource: "synthetic RUB NAV", GoldGramsPerUnit: modelDecimal("0.001"), GoldAsOf: now.Add(-26 * 24 * time.Hour), GoldValidUntil: now.Add(30 * 24 * time.Hour), GoldSource: "synthetic monthly holdings report", GoldGramsPerContract: one, ContractSource: "synthetic GL specification", FXRUBPerUSD: modelDecimal("100"), FXAsOf: now, FXSource: "synthetic FX quote", ExpenseAnnualCap: modelDecimal("0.02369"), ExpenseKnown: true, ExpenseSource: "synthetic expense cap", DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "synthetic no-distribution fund rules"}},
		{ID: "UGLD-offer-test", Strategy: CashOffer, PrimaryUID: "UGLD", PrimaryLots: 100, Verified: true, AsOf: now, ValidUntil: now.Add(30 * 24 * time.Hour), Source: "synthetic cash offer", Offer: &OfferReference{OfferPriceRUB: modelDecimal("10"), PriceSource: "synthetic official offer", AcceptanceDeadline: now.Add(9 * 24 * time.Hour), BrokerCutoff: now.Add(8 * 24 * time.Hour), BrokerCutoffVerified: true, BrokerSupportsTender: true, BrokerSource: "synthetic broker cutoff/support", EligibleSecurity: true, EligibilitySource: "synthetic share eligibility", PayoutDate: now.Add(11 * 24 * time.Hour), PayoutSource: "synthetic verified payout date", TerminalFeeKnown: true, TerminalFeeRUB: modelDecimal("10"), TerminalFeeSource: "synthetic broker tender fee", DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "synthetic announced calendar"}},
	}
	return s
}

func cashTestPrice(s *Snapshot, uid, price string) {
	value := modelDecimal(price)
	s.Books[uid] = scanner.Book{UID: uid, Time: s.ObservedAt, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: value.Sub(modelDecimal("0.001")), Lots: 1000000}}, Asks: []scanner.Level{{Price: value.Add(modelDecimal("0.001")), Lots: 1000000}}}
}

func cashTestAdvance(s Snapshot, at time.Time) Snapshot {
	data, _ := json.Marshal(s)
	var out Snapshot
	_ = json.Unmarshal(data, &out)
	out.ObservedAt = at
	for uid, b := range out.Books {
		b.Time = at
		out.Books[uid] = b
	}
	for n := range out.Relationships {
		r := &out.Relationships[n]
		if r.Holding != nil {
			r.Holding.DistributionsAsOf = at
			r.Holding.Borrow.AsOf = at
		}
		if r.Fund != nil {
			r.Fund.DistributionsAsOf = at
			r.Fund.NAVAsOf = at
			r.Fund.FXAsOf = at
		}
		if r.Offer != nil {
			r.Offer.DistributionsAsOf = at
		}
	}
	return out
}

func TestHoldingUsesShareCountHistoricalStubAndFundedShort(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	warm := evaluateCashRelationships(s, p, nil)
	var m Metric
	for _, v := range warm.Metrics {
		if v.Strategy == HoldingStub {
			m = v
		}
	}
	if !m.Value.Equal(modelDecimal("323.645")) {
		t.Fatalf("wrong share-count stub %s", m.Value)
	}
	if len(warm.Candidates) == 0 {
		t.Fatal("independent fund/offer monitors should remain during holding warmup")
	}
	cashTestPrice(&s, "ENPG", "900")
	out := evaluateCashRelationships(s, p, modelHistory(m, m.Value))
	c := modelFindCandidate(t, out, HoldingStub, "long_primary_short_hedge")
	if c.MinimumTariff == "" || c.Target.IsZero() || c.HedgeMeasure != "subsidiary_share_count" {
		t.Fatalf("holding convergence candidate not eligible: %+v", c.Warnings)
	}
	if c.Legs[1].Lots != 135 || !c.Legs[1].Units.Equal(modelDecimal("1350")) {
		t.Fatal("cash lots must be rounded from 13.5271 subsidiary shares per parent, not equal cash values")
	}
	if !c.CapitalRequired.GreaterThan(c.Legs[0].Value.Add(c.Legs[1].Value)) {
		t.Fatal("short cash proceeds must not fund the long; reserve plus buffer is required")
	}
	if !strings.Contains(strings.Join(c.Warnings, " "), "power business") {
		t.Fatal("unhedged residual business must be identified")
	}
	s.Relationships[0].Holding.SharesPerParent = modelDecimal("13")
	out = evaluateCashRelationships(s, p, modelHistory(m, m.Value))
	for _, c := range out.Candidates {
		if c.Strategy == HoldingStub {
			t.Fatal("changed ownership ratio reused prior metric history")
		}
	}
}

func TestPublishedBorrowBandsAreCalendarDayWholeBalanceAndRequireKnownBaseline(t *testing.T) {
	s := cashTestSnapshot()
	b := s.Relationships[0].Holding.Borrow
	for _, tc := range []struct{ profile, balance, want string }{{"investor", "100000", "80"}, {"investor", "100000.01", "190"}, {"premium", "50000", "35"}, {"trader", "10000001", "6800.00068"}, {"investor", "5000", "5"}} {
		got, err := cashBorrowDaily(b, tc.profile, modelDecimal(tc.balance))
		if err != nil || !got.Equal(modelDecimal(tc.want)) {
			t.Fatalf("%+v got %s %v", tc, got, err)
		}
	}
	b.ExistingBalanceKnown = false
	if validateBorrow(b, s.ObservedAt, s.ObservedAt.Add(time.Hour), modelPolicy()) == nil {
		t.Fatal("unknown existing uncovered balance became zero")
	}
	if cashBorrowDays(time.Date(2026, 10, 2, 20, 30, 0, 0, time.UTC), time.Date(2026, 10, 2, 21, 30, 0, 0, time.UTC)) != 1 {
		t.Fatal("calendar accrual must use Moscow dates, including non-trading days")
	}
}

func TestFundNAVUsesRecordedGramsAndDoesNotMultiplyRUBNAVByFXAgain(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	cashTestPrice(&s, "TGLD@", "8")
	out := evaluateCashRelationships(s, p, nil)
	c := modelFindCandidate(t, out, FundNAV, "long_primary_short_hedge")
	if c.MinimumTariff == "" || !c.Metric.Value.Equal(modelDecimal("-0.2")) || !c.Target.IsZero() || c.Legs[1].Lots != 10 {
		t.Fatalf("fund NAV/gram hedge wrong: %+v", c)
	}
	if c.ExpectedGross.GreaterThan(modelDecimal("20000")) || c.ExpectedGross.LessThan(modelDecimal("19000")) {
		t.Fatal("RUB NAV was transformed twice or wrong units")
	}
	if !strings.Contains(strings.Join(c.Warnings, " "), "review date") {
		t.Fatal("monthly report approximation should be explicit")
	}
	if len(c.ExtraCosts["investor"]) != 1 || c.ExtraCosts["investor"][0].Name != "forecast_fund_expense_drift" || !c.ExtraCosts["investor"][0].Amount.IsPositive() {
		t.Fatal("fund expense cap must reduce forecast")
	}
	position := Position{Entry: c, Policy: p, OpenedAt: s.ObservedAt}
	next := cashTestAdvance(s, s.ObservedAt.Add(24*time.Hour))
	_, costs, err := CashPositionAdjustments(position, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, lines := range costs {
		for _, line := range lines {
			if strings.Contains(line.Name, "expense") {
				t.Fatal("fund expense was counted again in marked NAV")
			}
		}
	}
	s.Relationships[1].Fund.NAVPerUnitRUB = decimal.Zero
	out = evaluateCashRelationships(s, p, nil)
	for _, c := range out.Candidates {
		if c.Strategy == FundNAV {
			t.Fatal("unknown NAV must not become a zero target")
		}
	}
}

func TestCashOfferOneLegHasNoExchangeExitFeeAndMissingBrokerCutoffBlocks(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	cashTestPrice(&s, "UGLD", "7")
	out := evaluateCashRelationships(s, p, nil)
	c := modelFindCandidate(t, out, CashOffer, "long_share_cash_offer")
	if c.MinimumTariff == "" || len(c.Legs) != 1 || !c.Target.Equal(modelDecimal("10")) {
		t.Fatal("verified one-leg cash offer should qualify")
	}
	for _, scenario := range c.Scenarios {
		terminal := false
		for _, fee := range scenario.Fees {
			if strings.HasPrefix(fee.Name, "exit_") && !fee.Amount.IsZero() {
				t.Fatal("cash offer settlement was charged as an exchange sale")
			}
			if fee.Name == "cash_offer_terminal_fee" && fee.Amount.Equal(modelDecimal("10")) {
				terminal = true
			}
		}
		if !terminal {
			t.Fatal("cash offer actual terminal fee missing")
		}
	}
	original := c.Metric.ModelSource
	s.Relationships[2].Offer.BrokerCutoff = time.Time{}
	out = evaluateCashRelationships(s, p, nil)
	c = modelFindCandidate(t, out, CashOffer, "long_share_cash_offer")
	if c.MinimumTariff != "" || !strings.Contains(strings.Join(c.Warnings, " "), "broker cutoff") {
		t.Fatal("incomplete event execution facts must produce a blocked monitor")
	}
	if c.Metric.ModelSource == original {
		t.Fatal("amended offer economic terms must change metric version")
	}
}

func TestCashDividendsNetLongGrossShortAndBorrowIntervalsStayFrozen(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	m, _ := cashRelationshipMetric(s, p, s.Relationships[0])
	cashTestPrice(&s, "ENPG", "900")
	c := modelFindCandidate(t, evaluateCashRelationships(s, p, modelHistory(m, m.Value)), HoldingStub, "long_primary_short_hedge")
	position := Position{Entry: c, Policy: p, OpenedAt: s.ObservedAt}
	next := cashTestAdvance(s, s.ObservedAt.Add(26*time.Hour))
	f := next.Relationships[0].Holding
	f.Dividends = []DividendEvent{{InstrumentUID: "ENPG", RecordDate: s.ObservedAt.Add(24 * time.Hour), PayDate: s.ObservedAt.Add(25 * time.Hour), GrossPerUnit: modelDecimal("10"), NetPerUnit: modelDecimal("8.7"), NetKnown: true, Verified: true, Source: "synthetic actual net receipt"}, {InstrumentUID: "RUAL", RecordDate: s.ObservedAt.Add(24 * time.Hour), PayDate: s.ObservedAt.Add(25 * time.Hour), GrossPerUnit: one, NetPerUnit: modelDecimal("0.87"), NetKnown: true, Verified: true, Source: "synthetic gross short reimbursement"}}
	income, costs, err := CashPositionAdjustments(position, next)
	if err != nil || !income.Equal(modelDecimal("870")) {
		t.Fatalf("net long income %s %v", income, err)
	}
	var borrow, short decimal.Decimal
	for _, fee := range costs["investor"] {
		if fee.Name == "borrow_estimated_calendar_days" {
			borrow = fee.Amount
		}
		if strings.HasPrefix(fee.Name, "short_distribution_") {
			short = short.Add(fee.Amount)
		}
	}
	if !borrow.Equal(modelDecimal("80")) || !short.Equal(modelDecimal("1350")) {
		t.Fatalf("wrong calendar borrow/gross short %s %s", borrow, short)
	}
	position.Latest = &Valuation{ObservedAt: next.ObservedAt, Snapshot: next, Legs: c.Legs, CashIncome: income}
	for _, profile := range scanner.Profiles() {
		position.Latest.Scenarios = append(position.Latest.Scenarios, scanner.Scenario{Profile: profile.Name, Fees: costs[profile.Name]})
	}
	last := cashTestAdvance(next, next.ObservedAt.Add(24*time.Hour))
	cashTestPrice(&last, "RUAL", "100")
	last.Relationships[0].Holding.Dividends = nil // Paid events may leave an API calendar.
	income, costs, err = CashPositionAdjustments(position, last)
	if err != nil {
		t.Fatal(err)
	}
	if !income.Equal(modelDecimal("870")) {
		t.Fatal("previous net receipt was erased when the API stopped returning its event")
	}
	for _, fee := range costs["investor"] {
		if fee.Name == "borrow_estimated_calendar_days" && !fee.Amount.Equal(modelDecimal("270")) {
			t.Fatalf("earlier80 must stay frozen plus new190, got %s", fee.Amount)
		}
	}
}

func TestDatedFundNAVIsMonitorOnlyAndDirectRUBNAVNeedsNoFakeFX(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	cashTestPrice(&s, "TGLD@", "8")
	f := s.Relationships[1].Fund
	f.NAVAsOf = s.ObservedAt.Add(-24 * time.Hour)
	f.FXAsOf = f.NAVAsOf
	c := modelFindCandidate(t, evaluateCashRelationships(s, p, nil), FundNAV, "long_primary_short_hedge")
	if c.MinimumTariff != "" || !strings.Contains(strings.Join(c.Warnings, " "), "dated NAV monitor") {
		t.Fatal("overnight gold move against yesterday NAV became a qualifying discount trade")
	}
	f.NAVAsOf = s.ObservedAt
	f.NAVIsRUB = true
	f.FXRUBPerUSD = decimal.Zero
	f.FXAsOf = time.Time{}
	f.FXSource = ""
	c = modelFindCandidate(t, evaluateCashRelationships(s, p, nil), FundNAV, "long_primary_short_hedge")
	if c.MinimumTariff == "" {
		t.Fatal("official synchronized RUB indicative NAV should not need a fabricated FX=1 quote")
	}
}

func TestCashDistributionEntitlementAmbiguityAndReceivablesFailClosed(t *testing.T) {
	s, p := cashTestSnapshot(), modelPolicy()
	leg := Leg{Instrument: s.Instruments[0], Side: "buy", Units: modelDecimal("100")}
	facts := cashFacts(s.Relationships[0])
	facts.events = []DividendEvent{{InstrumentUID: "ENPG", RecordDate: s.ObservedAt.Add(time.Hour), PayDate: s.ObservedAt.Add(24 * time.Hour), GrossPerUnit: one, NetPerUnit: modelDecimal("0.87"), NetKnown: true, Verified: true, Source: "synthetic"}}
	if _, _, err := cashDistributions(facts, []Leg{leg}, s.ObservedAt, s.ObservedAt.Add(48*time.Hour), s.ObservedAt, p); err == nil || !strings.Contains(err.Error(), "same-record-day") {
		t.Fatal("same record-day share acquisition has ambiguous settlement entitlement")
	}
	facts.events[0].RecordDate = s.ObservedAt.Add(24 * time.Hour)
	facts.events[0].PayDate = s.ObservedAt.Add(72 * time.Hour)
	if _, _, err := cashDistributions(facts, []Leg{leg}, s.ObservedAt, s.ObservedAt.Add(48*time.Hour), s.ObservedAt, p); err == nil || !strings.Contains(err.Error(), "unpaid") {
		t.Fatal("unpaid long dividend must not be silently valued at zero")
	}
}
