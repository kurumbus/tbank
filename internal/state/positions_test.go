package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

var positionStart = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func positionEntry(t *testing.T) scanner.Result {
	t.Helper()
	d := decimal.RequireFromString
	stock := scanner.Instrument{UID: "stock-uid", Ticker: "SBER", Kind: "share", Currency: "rub", Lot: 10, PositionUID: "underlying", PriceFactor: d("1"), APIAvailable: true, BuyAvailable: true}
	future := scanner.Instrument{UID: "future-uid", Ticker: "SRZ6", Kind: "future", Currency: "rub", Lot: 1, UnderlyingPositionUID: "underlying", AssetSize: d("100"), PriceFactor: d("1"), Expiry: positionStart.Add(30 * 24 * time.Hour), LastTrade: positionStart.Add(29 * 24 * time.Hour), Settlement: "physical", APIAvailable: true, SellAvailable: true, InitialMargin: d("2000"), ExchangeFee: d("5.90"), ExchangeFeeKnown: true, ExchangeFeeSource: "verified test specification"}
	snap := scanner.Snapshot{Pair: scanner.Pair{Stock: stock, Future: future}, ObservedAt: positionStart,
		StockBook:  scanner.Book{UID: stock.UID, Time: positionStart, Consistent: true, TradeAvailable: true, Asks: []scanner.Level{{Price: d("100"), Lots: 10}}},
		FutureBook: scanner.Book{UID: future.UID, Time: positionStart, Consistent: true, TradeAvailable: true, Bids: []scanner.Level{{Price: d("11000"), Lots: 1}}},
	}
	a := scanner.Assumptions{Contracts: 1, CapitalLimit: d("100000"), CashRate: d("0.1"), MaxQuoteAge: 10 * time.Second, MaxQuoteSkew: 2 * time.Second, MaxDays: 90}
	r, err := scanner.Evaluate(snap, a, scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	// The entry's immutable classification must survive subsequent marks and
	// changing configurations, even when later scenarios qualify differently.
	for i := range r.Scenarios {
		if r.Scenarios[i].Profile == "investor" {
			r.Scenarios[i].Qualifies = false
		}
	}
	return r
}

func savePositionRun(t *testing.T, s *Store, id, source string, at time.Time) {
	t.Helper()
	if err := s.SaveRun(context.Background(), id, at, source, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func openTestPosition(t *testing.T, s *Store) scanner.Position {
	t.Helper()
	savePositionRun(t, s, "entry", "production", positionStart)
	p, inserted, err := s.OpenPosition(context.Background(), "entry", positionEntry(t), decimal.RequireFromString("0.9"))
	if err != nil || !inserted {
		t.Fatalf("open: %v %v", inserted, err)
	}
	return p
}

func positionMark(p scanner.Position, at time.Time, close bool) scanner.Valuation {
	snap := p.Entry.Snapshot
	snap.ObservedAt = at
	snap.StockBook.Time = at
	snap.FutureBook.Time = at
	snap.Pair.Stock.SellAvailable = true
	snap.Pair.Future.BuyAvailable = true
	snap.StockBook.Bids = []scanner.Level{{Price: decimal.NewFromInt(100), Lots: p.Entry.StockLots}}
	futureAsk := decimal.NewFromInt(10600)
	if close {
		futureAsk = decimal.NewFromInt(10000)
	}
	snap.FutureBook.Asks = []scanner.Level{{Price: futureAsk, Lots: p.Entry.Contracts}}
	v := scanner.Valuation{Kind: "mark", ObservedAt: at, Snapshot: &snap, ShouldClose: close, GrossProfit: decimal.NewFromInt(400), Scenarios: []scanner.Scenario{{Profile: "investor", NetProfit: decimal.NewFromInt(350), Qualifies: true}}}
	if close {
		v.CloseReason = "capture_target"
	}
	return v
}

func TestPositionDuplicateAcrossRestartQuantityAndSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "positions.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := openTestPosition(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := positionEntry(t)
	r.Key += "different-quantity"
	r.Contracts = 2
	r.Shares *= 2
	r.Snapshot.ObservedAt = positionStart.Add(time.Hour)
	savePositionRun(t, s, "changed-config", "production", r.Snapshot.ObservedAt)
	duplicate, inserted, err := s.OpenPosition(ctx, "changed-config", r, decimal.RequireFromString("0.5"))
	if err != nil || inserted || duplicate.ID != p.ID || duplicate.Entry.Contracts != 1 || !duplicate.CaptureTarget.Equal(decimal.RequireFromString("0.9")) {
		t.Fatalf("duplicate/config mutated position: %+v %v %v", duplicate, inserted, err)
	}
	savePositionRun(t, s, "fixture-entry", "fixture", positionStart)
	fixture, inserted, err := s.OpenPosition(ctx, "fixture-entry", positionEntry(t), decimal.RequireFromString("0.9"))
	if err != nil || !inserted || fixture.ID == p.ID || fixture.Source != "fixture" {
		t.Fatalf("source isolation: %+v %v %v", fixture, inserted, err)
	}
	all, err := s.ListPositions(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	live, err := s.ListPositions(ctx, "production")
	if err != nil || len(live) != 1 {
		t.Fatalf("list source: %d %v", len(live), err)
	}
}

func TestPositionAuditImmutableAndObservationIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	if p.MinimumTariff != "trader" {
		t.Fatalf("entry classification %q", p.MinimumTariff)
	}
	var original string
	if err := s.db.QueryRow(`SELECT entry_json FROM positions WHERE id=?`, p.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "mark", "production", at)
	v := positionMark(p, at, false)
	marked, inserted, err := s.RecordValuation(ctx, "mark", p.ID, v)
	if err != nil || !inserted || marked.Latest == nil {
		t.Fatalf("mark: %+v %v %v", marked, inserted, err)
	}
	v.ShouldClose = true
	v.CloseReason = "changed replay"
	same, inserted, err := s.RecordValuation(ctx, "mark", p.ID, v)
	if err != nil || inserted || same.Status != "open" {
		t.Fatalf("same timestamp mutated status: %+v %v %v", same, inserted, err)
	}
	v = positionMark(p, at.Add(-time.Minute), false)
	if _, inserted, err = s.RecordValuation(ctx, "mark", p.ID, v); err != nil || inserted {
		t.Fatalf("backwards timestamp accepted: %v %v", inserted, err)
	}
	saved, err := s.GetPosition(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var current, tariff string
	if err := s.db.QueryRow(`SELECT entry_json,minimum_tariff FROM positions WHERE id=?`, p.ID).Scan(&current, &tariff); err != nil {
		t.Fatal(err)
	}
	if original != current || tariff != "trader" || saved.MinimumTariff != "trader" || saved.Entry.Scenarios[0].Qualifies {
		t.Fatal("mark rewrote immutable entry or classification")
	}
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 2 || history[1].Kind != "mark" || history[1].RunID != "mark" || history[1].Valuation == nil {
		t.Fatalf("audit: %+v %v", history, err)
	}
}

func TestPositionErrorPreservesValidMarkAndAuditsSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "mark", "production", at)
	p, _, _ = s.RecordValuation(ctx, "mark", p.ID, positionMark(p, at, false))
	savePositionRun(t, s, "failure", "production", at.Add(time.Hour))
	got, recorded, err := s.RecordPositionError(ctx, "failure", p.ID, at.Add(time.Hour), "missing future ask")
	if err != nil || !recorded || got.Latest == nil || !got.Latest.ObservedAt.Equal(at) {
		t.Fatalf("failure overwrote valid mark: %+v %v %v", got, recorded, err)
	}
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 3 || history[2].Error != "missing future ask" || history[2].Valuation != nil {
		t.Fatalf("failure audit: %+v %v", history, err)
	}
	savePositionRun(t, s, "wrong-source", "fixture", at.Add(2*time.Hour))
	if _, recorded, err := s.RecordPositionError(ctx, "wrong-source", p.ID, at.Add(2*time.Hour), "bad source"); err == nil || recorded {
		t.Fatalf("cross-source write accepted: %v %v", recorded, err)
	}
	if _, recorded, err := s.RecordPositionError(ctx, "missing-run", p.ID, at.Add(2*time.Hour), "no audit"); err == nil || recorded {
		t.Fatalf("orphan write accepted: %v %v", recorded, err)
	}
}

func TestClosedPositionReentryRequiresNewerSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "close", "production", at)
	closed, recorded, err := s.RecordValuation(ctx, "close", p.ID, positionMark(p, at, true))
	if err != nil || !recorded || closed.Status != "closed" || closed.ClosedAt == nil || !closed.ClosedAt.Equal(at) {
		t.Fatalf("close: %+v %v %v", closed, recorded, err)
	}
	for _, replayTime := range []time.Time{positionStart, at} {
		r := positionEntry(t)
		r.Snapshot.ObservedAt = replayTime
		got, inserted, err := s.OpenPosition(ctx, "close", r, decimal.RequireFromString("0.9"))
		if err != nil || inserted || got.ID != p.ID {
			t.Fatalf("closing/older snapshot reopened: %+v %v %v", got, inserted, err)
		}
	}
	r := positionEntry(t)
	r.Snapshot.ObservedAt = at.Add(time.Hour)
	r.Snapshot.StockBook.Time = r.Snapshot.ObservedAt
	r.Snapshot.FutureBook.Time = r.Snapshot.ObservedAt
	savePositionRun(t, s, "reentry", "production", r.Snapshot.ObservedAt)
	newPosition, inserted, err := s.OpenPosition(ctx, "reentry", r, decimal.RequireFromString("0.9"))
	if err != nil || !inserted || newPosition.ID == p.ID || newPosition.Status != "open" {
		t.Fatalf("new snapshot did not reenter: %+v %v %v", newPosition, inserted, err)
	}
	if _, changed, err := s.RecordPositionError(ctx, "reentry", p.ID, r.Snapshot.ObservedAt, "late old position observation"); err != nil || changed {
		t.Fatalf("closed position changed: %v %v", changed, err)
	}
}

func TestAwaitingBlocksReentryAndRequiresVerifiedSettlement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	at := p.Entry.Snapshot.Pair.Future.LastTrade
	savePositionRun(t, s, "await", "production", at)
	if _, ok, err := s.AwaitSettlement(ctx, "await", p.ID, at.Add(-time.Second)); err == nil || ok {
		t.Fatal("awaiting before last trade allowed")
	}
	p, ok, err := s.AwaitSettlement(ctx, "await", p.ID, at)
	if err != nil || !ok || p.Status != "awaiting_settlement" || p.Latest != nil {
		t.Fatalf("awaiting: %+v %v %v", p, ok, err)
	}
	r := positionEntry(t)
	r.Snapshot.ObservedAt = at.Add(time.Hour)
	r.Contracts = 2
	if got, opened, err := s.OpenPosition(ctx, "await", r, decimal.RequireFromString("0.9")); err != nil || opened || got.ID != p.ID {
		t.Fatalf("awaiting did not block entry: %+v %v %v", got, opened, err)
	}
	settledAt := p.Entry.Snapshot.Pair.Future.Expiry.Add(time.Minute)
	savePositionRun(t, s, "settlement", "production", settledAt)
	input := scanner.SettlementInput{ObservedAt: settledAt, StockPrice: decimal.NewFromInt(100), FuturePrice: decimal.NewFromInt(10000), Verified: true, Reference: "official test closing prices"}
	v, err := scanner.SettlePosition(p, input)
	if err != nil {
		t.Fatal(err)
	}
	closed, ok, err := s.SettlePosition(ctx, "settlement", p.ID, v)
	if err != nil || !ok || closed.Status != "closed" || closed.Latest == nil || closed.Latest.Kind != "settlement" {
		t.Fatalf("settlement: %+v %v %v", closed, ok, err)
	}
	if _, ok, err := s.SettlePosition(ctx, "settlement", p.ID, v); err != nil || ok {
		t.Fatalf("settlement replay: %v %v", ok, err)
	}
}

func TestConcurrentPositionWritesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
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
	p := openTestPosition(t, s1)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s1, "concurrent-mark", "production", at)
	stores := []*Store{s1, s2}
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, inserted, err := store.RecordValuation(ctx, "concurrent-mark", p.ID, positionMark(p, at, false))
			results <- inserted
			errs <- err
		}(store)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for inserted := range results {
		if inserted {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("same observation committed %d times", count)
	}
	history, err := s1.Observations(ctx, p.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("concurrent audit duplicated: %d %v", len(history), err)
	}
}

func TestLegacyIntentIsNotAnOpenedPosition(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := positionEntry(t)
	savePositionRun(t, s, "legacy", "production", positionStart)
	if ok, err := s.RecordMock(ctx, "legacy", r); err != nil || !ok {
		t.Fatalf("legacy record: %v %v", ok, err)
	}
	all, err := s.ListPositions(ctx, "")
	if err != nil || len(all) != 0 {
		t.Fatal("legacy intent reinterpreted as position")
	}
	p, ok, err := s.OpenPosition(ctx, "legacy", r, decimal.RequireFromString("0.9"))
	if err != nil || !ok {
		t.Fatalf("legacy intent blocked explicit open: %v %v", ok, err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT result_json FROM mock_records`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved scanner.Result
	if err := json.Unmarshal([]byte(raw), &saved); err != nil || saved.Key != p.Entry.Key {
		t.Fatal("legacy audit changed")
	}
}

func TestObservationAndPositionUpdateAreAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "failed-commit", "production", at)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_position_update BEFORE UPDATE ON positions BEGIN SELECT RAISE(ABORT,'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := s.RecordValuation(ctx, "failed-commit", p.ID, positionMark(p, at, false)); err == nil || inserted {
		t.Fatalf("injected failure not propagated: %v %v", inserted, err)
	}
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("observation survived failed position update: %d %v", len(history), err)
	}
	got, err := s.GetPosition(ctx, p.ID)
	if err != nil || got.Latest != nil || got.Status != "open" {
		t.Fatalf("failed update changed position: %+v %v", got, err)
	}
}

func TestHistoricalVerifiedSettlementAfterAwaitingAndError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	expiry := p.Entry.Snapshot.Pair.Future.Expiry
	savePositionRun(t, s, "await-expiry", "production", expiry)
	p, _, _ = s.AwaitSettlement(ctx, "await-expiry", p.ID, expiry)
	savePositionRun(t, s, "missing-settlement", "production", expiry.Add(time.Hour))
	p, _, _ = s.RecordPositionError(ctx, "missing-settlement", p.ID, expiry.Add(time.Hour), "official settlement not yet supplied")
	savePositionRun(t, s, "verified-later", "production", expiry.Add(2*time.Hour))
	input := scanner.SettlementInput{ObservedAt: expiry, StockPrice: decimal.NewFromInt(100), FuturePrice: decimal.NewFromInt(10000), Verified: true, Reference: "official close at expiry"}
	v, err := scanner.SettlePosition(p, input)
	if err != nil {
		t.Fatal(err)
	}
	closed, ok, err := s.SettlePosition(ctx, "verified-later", p.ID, v)
	if err != nil || !ok || closed.Status != "closed" || closed.ClosedAt == nil || !closed.ClosedAt.Equal(expiry) {
		t.Fatalf("historical settlement blocked: %+v %v %v", closed, ok, err)
	}
	var eventClock int64
	if err := s.db.QueryRow(`SELECT last_event_ns FROM positions WHERE id=?`, p.ID).Scan(&eventClock); err != nil || eventClock != expiry.Add(time.Hour).UnixNano() {
		t.Fatalf("event clock regressed: %d %v", eventClock, err)
	}
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 4 {
		t.Fatalf("settlement audit: %+v %v", history, err)
	}
	kinds := map[string]bool{}
	for _, o := range history {
		kinds[o.Kind] = true
	}
	if !kinds["awaiting_settlement"] || !kinds["error"] || !kinds["settlement"] {
		t.Fatalf("lost historical observations: %+v", history)
	}
	if _, ok, err := s.SettlePosition(ctx, "verified-later", p.ID, v); err != nil || ok {
		t.Fatalf("historical settlement replay: %v %v", ok, err)
	}
}

func TestObservationIdentityMigrationPreservesRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-observation-key.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := openTestPosition(t, s)
	oldDefinition := strings.Replace(positionObservationDefinition, "UNIQUE(position_id,observed_at_ns,kind)", "UNIQUE(position_id,observed_at_ns)", 1)
	_, err = s.db.Exec(`CREATE TABLE old_observations ` + oldDefinition + `; INSERT INTO old_observations SELECT * FROM position_observations; DROP TABLE position_observations; ALTER TABLE old_observations RENAME TO position_observations;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 1 || history[0].Kind != "open" {
		t.Fatalf("migration lost audit: %+v %v", history, err)
	}
	var definition string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='position_observations'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(definition), ""), "UNIQUE(position_id,observed_at_ns,kind)") {
		t.Fatal("old uniqueness remains")
	}
}

func TestPrecomputedMarkCannotRegressBooksOrForgeDecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	p := openTestPosition(t, s)
	at := positionStart.Add(time.Hour)
	savePositionRun(t, s, "first-mark", "production", at)
	first := positionMark(p, at, false)
	first.ShouldClose = true
	first.GrossProfit = decimal.NewFromInt(999999)
	marked, ok, err := s.RecordValuation(ctx, "first-mark", p.ID, first)
	if err != nil || !ok || marked.Status != "open" || !marked.Latest.GrossProfit.Equal(decimal.NewFromInt(400)) {
		t.Fatalf("trusted forged precomputed mark: %+v %v %v", marked, ok, err)
	}
	// Both snapshots are individually fresh at their observation times, but
	// this one was computed against the old position and predates committed books.
	later := at.Add(time.Second)
	stale := positionMark(p, later, false)
	stale.Snapshot.StockBook.Time = at.Add(-time.Second)
	stale.Snapshot.FutureBook.Time = at.Add(-time.Second)
	if _, err := scanner.MarkPosition(p, *stale.Snapshot); err != nil {
		t.Fatalf("fixture not valid against old position: %v", err)
	}
	savePositionRun(t, s, "stale-precomputed", "production", later)
	if _, ok, err := s.RecordValuation(ctx, "stale-precomputed", p.ID, stale); err == nil || ok || !strings.Contains(err.Error(), "backwards") {
		t.Fatalf("regressed books accepted: %v %v", ok, err)
	}
	history, err := s.Observations(ctx, p.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("invalid mark appended: %d %v", len(history), err)
	}
}
