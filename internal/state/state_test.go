package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "scanner.db"))
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

func exampleResult(now time.Time) scanner.Result {
	return scanner.Result{
		Key: "stock-uid:future-uid",
		Snapshot: scanner.Snapshot{
			ObservedAt: now,
			Pair: scanner.Pair{
				Stock:  scanner.Instrument{UID: "stock-uid", Ticker: "SBER"},
				Future: scanner.Instrument{UID: "future-uid", Ticker: "SRZ6", Expiry: now.Add(30 * 24 * time.Hour), ExchangeFeeKnown: true, ExchangeFeeSource: "documented-test-fee"},
			},
		},
		Contracts: 1, Shares: 100, StockLots: 100,
		StockLimit: decimal.NewFromInt(250), FutureLimit: decimal.NewFromInt(26000),
		Assumptions: scanner.Assumptions{CashRate: decimal.RequireFromString("0.15"), DividendPerShare: decimal.Zero},
		Scenarios:   []scanner.Scenario{{Profile: "investor", NetProfit: decimal.NewFromInt(100), ExcessProfit: decimal.NewFromInt(50)}},
	}
}

func TestSaveRunPreservesAuditAndSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	r := exampleResult(now)
	if err := s.SaveRun(ctx, "run-1", now, "fixture", []scanner.Result{r}, []string{"missing fee: other instrument"}); err != nil {
		t.Fatal(err)
	}
	var source, raw, skips string
	if err := s.db.QueryRow(`SELECT source,results_json,skips_json FROM runs WHERE run_id='run-1'`).Scan(&source, &raw, &skips); err != nil {
		t.Fatal(err)
	}
	var saved []scanner.Result
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if source != "fixture" || len(saved) != 1 || saved[0].Snapshot.Pair.Future.ExchangeFeeSource != "documented-test-fee" || !saved[0].Assumptions.CashRate.Equal(r.Assumptions.CashRate) || len(saved[0].Scenarios) != 1 {
		t.Fatalf("incomplete audit: %s %s", source, raw)
	}
	if skips != `["missing fee: other instrument"]` {
		t.Fatalf("skips: %s", skips)
	}
	if err := s.SaveRun(ctx, "run-1", now, "production", nil, nil); err == nil {
		t.Fatal("duplicate run silently overwrote immutable audit")
	}
}

func TestDedupOnlyMarksAfterSuccess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	profit := decimal.NewFromInt(100)
	check := func(at time.Time, amount decimal.Decimal, want bool) {
		t.Helper()
		got, err := s.ShouldNotify(ctx, "pair", at, amount, time.Hour, decimal.NewFromInt(20))
		if err != nil || got != want {
			t.Fatalf("ShouldNotify = %v,%v; want %v", got, err, want)
		}
	}
	check(now, profit, true)
	// A failed send does not call MarkNotified: a retry must still be allowed.
	check(now.Add(time.Minute), profit, true)
	if err := s.MarkNotified(ctx, "pair", now, profit); err != nil {
		t.Fatal(err)
	}
	check(now.Add(time.Minute), profit, false)
	check(now.Add(time.Minute), decimal.NewFromInt(119), false)
	check(now.Add(time.Minute), decimal.NewFromInt(120), true)
	check(now.Add(time.Hour), profit, true)
	got, err := s.ShouldNotify(ctx, "pair", now.Add(time.Minute), profit, time.Hour, decimal.Zero)
	if err != nil || got {
		t.Fatalf("zero threshold must not notify on unchanged profit: %v,%v", got, err)
	}
}

func TestMockIsIdempotentAuditableIntent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r := exampleResult(now)
	if err := s.SaveRun(ctx, "run-1", now, "fixture", []scanner.Result{r}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordMock(ctx, "run-1", r); err != nil || !ok {
		t.Fatalf("first intent: %v %v", ok, err)
	}
	if err := s.SaveRun(ctx, "run-2", now, "fixture", []scanner.Result{r}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordMock(ctx, "run-2", r); err != nil || ok {
		t.Fatalf("duplicate intent: %v %v", ok, err)
	}
	var count int
	var status, source, raw string
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mock_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT m.status,r.source,m.result_json FROM mock_records m JOIN runs r USING(run_id)`).Scan(&status, &source, &raw); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != "snapshot_fill_intent" || source != "fixture" {
		t.Fatalf("mock identity/source changed: %d %s %s", count, status, source)
	}
	var saved scanner.Result
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Key != r.Key || saved.Shares != r.Shares {
		t.Fatal("mock result lost")
	}
	if err := s.SaveRun(ctx, "live-run", now, "production", []scanner.Result{r}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordMock(ctx, "live-run", r); err != nil || !ok {
		t.Fatalf("fixture suppressed production intent: %v %v", ok, err)
	}
	if ok, err := s.RecordMock(ctx, "live-run", r); err != nil || ok {
		t.Fatalf("production intent duplicated: %v %v", ok, err)
	}
}

func TestMockRejectsUnknownPriceExpiryAndRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r := exampleResult(now)
	if ok, err := s.RecordMock(ctx, "not-saved", r); err == nil || ok {
		t.Fatalf("orphan mock: %v %v", ok, err)
	}
	if err := s.SaveRun(ctx, "run", now, "production", nil, nil); err != nil {
		t.Fatal(err)
	}
	r.StockLimit = decimal.Zero
	if ok, err := s.RecordMock(ctx, "run", r); err == nil || ok {
		t.Fatalf("unknown price: %v %v", ok, err)
	}
	r = exampleResult(now)
	r.Snapshot.Pair.Future.Expiry = now.Add(-time.Second)
	if ok, err := s.RecordMock(ctx, "run", r); err == nil || ok {
		t.Fatalf("expired intent: %v %v", ok, err)
	}
}
