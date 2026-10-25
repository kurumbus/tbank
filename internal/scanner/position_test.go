package scanner

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func openedFixture(t *testing.T) Position {
	t.Helper()
	s, a := fixture()
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPosition(r, "test", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func closeSnapshot(p Position, after time.Duration, stockPrice, futurePrice string) Snapshot {
	s := p.Entry.Snapshot
	s.ObservedAt = p.OpenedAt.Add(after)
	s.Pair.Stock.SellAvailable = true
	s.Pair.Future.BuyAvailable = true
	s.StockBook.Time, s.FutureBook.Time = s.ObservedAt.Add(-time.Second), s.ObservedAt.Add(-time.Second)
	s.StockBook.Bids = []Level{{Price: dec(stockPrice), Lots: p.Entry.StockLots}}
	s.FutureBook.Asks = []Level{{Price: dec(futurePrice), Lots: p.Entry.Contracts}}
	return s
}

func scenario(t *testing.T, values []Scenario, name string) Scenario {
	t.Helper()
	for _, s := range values {
		if s.Profile == name {
			return s
		}
	}
	t.Fatalf("missing scenario %s", name)
	return Scenario{}
}

func fee(t *testing.T, s Scenario, name string) decimal.Decimal {
	t.Helper()
	for _, line := range s.Fees {
		if line.Name == name {
			return line.Amount
		}
	}
	t.Fatalf("missing fee %s", name)
	return decimal.Zero
}

func TestMinimumTariffAndImmutableOpeningSnapshot(t *testing.T) {
	s, a := fixture()
	a.MinProfit = dec("930")
	a.CashRate = decimal.Zero
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately reorder the scenarios; product order still defines minimum.
	r.Scenarios[0], r.Scenarios[2] = r.Scenarios[2], r.Scenarios[0]
	if got := MinimumEligibleTariff(r); got != "trader" {
		t.Fatalf("minimum=%s", got)
	}
	p, err := NewPosition(r, "mock", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	r.Scenarios[1].Fees[0].Amount = dec("99999")
	r.Snapshot.StockBook.Asks[0].Price = dec("99999")
	equalDecimal(t, "frozen fee", fee(t, scenario(t, p.Entry.Scenarios, "trader"), "stock_entry"), "5")
	equalDecimal(t, "frozen opening ask", p.Entry.Snapshot.StockBook.Asks[0].Price, "100")
	v, err := MarkPosition(p, closeSnapshot(p, 24*time.Hour, "110", "10900"))
	if err != nil {
		t.Fatal(err)
	}
	if !scenario(t, v.Scenarios, "investor").Qualifies || !v.ShouldClose {
		t.Fatal("expected all tariffs to qualify after price improvement")
	}
	if p.MinimumTariff != "trader" {
		t.Fatal("current cheaper tariff changed the opening classification")
	}
	if p.Latest != nil || p.Status != PositionOpen {
		t.Fatal("pure valuation mutated the position")
	}
}

func TestMarkUsesExecutableCloseSidesAndElapsedCashHurdle(t *testing.T) {
	p := openedFixture(t)
	s := closeSnapshot(p, 24*time.Hour, "110", "11200")
	// The other sides are intentionally attractive and must not be used.
	s.StockBook.Asks = []Level{{Price: dec("999"), Lots: 10}}
	s.FutureBook.Bids = []Level{{Price: dec("1"), Lots: 1}}
	v, err := MarkPosition(p, s)
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "owned stock sold at bid", v.StockValue, "11000")
	equalDecimal(t, "short future bought at ask", v.FutureValue, "11200")
	equalDecimal(t, "price-only profit", v.GrossProfit, "800")
	equalDecimal(t, "remaining executable spread", v.ResidualBasis, "200")
	equalDecimal(t, "captured opening spread", v.CapturedFraction, "0.8")
	equalDecimal(t, "elapsed days", v.Days, "1")
	if !v.CashBenchmark.Equal(p.Entry.CapitalRequired.Mul(dec("0.1")).Div(dec("365"))) {
		t.Fatal("benchmark did not use elapsed time")
	}
	i := scenario(t, v.Scenarios, "investor")
	equalDecimal(t, "all entry and exit fees", i.TradingFees, "85.20")
	equalDecimal(t, "net liquidatable P&L", i.NetProfit, "714.80")
	if v.ShouldClose {
		t.Fatal("80% capture must not meet a 90% target")
	}
	s = closeSnapshot(p, 24*time.Hour, "110", "11100")
	v, err = MarkPosition(p, s)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ShouldClose || v.CloseReason != "capture_target_met" {
		t.Fatal("90% capture and positive net/excess did not close")
	}
}

func TestCaptureTargetCannotCloseAtNetLoss(t *testing.T) {
	s, a := fixture()
	s.FutureBook.Bids[0].Price = dec("10020")
	a.CashRate = decimal.Zero
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPosition(r, "test", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	if p.MinimumTariff != "trader" {
		t.Fatalf("wanted trader, got %s", p.MinimumTariff)
	}
	v, err := MarkPosition(p, closeSnapshot(p, time.Hour, "100", "10002"))
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "90 percent captured", v.CapturedFraction, "0.9")
	equalDecimal(t, "trader loss after four legs", scenario(t, v.Scenarios, "trader").NetProfit, "-3.80")
	if v.ShouldClose {
		t.Fatal("spread capture alone must not trigger a losing close")
	}
}

func TestCaptureTargetAlsoRequiresFrozenExcessProfitHurdle(t *testing.T) {
	s, a := fixture()
	a.CashRate = dec("0.8")
	a.MinExcessProfit = dec("100")
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPosition(r, "test", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	if p.MinimumTariff != "investor" {
		t.Fatalf("unexpected opening tariff %s", p.MinimumTariff)
	}
	v, err := MarkPosition(p, closeSnapshot(p, 28*24*time.Hour, "110", "11100"))
	if err != nil {
		t.Fatal(err)
	}
	i := scenario(t, v.Scenarios, "investor")
	if !i.NetProfit.IsPositive() || !i.ExcessProfit.LessThan(dec("100")) || v.CapturedFraction.LessThan(dec("0.9")) {
		t.Fatal("fixture did not isolate the cash-benchmark hurdle")
	}
	if v.ShouldClose {
		t.Fatal("positive net profit and spread capture bypassed the frozen excess-profit hurdle")
	}
}

func TestEntryTurnoverDiscountDoesNotCarryIntoExitDay(t *testing.T) {
	s, a := fixture()
	s.Pair.Future.ExchangeFee = dec("0.01")
	profiles := Profiles()
	profiles[1].FutureTurnover = dec("6000000")
	r, err := Evaluate(s, a, profiles)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPosition(r, "test", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	entry := scenario(t, p.Entry.Scenarios, "trader")
	equalDecimal(t, "discounted entry future rate", entry.FutureRate, "0.0003")
	equalDecimal(t, "frozen entry future fee", fee(t, entry, "future_entry_including_exchange_floor"), "3.30")
	v, err := MarkPosition(p, closeSnapshot(p, 24*time.Hour, "110", "11100"))
	if err != nil {
		t.Fatal(err)
	}
	exit := scenario(t, v.Scenarios, "trader")
	equalDecimal(t, "next-day exit base rate", exit.FutureRate, "0.0004")
	equalDecimal(t, "entry fee retained without recomputation", fee(t, exit, "future_entry_including_exchange_floor"), "3.30")
	equalDecimal(t, "exit uses base rate", fee(t, exit, "future_exit_including_exchange_floor"), "4.44")
	// Higher current organiser charges must still floor the frozen exit rate.
	next := closeSnapshot(p, 25*time.Hour, "110", "11100")
	next.Pair.Future.ExchangeFee = dec("8")
	v, err = MarkPosition(p, next)
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "current exchange floor", fee(t, scenario(t, v.Scenarios, "trader"), "future_exit_including_exchange_floor"), "8")
}

func TestReserveChargedOnceAndProjectedDividendsNotCredited(t *testing.T) {
	s, a := fixture()
	a.ExecutionBufferBPS = dec("10")
	a.DividendPerShare = dec("5")
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPosition(r, "test", dec("0.9"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := MarkPosition(p, closeSnapshot(p, time.Hour, "110", "11200"))
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "no projected dividends", v.Dividends, "0")
	equalDecimal(t, "original fixed reserve", v.ExecutionReserve, "21")
	equalDecimal(t, "net reserve once", scenario(t, v.Scenarios, "investor").NetProfit, "693.80")
	p.Latest = &v
	v2, err := MarkPosition(p, closeSnapshot(p, 2*time.Hour, "110", "11200"))
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "reserve not accumulated each mark", scenario(t, v2.Scenarios, "investor").NetProfit, "693.80")
}

func TestMarkRejectsInvalidExitSnapshots(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"stale", func(s *Snapshot) { s.StockBook.Time = s.ObservedAt.Add(-time.Minute) }},
		{"future timestamp", func(s *Snapshot) { s.StockBook.Time = s.ObservedAt.Add(time.Second) }},
		{"wrong book", func(s *Snapshot) { s.StockBook.UID = "other" }},
		{"wrong contract", func(s *Snapshot) { s.Pair.Future.UID = "other" }},
		{"changed multiplier", func(s *Snapshot) { s.Pair.Future.PriceFactor = dec("100") }},
		{"shares cannot sell", func(s *Snapshot) { s.Pair.Stock.SellAvailable = false }},
		{"future cannot buy", func(s *Snapshot) { s.Pair.Future.BuyAvailable = false }},
		{"inconsistent", func(s *Snapshot) { s.FutureBook.Consistent = false }},
		{"insufficient bids", func(s *Snapshot) { s.StockBook.Bids[0].Lots = 9 }},
		{"missing asks", func(s *Snapshot) { s.FutureBook.Asks = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := openedFixture(t)
			s := closeSnapshot(p, time.Hour, "110", "11100")
			tc.mutate(&s)
			if _, err := MarkPosition(p, s); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	p := openedFixture(t)
	v, err := MarkPosition(p, closeSnapshot(p, 2*time.Hour, "110", "11200"))
	if err != nil {
		t.Fatal(err)
	}
	p.Latest = &v
	if _, err = MarkPosition(p, closeSnapshot(p, time.Hour, "110", "11100")); err == nil {
		t.Fatal("out-of-order observation accepted")
	}
	s := closeSnapshot(p, 2*time.Hour+time.Second, "110", "11100")
	s.StockBook.Time = v.Snapshot.StockBook.Time.Add(-time.Second)
	if _, err = MarkPosition(p, s); err == nil {
		t.Fatal("backwards book timestamp accepted")
	}
}

func TestSettlementRequiresOfficialInputsAndUsesDeliveryFeeOnly(t *testing.T) {
	p := openedFixture(t)
	s := closeSnapshot(p, p.Entry.Snapshot.Pair.Future.LastTrade.Sub(p.OpenedAt), "110", "11100")
	if _, err := MarkPosition(p, s); !errors.Is(err, ErrAwaitingSettlement) {
		t.Fatalf("wanted awaiting settlement, got %v", err)
	}
	p.Status = PositionAwaitingSettlement
	input := SettlementInput{StockPrice: dec("120"), FuturePrice: dec("12000"), ObservedAt: p.Entry.Snapshot.Pair.Future.Expiry, Reference: "MOEX official delivery example", Dividends: dec("200"), Verified: true}
	v, err := SettlePosition(p, input)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ShouldClose || v.Kind != "settlement" || v.CloseReason != "verified_settlement" || v.Snapshot != nil || v.Settlement == nil {
		t.Fatal("incorrect settlement lifecycle")
	}
	equalDecimal(t, "actual delivered stock", v.StockValue, "12000")
	equalDecimal(t, "actual final future", v.FutureValue, "12000")
	equalDecimal(t, "price-only realised basis", v.GrossProfit, "1000")
	equalDecimal(t, "verified received dividends", v.Dividends, "200")
	i := scenario(t, v.Scenarios, "investor")
	if len(i.Fees) != 3 {
		t.Fatal("settlement must contain only two entry fees and stock delivery")
	}
	equalDecimal(t, "actual stock delivery commission", fee(t, i, "stock_delivery"), "36")
	equalDecimal(t, "settled lifetime P&L", i.NetProfit, "1123")
	for _, bad := range []SettlementInput{
		{StockPrice: dec("120"), FuturePrice: dec("12000"), ObservedAt: input.ObservedAt, Reference: "unverified"},
		{StockPrice: dec("120"), FuturePrice: dec("12000"), ObservedAt: input.ObservedAt, Verified: true},
		{StockPrice: dec("120"), ObservedAt: input.ObservedAt, Reference: "missing future", Verified: true},
		{StockPrice: dec("120"), FuturePrice: dec("12000"), ObservedAt: input.ObservedAt.Add(-time.Second), Reference: "too early", Verified: true},
	} {
		if _, err := SettlePosition(p, bad); err == nil {
			t.Fatal("invalid settlement accepted")
		}
	}
	p.Status = PositionClosed
	if _, err := SettlePosition(p, input); err == nil {
		t.Fatal("closed position settled twice")
	}
}

func TestNewPositionRequiresPositiveEligibleBasisAndTarget(t *testing.T) {
	s, a := fixture()
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"0", "-0.1", "1.01"} {
		if _, err := NewPosition(r, "test", dec(target)); err == nil {
			t.Fatalf("invalid target %s", target)
		}
	}
	r.GrossBasis = decimal.Zero
	if _, err := NewPosition(r, "test", dec("0.9")); err == nil {
		t.Fatal("zero opening basis accepted")
	}
}
