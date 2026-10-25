package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

func curveEntry(t *testing.T) curve.Candidate {
	t.Helper()
	d := decimal.RequireFromString
	near := curve.Instrument{UID: "curve-near", Ticker: "KKZ6", Kind: "future", Family: "KEYRATE", Currency: "rub", Lot: 1, PriceFactor: d("1000"), Expiry: positionStart.Add(60 * 24 * time.Hour), LastTrade: positionStart.Add(59 * 24 * time.Hour), APIAvailable: true, BuyAvailable: true, SellAvailable: true, ShortEnabled: true, InitialMarginBuy: d("1000"), InitialMarginSell: d("1000"), ExchangeFee: d("1"), ExchangeFeeKnown: true, ExchangeFeeSource: "verified test tariff"}
	far := near
	far.UID = "curve-far"
	far.Ticker = "KKG7"
	far.Expiry = positionStart.Add(90 * 24 * time.Hour)
	far.LastTrade = positionStart.Add(89 * 24 * time.Hour)
	snapshot := curve.Snapshot{ObservedAt: positionStart, Instruments: []curve.Instrument{near, far}, Books: map[string]scanner.Book{
		near.UID: {UID: near.UID, Time: positionStart, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: d("14.99"), Lots: 100}}, Asks: []scanner.Level{{Price: d("15.01"), Lots: 100}}},
		far.UID:  {UID: far.UID, Time: positionStart, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: d("15.99"), Lots: 100}}, Asks: []scanner.Level{{Price: d("16.01"), Lots: 100}}},
	}}
	policy := curve.DefaultPolicy()
	policy.FutureContracts = 1
	policy.CashRate = decimal.Zero
	policy.CarryReserveRate = decimal.Zero
	policy.ExecutionBufferBPS = decimal.Zero
	policy.MinProfit = d("1")
	policy.MinExcessProfit = d("1")
	policy.MinDeviationBPS = d("1")
	policy.MinHistory = 2
	policy.MinHistorySpan = time.Hour
	warm, err := curve.Evaluate(snapshot, policy, nil)
	if err != nil || len(warm.Metrics) != 1 {
		t.Fatalf("calendar warmup: %+v %v", warm, err)
	}
	history := []curve.Metric{}
	for _, hours := range []int{3, 1} {
		m := warm.Metrics[0]
		m.ObservedAt = positionStart.Add(-time.Duration(hours) * time.Hour)
		m.QuoteTimes = []time.Time{m.ObservedAt, m.ObservedAt}
		m.Value = decimal.Zero
		history = append(history, m)
	}
	scan, err := curve.Evaluate(snapshot, policy, history)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range scan.Candidates {
		if candidate.MinimumTariff != "" {
			return candidate
		}
	}
	t.Fatalf("no eligible calendar in deterministic fixture: %+v", scan)
	return curve.Candidate{}
}

func curveSnapshotAt(t *testing.T, p curve.Position, at time.Time, farMid string) curve.Snapshot {
	t.Helper()
	raw, err := json.Marshal(p.Entry.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snap curve.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	snap.ObservedAt = at
	for uid, book := range snap.Books {
		book.Time = at
		if uid == "curve-far" {
			mid := decimal.RequireFromString(farMid)
			book.Bids = []scanner.Level{{Price: mid.Sub(decimal.RequireFromString("0.01")), Lots: 100}}
			book.Asks = []scanner.Level{{Price: mid.Add(decimal.RequireFromString("0.01")), Lots: 100}}
		}
		snap.Books[uid] = book
	}
	return snap
}

func openTestCurvePosition(t *testing.T, s *Store) curve.Position {
	t.Helper()
	ctx := context.Background()
	candidate := curveEntry(t)
	savePositionRun(t, s, "curve-entry", "production", positionStart)
	if err := s.SaveCurveScan(ctx, "curve-entry", candidate.Snapshot, []curve.Metric{candidate.Metric}, []curve.Candidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.OpenCurvePosition(ctx, "curve-entry", candidate)
	if err != nil || !ok {
		t.Fatalf("open curve position: %+v %v %v", p, ok, err)
	}
	return p
}

func TestCurvePositionRestartOppositeDirectionAndQuantityDedup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "curve.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := openTestCurvePosition(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	changed := curveEntry(t)
	changed.Key = "opposite-plan"
	changed.Direction = "opposite"
	changed.Policy.MaxHolding = time.Hour
	changed.Snapshot.ObservedAt = positionStart.Add(time.Hour)
	changed.Legs[0], changed.Legs[1] = changed.Legs[1], changed.Legs[0]
	for i := range changed.Legs {
		changed.Legs[i].Lots = 20
	}
	savePositionRun(t, s, "changed-curve-config", "production", changed.Snapshot.ObservedAt)
	got, inserted, err := s.OpenCurvePosition(ctx, "changed-curve-config", changed)
	if err != nil || inserted || got.ID != p.ID || got.Policy.MaxHolding != p.Policy.MaxHolding || got.Entry.Direction != p.Entry.Direction || got.Entry.Legs[0].Lots != p.Entry.Legs[0].Lots {
		t.Fatalf("active pair duplicated or changed: %+v %v %v", got, inserted, err)
	}
	savePositionRun(t, s, "curve-fixture", "fixture", positionStart)
	fixture, inserted, err := s.OpenCurvePosition(ctx, "curve-fixture", curveEntry(t))
	if err != nil || !inserted || fixture.ID == p.ID {
		t.Fatalf("source isolation: %+v %v %v", fixture, inserted, err)
	}
	all, err := s.ListCurvePositions(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all curve positions: %d %v", len(all), err)
	}
	live, err := s.ListCurvePositions(ctx, "production")
	if err != nil || len(live) != 1 {
		t.Fatalf("source curve positions: %d %v", len(live), err)
	}
	stock, err := s.ListPositions(ctx, "")
	if err != nil || len(stock) != 0 {
		t.Fatal("curve positions contaminated stock ledger")
	}
}

func TestCurveMarksAuditFreezePolicyAndHandleErrors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestCurvePosition(t, s)
	var entry, policy string
	if err := s.db.QueryRow(`SELECT entry_json,policy_json FROM curve_positions WHERE id=?`, p.ID).Scan(&entry, &policy); err != nil {
		t.Fatal(err)
	}
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "curve-mark", "production", at)
	snapshot := curveSnapshotAt(t, p, at, "15.5")
	marked, ok, err := s.RecordCurveMark(ctx, "curve-mark", p.ID, snapshot)
	if err != nil || !ok || marked.Status != "open" || marked.Latest == nil {
		t.Fatalf("curve mark: %+v %v %v", marked, ok, err)
	}
	if _, ok, err := s.RecordCurveMark(ctx, "curve-mark", p.ID, snapshot); err != nil || ok {
		t.Fatalf("duplicate curve mark: %v %v", ok, err)
	}
	snapshot.ObservedAt = positionStart.Add(time.Minute)
	if _, ok, err := s.RecordCurveMark(ctx, "curve-mark", p.ID, snapshot); err != nil || ok {
		t.Fatalf("old curve mark accepted: %v %v", ok, err)
	}
	savePositionRun(t, s, "curve-error", "production", at.Add(time.Hour))
	failed, ok, err := s.RecordCurvePositionError(ctx, "curve-error", p.ID, at.Add(time.Hour), "future book unavailable")
	if err != nil || !ok || failed.Latest == nil || !failed.Latest.ObservedAt.Equal(at) {
		t.Fatalf("failed observation erased valid mark: %+v %v %v", failed, ok, err)
	}
	var currentEntry, currentPolicy string
	if err := s.db.QueryRow(`SELECT entry_json,policy_json FROM curve_positions WHERE id=?`, p.ID).Scan(&currentEntry, &currentPolicy); err != nil {
		t.Fatal(err)
	}
	if currentEntry != entry || currentPolicy != policy || failed.Entry.MinimumTariff != p.Entry.MinimumTariff {
		t.Fatal("immutable entry/classification/policy changed")
	}
	history, err := s.CurveObservations(ctx, p.ID)
	if err != nil || len(history) != 3 || history[1].Valuation == nil || history[2].Error != "future book unavailable" || history[2].Valuation != nil {
		t.Fatalf("curve history: %+v %v", history, err)
	}
	savePositionRun(t, s, "curve-wrong-source", "fixture", at.Add(2*time.Hour))
	if _, ok, err := s.RecordCurvePositionError(ctx, "curve-wrong-source", p.ID, at.Add(2*time.Hour), "wrong source"); err == nil || ok {
		t.Fatalf("cross-source curve write: %v %v", ok, err)
	}
}

func TestCurveCloseReplayAndAwaitingLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestCurvePosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "curve-close", "production", at)
	closed, ok, err := s.RecordCurveMark(ctx, "curve-close", p.ID, curveSnapshotAt(t, p, at, "15"))
	if err != nil || !ok || closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatalf("curve target close: %+v %v %v", closed, ok, err)
	}
	for _, when := range []time.Time{positionStart, at} {
		entry := curveEntry(t)
		entry.Snapshot.ObservedAt = when
		got, opened, err := s.OpenCurvePosition(ctx, "curve-close", entry)
		if err != nil || opened || got.ID != p.ID {
			t.Fatalf("closed/older quote reopened: %+v %v %v", got, opened, err)
		}
	}
	entry := curveEntry(t)
	newAt := at.Add(time.Hour)
	entry.Snapshot.ObservedAt = newAt
	entry.Metric.ObservedAt = newAt
	entry.Metric.QuoteTimes = []time.Time{newAt, newAt}
	for uid, book := range entry.Snapshot.Books {
		book.Time = newAt
		entry.Snapshot.Books[uid] = book
	}
	savePositionRun(t, s, "curve-reentry", "production", newAt)
	newPosition, opened, err := s.OpenCurvePosition(ctx, "curve-reentry", entry)
	if err != nil || !opened || newPosition.ID == p.ID {
		t.Fatalf("new curve quote did not reopen: %+v %v %v", newPosition, opened, err)
	}
	lastTrade := curve.LastTrade(newPosition)
	savePositionRun(t, s, "curve-expired", "production", lastTrade)
	if _, ok, err := s.RecordCurveMark(ctx, "curve-expired", newPosition.ID, curve.Snapshot{ObservedAt: lastTrade}); !errors.Is(err, curve.ErrAwaitingSettlement) || ok {
		t.Fatalf("expiry fabricated mark: %v %v", ok, err)
	}
	awaiting, ok, err := s.AwaitCurveSettlement(ctx, "curve-expired", newPosition.ID, lastTrade)
	if err != nil || !ok || awaiting.Status != "awaiting_settlement" || awaiting.Latest != nil {
		t.Fatalf("curve awaiting: %+v %v %v", awaiting, ok, err)
	}
	entry.Snapshot.ObservedAt = lastTrade.Add(time.Hour)
	if got, opened, err := s.OpenCurvePosition(ctx, "curve-expired", entry); err != nil || opened || got.ID != newPosition.ID {
		t.Fatalf("awaiting position allowed replacement: %+v %v %v", got, opened, err)
	}
}

func TestCurveConcurrentMarksUsePersistedQuotes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "curve-concurrent.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	p := openTestCurvePosition(t, s1)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s1, "curve-concurrent-mark", "production", at)
	snapshot := curveSnapshotAt(t, p, at, "15.5")
	type outcome struct {
		ok  bool
		err error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{s1, s2} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, ok, err := store.RecordCurveMark(ctx, "curve-concurrent-mark", p.ID, snapshot)
			outcomes <- outcome{ok, err}
		}(store)
	}
	wg.Wait()
	close(outcomes)
	count := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("curve mark stored %d times", count)
	}
	later := at.Add(time.Second)
	stale := curveSnapshotAt(t, p, later, "15.5")
	for uid, book := range stale.Books {
		book.Time = at.Add(-time.Second)
		stale.Books[uid] = book
	}
	savePositionRun(t, s1, "curve-stale-mark", "production", later)
	if _, ok, err := s1.RecordCurveMark(ctx, "curve-stale-mark", p.ID, stale); err == nil || ok || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("current curve quotes regressed: %v %v", ok, err)
	}
}

func TestCurveObservationAndUpdateRollbackTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestCurvePosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "curve-injected-failure", "production", at)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_curve_update BEFORE UPDATE ON curve_positions BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.RecordCurveMark(ctx, "curve-injected-failure", p.ID, curveSnapshotAt(t, p, at, "15.5")); err == nil || ok {
		t.Fatalf("failed curve update not propagated: %v %v", ok, err)
	}
	history, err := s.CurveObservations(ctx, p.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("orphaned curve observation: %+v %v", history, err)
	}
	stored, err := s.GetCurvePosition(ctx, p.ID)
	if err != nil || stored.Latest != nil {
		t.Fatalf("failed curve mark mutated position: %+v %v", stored, err)
	}
}
