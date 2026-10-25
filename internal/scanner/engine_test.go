package scanner

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func fixture() (Snapshot, Assumptions) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	stock := Instrument{UID: "stock", Ticker: "SBER", Kind: "share", Currency: "rub", Lot: 10, PositionUID: "underlying", PriceFactor: dec("1"), APIAvailable: true, BuyAvailable: true}
	future := Instrument{UID: "future", Ticker: "SRZ6", Kind: "future", Currency: "rub", Lot: 1, UnderlyingPositionUID: "underlying", AssetSize: dec("100"), PriceFactor: dec("1"), Expiry: now.Add(30 * 24 * time.Hour), LastTrade: now.Add(29 * 24 * time.Hour), Settlement: "physical", APIAvailable: true, SellAvailable: true, InitialMargin: dec("2000"), ExchangeFee: dec("5.90"), ExchangeFeeKnown: true, ExchangeFeeSource: "verified test specification"}
	s := Snapshot{Pair: Pair{Stock: stock, Future: future}, ObservedAt: now,
		StockBook:  Book{UID: "stock", Time: now.Add(-time.Second), Consistent: true, TradeAvailable: true, Asks: []Level{{Price: dec("100"), Lots: 10}}},
		FutureBook: Book{UID: "future", Time: now.Add(-time.Second), Consistent: true, TradeAvailable: true, Bids: []Level{{Price: dec("11000"), Lots: 1}}},
	}
	a := Assumptions{Contracts: 1, CapitalLimit: dec("100000"), CashRate: dec("0.10"), MaxQuoteAge: 10 * time.Second, MaxQuoteSkew: 2 * time.Second, MaxDays: 90}
	return s, a
}

func TestKnownCashAndCarryWithDelivery(t *testing.T) {
	s, a := fixture()
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	if r.Shares != 100 || r.StockLots != 10 || r.Contracts != 1 {
		t.Fatalf("unexpected sizing: shares=%d stock lots=%d futures=%d", r.Shares, r.StockLots, r.Contracts)
	}
	equalDecimal(t, "stock cost", r.StockCost, "10000")
	equalDecimal(t, "future sale", r.FutureValue, "11000")
	equalDecimal(t, "gross basis", r.GrossBasis, "1000")
	equalDecimal(t, "funded capital includes both stock and margin plus largest entry fee", r.CapitalRequired, "12041")
	equalDecimal(t, "days", r.Days, "30")
	if !r.CashBenchmark.Equal(dec("12041").Mul(dec("0.1")).Mul(dec("30")).Div(dec("365"))) {
		t.Fatal("wrong cash benchmark")
	}
	investor := r.Scenarios[0]
	if len(investor.Fees) != 3 {
		t.Fatalf("delivery should have three commissions, got %d", len(investor.Fees))
	}
	equalDecimal(t, "opening stock fee", investor.Fees[0].Amount, "30")
	equalDecimal(t, "opening future fee", investor.Fees[1].Amount, "11")
	equalDecimal(t, "delivery stock fee, not future exit fee", investor.Fees[2].Amount, "33")
	equalDecimal(t, "total fees", investor.TradingFees, "74")
	equalDecimal(t, "net profit", investor.NetProfit, "926")
	if !investor.Qualifies || r.Preview.Status != "" {
		t.Fatal("unexpected eligibility or fabricated broker preview")
	}
}

func TestDepthPriceConversionAndLotMatching(t *testing.T) {
	s, a := fixture()
	a.Contracts = 2
	s.StockBook.Asks = []Level{{Price: dec("100"), Lots: 5}, {Price: dec("101"), Lots: 15}}
	s.FutureBook.Bids = []Level{{Price: dec("110"), Lots: 1}, {Price: dec("109"), Lots: 1}}
	s.Pair.Future.PriceFactor = dec("100")
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "weighted executable stock cost", r.StockCost, "20150")
	equalDecimal(t, "converted weighted future sale", r.FutureValue, "21900")
	equalDecimal(t, "raw worst stock limit", r.StockLimit, "101")
	equalDecimal(t, "raw worst future limit", r.FutureLimit, "109")
	if r.Shares != 200 || r.StockLots != 20 {
		t.Fatal("contract share multiplier was confused with stock lot")
	}
	s.Pair.Stock.Lot = 30
	if _, err = Evaluate(s, a, Profiles()); err == nil || !strings.Contains(err.Error(), "stock lots") {
		t.Fatalf("wanted partial stock lot rejection, got %v", err)
	}
}

func TestRejectsUnsafeOrUnverifiedInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot, *Assumptions)
	}{
		{"stale", func(s *Snapshot, a *Assumptions) { s.StockBook.Time = s.ObservedAt.Add(-time.Minute) }},
		{"future timestamp", func(s *Snapshot, a *Assumptions) { s.StockBook.Time = s.ObservedAt.Add(time.Nanosecond) }},
		{"skew", func(s *Snapshot, a *Assumptions) { s.StockBook.Time = s.ObservedAt.Add(-5 * time.Second) }},
		{"inconsistent", func(s *Snapshot, a *Assumptions) { s.FutureBook.Consistent = false }},
		{"closed", func(s *Snapshot, a *Assumptions) { s.FutureBook.TradeAvailable = false }},
		{"wrong underlying", func(s *Snapshot, a *Assumptions) { s.Pair.Future.UnderlyingPositionUID = "other" }},
		{"cash settled", func(s *Snapshot, a *Assumptions) { s.Pair.Future.Settlement = "cash" }},
		{"non RUB", func(s *Snapshot, a *Assumptions) { s.Pair.Future.Currency = "usd" }},
		{"missing multiplier", func(s *Snapshot, a *Assumptions) { s.Pair.Future.PriceFactor = decimal.Zero }},
		{"unknown exchange fee", func(s *Snapshot, a *Assumptions) { s.Pair.Future.ExchangeFeeKnown = false }},
		{"unsourced exchange fee", func(s *Snapshot, a *Assumptions) { s.Pair.Future.ExchangeFeeSource = "" }},
		{"expired", func(s *Snapshot, a *Assumptions) { s.Pair.Future.Expiry = s.ObservedAt }},
		{"last trade passed", func(s *Snapshot, a *Assumptions) { s.Pair.Future.LastTrade = s.ObservedAt.Add(-time.Second) }},
		{"too far", func(s *Snapshot, a *Assumptions) { a.MaxDays = 20 }},
		{"insufficient future depth", func(s *Snapshot, a *Assumptions) { a.Contracts = 2; s.StockBook.Asks[0].Lots = 20 }},
		{"insufficient stock depth", func(s *Snapshot, a *Assumptions) { s.StockBook.Asks[0].Lots = 9 }},
		{"zero price", func(s *Snapshot, a *Assumptions) { s.StockBook.Asks[0].Price = decimal.Zero }},
		{"fractional share contract", func(s *Snapshot, a *Assumptions) { s.Pair.Future.AssetSize = dec("100.5") }},
		{"future order lot", func(s *Snapshot, a *Assumptions) { s.Pair.Future.Lot = 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a := fixture()
			tc.mutate(&s, &a)
			if _, err := Evaluate(s, a, Profiles()); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	s, a := fixture()
	p := Profiles()
	p[0].StockRate = decimal.Zero
	if _, err := Evaluate(s, a, p); err == nil {
		t.Fatal("zero commission rate must not silently make a trade profitable")
	}
}

func TestBuffersAndThresholds(t *testing.T) {
	s, a := fixture()
	a.MarginBufferRate = dec("0.25")
	a.ExecutionBufferBPS = dec("10")
	a.SettlementPriceBufferRate = dec("0.20")
	r, err := Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	equalDecimal(t, "capital with funded margin buffer", r.CapitalRequired, "12541")
	equalDecimal(t, "both entry notionals execution allowance", r.ExecutionBuffer, "21")
	equalDecimal(t, "buffered delivery commission", r.Scenarios[0].Fees[2].Amount, "39.60")
	equalDecimal(t, "net with buffers", r.Scenarios[0].NetProfit, "898.40")
	a.MinProfit = dec("10000")
	r, err = Evaluate(s, a, Profiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range r.Scenarios {
		if scenario.Qualifies {
			t.Fatal("profit threshold ignored")
		}
	}
	a.MinProfit = decimal.Zero
	a.MinExcessProfit = dec("10000")
	r, _ = Evaluate(s, a, Profiles())
	for _, scenario := range r.Scenarios {
		if scenario.Qualifies {
			t.Fatal("excess return threshold ignored")
		}
	}
	a.MinExcessProfit = decimal.Zero
	a.CapitalLimit = dec("12000")
	r, _ = Evaluate(s, a, Profiles())
	for _, scenario := range r.Scenarios {
		if scenario.Qualifies {
			t.Fatal("capital limit ignored")
		}
	}
}
