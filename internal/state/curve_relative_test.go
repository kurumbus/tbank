package state

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

func cashOfferEntry(t *testing.T) curve.Candidate {
	t.Helper()
	d := decimal.RequireFromString
	at := positionStart
	instrument := curve.Instrument{UID: "cash-offer-share", Ticker: "OFFER", ClassCode: "TQBR", Kind: "share", Currency: "rub", PriceCurrency: "rub", Lot: 1, UnitSize: d("1"), PriceFactor: d("1"), PriceFactorKnown: true, PriceFactorAt: at, PriceFactorSource: "verified RUB share units", APIAvailable: true, BuyAvailable: true, SellAvailable: true}
	offer := &curve.OfferReference{OfferPriceRUB: d("100"), PriceSource: "verified offer terms", AcceptanceDeadline: at.Add(48 * time.Hour), BrokerCutoff: at.Add(24 * time.Hour), BrokerCutoffVerified: true, BrokerSupportsTender: true, BrokerSource: "verified broker cutoff", EligibleSecurity: true, EligibilitySource: "verified eligible class", PayoutDate: at.Add(72 * time.Hour), PayoutSource: "verified payout terms", TerminalFeeKnown: true, TerminalFeeRUB: d("1"), TerminalFeeSource: "verified tender fee", DistributionsKnown: true, DistributionsAsOf: at, DistributionsValidUntil: at.Add(14 * 24 * time.Hour), DistributionsSource: "verified no distributions over event window"}
	relationship := curve.Relationship{ID: "cash-offer-event", Strategy: curve.CashOffer, PrimaryUID: instrument.UID, PrimaryLots: 10, Verified: true, Source: "official event identity", AsOf: at, ValidUntil: at.Add(14 * 24 * time.Hour), PrimaryPriceScale: d("1"), Offer: offer}
	snapshot := curve.Snapshot{ObservedAt: at, Instruments: []curve.Instrument{instrument}, Relationships: []curve.Relationship{relationship}, Books: map[string]scanner.Book{instrument.UID: {UID: instrument.UID, Time: at, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: d("89.9"), Lots: 100}}, Asks: []scanner.Level{{Price: d("90"), Lots: 100}}}}}
	policy := curve.DefaultPolicy()
	policy.CapitalLimit = d("100000")
	policy.CashRate = d("0.1")
	policy.CarryReserveRate = d("0.02")
	policy.ExecutionBufferBPS = decimal.Zero
	policy.MinProfit = d("1")
	policy.MinExcessProfit = d("1")
	policy.MinDeviationBPS = d("1")
	scan, err := curve.EvaluateRelationships(snapshot, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range scan.Candidates {
		if candidate.Strategy == curve.CashOffer && candidate.MinimumTariff != "" {
			return candidate
		}
	}
	t.Fatalf("no eligible deterministic cash offer: %+v", scan)
	return curve.Candidate{}
}

func openCashOfferPosition(t *testing.T, s *Store) curve.Position {
	t.Helper()
	ctx := context.Background()
	c := cashOfferEntry(t)
	savePositionRun(t, s, "cash-offer-entry", "production", c.Snapshot.ObservedAt)
	if err := s.SaveCurveScan(ctx, "cash-offer-entry", c.Snapshot, []curve.Metric{c.Metric}, []curve.Candidate{c}, nil); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.OpenCurvePosition(ctx, "cash-offer-entry", c)
	if err != nil || !ok {
		t.Fatalf("open cash offer: %+v %v %v", p, ok, err)
	}
	return p
}

func TestCashOfferPersistsFrozenEventAcrossTermRevisions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openCashOfferPosition(t, s)
	revised := cashOfferEntry(t)
	revised.Relationship.Offer.OfferPriceRUB = decimal.NewFromInt(110)
	revised.Policy.MaxHolding = 7 * 24 * time.Hour
	revised.Legs[0].Lots = 30
	revised.Metric.ModelSource = "amended terms"
	revised.Key = "new event terms"
	revised.Snapshot.ObservedAt = positionStart.Add(time.Hour)
	savePositionRun(t, s, "cash-offer-amendment", "production", revised.Snapshot.ObservedAt)
	got, opened, err := s.OpenCurvePosition(ctx, "cash-offer-amendment", revised)
	if err != nil || opened || got.ID != p.ID || !got.Entry.Relationship.Offer.OfferPriceRUB.Equal(decimal.NewFromInt(100)) || got.Policy.MaxHolding != p.Policy.MaxHolding || got.Entry.Legs[0].Lots != 10 {
		t.Fatalf("revision changed active frozen event: %+v %v %v", got, opened, err)
	}
	history, err := s.CurveHistory(ctx, "production", curve.CashOffer, p.Entry.Metric.Key, positionStart.Add(time.Hour), 100)
	if err != nil || len(history) != 1 || len(history[0].InstrumentUIDs) != 1 {
		t.Fatalf("one-leg event metric was not persisted: %+v %v", history, err)
	}
}

func TestCashOfferSettlementUsesFrozenTermsAndActualPaidTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openCashOfferPosition(t, s)
	cutoff := p.Entry.Relationship.Offer.BrokerCutoff
	savePositionRun(t, s, "cash-offer-cutoff", "production", cutoff)
	if _, ok, err := s.AwaitCurveSettlement(ctx, "cash-offer-cutoff", p.ID, cutoff.Add(-time.Second)); err == nil || ok {
		t.Fatal("cash offer awaited before verified cutoff")
	}
	p, ok, err := s.AwaitCurveSettlement(ctx, "cash-offer-cutoff", p.ID, cutoff)
	if err != nil || !ok || p.Status != "awaiting_settlement" {
		t.Fatalf("cash offer cutoff: %+v %v %v", p, ok, err)
	}
	paidAt := p.Entry.Relationship.Offer.PayoutDate
	savePositionRun(t, s, "cash-offer-pending", "production", paidAt.Add(time.Hour))
	if _, ok, err := s.RecordCurvePositionError(ctx, "cash-offer-pending", p.ID, paidAt.Add(time.Hour), "waiting for payment confirmation"); err != nil || !ok {
		t.Fatalf("cash offer failure audit: %v %v", ok, err)
	}
	input := curve.RelativeSettlement{ObservedAt: paidAt.Add(30 * time.Minute), InstrumentUID: p.Entry.Legs[0].Instrument.UID, AcceptedAt: positionStart.Add(12 * time.Hour), AcceptedUnits: p.Entry.Legs[0].Units, PaidAt: paidAt, GrossProceedsRUB: decimal.NewFromInt(1000), TerminalFeeRUB: decimal.NewFromInt(1), FeeKnown: true, DistributionsKnown: true, Verified: true, Reference: "verified acceptance and payment receipts"}
	savePositionRun(t, s, "cash-offer-paid", "production", paidAt.Add(2*time.Hour))
	bad := input
	bad.GrossProceedsRUB = decimal.NewFromInt(1100)
	if _, ok, err := s.SettleRelativePosition(ctx, "cash-offer-paid", p.ID, bad); err == nil || ok {
		t.Fatalf("amended/unverified cash amount accepted: %v %v", ok, err)
	}
	bad = input
	bad.AcceptedUnits = decimal.NewFromInt(5)
	if _, ok, err := s.SettleRelativePosition(ctx, "cash-offer-paid", p.ID, bad); err == nil || ok {
		t.Fatalf("partial acceptance silently closed full position: %v %v", ok, err)
	}
	bad = input
	bad.FeeKnown = false
	if _, ok, err := s.SettleRelativePosition(ctx, "cash-offer-paid", p.ID, bad); err == nil || ok {
		t.Fatalf("unknown terminal fee accepted: %v %v", ok, err)
	}
	closed, ok, err := s.SettleRelativePosition(ctx, "cash-offer-paid", p.ID, input)
	if err != nil || !ok || closed.Status != "closed" || closed.ClosedAt == nil || !closed.ClosedAt.Equal(paidAt) || closed.Latest == nil || !closed.Latest.ObservedAt.Equal(input.ObservedAt) {
		t.Fatalf("cash-offer settlement failed: %+v %v %v", closed, ok, err)
	}
	if !closed.Latest.GrossProfit.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("wrong frozen proceeds P&L: %s", closed.Latest.GrossProfit)
	}
	for _, scenario := range closed.Latest.Scenarios {
		if len(scenario.Fees) != 2 || scenario.Fees[1].Name != "cash_offer_terminal" || !scenario.Fees[1].Amount.Equal(input.TerminalFeeRUB) {
			t.Fatalf("settlement invented exchange sale commission: %+v", scenario.Fees)
		}
		var opening decimal.Decimal
		for _, entryScenario := range p.Entry.Scenarios {
			if entryScenario.Profile == scenario.Profile {
				for _, fee := range entryScenario.Fees {
					if fee.Name == scenario.Fees[0].Name {
						opening = fee.Amount
					}
				}
			}
		}
		if !scenario.Fees[0].Amount.Equal(opening) {
			t.Fatalf("opening commission changed at settlement: %+v", scenario.Fees)
		}
	}
	var clock int64
	if err := s.db.QueryRow(`SELECT last_event_ns FROM curve_positions WHERE id=?`, p.ID).Scan(&clock); err != nil || clock != paidAt.Add(time.Hour).UnixNano() {
		t.Fatalf("settlement regressed observation clock: %d %v", clock, err)
	}
	history, err := s.CurveObservations(ctx, p.ID)
	if err != nil || len(history) != 4 {
		t.Fatalf("cash-offer settlement audit: %+v %v", history, err)
	}
	settlements := 0
	for _, observation := range history {
		if observation.Kind == "settlement" {
			settlements++
			if observation.Valuation == nil || observation.Valuation.Settlement == nil || observation.Valuation.Settlement.Reference != input.Reference {
				t.Fatal("payment evidence missing from audit")
			}
		}
	}
	if settlements != 1 {
		t.Fatalf("expected one settlement, got %d", settlements)
	}
	if _, ok, err := s.SettleRelativePosition(ctx, "cash-offer-paid", p.ID, input); err != nil || ok {
		t.Fatalf("cash-offer settlement replay: %v %v", ok, err)
	}
}

func TestRelativeSettlementRejectsWrongSourceAndUnsupportedStrategy(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openCashOfferPosition(t, s)
	input := curve.RelativeSettlement{ObservedAt: positionStart.Add(4 * 24 * time.Hour)}
	savePositionRun(t, s, "wrong-relative-source", "fixture", input.ObservedAt)
	if _, ok, err := s.SettleRelativePosition(ctx, "wrong-relative-source", p.ID, input); err == nil || ok {
		t.Fatalf("cross-source settlement accepted: %v %v", ok, err)
	}
	calendar := openTestCurvePosition(t, s)
	savePositionRun(t, s, "unsupported-relative-settlement", "production", input.ObservedAt)
	if _, ok, err := s.SettleRelativePosition(ctx, "unsupported-relative-settlement", calendar.ID, input); err == nil || ok {
		t.Fatalf("curve derivative settlement fabricated: %v %v", ok, err)
	}
}
