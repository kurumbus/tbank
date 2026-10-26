package curve

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func positionTestEntry(t *testing.T) Candidate {
	t.Helper()
	s, policy := modelCalendar("KEYRATE"), modelPolicy()
	policy.FutureTurnover = modelDecimal("30000000")
	m := firstMetric(t, s, policy)
	c, err := calendarCandidate(s, policy, s.Instruments[0], s.Instruments[1], m, decimal.Zero, "frozen synthetic target", false)
	if err != nil || c.MinimumTariff == "" {
		t.Fatalf("entry: %v %+v", err, c)
	}
	return c
}

func positionTestSnapshot(t *testing.T, p Position, at time.Time, farPrice string) Snapshot {
	t.Helper()
	s, err := clonePositionJSON(p.Entry.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	s.ObservedAt = at
	for uid, book := range s.Books {
		book.Time = at
		if uid == p.Entry.Legs[1].Instrument.UID {
			price := modelDecimal(farPrice)
			book.Bids[0].Price = price.Sub(modelDecimal("0.01"))
			book.Asks[0].Price = price.Add(modelDecimal("0.01"))
		}
		s.Books[uid] = book
	}
	return s
}

func TestCurvePositionFreezesEntryAndIndependentExitSchedules(t *testing.T) {
	entry := positionTestEntry(t)
	p, err := NewPosition(entry, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	fee := p.Entry.Scenarios[1].Fees[0].Amount
	if !p.Entry.Scenarios[1].FutureRate.Equal(modelDecimal("0.00025")) || !p.ExitProfiles[1].FutureRate.Equal(modelDecimal("0.0004")) || !p.ExitProfiles[1].FutureTurnover.IsZero() {
		t.Fatal("entry-day turnover discount leaked to closing schedule")
	}
	entry.Scenarios[1].Fees[0].Amount = one
	entry.Legs[0].Instrument.UID = "changed"
	entry.Snapshot.Books[entry.Legs[1].Instrument.UID] = scanner.Book{}
	entry.Policy.MaxHolding = time.Minute
	if !p.Entry.Scenarios[1].Fees[0].Amount.Equal(fee) || p.Entry.Legs[0].Instrument.UID == "changed" || p.Policy.MaxHolding == time.Minute {
		t.Fatal("input mutation changed frozen entry")
	}
	v, err := MarkPosition(p, positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Scenarios) != 3 || !v.Scenarios[1].Fees[0].Amount.Equal(fee) || !v.Scenarios[1].FutureRate.Equal(modelDecimal("0.0004")) {
		t.Fatal("mark lost opening fees or frozen base exit rates")
	}
}

func TestCurvePositionRejectsMissingEntryEvidence(t *testing.T) {
	for _, mutate := range []func(*Candidate){
		func(c *Candidate) { c.MinimumTariff = "" },
		func(c *Candidate) { c.CapitalRequired = decimal.Zero },
		func(c *Candidate) { c.Scenarios = c.Scenarios[:2] },
		func(c *Candidate) { c.Scenarios[0].Fees[0].Name = "missing_entry" },
		func(c *Candidate) { c.Scenarios[0].Qualifies = false },
	} {
		entry := positionTestEntry(t)
		mutate(&entry)
		if _, err := NewPosition(entry, "fixture"); err == nil {
			t.Fatal("invalid entry accepted")
		}
	}
}

func TestCurvePositionLifetimeCostsUseElapsedTimeAndOneExecutionReserve(t *testing.T) {
	entry := positionTestEntry(t)
	entry.Policy.CashRate = modelDecimal("0.15")
	entry.Policy.CarryReserveRate = modelDecimal("0.05")
	entry.ExecutionReserve = modelDecimal("123")
	p, err := NewPosition(entry, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{time.Hour, 2 * time.Hour} {
		v, err := MarkPosition(p, positionTestSnapshot(t, p, p.OpenedAt.Add(elapsed), "10.5"))
		if err != nil {
			t.Fatal(err)
		}
		carry := entry.CapitalRequired.Mul(entry.Policy.CarryReserveRate).Mul(years(elapsed))
		if !v.CarryReserve.Equal(carry) || !v.CashBenchmark.IsZero() || !v.ExecutionReserve.Equal(entry.ExecutionReserve) {
			t.Fatal("mark reused forecast horizon or compounded execution reserve")
		}
		for _, scenario := range v.Scenarios {
			want := v.GrossProfit.Add(v.CouponIncome).Sub(scenario.TradingFees).Sub(entry.ExecutionReserve).Sub(carry)
			if !scenario.NetProfit.Equal(want) || !scenario.ExcessProfit.Equal(want) {
				t.Fatal("lifetime profit cost accounting differs")
			}
		}
		p.Latest = &v
	}
}

func TestCurvePositionTargetRequiresCrossingAndOpeningTariffHurdles(t *testing.T) {
	p, err := NewPosition(positionTestEntry(t), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	partial, err := MarkPosition(p, positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10.5"))
	if err != nil || partial.ShouldClose {
		t.Fatalf("partial convergence should hold: %+v %v", partial, err)
	}
	complete, err := MarkPosition(p, positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10"))
	if err != nil || !complete.ShouldClose || complete.CloseReason != "target_met" || p.Entry.MinimumTariff != "investor" {
		t.Fatalf("target close: %+v %v", complete, err)
	}
	// The midpoint reaches target while an expensive executable close loses
	// money. A qualifying cheaper tariff cannot override the opening label.
	s := positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10")
	for uid, b := range s.Books {
		b.Bids[0].Price = modelDecimal("9.5")
		b.Asks[0].Price = modelDecimal("10.5")
		s.Books[uid] = b
	}
	v, err := MarkPosition(p, s)
	if err != nil || v.ShouldClose || !v.Metric.Value.IsZero() {
		t.Fatalf("unprofitable target crossing closed: %+v %v", v, err)
	}
}

func TestCurvePositionTimeExitsCanCloseAtLossButNeverAfterLastTrade(t *testing.T) {
	p, err := NewPosition(positionTestEntry(t), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at     time.Time
		reason string
	}{
		{p.OpenedAt.Add(p.Policy.MaxHolding), "max_holding"},
		{LastTrade(p).Add(-p.Policy.ExpiryBuffer), "expiry_buffer"},
	} {
		v, err := MarkPosition(p, positionTestSnapshot(t, p, tc.at, "12"))
		if err != nil || !v.ShouldClose || v.CloseReason != tc.reason || !v.Scenarios[0].NetProfit.IsNegative() {
			t.Fatalf("forced loss close %s: %+v %v", tc.reason, v, err)
		}
	}
	if _, err := MarkPosition(p, Snapshot{ObservedAt: LastTrade(p)}); !errors.Is(err, ErrAwaitingSettlement) {
		t.Fatalf("last trade must precede book validation: %v", err)
	}
	s := positionTestSnapshot(t, p, p.OpenedAt.Add(p.Policy.MaxHolding), "12")
	b := s.Books[p.Entry.Legs[0].Instrument.UID]
	b.Bids[0].Lots = 1
	s.Books[b.UID] = b
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("time exit invented missing depth")
	}
}

func TestCurvePositionClosesOwnedLongWithoutShortEligibility(t *testing.T) {
	p, err := NewPosition(positionTestEntry(t), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10")
	s.Instruments[0].ShortEnabled = false
	v, err := MarkPosition(p, s)
	if err != nil || !v.ShouldClose {
		t.Fatalf("selling owned long required short permission: %+v %v", v, err)
	}
	if v.Legs[0].Instrument.ShortEnabled || v.Snapshot.Instruments[0].ShortEnabled {
		t.Fatal("close overwrote actual eligibility metadata")
	}
	s.Instruments[0].SellAvailable = false
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("close ignored sell permission")
	}
}

func TestCurvePositionRejectsChangingContractAndRegressingQuotes(t *testing.T) {
	p, err := NewPosition(positionTestEntry(t), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := positionTestSnapshot(t, p, p.OpenedAt.Add(time.Hour), "10.5")
	v, err := MarkPosition(p, s)
	if err != nil {
		t.Fatal(err)
	}
	p.Latest = &v
	s.ObservedAt = s.ObservedAt.Add(time.Second)
	b := s.Books[p.Entry.Legs[0].Instrument.UID]
	b.Time = b.Time.Add(-time.Second)
	s.Books[b.UID] = b
	if _, err := MarkPosition(p, s); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("regressing quote accepted: %v", err)
	}
	s = positionTestSnapshot(t, p, p.OpenedAt.Add(2*time.Hour), "10.5")
	s.Instruments[0].PriceFactor = modelDecimal("100")
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("changed contract multiplier accepted")
	}
}

func TestCurvePositionForcedExitSurvivesUnavailableMetric(t *testing.T) {
	s, policy := modelBondUniverse(t), modelPolicy()
	policy.BondLots, policy.CapitalLimit = 1000, modelDecimal("10000000")
	scan, err := Evaluate(s, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry Candidate
	for _, c := range scan.Candidates {
		if c.Legs[0].Instrument.UID == "OFZ-5" {
			entry = c
		}
	}
	p, err := NewPosition(entry, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	mark, err := clonePositionJSON(p.Entry.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	mark.ObservedAt = p.OpenedAt.Add(p.Policy.MaxHolding)
	mark.Instruments = nil
	for _, leg := range p.Entry.Legs {
		i, _ := clonePositionJSON(leg.Instrument)
		if i.Bond != nil {
			i.Bond.AccruedAt = mark.ObservedAt
		}
		mark.Instruments = append(mark.Instruments, i)
		b := mark.Books[i.UID]
		b.Time = mark.ObservedAt
		mark.Books[i.UID] = b
	}
	v, err := MarkPosition(p, mark)
	if err != nil || !v.ShouldClose || v.CloseReason != "max_holding" || len(v.Warnings) == 0 {
		t.Fatalf("peer failure blocked valid timed close: %+v %v", v, err)
	}
}

func TestCurvePositionCouponEntitlementAndUnpaidWindow(t *testing.T) {
	opened := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	record := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	paid := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	leg := Leg{Instrument: Instrument{UID: "bond", Kind: "bond", Bond: &BondTerms{Cashflows: []Cashflow{{Kind: "coupon", Amount: modelDecimal("20"), RecordDate: record, Date: paid}}}}, Side: "buy", Units: modelDecimal("10")}
	if _, err := positionCoupons(leg, opened, record.Add(12*time.Hour)); err != nil {
		t.Fatalf("record day was prematurely treated as ex-record: %v", err)
	}
	if _, err := positionCoupons(leg, opened, record.Add(21*time.Hour)); err == nil || !strings.Contains(err.Error(), "unpaid") {
		t.Fatalf("Moscow next-date receivable was ignored: %v", err)
	}
	income, err := positionCoupons(leg, opened, paid)
	if err != nil || !income.Equal(modelDecimal("200")) {
		t.Fatalf("earned paid coupon missing: %s %v", income, err)
	}
	leg.Instrument.Bond.Cashflows[0].RecordDate = opened.Add(-24 * time.Hour)
	income, err = positionCoupons(leg, opened, paid)
	if err != nil || !income.IsZero() {
		t.Fatal("coupon belonging to prior holder was credited")
	}
	leg.Instrument.Bond.Cashflows[0].RecordDate = opened.Add(-12 * time.Hour)
	if _, err := positionCoupons(leg, opened, paid); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("same record-day entitlement was invented: %v", err)
	}
	leg.Instrument.Bond.Cashflows[0].RecordDate = time.Time{}
	if _, err := positionCoupons(leg, opened, paid); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown entitlement was invented: %v", err)
	}
}
