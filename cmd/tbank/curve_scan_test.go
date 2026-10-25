package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/state"
)

func curveSample(t *testing.T) ([]curve.Snapshot, curveConfig) {
	t.Helper()
	snapshots, err := readCurveFixture("../../testdata/curves.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadCurveConfig("../../config/curves.json")
	if err != nil {
		t.Fatal(err)
	}
	return snapshots, c
}

func TestCurveFixtureWarmsOpensClosesAndReplays(t *testing.T) {
	snapshots, c := curveSample(t)
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opened, closed := 0, 0
	for i := range snapshots {
		r, e := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshots[i], nil)
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Errors) > 0 {
			t.Fatal(r.Errors)
		}
		if i < 30 && r.Opened != 0 {
			t.Fatal("opened before baseline warmup")
		}
		opened += r.Opened
		closed += r.Closed
	}
	if opened != 1 || closed != 1 {
		t.Fatalf("opened=%d closed=%d", opened, closed)
	}
	positions, err := store.ListCurvePositions(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].Status != "closed" || positions[0].Entry.MinimumTariff == "" || positions[0].Latest == nil {
		t.Fatalf("unexpected lifecycle: %+v", positions)
	}
	if len(positions[0].Latest.Scenarios) != 3 {
		t.Fatal("missing tariff scenarios")
	}
	for _, s := range positions[0].Latest.Scenarios {
		if !s.TradingFees.IsPositive() || !s.NetProfit.IsPositive() {
			t.Fatalf("bad closed economics: %+v", s)
		}
	}
	for i := range snapshots {
		r, e := runCurveScan(ctx, store, nil, c, "fixture", "", &snapshots[i], nil)
		if e != nil {
			t.Fatal(e)
		}
		if r.Opened+r.Closed+r.Marked > 0 {
			t.Fatal("replay duplicated position lifecycle")
		}
	}
	// Persisted fixture history must never seed sandbox baselines.
	r, err := runCurveScan(ctx, store, nil, c, "sandbox", "", &snapshots[30], nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Opened != 0 {
		t.Fatal("fixture baseline leaked into sandbox")
	}
}

func TestCurveConfigDurationsAreExplicitAndStrict(t *testing.T) {
	_, c := curveSample(t)
	if c.Policy.MaxHolding != 14*24*time.Hour || c.Policy.MinHistorySpan != 24*time.Hour || c.Policy.AllowDurationProxy {
		t.Fatal("unsafe defaults")
	}
	data, err := os.ReadFile("../../config/curves.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(data), `"24h"`, `86400`, 1), strings.Replace(string(data), `"bond_lots"`, `"bond_lots_typo"`, 1), strings.Replace(string(data), `"min_history": 30`, `"min_history": 0`, 1)} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err = os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = loadCurveConfig(path); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestCurvePreviewAndQuoteGates(t *testing.T) {
	snapshots, c := curveSample(t)
	s := snapshots[30]
	legs := []curve.Leg{{Instrument: s.Instruments[0], Side: "buy", Lots: 2}, {Instrument: s.Instruments[1], Side: "sell", Lots: 1}}
	candidate := curve.Candidate{Snapshot: s, Policy: c.Policy.Policy, Legs: legs, Previews: []curve.LegPreview{{UID: legs[0].Instrument.UID, Status: "matched", InitialMargin: decimal.NewFromInt(2000)}, {UID: legs[1].Instrument.UID, Status: "matched", InitialMargin: decimal.NewFromInt(1000)}}}
	if !curvePreviewMatches(candidate) || !curveCandidateFresh(candidate, s.ObservedAt) {
		t.Fatal("valid gate rejected")
	}
	candidate.Previews[0].InitialMargin = decimal.NewFromInt(2001)
	if curvePreviewMatches(candidate) {
		t.Fatal("underfunded actual margin accepted")
	}
	if curveCandidateFresh(candidate, s.ObservedAt.Add(time.Minute)) {
		t.Fatal("expired quotes accepted")
	}
	candidate.ReferenceUIDs = []string{"pricing-peer"}
	peer := s.Books[legs[0].Instrument.UID]
	peer.UID = "pricing-peer"
	peer.Time = s.ObservedAt.Add(-time.Minute)
	candidate.Snapshot.Books[peer.UID] = peer
	if curveCandidateFresh(candidate, s.ObservedAt) {
		t.Fatal("fresh legs hid an expired curve-fitting input")
	}
	candidate.Previews[0] = candidate.Previews[1]
	if curvePreviewMatches(candidate) {
		t.Fatal("duplicate leg preview accepted")
	}
}

func TestCurveFixtureRejectsReversedTimeAndNotifications(t *testing.T) {
	snapshots, _ := curveSample(t)
	path := filepath.Join(t.TempDir(), "fixture.json")
	data, _ := json.Marshal([]curve.Snapshot{snapshots[1], snapshots[0]})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCurveFixture(path); err == nil {
		t.Fatal("reversed fixture accepted")
	}
	if err := cmdCurveScan([]string{"-fixture", path, "-notify"}); err == nil {
		t.Fatal("fixture Telegram enabled")
	}
}

type fakeCurveNotifier struct {
	calls int
	fail  bool
}

func (n *fakeCurveNotifier) SendCurvePosition(context.Context, curve.Position) error {
	n.calls++
	if n.fail {
		return errors.New("delivery failed")
	}
	return nil
}

func TestCurveClosedNotificationRetriesWithoutAnotherMark(t *testing.T) {
	_, c := curveSample(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := curveReport{Source: "fixture", Positions: []curve.Position{{ID: 1, Status: "closed", Source: "fixture"}}}
	n := &fakeCurveNotifier{fail: true}
	ctx := context.Background()
	notifyCurvePositions(ctx, store, n, &r, c, nil)
	if n.calls != 1 || r.Notifications != 0 {
		t.Fatal("first failed delivery was not attempted")
	}
	n.fail = false
	notifyCurvePositions(ctx, store, n, &r, c, nil)
	if n.calls != 2 || r.Notifications != 1 {
		t.Fatal("unchanged final position was not retried")
	}
	notifyCurvePositions(ctx, store, n, &r, c, nil)
	if n.calls != 2 {
		t.Fatal("delivered final status was sent twice")
	}
}
