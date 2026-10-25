package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/state"
)

func relativeSample(t *testing.T) ([]curve.Snapshot, curveConfig) {
	t.Helper()
	snapshots, err := readCurveFixture("../../testdata/relative.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadCurveConfig("../../config/relative.json")
	if err != nil {
		t.Fatal(err)
	}
	return snapshots, c
}

func relativeStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "relative.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestRelativeFixtureCoversAllSixLifecyclesAndReplay(t *testing.T) {
	snapshots, c := relativeSample(t)
	ctx := context.Background()
	store := relativeStore(t)
	metrics := map[string]bool{}
	opened := map[string]bool{}
	for i := range snapshots {
		r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshots[i], nil)
		if err != nil || len(r.Errors) > 0 {
			t.Fatalf("relative frame %d: %v %v", i, err, r.Errors)
		}
		for _, m := range r.Scan.Metrics {
			if !curve.IsRelativeStrategy(m.Strategy) {
				t.Fatalf("relative command evaluated curve family %s", m.Strategy)
			}
			metrics[m.Strategy] = true
		}
		for _, p := range r.Positions {
			opened[p.Entry.Strategy] = true
			if !curve.IsRelativeStrategy(p.Entry.Strategy) {
				t.Fatalf("relative report contains curve position %d", p.ID)
			}
		}
		if i < 30 {
			for _, p := range r.Positions {
				if p.Entry.Strategy == curve.ShareClass || p.Entry.Strategy == curve.PerpetualDated || p.Entry.Strategy == curve.HoldingStub {
					t.Fatalf("history-based %s opened before 30 prior observations", p.Entry.Strategy)
				}
			}
		}
	}
	for _, strategy := range []string{curve.StandardMini, curve.ShareClass, curve.PerpetualDated, curve.HoldingStub, curve.FundNAV, curve.CashOffer} {
		if !metrics[strategy] || !opened[strategy] {
			t.Errorf("strategy %s metric=%v opened=%v", strategy, metrics[strategy], opened[strategy])
		}
	}
	positions, err := store.ListCurvePositions(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if p.Entry.MinimumTariff == "" || len(p.Entry.Scenarios) != 3 {
			t.Fatalf("position %d lost opening qualification/scenarios", p.ID)
		}
		if p.Status != "closed" && p.Status != "awaiting_settlement" {
			t.Errorf("fixture left %s position %d unexpectedly %s", p.Entry.Strategy, p.ID, p.Status)
		}
		if p.Status == "closed" && (p.Latest == nil || len(p.Latest.Scenarios) != 3) {
			t.Errorf("closed position %d missing lifetime economics", p.ID)
		}
	}
	for i := range snapshots {
		r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshots[i], nil)
		if err != nil || r.Opened+r.Marked+r.Closed+r.Awaiting != 0 {
			t.Fatalf("relative replay frame %d mutated state: %+v %v", i, r, err)
		}
	}
	after, err := store.ListCurvePositions(ctx, "fixture")
	if err != nil || len(after) != len(positions) {
		t.Fatalf("replay duplicated positions: %d -> %d %v", len(positions), len(after), err)
	}
}

func TestRelativeAndCurveModesKeepSharedLedgerIsolated(t *testing.T) {
	relative, c := relativeSample(t)
	curves, curveCfg := curveSample(t)
	ctx := context.Background()
	store := relativeStore(t)
	for i := 0; i < len(relative)-1; i++ {
		r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &relative[i], nil)
		if err != nil || len(r.Errors) > 0 {
			t.Fatalf("relative seed: %v %v", err, r.Errors)
		}
	}
	before, err := store.ListCurvePositions(ctx, "fixture")
	if err != nil || len(before) == 0 {
		t.Fatalf("relative seed empty: %v", err)
	}
	beforeCounts := map[int64]int{}
	for _, p := range before {
		history, e := store.CurveObservations(ctx, p.ID)
		if e != nil {
			t.Fatal(e)
		}
		beforeCounts[p.ID] = len(history)
	}
	for i := range curves {
		r, e := runCurveScan(ctx, store, nil, curveCfg, "fixture", "", &curves[i], nil)
		if e != nil || len(r.Errors) > 0 {
			t.Fatalf("curve scan with relative holdings: %v %v", e, r.Errors)
		}
		for _, p := range r.Positions {
			if curve.IsRelativeStrategy(p.Entry.Strategy) {
				t.Fatal("curve report leaked relative position")
			}
		}
	}
	for id, count := range beforeCounts {
		history, e := store.CurveObservations(ctx, id)
		if e != nil || len(history) != count {
			t.Fatalf("curve command touched relative %d: %d -> %d %v", id, count, len(history), e)
		}
	}
	all, err := store.ListCurvePositions(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	curveCounts := map[int64]int{}
	for _, p := range all {
		if !curve.IsRelativeStrategy(p.Entry.Strategy) {
			history, e := store.CurveObservations(ctx, p.ID)
			if e != nil {
				t.Fatal(e)
			}
			curveCounts[p.ID] = len(history)
		}
	}
	if len(curveCounts) == 0 {
		t.Fatal("curve seed did not create a position")
	}
	r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &relative[len(relative)-1], nil)
	if err != nil || len(r.Errors) > 0 {
		t.Fatalf("relative scan with curve holdings: %v %v", err, r.Errors)
	}
	for id, count := range curveCounts {
		history, e := store.CurveObservations(ctx, id)
		if e != nil || len(history) != count {
			t.Fatalf("relative command touched curve %d: %d -> %d %v", id, count, len(history), e)
		}
	}
}

type relativeRoutingProvider struct {
	snapshot          curve.Snapshot
	emptyRecheck      bool
	curveCalls        int
	relativeCalls     int
	previewCalls      int
	curvePreviewCalls int
	selection         curve.RelativeSelection
}

func (p *relativeRoutingProvider) CurveSnapshot(context.Context, curve.Selection) (curve.Snapshot, error) {
	p.curveCalls++
	return curve.Snapshot{}, fmt.Errorf("wrong curve provider routing")
}
func (p *relativeRoutingProvider) RelativeSnapshot(_ context.Context, selection curve.RelativeSelection) (curve.Snapshot, error) {
	p.relativeCalls++
	p.selection = selection
	snapshot := p.snapshot
	if p.emptyRecheck && p.relativeCalls > 1 {
		snapshot.Relationships = nil
	}
	return snapshot, nil
}
func (p *relativeRoutingProvider) CurvePreview(context.Context, []curve.Leg, string) ([]curve.LegPreview, error) {
	p.curvePreviewCalls++
	return nil, fmt.Errorf("wrong curve preview routing")
}
func (p *relativeRoutingProvider) RelativePreview(_ context.Context, legs []curve.Leg, _ string) ([]curve.LegPreview, error) {
	p.previewCalls++
	previews := make([]curve.LegPreview, 0, len(legs))
	for _, leg := range legs {
		margin := leg.Instrument.InitialMarginBuy
		if leg.Side == "sell" {
			margin = leg.Instrument.InitialMarginSell
		}
		previews = append(previews, curve.LegPreview{UID: leg.Instrument.UID, Status: "matched", InitialMargin: margin.Mul(decimal.NewFromInt(leg.Lots))})
	}
	return previews, nil
}

// Rebase every structured timestamp together so a synthetic fixture can exercise
// the production dispatch/freshness path without connecting to a broker.
func relativeRebase(t *testing.T, snapshot curve.Snapshot, at time.Time) curve.Snapshot {
	t.Helper()
	offset := at.Sub(snapshot.ObservedAt)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if date, e := time.Parse(time.RFC3339Nano, x); e == nil && !date.IsZero() {
				return date.Add(offset).UTC().Format(time.RFC3339Nano)
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case map[string]any:
			for k, value := range x {
				x[k] = walk(value)
			}
			return x
		default:
			return v
		}
	}
	raw, err = json.Marshal(walk(data))
	if err != nil {
		t.Fatal(err)
	}
	var result curve.Snapshot
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRelativeProductionRoutesDiscoveryAndRecheck(t *testing.T) {
	snapshots, c := relativeSample(t)
	for _, emptyRecheck := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_recheck_%t", emptyRecheck), func(t *testing.T) {
			store := relativeStore(t)
			provider := &relativeRoutingProvider{snapshot: relativeRebase(t, snapshots[len(snapshots)-2], time.Now().UTC().Add(-time.Second)), emptyRecheck: emptyRecheck}
			r, err := runCurveScan(context.Background(), store, provider, c, "production", "test-account", nil, nil)
			if err != nil || len(r.Errors) > 0 {
				t.Fatalf("relative production path: %v %v", err, r.Errors)
			}
			if provider.curveCalls != 0 || provider.curvePreviewCalls != 0 || provider.relativeCalls != 2 {
				t.Fatalf("wrong provider route/recheck: curve=%d curve-preview=%d relative=%d", provider.curveCalls, provider.curvePreviewCalls, provider.relativeCalls)
			}
			if len(provider.selection.Relationships) != len(c.Relationships) {
				t.Fatal("configured relationships not sent to relative provider")
			}
			if emptyRecheck {
				if r.Opened != 0 || len(r.Scan.Metrics) != 0 || provider.previewCalls != 0 {
					t.Fatalf("stale discovery survived empty relative recheck: %+v", r)
				}
			} else {
				if r.Opened == 0 || provider.previewCalls == 0 {
					t.Fatalf("eligible live-mode synthetic candidate not previewed/opened: %+v", r)
				}
			}
			for _, m := range r.Scan.Metrics {
				if !curve.IsRelativeStrategy(m.Strategy) {
					t.Fatal("production relative mode used old curve evaluator")
				}
			}
		})
	}
}

func TestRelativeCommandModeAndFixtureGuards(t *testing.T) {
	if err := cmdRelativeScan([]string{"-config", "../../config/curves.json", "-fixture", "../../testdata/curves.json"}); err == nil || !strings.Contains(err.Error(), "matching configuration mode") {
		t.Fatalf("relative command accepted curve config: %v", err)
	}
	if err := cmdCurveScan([]string{"-config", "../../config/relative.json", "-fixture", "../../testdata/relative.json"}); err == nil || !strings.Contains(err.Error(), "matching configuration mode") {
		t.Fatalf("curve command accepted relative config: %v", err)
	}
	if err := cmdRelativeScan([]string{"-fixture", "../../testdata/relative.json", "-notify"}); err == nil {
		t.Fatal("relative fixture allowed real notifications")
	}
}

func TestRelativeEntryFreshnessRechecksNestedReferences(t *testing.T) {
	snapshots, c := relativeSample(t)
	snapshot := snapshots[len(snapshots)-2]
	scan, err := curve.EvaluateRelationships(snapshot, c.Policy.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	var candidate *curve.Candidate
	for i := range scan.Candidates {
		if scan.Candidates[i].Strategy == curve.FundNAV && scan.Candidates[i].MinimumTariff != "" {
			candidate = &scan.Candidates[i]
			break
		}
	}
	if candidate == nil {
		t.Fatal("fixture has no eligible fund/NAV candidate")
	}
	// NAV must initially be synchronized with the books. A preview delay can
	// expire that slightly older NAV while the quoted books remain usable.
	navAge := candidate.Policy.MaxQuoteSkew - time.Second
	candidate.Relationship.Fund.NAVAsOf = snapshot.ObservedAt.Add(-navAge)
	if !curveCandidateFresh(*candidate, snapshot.ObservedAt) {
		t.Fatal("fresh nested reference rejected")
	}
	previewDone := snapshot.ObservedAt.Add(candidate.Policy.MaxQuoteAge - navAge + time.Second)
	if curveCandidateFresh(*candidate, previewDone) {
		t.Fatal("NAV reference that expired during preview passed final entry check")
	}
}

func TestCashOfferCutoffWithoutFutureEntersAwaiting(t *testing.T) {
	snapshots, c := relativeSample(t)
	snapshot := snapshots[len(snapshots)-2]
	var relationship curve.Relationship
	for _, r := range snapshot.Relationships {
		if r.Strategy == curve.CashOffer {
			relationship = r
			break
		}
	}
	if relationship.Offer == nil {
		t.Fatal("cash offer fixture missing")
	}
	snapshot.Relationships = []curve.Relationship{relationship}
	var instrument curve.Instrument
	for _, i := range snapshot.Instruments {
		if i.UID == relationship.PrimaryUID {
			instrument = i
		}
	}
	snapshot.Instruments = []curve.Instrument{instrument}
	if !instrument.LastTrade.IsZero() {
		t.Fatal("cash offer fixture must have no futures last-trade instant")
	}
	store := relativeStore(t)
	ctx := context.Background()
	r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshot, nil)
	if err != nil || len(r.Errors) > 0 || r.Opened != 1 {
		t.Fatalf("cash offer opening: %+v %v", r, err)
	}
	snapshot.ObservedAt = relationship.Offer.BrokerCutoff
	for uid, b := range snapshot.Books {
		b.Time = snapshot.ObservedAt
		snapshot.Books[uid] = b
	}
	r, err = runCurveScan(ctx, store, nil, c, "fixture", "", &snapshot, nil)
	if err != nil || len(r.Errors) > 0 || r.Awaiting != 1 || len(r.Positions) != 1 || r.Positions[0].Status != "awaiting_settlement" || r.Positions[0].Latest != nil {
		t.Fatalf("cash offer cutoff fabricated mark/payment or failed transition: %+v %v", r, err)
	}
}

func TestRelativeCLISettlesVerifiedCashOfferAndReplays(t *testing.T) {
	snapshots, c := relativeSample(t)
	snapshot := snapshots[len(snapshots)-2]
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relative.sqlite")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshot, nil)
	if err != nil || len(r.Errors) != 0 {
		t.Fatalf("seed cash offer: %v %v", err, r.Errors)
	}
	var entry curve.Position
	for _, p := range r.Positions {
		if p.Entry.Strategy == curve.CashOffer {
			entry = p
			break
		}
	}
	if entry.ID == 0 {
		t.Fatal("fixture did not open a cash offer")
	}
	beforeEntry, err := json.Marshal(entry.Entry)
	if err != nil {
		t.Fatal(err)
	}
	offer, leg := entry.Entry.Relationship.Offer, entry.Entry.Legs[0]
	// A later no-payment check must not prevent recording already received
	// payment when its verified evidence arrives afterward.
	awaitAt := offer.PayoutDate.Add(24 * time.Hour)
	if err := store.SaveRun(ctx, "late-await", awaitAt, "fixture", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.AwaitCurveSettlement(ctx, "late-await", entry.ID, awaitAt); err != nil || !changed {
		t.Fatalf("await: changed=%v err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	gross := offer.OfferPriceRUB.Mul(leg.Units).Round(2)
	input := curve.RelativeSettlement{
		ObservedAt: offer.PayoutDate.Add(time.Hour), InstrumentUID: leg.Instrument.UID,
		AcceptedAt: entry.OpenedAt.Add(time.Minute), AcceptedUnits: leg.Units,
		PaidAt: offer.PayoutDate, GrossProceedsRUB: gross,
		TerminalFeeRUB: offer.TerminalFeeRUB.Add(gross.Mul(offer.TerminalFeeRate)).Round(2),
		FeeKnown:       true, DistributionsKnown: true, Verified: true, Reference: "synthetic full acceptance and payment",
	}
	inputPath := filepath.Join(dir, "settlement.json")
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	settle := func() (curve.Position, bool) {
		t.Helper()
		raw, err := captureCommand(t, func() error {
			return cmdCurvePositions([]string{"settle", "-state", path, "-id", strconv.FormatInt(entry.ID, 10), "-input", inputPath})
		})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Position curve.Position
			Changed  bool
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result.Position, result.Changed
	}
	settled, changed := settle()
	if !changed || settled.Status != "closed" || settled.ClosedAt == nil || !settled.ClosedAt.Equal(input.PaidAt) || settled.Latest == nil || !settled.Latest.ObservedAt.Equal(input.ObservedAt) {
		t.Fatalf("wrong settlement lifecycle: %+v changed=%v", settled, changed)
	}
	afterEntry, err := json.Marshal(settled.Entry)
	if err != nil || string(beforeEntry) != string(afterEntry) {
		t.Fatal("settlement rewrote frozen entry", err)
	}
	for _, scenario := range settled.Latest.Scenarios {
		if len(scenario.Fees) != 2 || scenario.Fees[1].Name != "cash_offer_terminal" || !scenario.Fees[1].Amount.Equal(input.TerminalFeeRUB) {
			t.Fatalf("cash tender charged a fictitious exchange exit: %+v", scenario.Fees)
		}
	}
	if repeated, changed := settle(); changed || repeated.Status != "closed" || !repeated.ClosedAt.Equal(input.PaidAt) {
		t.Fatalf("settlement replay changed terminal position: %+v changed=%v", repeated, changed)
	}
	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.CurveObservations(ctx, entry.ID)
	if err != nil || len(history) != 3 || history[1].Kind != "settlement" || history[2].Kind != "awaiting_settlement" {
		t.Fatalf("settlement replay duplicated audit events: %+v %v", history, err)
	}
}
