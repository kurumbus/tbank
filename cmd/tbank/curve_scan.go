package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"tbank/internal/client"
	"tbank/internal/config"
	"tbank/internal/curve"
	"tbank/internal/market"
	"tbank/internal/notify"
	"tbank/internal/state"
)

const curveScanUsage = `usage: tbank curve-scan [-config config/curves.json] [-fixture testdata/curves.json] [-state path] [-notify]

Scans OFZ/RGBI and KEYRATE, RUONIA, RGBI calendar relative-value strategies.
Execution is always MOCK. No brokerage orders are placed.
Historical baselines require distinct, earlier quote observations.
Fixtures are a chronological JSON array of snapshots; Telegram is disabled.`

type curveProvider interface {
	CurveSnapshot(context.Context, curve.Selection) (curve.Snapshot, error)
	CurvePreview(context.Context, []curve.Leg, string) ([]curve.LegPreview, error)
}

type curveReport struct {
	RunID         string           `json:"run_id"`
	Source        string           `json:"source"`
	Execution     string           `json:"execution"`
	Scan          curve.Scan       `json:"scan"`
	Opened        int              `json:"positions_opened"`
	Marked        int              `json:"positions_marked"`
	Closed        int              `json:"positions_closed"`
	Awaiting      int              `json:"positions_awaiting_settlement"`
	Positions     []curve.Position `json:"positions"`
	Notifications int              `json:"notifications_sent"`
	Errors        []string         `json:"errors,omitempty"`
}

func cmdCurveScan(args []string) error {
	fs := flag.NewFlagSet("curve-scan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "config/curves.json", "curve settings")
	statePath := fs.String("state", "", "override base database path")
	fixturePath := fs.String("fixture", "", "offline chronological snapshots")
	send := fs.Bool("notify", false, "send Telegram alerts")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError{msg: curveScanUsage}
	}
	if *fixturePath != "" && *send {
		return fmt.Errorf("fixture scans cannot send Telegram notifications")
	}
	c, err := loadCurveConfig(*path)
	if err != nil {
		return err
	}
	if *statePath != "" {
		c.StatePath = *statePath
	}
	if err = config.LoadDotEnv(".env"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	var p curveProvider
	var account string
	var fixtures []curve.Snapshot
	source := "production"
	if *fixturePath != "" {
		source = "fixture"
		c.StatePath += ".fixture"
		fixtures, err = readCurveFixture(*fixturePath)
		if err != nil {
			return err
		}
	} else {
		settings, e := config.Load()
		if e != nil {
			return e
		}
		if e = validateScanConnection(settings); e != nil {
			return e
		}
		account = settings.AccountID
		if settings.Endpoint == "sandbox-invest-public-api.tbank.ru:443" {
			source = "sandbox"
			c.StatePath += ".sandbox"
		}
		api, e := client.Dial(ctx, settings)
		if e != nil {
			return e
		}
		defer api.Stop()
		p = market.New(api)
	}
	var telegram *notify.Client
	if *send {
		telegram, err = notify.NewFromEnv()
		if err != nil {
			return err
		}
		telegram.Source = source
	}
	store, err := state.Open(c.StatePath)
	if err != nil {
		return err
	}
	defer store.Close()
	iterations := len(fixtures)
	if source != "fixture" {
		iterations = 1
	}
	reports := make([]curveReport, 0, iterations)
	failures := 0
	for i := 0; i < iterations; i++ {
		var fixture *curve.Snapshot
		if source == "fixture" {
			fixture = &fixtures[i]
		}
		r, e := runCurveScan(ctx, store, p, c, source, account, fixture, telegram)
		if e != nil {
			return e
		}
		reports = append(reports, r)
		failures += len(r.Errors)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err = enc.Encode(reports); err != nil {
		return err
	}
	last := reports[len(reports)-1]
	fmt.Fprintf(os.Stderr, "curve scan %s: %d metrics, %d candidates, %d opened, %d marked, %d closed, %d awaiting settlement\n", source, len(last.Scan.Metrics), len(last.Scan.Candidates), last.Opened, last.Marked, last.Closed, last.Awaiting)
	if failures > 0 {
		return fmt.Errorf("curve scan completed with %d operational errors; inspect JSON report", failures)
	}
	return nil
}

func runCurveScan(ctx context.Context, store *state.Store, p curveProvider, c curveConfig, source, account string, fixture *curve.Snapshot, telegram *notify.Client) (curveReport, error) {
	r := curveReport{RunID: uuid.NewString(), Source: source, Execution: "mock_only"}
	positions, err := store.ListCurvePositions(ctx, source)
	if err != nil {
		return r, err
	}
	selection := curve.Selection{BondTickers: c.BondTickers, FutureCount: c.FutureCount, MaxFutureDays: c.MaxFutureDays, Depth: c.Depth}
	seen := map[string]bool{}
	for _, position := range positions {
		if position.Status == "closed" {
			continue
		}
		for _, leg := range position.Entry.Legs {
			if !seen[leg.Instrument.UID] {
				selection.Tracked = append(selection.Tracked, leg.Instrument)
				seen[leg.Instrument.UID] = true
			}
		}
	}
	var snap curve.Snapshot
	if fixture != nil {
		snap = *fixture
	} else {
		snap, err = p.CurveSnapshot(ctx, selection)
	}
	if err != nil {
		r.Errors = append(r.Errors, "curve market data: "+err.Error())
		snap = curve.Snapshot{ObservedAt: time.Now().UTC()}
	} else {
		r.Scan, err = evaluateCurveHistory(ctx, store, source, snap, c.Policy.Policy)
		if err != nil {
			return r, err
		}
		// A fresh complete snapshot rechecks every leg and reference before entry.
		// The first observation is not inserted into history or used as its own target.
		if fixture == nil && hasCurveCandidate(r.Scan) {
			refreshed, e := p.CurveSnapshot(ctx, selection)
			if e != nil {
				r.Errors = append(r.Errors, "curve recheck: "+e.Error())
				for j := range r.Scan.Candidates {
					blockCurveCandidate(&r.Scan.Candidates[j], "market-data recheck failed")
				}
			} else {
				snap = refreshed
				r.Scan, err = evaluateCurveHistory(ctx, store, source, snap, c.Policy.Policy)
				if err != nil {
					return r, err
				}
			}
		}
	}
	if source == "production" {
		for i := range r.Scan.Candidates {
			candidate := &r.Scan.Candidates[i]
			if candidate.MinimumTariff == "" {
				continue
			}
			previews, e := p.CurvePreview(ctx, candidate.Legs, account)
			candidate.Previews = previews
			if e != nil {
				blockCurveCandidate(candidate, "broker fee preview failed: "+e.Error())
				r.Errors = append(r.Errors, "curve fee preview unavailable")
				continue
			}
			if !curvePreviewMatches(*candidate) {
				blockCurveCandidate(candidate, "broker fee or margin preview did not match modeled entry")
				r.Errors = append(r.Errors, "curve fee/margin preview mismatch")
			}
		}
	} else {
		for i := range r.Scan.Candidates {
			for _, leg := range r.Scan.Candidates[i].Legs {
				r.Scan.Candidates[i].Previews = append(r.Scan.Candidates[i].Previews, curve.LegPreview{UID: leg.Instrument.UID, Status: source + "_unverified", Message: "Modeled tariff; no production broker fee validation"})
			}
		}
	}
	if fixture == nil {
		for i := range r.Scan.Candidates {
			if !curveCandidateFresh(r.Scan.Candidates[i], time.Now().UTC()) {
				blockCurveCandidate(&r.Scan.Candidates[i], "quotes expired before mock entry")
			}
		}
	}
	if r.Scan.ObservedAt.IsZero() {
		r.Scan.ObservedAt = snap.ObservedAt
	}
	r.Scan.Skips = append(r.Scan.Skips, snap.Warnings...)
	if len(snap.Books) == 0 && len(r.Errors) == 0 {
		r.Errors = append(r.Errors, "no usable curve order books; inspect snapshot warnings")
	}
	if err = store.SaveRun(ctx, r.RunID, snap.ObservedAt, source, nil, r.Scan.Skips); err != nil {
		return r, err
	}
	if err = store.SaveCurveScan(ctx, r.RunID, snap, r.Scan.Metrics, r.Scan.Candidates, r.Scan.Skips); err != nil {
		return r, err
	}
	touched := map[string]bool{}
	changed := map[int64]bool{}
	markSnapshot := snap
	if fixture == nil {
		// Fee previews and persistence may take time. Existing positions must
		// pass freshness checks at the actual mark time as well as at discovery.
		markSnapshot.ObservedAt = time.Now().UTC()
	}
	for _, position := range positions {
		if position.Status == "closed" {
			continue
		}
		touched[curvePairIdentity(position.Entry)] = true
		var updated curve.Position
		var didChange bool
		last := curve.LastTrade(position)
		if !last.IsZero() && !markSnapshot.ObservedAt.Before(last) {
			updated, didChange, err = store.AwaitCurveSettlement(ctx, r.RunID, position.ID, markSnapshot.ObservedAt)
		} else {
			updated, didChange, err = store.RecordCurveMark(ctx, r.RunID, position.ID, markSnapshot)
		}
		if err != nil {
			if _, _, e := store.RecordCurvePositionError(ctx, r.RunID, position.ID, markSnapshot.ObservedAt, err.Error()); e != nil {
				return r, e
			}
			r.Errors = append(r.Errors, fmt.Sprintf("curve position %d: %s", position.ID, err))
			continue
		}
		if didChange {
			changed[updated.ID] = true
			switch updated.Status {
			case "closed":
				r.Closed++
			case "awaiting_settlement":
				r.Awaiting++
			default:
				r.Marked++
			}
		}
	}
	for _, candidate := range r.Scan.Candidates {
		if candidate.MinimumTariff == "" || touched[curvePairIdentity(candidate)] {
			continue
		}
		if fixture == nil && !curveCandidateFresh(candidate, time.Now().UTC()) {
			continue
		}
		position, inserted, e := store.OpenCurvePosition(ctx, r.RunID, candidate)
		if e != nil {
			r.Errors = append(r.Errors, "open curve position: "+e.Error())
			continue
		}
		if inserted {
			r.Opened++
			changed[position.ID] = true
			touched[curvePairIdentity(candidate)] = true
		}
	}
	r.Positions, err = store.ListCurvePositions(ctx, source)
	if err != nil {
		return r, err
	}
	if telegram != nil {
		notifyCurvePositions(ctx, store, telegram, &r, c, changed)
	}
	return r, nil
}

func evaluateCurveHistory(ctx context.Context, store *state.Store, source string, snap curve.Snapshot, policy curve.Policy) (curve.Scan, error) {
	initial, err := curve.Evaluate(snap, policy, nil)
	if err != nil {
		return initial, err
	}
	var history []curve.Metric
	seen := map[string]bool{}
	for _, metric := range initial.Metrics {
		key := metric.Strategy + ":" + metric.Key
		if seen[key] {
			continue
		}
		seen[key] = true
		values, e := store.CurveHistory(ctx, source, metric.Strategy, metric.Key, snap.ObservedAt, 1000)
		if e != nil {
			return initial, e
		}
		history = append(history, values...)
	}
	return curve.Evaluate(snap, policy, history)
}

func hasCurveCandidate(scan curve.Scan) bool {
	for _, c := range scan.Candidates {
		if c.MinimumTariff != "" {
			return true
		}
	}
	return false
}
func blockCurveCandidate(c *curve.Candidate, reason string) {
	c.MinimumTariff = ""
	c.Warnings = append(c.Warnings, reason)
	for i := range c.Scenarios {
		c.Scenarios[i].Qualifies = false
	}
}
func curvePairIdentity(c curve.Candidate) string {
	ids := make([]string, 0, len(c.Legs))
	for _, l := range c.Legs {
		ids = append(ids, l.Instrument.UID)
	}
	sort.Strings(ids)
	return c.Strategy + ":" + strings.Join(ids, ",")
}
func curveCandidateFresh(c curve.Candidate, now time.Time) bool {
	if c.Strategy == curve.OFZRGBI {
		ref := c.Snapshot.RGBIReference
		if !ref.Known || ref.AsOf.IsZero() || ref.AsOf.After(now) || now.Sub(ref.AsOf) > c.Policy.MaxReferenceAge {
			return false
		}
	}
	uids := append([]string(nil), c.ReferenceUIDs...)
	for _, leg := range c.Legs {
		uids = append(uids, leg.Instrument.UID)
	}
	for _, uid := range uids {
		b, ok := c.Snapshot.Books[uid]
		if !ok || b.Time.IsZero() || b.Time.After(now) || now.Sub(b.Time) > c.Policy.MaxQuoteAge {
			return false
		}
	}
	return len(c.Legs) == 2
}

func curvePreviewMatches(c curve.Candidate) bool {
	if len(c.Previews) != len(c.Legs) {
		return false
	}
	seen := map[string]bool{}
	for _, leg := range c.Legs {
		found := false
		for _, p := range c.Previews {
			if p.UID != leg.Instrument.UID {
				continue
			}
			if seen[p.UID] || p.Status != "matched" {
				return false
			}
			seen[p.UID] = true
			found = true
			margin := leg.Instrument.InitialMarginBuy
			if leg.Side == "sell" {
				margin = leg.Instrument.InitialMarginSell
			}
			if leg.Instrument.Kind == "future" && p.InitialMargin.GreaterThan(margin.Mul(decimal.NewFromInt(leg.Lots))) {
				return false
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type curveNotifier interface {
	SendCurvePosition(context.Context, curve.Position) error
}

func notifyCurvePositions(ctx context.Context, store *state.Store, telegram curveNotifier, r *curveReport, c curveConfig, changed map[int64]bool) {
	for _, p := range r.Positions {
		terminal := p.Status == "closed" || p.Status == "awaiting_settlement"
		if !changed[p.ID] && !terminal {
			continue
		}
		scenarios := p.Entry.Scenarios
		if p.Latest != nil {
			scenarios = p.Latest.Scenarios
		}
		best := decimal.Zero
		for _, s := range scenarios {
			if s.ExcessProfit.GreaterThan(best) {
				best = s.ExcessProfit
			}
		}
		key := fmt.Sprintf("%s:curve:%d:%s", r.Source, p.ID, p.Status)
		now := time.Now().UTC()
		cooldown := time.Duration(c.CooldownMinutes) * time.Minute
		if terminal {
			// Final states are immutable notifications. Retry failed deliveries on
			// later scans, but do not resend a delivered result every daily cooldown.
			cooldown = 100 * 365 * 24 * time.Hour
		}
		ok, err := store.ShouldNotify(ctx, key, now, best, cooldown, c.AlertImprovement)
		if err != nil {
			r.Errors = append(r.Errors, "curve alert ledger: "+err.Error())
			continue
		}
		if !ok {
			continue
		}
		if err = telegram.SendCurvePosition(ctx, p); err != nil {
			r.Errors = append(r.Errors, "curve Telegram: "+err.Error())
			continue
		}
		if err = store.MarkNotified(ctx, key, now, best); err != nil {
			r.Errors = append(r.Errors, "curve notification ledger: "+err.Error())
			continue
		}
		r.Notifications++
	}
}

func readCurveFixture(path string) ([]curve.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 32<<20))
	d.DisallowUnknownFields()
	var snapshots []curve.Snapshot
	if err = d.Decode(&snapshots); err != nil {
		return nil, err
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("curve fixture must contain one JSON array")
	}
	if len(snapshots) == 0 || len(snapshots) > 1000 {
		return nil, fmt.Errorf("curve fixture requires 1..1000 snapshots")
	}
	for i, s := range snapshots {
		if s.ObservedAt.IsZero() || (i > 0 && !s.ObservedAt.After(snapshots[i-1].ObservedAt)) {
			return nil, fmt.Errorf("curve fixture timestamps must be strictly increasing")
		}
	}
	return snapshots, nil
}
