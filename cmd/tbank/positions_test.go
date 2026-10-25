package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
	"tbank/internal/state"
)

// Exercise the actual CLI and persistent database, including replay after the
// process has closed its connection. All inputs are invented offline quotes.
func TestCLIPositionLifecycleAndReplay(t *testing.T) {
	base := filepath.Join(t.TempDir(), "positions.sqlite")
	config := filepath.Join("..", "..", "config", "scanner.json")
	fixture := func(name string) string { return filepath.Join("..", "..", "testdata", name) }
	scan := func(name string) scanReport {
		t.Helper()
		var report scanReport
		raw, err := captureCommand(t, func() error { return cmdScan([]string{"-config", config, "-state", base, "-fixture", fixture(name)}) })
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	entry := scan("carry.json")
	if entry.PositionsOpened != 1 || len(entry.Positions) != 1 || entry.Positions[0].MinimumTariff != "investor" {
		t.Fatalf("entry: %+v", entry)
	}
	if repeat := scan("carry.json"); repeat.PositionsOpened != 0 || repeat.PositionsMarked != 0 {
		t.Fatal("duplicate entry/mark")
	}
	closed := scan("carry-close.json")
	if closed.PositionsClosed != 1 || closed.PositionsMarked != 1 || closed.PositionsOpened != 0 {
		t.Fatalf("closure counts: %+v", closed)
	}
	p := closed.Positions[0]
	if p.Status != scanner.PositionClosed || p.MinimumTariff != entry.Positions[0].MinimumTariff || p.CloseReason != "capture_target_met" {
		t.Fatalf("closure: %+v", p)
	}
	if !p.Latest.GrossProfit.Equal(decimal.NewFromInt(2800)) || !p.Latest.ResidualBasis.Equal(decimal.NewFromInt(200)) {
		t.Fatal("wrong executable close economics", p.Latest)
	}
	if repeat := scan("carry-close.json"); repeat.PositionsClosed != 0 || repeat.PositionsOpened != 0 {
		t.Fatal("replayed closure mutated lifecycle")
	}
	if repeat := scan("carry.json"); repeat.PositionsOpened != 0 {
		t.Fatal("old entry resurrected closed position")
	}
	raw, err := captureCommand(t, func() error { return cmdPositions([]string{"show", "-state", base + ".fixture", "-id", "1"}) })
	if err != nil {
		t.Fatal(err)
	}
	var show struct {
		Position     scanner.Position
		Observations []state.PositionObservation
	}
	if err := json.Unmarshal(raw, &show); err != nil {
		t.Fatal(err)
	}
	if len(show.Observations) != 2 || show.Observations[0].Kind != "open" || show.Observations[1].Kind != "mark" {
		t.Fatalf("unexpected history: %+v", show.Observations)
	}
}

func TestCLIAwaitsAndSettlesWithoutExpiredQuotes(t *testing.T) {
	base := filepath.Join(t.TempDir(), "settlement.sqlite")
	config := filepath.Join("..", "..", "config", "scanner.json")
	for _, file := range []string{"carry.json", "carry-awaiting.json"} {
		_, err := captureCommand(t, func() error {
			return cmdScan([]string{"-config", config, "-state", base, "-fixture", filepath.Join("..", "..", "testdata", file)})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(base + ".fixture")
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.GetPosition(context.Background(), 1)
	store.Close()
	if err != nil || p.Status != scanner.PositionAwaitingSettlement || p.Latest != nil {
		t.Fatalf("expiry invented a valuation: %+v %v", p, err)
	}
	raw, err := captureCommand(t, func() error {
		return cmdPositions([]string{"settle", "-state", base + ".fixture", "-id", "1", "-input", filepath.Join("..", "..", "testdata", "carry-settlement.json")})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Position scanner.Position
		Changed  bool
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Position.Status != scanner.PositionClosed || result.Position.CloseReason != "verified_settlement" {
		t.Fatalf("settlement: %+v", result)
	}
	for _, scenario := range result.Position.Latest.Scenarios {
		if len(scenario.Fees) != 3 {
			t.Fatal("delivery must have exactly two entry fees and one stock delivery fee", scenario.Fees)
		}
	}
}

func TestTrackedPositionSurvivesMissingDiscoveryAndBadQuotes(t *testing.T) {
	snapshot, c := sample(t)
	entry, err := scanner.Evaluate(snapshot, c.assumptions(), scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SaveRun(ctx, "entry", snapshot.ObservedAt, "fixture", []scanner.Result{entry}, nil); err != nil {
		t.Fatal(err)
	}
	p, _, err := store.OpenPosition(ctx, "entry", entry, c.ExitCaptureFraction)
	if err != nil {
		t.Fatal(err)
	}
	now := snapshot.ObservedAt.Add(time.Hour)
	report := scanReport{RunID: "hourly", Source: "fixture", Started: now}
	if err := store.SaveRun(ctx, report.RunID, now, report.Source, nil, nil); err != nil {
		t.Fatal(err)
	}
	called := false
	touched, err := updatePositions(ctx, store, &report, now, func(_ context.Context, pair scanner.Pair) (scanner.Snapshot, error) {
		called = true
		if pair.Future.UID != snapshot.Pair.Future.UID {
			t.Fatal("did not load persisted identity")
		}
		return scanner.Snapshot{}, errors.New("quotes unavailable")
	}, c)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !touched[pairKey(snapshot.Pair)] || report.PositionsClosed != 0 {
		t.Fatal("existing position not tracked independently")
	}
	history, err := store.Observations(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Kind != "error" || history[1].Error != "quotes unavailable" {
		t.Fatalf("failed observation missing: %+v", history)
	}
	p, err = store.GetPosition(ctx, p.ID)
	if err != nil || p.Status != scanner.PositionOpen || p.Latest != nil {
		t.Fatal("failed quote closed/valued position")
	}
}

func TestPositionsInspectionDoesNotCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if err := cmdPositions([]string{"list", "-state", path}); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("inspection created a database")
	}
}

func captureCommand(t *testing.T, call func() error) ([]byte, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = previous; f.Close() }()
	err = call()
	if closeErr := f.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	raw, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return raw, err
}

func TestPositionAlertUsesFrozenQuoteAge(t *testing.T) {
	s, c := sample(t)
	entry, err := scanner.Evaluate(s, c.assumptions(), scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p, err := scanner.NewPosition(entry, "production", c.ExitCaptureFraction)
	if err != nil {
		t.Fatal(err)
	}
	c.QuoteAgeSeconds = 300
	later := s.ObservedAt.Add(time.Minute)
	if !quotesFreshAt(entry, later, c) {
		t.Fatal("test setup must pass the relaxed current config")
	}
	if positionQuotesFreshAt(p, later) {
		t.Fatal("relaxed current config made old position quotes fresh")
	}
	if !positionQuotesFreshAt(p, s.ObservedAt) {
		t.Fatal("fresh entry quote rejected")
	}
}
