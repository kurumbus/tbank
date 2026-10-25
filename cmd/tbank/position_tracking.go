package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/notify"
	"tbank/internal/scanner"
	"tbank/internal/state"
)

type snapshotLoader func(context.Context, scanner.Pair) (scanner.Snapshot, error)

func pairKey(p scanner.Pair) string { return p.Stock.UID + ":" + p.Future.UID }

// Saved positions are followed independently of the current discovery universe.
func positionSnapshotLoader(p scanProvider, fixture []scanner.Snapshot, depth int) snapshotLoader {
	return func(ctx context.Context, pair scanner.Pair) (scanner.Snapshot, error) {
		if p == nil {
			for _, s := range fixture {
				if pairKey(s.Pair) == pairKey(pair) {
					return s, nil
				}
			}
			return scanner.Snapshot{}, fmt.Errorf("tracked pair is absent from this fixture")
		}
		if refresher, ok := p.(interface {
			RefreshPair(context.Context, scanner.Pair) (scanner.Pair, error)
		}); ok {
			var err error
			pair, err = refresher.RefreshPair(ctx, pair)
			if err != nil {
				return scanner.Snapshot{}, err
			}
		}
		return p.Snapshot(ctx, pair, depth)
	}
}

func updatePositions(ctx context.Context, store *state.Store, report *scanReport, now time.Time, load snapshotLoader, c scanConfig) (map[string]bool, error) {
	positions, err := store.ListPositions(ctx, report.Source)
	if err != nil {
		return nil, err
	}
	touched := map[string]bool{}
	for _, position := range positions {
		if position.Status == scanner.PositionClosed {
			continue
		}
		// Even if this check closes a position, don't reopen the same pair using
		// a candidate snapshot collected earlier in this run.
		touched[pairKey(position.Entry.Snapshot.Pair)] = true
		if position.Status == scanner.PositionAwaitingSettlement {
			continue
		}
		if !now.Before(position.Entry.Snapshot.Pair.Future.LastTrade) {
			_, changed, err := store.AwaitSettlement(ctx, report.RunID, position.ID, now)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("position %d: %v", position.ID, err))
			} else if changed {
				report.PositionsAwaiting++
			}
			continue
		}
		snapshot, markErr := load(ctx, position.Entry.Snapshot.Pair)
		var valuation scanner.Valuation
		if markErr == nil {
			valuation, markErr = scanner.MarkPosition(position, snapshot)
		}
		if errors.Is(markErr, scanner.ErrAwaitingSettlement) {
			_, changed, err := store.AwaitSettlement(ctx, report.RunID, position.ID, snapshot.ObservedAt)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("position %d: %v", position.ID, err))
			} else if changed {
				report.PositionsAwaiting++
			}
			continue
		}
		if markErr == nil && report.Source != "fixture" {
			// Use the opening quote-age policy, not a changed scan configuration.
			age := position.Entry.Assumptions.MaxQuoteAge
			for _, ts := range []time.Time{snapshot.StockBook.Time, snapshot.FutureBook.Time} {
				d := time.Since(ts)
				if ts.IsZero() || d < 0 || d > age {
					markErr = fmt.Errorf("exit quotes became stale before recording")
					break
				}
			}
		}
		if markErr != nil {
			_, _, saveErr := store.RecordPositionError(ctx, report.RunID, position.ID, now, markErr.Error())
			report.Skipped = append(report.Skipped, fmt.Sprintf("position %d: %v", position.ID, markErr))
			if saveErr != nil {
				report.Errors = append(report.Errors, "save position observation: "+saveErr.Error())
			}
			continue
		}
		updated, changed, err := store.RecordValuation(ctx, report.RunID, position.ID, valuation)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("position %d: %v", position.ID, err))
			continue
		}
		if changed {
			report.PositionsMarked++
			if updated.Status == scanner.PositionClosed {
				report.PositionsClosed++
			}
		}
	}
	return touched, nil
}

func notifyPositions(ctx context.Context, store *state.Store, telegram *notify.Client, report *scanReport, c scanConfig) {
	for _, p := range report.Positions {
		key := fmt.Sprintf("%s:position:%d:%s", p.Source, p.ID, p.Status)
		cooldown := time.Duration(c.CooldownMinutes) * time.Minute
		profit := decimal.Zero
		scenarios := p.Entry.Scenarios
		if p.Latest != nil {
			scenarios = p.Latest.Scenarios
		}
		for _, scenario := range scenarios {
			if scenario.Profile == p.MinimumTariff {
				profit = scenario.ExcessProfit
			}
		}
		if p.Status != scanner.PositionOpen {
			// One receipt per terminal/status event; failed sends remain retryable.
			cooldown = 100 * 365 * 24 * time.Hour
		} else {
			if !positionQuotesFreshAt(p, time.Now().UTC()) {
				continue
			}
		}
		due, err := store.ShouldNotify(ctx, key, report.Started, profit, cooldown, c.AlertImprovement)
		if err != nil {
			report.Errors = append(report.Errors, "notification state: "+err.Error())
			continue
		}
		if !due {
			continue
		}
		if err = telegram.SendPosition(ctx, p); err != nil {
			report.Errors = append(report.Errors, "Telegram: "+err.Error())
			continue
		}
		if err = store.MarkNotified(ctx, key, report.Started, profit); err != nil {
			report.Errors = append(report.Errors, "notification receipt: "+err.Error())
			continue
		}
		report.Notifications++
	}
}

// A later configuration change cannot relax an existing position's freshness policy.
func positionQuotesFreshAt(p scanner.Position, now time.Time) bool {
	snapshot := p.Entry.Snapshot
	if p.Latest != nil && p.Latest.Snapshot != nil {
		snapshot = *p.Latest.Snapshot
	}
	for _, ts := range []time.Time{snapshot.StockBook.Time, snapshot.FutureBook.Time} {
		age := now.Sub(ts)
		if ts.IsZero() || age < 0 || age > p.Entry.Assumptions.MaxQuoteAge {
			return false
		}
	}
	return p.Entry.Assumptions.MaxQuoteAge > 0
}
