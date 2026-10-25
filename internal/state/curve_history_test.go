package state

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

func historyMetric(at time.Time, value int64) curve.Metric {
	return curve.Metric{Key: "keyrate:near:far", Strategy: curve.KEYRATECalendar, Unit: "percentage_points", ModelSource: "verified test contract convention", InstrumentUIDs: []string{"near", "far"}, ObservedAt: at, QuoteTimes: []time.Time{at.Add(-time.Second), at}, Value: decimal.NewFromInt(value)}
}

func saveCurveMetric(t *testing.T, s *Store, id, source string, metric curve.Metric) {
	t.Helper()
	savePositionRun(t, s, id, source, metric.ObservedAt)
	snap := curve.Snapshot{ObservedAt: metric.ObservedAt, Books: map[string]scanner.Book{"near": {UID: "near", Time: metric.QuoteTimes[0], Bids: []scanner.Level{{Price: decimal.NewFromInt(100), Lots: 1}}}}, Warnings: []string{"audit marker"}}
	if err := s.SaveCurveScan(context.Background(), id, snap, []curve.Metric{metric}, nil, []string{"baseline warming; no signal"}); err != nil {
		t.Fatal(err)
	}
}

func TestCurveHistoryWarmsWithoutSignalsAndBoundsChronologically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	start := positionStart
	for i := 0; i < 5; i++ {
		saveCurveMetric(t, s, fmt.Sprintf("history-%d", i), "production", historyMetric(start.Add(time.Duration(i)*time.Hour), int64(i)))
	}
	history, err := s.CurveHistory(ctx, "production", curve.KEYRATECalendar, "keyrate:near:far", start.Add(4*time.Hour), 2)
	if err != nil || len(history) != 2 || !history[0].Value.Equal(decimal.NewFromInt(2)) || !history[1].Value.Equal(decimal.NewFromInt(3)) {
		t.Fatalf("bounded strict-before chronological history: %+v %v", history, err)
	}
	if !history[0].ObservedAt.Before(history[1].ObservedAt) {
		t.Fatal("history not chronological")
	}
	var candidates, snapshot, skips string
	if err := s.db.QueryRow(`SELECT candidates_json,snapshot_json,skips_json FROM curve_scans WHERE run_id='history-0'`).Scan(&candidates, &snapshot, &skips); err != nil {
		t.Fatal(err)
	}
	if candidates != "null" {
		t.Fatalf("invented eligible signal: %s", candidates)
	}
	var saved curve.Snapshot
	if err := json.Unmarshal([]byte(snapshot), &saved); err != nil || len(saved.Books["near"].Bids) != 1 || len(saved.Warnings) != 1 {
		t.Fatalf("incomplete snapshot audit: %s %v", snapshot, err)
	}
	if skips != `["baseline warming; no signal"]` {
		t.Fatalf("lost skip audit: %s", skips)
	}
	if _, err := s.CurveHistory(ctx, "production", curve.KEYRATECalendar, "keyrate:near:far", start, 10001); err == nil {
		t.Fatal("unbounded history accepted")
	}
}

func TestCurveHistoryDeduplicatesQuoteTimesNotScanTimes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := historyMetric(positionStart, 1)
	saveCurveMetric(t, s, "first-quote", "production", m)
	m.ObservedAt = m.ObservedAt.Add(time.Hour)
	m.Value = decimal.NewFromInt(99)
	// Changed scan time/value is auditable but cannot multiply one quote pair.
	saveCurveMetric(t, s, "same-quote-later", "production", m)
	m.ObservedAt = m.ObservedAt.Add(time.Hour)
	m.InstrumentUIDs = []string{"far", "near"}
	m.QuoteTimes = []time.Time{m.QuoteTimes[1], m.QuoteTimes[0]}
	saveCurveMetric(t, s, "same-quote-reordered", "production", m)
	history, err := s.CurveHistory(ctx, "production", m.Strategy, m.Key, m.ObservedAt.Add(time.Hour), 100)
	if err != nil || len(history) != 1 || !history[0].Value.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("duplicate quote warmed history: %+v %v", history, err)
	}
	var scans int
	if err := s.db.QueryRow(`SELECT count(*) FROM curve_scans`).Scan(&scans); err != nil || scans != 3 {
		t.Fatalf("duplicate scans lost audit: %d %v", scans, err)
	}
}

func TestCurveHistorySourceStrategyAndPairIsolation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := historyMetric(positionStart, 1)
	saveCurveMetric(t, s, "prod", "production", m)
	m.Value = decimal.NewFromInt(2)
	saveCurveMetric(t, s, "fixture", "fixture", m)
	m.Strategy = curve.RUONIACalendar
	m.Value = decimal.NewFromInt(3)
	saveCurveMetric(t, s, "other-family", "production", m)
	m = historyMetric(positionStart, 4)
	m.Key = "keyrate:other:far"
	m.InstrumentUIDs[0] = "other"
	saveCurveMetric(t, s, "other-pair", "production", m)
	history, err := s.CurveHistory(ctx, "production", curve.KEYRATECalendar, "keyrate:near:far", positionStart.Add(time.Hour), 100)
	if err != nil || len(history) != 1 || !history[0].Value.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("history contaminated: %+v %v", history, err)
	}
	fixture, err := s.CurveHistory(ctx, "fixture", curve.KEYRATECalendar, "keyrate:near:far", positionStart.Add(time.Hour), 100)
	if err != nil || len(fixture) != 1 || !fixture[0].Value.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("fixture history wrong: %+v %v", fixture, err)
	}
}

func TestCurveScanRequiresRunAndRollsBackMalformedMetric(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := historyMetric(positionStart, 1)
	snap := curve.Snapshot{ObservedAt: positionStart}
	if err := s.SaveCurveScan(ctx, "missing", snap, []curve.Metric{m}, nil, nil); err == nil {
		t.Fatal("orphan scan accepted")
	}
	savePositionRun(t, s, "bad-metric", "production", positionStart)
	bad := m
	bad.QuoteTimes = nil
	if err := s.SaveCurveScan(ctx, "bad-metric", snap, []curve.Metric{m, bad}, nil, nil); err == nil {
		t.Fatal("missing quote times accepted")
	}
	var scans, metrics int
	if err := s.db.QueryRow(`SELECT count(*) FROM curve_scans`).Scan(&scans); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM curve_metrics`).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if scans != 0 || metrics != 0 {
		t.Fatalf("partial curve audit committed: scans=%d metrics=%d", scans, metrics)
	}
	if err := s.SaveCurveScan(ctx, "bad-metric", snap, []curve.Metric{m}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCurveScan(ctx, "bad-metric", snap, nil, nil, nil); err == nil {
		t.Fatal("immutable scan overwritten")
	}
}

func TestCurveActiveIdentityIgnoresDirectionAndQuantity(t *testing.T) {
	candidate := curve.Candidate{Key: "first-plan", Strategy: curve.KEYRATECalendar, Direction: "buy_near_sell_far", Metric: historyMetric(positionStart, 1), Legs: []curve.Leg{{Instrument: curve.Instrument{UID: "near"}, Side: "buy", Lots: 1}, {Instrument: curve.Instrument{UID: "far"}, Side: "sell", Lots: 1}}}
	first, err := curveCandidatePairKey(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Key = "opposite-larger-plan"
	candidate.Direction = "sell_near_buy_far"
	candidate.Legs[0], candidate.Legs[1] = candidate.Legs[1], candidate.Legs[0]
	for i := range candidate.Legs {
		candidate.Legs[i].Lots = 10
	}
	second, err := curveCandidatePairKey(candidate)
	if err != nil || second != first {
		t.Fatalf("direction/size bypassed identity: %q %q %v", first, second, err)
	}
	candidate.Metric.InstrumentUIDs = []string{"different", "far"}
	if _, err := curveCandidatePairKey(candidate); err == nil {
		t.Fatal("metric identity can differ from actual traded legs")
	}
}
