package curve

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func relativePositionUniverse(strategy string) (Snapshot, Policy) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	p := modelPolicy()
	p.CapitalLimit = modelDecimal("10000000")
	p.MaxHolding = 7 * 24 * time.Hour
	future := func(uid, family, underlying string, units, factor string) Instrument {
		return Instrument{UID: uid, Ticker: uid, Kind: "future", Family: family, Currency: "rub", Lot: 1, PriceFactor: modelDecimal(factor), UnitSize: modelDecimal(units), PriceCurrency: "rub", PriceFactorKnown: true, PriceFactorAt: now, PriceFactorSource: "synthetic contract multiplier", SettlementReference: "synthetic common fixing", SettlementType: "cash", UnderlyingUID: underlying, Expiry: now.Add(60 * 24 * time.Hour), LastTrade: now.Add(59 * 24 * time.Hour), APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginBuy: modelDecimal("100"), InitialMarginSell: modelDecimal("100"), ExchangeFeeKnown: true, ExchangeFee: modelDecimal("0.01"), ExchangeFeeSource: "synthetic exchange schedule"}
	}
	cash := func(uid, kind string) Instrument {
		return Instrument{UID: uid, Ticker: uid, Kind: kind, Currency: "rub", Lot: 1, PriceFactor: one, UnitSize: one, APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginSell: modelDecimal("30")}
	}
	borrow := &BorrowReference{Verified: true, AsOf: now, ValidUntil: now.Add(30 * 24 * time.Hour), Source: "synthetic dated borrow tariff", ExistingBalanceKnown: true, ProfileTiers: map[string][]BorrowTier{}}
	for _, profile := range scanner.Profiles() {
		borrow.ProfileTiers[profile.Name] = []BorrowTier{{DailyFeeRUB: one}}
	}
	r := Relationship{ID: "paper-test-" + strategy, Strategy: strategy, PrimaryUID: "primary", HedgeUID: "hedge", PrimaryLots: 100, HedgeLotsPerPrimaryLot: one, PrimaryPriceScale: one, HedgePriceScale: one, Verified: true, Source: "synthetic relationship", AsOf: now, ValidUntil: now.Add(30 * 24 * time.Hour)}
	var instruments []Instrument
	prices := []string{"100", "110"}
	switch strategy {
	case StandardMini:
		a, b := future("primary", "GD", "gold", "1", "80"), future("hedge", "GN", "gold", "0.1", "8")
		a.PriceCurrency, b.PriceCurrency = "usd", "usd"
		instruments, r.PrimaryLots, r.HedgeLotsPerPrimaryLot = []Instrument{a, b}, 1, modelDecimal("10")
	case ShareClass:
		instruments = []Instrument{future("primary", "SP", "preferred", "100", "1"), future("hedge", "SR", "ordinary", "100", "1")}
		r.PrimaryLots, r.PrimaryPriceScale, r.HedgePriceScale = 1, modelDecimal("0.01"), modelDecimal("0.01")
		prices = []string{"9000", "10000"}
	case PerpetualDated:
		a, b := future("primary", "GLDRUBF", "gold-rub", "1", "1"), future("hedge", "GL", "gold-rub", "1", "1")
		a.Perpetual, a.Expiry, a.LastTrade = true, time.Time{}, time.Time{}
		instruments = []Instrument{a, b}
		r.Funding = &FundingReference{InstrumentUID: a.UID, Known: true, Source: "synthetic complete funding history", CoverageStart: now.Add(-24 * time.Hour), CoverageEnd: now}
	case HoldingStub:
		instruments, prices = []Instrument{cash("primary", "share"), cash("hedge", "share")}, []string{"100", "60"}
		r.Holding = &HoldingReference{SharesPerParent: modelDecimal("2"), RatioSource: "synthetic ownership", RatioAsOf: now, Borrow: borrow, DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "verified no distributions"}
	case FundNAV:
		instruments, prices = []Instrument{cash("primary", "etf"), future("hedge", "GL", "gold-rub", "1", "1")}, []string{"100", "12000"}
		r.Fund = &FundReference{NAVPerUnitRUB: modelDecimal("120"), NAVAsOf: now, NAVSource: "synthetic NAV", GoldGramsPerUnit: modelDecimal("0.01"), GoldAsOf: now, GoldValidUntil: now.Add(30 * 24 * time.Hour), GoldSource: "synthetic holdings", GoldGramsPerContract: one, ContractSource: "synthetic GL contract", FXRUBPerUSD: modelDecimal("80"), FXAsOf: now, FXSource: "synthetic FX", ExpenseKnown: true, ExpenseAnnualCap: modelDecimal("0.02"), ExpenseSource: "synthetic cap", Borrow: borrow, DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "verified no distributions"}
	case CashOffer:
		instruments, prices, r.HedgeUID = []Instrument{cash("primary", "share")}, []string{"100"}, ""
		r.Offer = &OfferReference{OfferPriceRUB: modelDecimal("120"), PriceSource: "synthetic offer", AcceptanceDeadline: now.Add(48 * time.Hour), BrokerCutoff: now.Add(24 * time.Hour), BrokerCutoffVerified: true, BrokerSupportsTender: true, BrokerSource: "synthetic broker terms", EligibleSecurity: true, EligibilitySource: "synthetic eligibility", PayoutDate: now.Add(72 * time.Hour), PayoutSource: "synthetic payout date", TerminalFeeKnown: true, TerminalFeeRUB: modelDecimal("5"), TerminalFeeSource: "synthetic terminal action tariff", DistributionsKnown: true, DistributionsAsOf: now, DistributionsValidUntil: now.Add(30 * 24 * time.Hour), DistributionsSource: "verified no distributions"}
	}
	s := Snapshot{ObservedAt: now, Instruments: instruments, Relationships: []Relationship{r}, Books: map[string]scanner.Book{}}
	for n, instrument := range instruments {
		price := modelDecimal(prices[n])
		s.Books[instrument.UID] = scanner.Book{UID: instrument.UID, Time: now, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: price.Sub(modelDecimal("0.01")), Lots: 10000}}, Asks: []scanner.Level{{Price: price.Add(modelDecimal("0.01")), Lots: 10000}}}
	}
	return s, p
}

func relativePositionEntry(t *testing.T, strategy string) Candidate {
	t.Helper()
	s, p := relativePositionUniverse(strategy)
	warm, err := EvaluateRelationships(s, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	var history []Metric
	for _, metric := range warm.Metrics {
		history = append(history, modelHistory(metric, decimal.Zero)...)
	}
	out, err := EvaluateRelationships(s, p, history)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Candidates {
		if c.Strategy == strategy && c.MinimumTariff != "" {
			return c
		}
	}
	t.Fatalf("no qualifying %s fixture; skips=%v candidates=%d", strategy, out.Skips, len(out.Candidates))
	return Candidate{}
}

func relativePositionMark(t *testing.T, p Position, at time.Time) Snapshot {
	t.Helper()
	s, err := clonePositionJSON(p.Entry.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	s.ObservedAt = at
	for n := range s.Instruments {
		s.Instruments[n].PriceFactorAt = at
	}
	for uid, b := range s.Books {
		b.Time = at
		s.Books[uid] = b
	}
	for n := range s.Relationships {
		r := &s.Relationships[n]
		r.AsOf = at
		if r.Funding != nil {
			r.Funding.CoverageEnd = at
		}
		if r.Holding != nil {
			r.Holding.DistributionsAsOf = at
			if r.Holding.Borrow != nil {
				r.Holding.Borrow.AsOf = at
			}
		}
		if r.Fund != nil {
			r.Fund.NAVAsOf, r.Fund.FXAsOf, r.Fund.DistributionsAsOf = at, at, at
			if r.Fund.Borrow != nil {
				r.Fund.Borrow.AsOf = at
			}
		}
		if r.Offer != nil {
			r.Offer.DistributionsAsOf = at
		}
	}
	return s
}

func TestRelativePaperMarkAllSixFamilies(t *testing.T) {
	for _, strategy := range []string{StandardMini, ShareClass, PerpetualDated, HoldingStub, FundNAV, CashOffer} {
		t.Run(strategy, func(t *testing.T) {
			p, err := NewPosition(relativePositionEntry(t, strategy), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			s := relativePositionMark(t, p, p.OpenedAt.Add(time.Hour))
			uid := p.Entry.Legs[0].Instrument.UID
			b := s.Books[uid]
			b.Bids[0].Price = b.Bids[0].Price.Add(one)
			b.Asks[0].Price = b.Asks[0].Price.Add(one)
			s.Books[uid] = b
			v, err := MarkPosition(p, s)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Legs) != len(p.Entry.Legs) || len(v.Scenarios) != 3 || v.GrossProfit.IsZero() {
				t.Fatal("mark did not retain full quantity, all tariffs and price P&L")
			}
			for n, scenario := range v.Scenarios {
				fees, err := positionEntryFees(p.Entry, scenario.Profile)
				if err != nil || !scenario.Fees[0].Amount.Equal(fees[0].Amount) || !scenario.FutureRate.Equal(p.ExitProfiles[n].FutureRate) {
					t.Fatal("relative mark changed frozen entry fees or exit rates")
				}
			}
			if strategy == HoldingStub && (!LastTrade(p).IsZero() || !SettlementDue(p).IsZero()) {
				t.Fatal("cash spread invented an expiry")
			}
		})
	}
}

func TestRelativeUSDConversionCannotManufactureFutureProfit(t *testing.T) {
	entry := Leg{Instrument: Instrument{Kind: "future", Lot: 1, PriceFactor: modelDecimal("80")}, Side: "buy", Lots: 3, Price: modelDecimal("4000"), Value: modelDecimal("960000")}
	exit := entry
	exit.Instrument.PriceFactor, exit.Value = modelDecimal("100"), modelDecimal("1200000")
	if !LegProfit(entry, exit).IsZero() {
		t.Fatal("unchanged USD quote produced currency notional profit")
	}
	exit.Price = modelDecimal("4010")
	if !LegProfit(entry, exit).Equal(modelDecimal("3000")) {
		t.Fatal("USD quote change was not translated at current per-point value")
	}
	entry.Side = "sell"
	if !LegProfit(entry, exit).Equal(modelDecimal("-3000")) {
		t.Fatal("short-side currency translated P&L has wrong sign")
	}

	p, err := NewPosition(relativePositionEntry(t, StandardMini), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(time.Hour))
	b := s.Books["primary"]
	b.Bids[0].Price = b.Bids[0].Price.Add(modelDecimal("10"))
	b.Asks[0].Price = b.Asks[0].Price.Add(modelDecimal("10"))
	s.Books["primary"] = b
	v, err := MarkPosition(p, s)
	if err != nil {
		t.Fatal(err)
	}
	p.Latest = &v
	s.ObservedAt = s.ObservedAt.Add(time.Second)
	for uid, book := range s.Books {
		book.Time = s.ObservedAt
		s.Books[uid] = book
	}
	for n := range s.Instruments {
		s.Instruments[n].PriceFactorAt = s.ObservedAt
	}
	s.Instruments[0].PriceFactor = modelDecimal("100")
	s.Instruments[1].PriceFactor = modelDecimal("10")
	v2, err := MarkPosition(p, s)
	if err != nil {
		t.Fatalf("verified current FX conversion rejected: %v", err)
	}
	if !v2.GrossProfit.Equal(v.GrossProfit) || len(v2.LegProfits) != 2 || !v2.LegProfits[0].Equal(v.LegProfits[0]) || !strings.Contains(strings.Join(v2.Warnings, " "), "paper approximation") {
		t.Fatal("flat raw quotes retranslated prior accrued profit or omitted FX approximation warning")
	}
	b = s.Books["primary"]
	b.Bids[0].Price = b.Bids[0].Price.Add(one)
	b.Asks[0].Price = b.Asks[0].Price.Add(one)
	s.Books["primary"] = b
	v3, err := MarkPosition(p, s)
	if err != nil || !v3.GrossProfit.Sub(v.GrossProfit).Equal(modelDecimal("100")) {
		t.Fatalf("new raw quote increment was not converted at current FX: %s %v", v3.GrossProfit.Sub(v.GrossProfit), err)
	}
	s.Instruments[0].PriceFactorAt = v.Legs[0].Instrument.PriceFactorAt.Add(-time.Second)
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("regressing FX reference accepted")
	}
	s.Instruments[0].PriceFactorAt = s.ObservedAt
	s.Instruments[0].UnitSize = modelDecimal("2")
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("FX flexibility allowed changing economic contract size")
	}
}

func TestRelativeTargetClosesAllSixFamilies(t *testing.T) {
	for _, strategy := range []string{StandardMini, ShareClass, PerpetualDated, HoldingStub, FundNAV, CashOffer} {
		t.Run(strategy, func(t *testing.T) {
			p, err := NewPosition(relativePositionEntry(t, strategy), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			s := relativePositionMark(t, p, p.OpenedAt.Add(time.Hour))
			uid, center := "primary", modelDecimal("120")
			switch strategy {
			case StandardMini:
				center = modelDecimal("110")
			case ShareClass:
				center = modelDecimal("10000")
			case PerpetualDated:
				uid, center = "hedge", modelDecimal("100")
			}
			b := s.Books[uid]
			b.Bids[0].Price, b.Asks[0].Price = center.Sub(modelDecimal("0.01")), center.Add(modelDecimal("0.01"))
			s.Books[uid] = b
			v, err := MarkPosition(p, s)
			if err != nil || !v.ShouldClose || v.CloseReason != "target_met" {
				t.Fatalf("executable full convergence did not close: reason=%s warnings=%v err=%v", v.CloseReason, v.Warnings, err)
			}
		})
	}
}

func TestPureCashTimeExitHasNoInventedExpiry(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, HoldingStub), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(p.Policy.MaxHolding))
	book := s.Books["primary"]
	book.Asks = nil // The owned long can still close against its full bid depth.
	s.Books["primary"] = book
	v, err := MarkPosition(p, s)
	if err != nil || !v.ShouldClose || v.CloseReason != "max_holding" || !LastTrade(p).IsZero() || !SettlementDue(p).IsZero() {
		t.Fatalf("cash holding-period exit required an expiry or unavailable metric: %s %v", v.CloseReason, err)
	}
}

func TestFundTargetCannotCloseAgainstStaleNAV(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, FundNAV), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(time.Hour))
	s.Relationships[0].Fund.NAVAsOf = p.OpenedAt
	s.Relationships[0].Fund.FXAsOf = p.OpenedAt
	book := s.Books["primary"]
	book.Bids[0].Price, book.Asks[0].Price = modelDecimal("119.99"), modelDecimal("120.01")
	s.Books["primary"] = book
	v, err := MarkPosition(p, s)
	if err != nil || v.ShouldClose || !strings.Contains(strings.Join(v.Warnings, " "), "metric unavailable") {
		t.Fatalf("stale fund NAV drove target close: close=%v warnings=%v err=%v", v.ShouldClose, v.Warnings, err)
	}
}

func relativeFee(v Valuation, profile, prefix string) decimal.Decimal {
	result := decimal.Zero
	for _, scenario := range v.Scenarios {
		if scenario.Profile == profile {
			for _, fee := range scenario.Fees {
				if strings.HasPrefix(fee.Name, prefix) {
					result = result.Add(fee.Amount)
				}
			}
		}
	}
	return result
}

func TestRelativeBorrowEstimatesFreezePriorIntervals(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, HoldingStub), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(12*time.Hour))
	v, err := MarkPosition(p, s)
	if err != nil || !relativeFee(v, "investor", "borrow_estimated_").Equal(one) {
		t.Fatalf("first Moscow calendar-day charge: %s %v", relativeFee(v, "investor", "borrow_estimated_"), err)
	}
	p.Latest = &v
	s = relativePositionMark(t, p, p.OpenedAt.Add(36*time.Hour))
	for _, profile := range scanner.Profiles() {
		s.Relationships[0].Holding.Borrow.ProfileTiers[profile.Name][0].DailyFeeRUB = modelDecimal("10")
	}
	v2, err := MarkPosition(p, s)
	if err != nil || !relativeFee(v2, "investor", "borrow_estimated_").Equal(modelDecimal("11")) {
		t.Fatalf("tariff revision repriced prior interval: %s %v", relativeFee(v2, "investor", "borrow_estimated_"), err)
	}
	p.Latest = &v2
	v3, err := MarkPosition(p, s)
	if err != nil || !relativeFee(v3, "investor", "borrow_estimated_").Equal(modelDecimal("11")) {
		t.Fatalf("same observation charged borrow twice: %s %v", relativeFee(v3, "investor", "borrow_estimated_"), err)
	}
}

func TestRelativeNetLongAndGrossShortDistributionsAreCumulative(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, HoldingStub), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(60*time.Hour))
	for _, l := range p.Entry.Legs {
		s.Relationships[0].Holding.Dividends = append(s.Relationships[0].Holding.Dividends, DividendEvent{InstrumentUID: l.Instrument.UID, RecordDate: p.OpenedAt.Add(24 * time.Hour), PayDate: p.OpenedAt.Add(48 * time.Hour), GrossPerUnit: one, NetPerUnit: modelDecimal("0.87"), NetKnown: true, Verified: true, Source: "synthetic announced distributions and net receipts"})
	}
	v, err := MarkPosition(p, s)
	if err != nil || !v.CashIncome.Equal(modelDecimal("87")) || !relativeFee(v, "investor", "short_distribution_").Equal(modelDecimal("200")) {
		t.Fatalf("net long/full gross short entitlement: income=%s short=%s err=%v", v.CashIncome, relativeFee(v, "investor", "short_distribution_"), err)
	}
	p.Latest = &v
	v2, err := MarkPosition(p, s)
	if err != nil || !v2.CashIncome.Equal(v.CashIncome) || !relativeFee(v2, "investor", "short_distribution_").Equal(modelDecimal("200")) {
		t.Fatalf("distribution repeat changed cumulative cash: %s %v", v2.CashIncome, err)
	}
}

func TestRelativeFundingPaymentsAreCumulativeWithoutRateExtrapolation(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, PerpetualDated), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := relativePositionMark(t, p, p.OpenedAt.Add(time.Hour))
	s.Relationships[0].Funding.Events = []FundingEvent{{At: p.OpenedAt.Add(30 * time.Minute), AmountPerLongContract: modelDecimal("-1.5"), Reference: "synthetic first completed payment"}}
	v, err := MarkPosition(p, s)
	if err != nil || !v.FundingIncome.Equal(modelDecimal("-150")) {
		t.Fatalf("signed funding times contracts: %s %v", v.FundingIncome, err)
	}
	p.Latest = &v
	s = relativePositionMark(t, p, p.OpenedAt.Add(2*time.Hour))
	s.Relationships[0].Funding.Events = append(v.Snapshot.Relationships[0].Funding.Events, FundingEvent{At: p.OpenedAt.Add(90 * time.Minute), AmountPerLongContract: modelDecimal("0.5"), Reference: "synthetic second completed payment"})
	v2, err := MarkPosition(p, s)
	if err != nil || !v2.FundingIncome.Equal(modelDecimal("-100")) {
		t.Fatalf("funding was extrapolated or paid twice: %s %v", v2.FundingIncome, err)
	}
	p.Latest = &v2
	v3, err := MarkPosition(p, s)
	if err != nil || !v3.FundingIncome.Equal(v2.FundingIncome) {
		t.Fatalf("replayed funding changed cumulative income: %s %v", v3.FundingIncome, err)
	}
	s.Relationships[0].Funding.Events = append(s.Relationships[0].Funding.Events, s.Relationships[0].Funding.Events[0])
	if _, err := MarkPosition(p, s); err == nil {
		t.Fatal("duplicate funding payment accepted")
	}
}

func relativePaidInput(p Position) RelativeSettlement {
	offer, leg := p.Entry.Relationship.Offer, p.Entry.Legs[0]
	return RelativeSettlement{ObservedAt: offer.PayoutDate.Add(time.Hour), InstrumentUID: leg.Instrument.UID, AcceptedAt: p.OpenedAt.Add(time.Hour), AcceptedUnits: leg.Units, PaidAt: offer.PayoutDate, GrossProceedsRUB: offer.OfferPriceRUB.Mul(leg.Units), TerminalFeeRUB: offer.TerminalFeeRUB, FeeKnown: true, DistributionsKnown: true, Verified: true, Reference: "synthetic confirmation of full accepted and paid tender"}
}

func TestCashOfferWaitsForVerifiedFullPayment(t *testing.T) {
	p, err := NewPosition(relativePositionEntry(t, CashOffer), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !LastTrade(p).IsZero() || !SettlementDue(p).Equal(p.Entry.Relationship.Offer.BrokerCutoff) {
		t.Fatal("tender cutoff confused with exchange expiry")
	}
	if _, err := MarkPosition(p, Snapshot{ObservedAt: SettlementDue(p)}); !errors.Is(err, ErrAwaitingSettlement) {
		t.Fatalf("cutoff invented a close instead of unresolved settlement: %v", err)
	}
	p.Status = PositionAwaitingSettlement
	input := relativePaidInput(p)
	v, err := SettleRelativePosition(p, input)
	if err != nil || !v.ShouldClose || v.Kind != "settlement" || v.Settlement == nil {
		t.Fatalf("verified full payout: %+v %v", v, err)
	}
	for _, scenario := range v.Scenarios {
		if len(scenario.Fees) != 2 || scenario.Fees[1].Name != "cash_offer_terminal" || !scenario.Fees[1].Amount.Equal(input.TerminalFeeRUB) {
			t.Fatal("settlement charged a fictitious market sale or omitted terminal action")
		}
	}
	for _, mutate := range []func(*RelativeSettlement){
		func(s *RelativeSettlement) { s.Verified = false },
		func(s *RelativeSettlement) { s.Reference = "" },
		func(s *RelativeSettlement) { s.AcceptedUnits = s.AcceptedUnits.Sub(one) },
		func(s *RelativeSettlement) { s.AcceptedAt = p.Entry.Relationship.Offer.BrokerCutoff.Add(time.Second) },
		func(s *RelativeSettlement) { s.GrossProceedsRUB = s.GrossProceedsRUB.Sub(one) },
		func(s *RelativeSettlement) { s.FeeKnown = false },
		func(s *RelativeSettlement) { s.DistributionsKnown = false },
		func(s *RelativeSettlement) { s.TerminalFeeRUB = s.TerminalFeeRUB.Add(one) },
	} {
		bad := input
		mutate(&bad)
		if _, err := SettleRelativePosition(p, bad); err == nil {
			t.Fatal("incomplete or inconsistent actual tender evidence accepted")
		}
	}
	unknown := p.Entry
	unknown.Relationship, _ = clonePositionJSON(p.Entry.Relationship)
	unknown.Relationship.Offer.BrokerSupportsTender = false
	if _, err := NewPosition(unknown, "fixture"); err == nil || !strings.Contains(err.Error(), "monitor only") {
		t.Fatalf("unknown broker processing opened a paper tender: %v", err)
	}
}
