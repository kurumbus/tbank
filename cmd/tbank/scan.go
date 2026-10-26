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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"tbank/internal/client"
	"tbank/internal/config"
	"tbank/internal/market"
	"tbank/internal/notify"
	"tbank/internal/scanner"
	"tbank/internal/state"
)

const scanUsage = `usage: tbank scan [-config config/scanner.json] [-fixture testdata/carry.json] [-state path] [-notify]

One scan of fully funded RUB shares against matching dated deliverable futures.
Execution is always MOCK: this command never places brokerage orders.
Quotes require a production or sandbox TBANK_ENDPOINT and explicit TBANK_ACCOUNT_ID.
-fixture runs offline against its recorded simulation clock and disables Telegram.
-notify sends qualifying, rechecked candidates to TELEGRAM_CHAT_ID using TELEGRAM_BOT_TOKEN.`

type scanProvider interface {
	Discover(context.Context, []string, int) ([]scanner.Pair, error)
	Snapshot(context.Context, scanner.Pair, int) (scanner.Snapshot, error)
	Preview(context.Context, scanner.Result, string) (scanner.Preview, error)
}

type scanDataError struct{ err error }

func (e scanDataError) Error() string { return e.err.Error() }
func (e scanDataError) Unwrap() error { return e.err }

type scanReport struct {
	RunID             string             `json:"run_id"`
	Started           time.Time          `json:"started_at"`
	Source            string             `json:"source"`
	Execution         string             `json:"execution"`
	Results           []scanner.Result   `json:"results"`
	Skipped           []string           `json:"skipped"`
	PositionsOpened   int                `json:"positions_opened"`
	PositionsMarked   int                `json:"positions_marked"`
	PositionsClosed   int                `json:"positions_closed"`
	PositionsAwaiting int                `json:"positions_awaiting_settlement"`
	Positions         []scanner.Position `json:"positions"`
	Notifications     int                `json:"notifications_sent"`
	Errors            []string           `json:"errors,omitempty"`
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config/scanner.json", "scanner settings")
	statePath := fs.String("state", "", "override base state database path")
	fixturePath := fs.String("fixture", "", "offline snapshot fixture")
	send := fs.Bool("notify", false, "send Telegram alerts")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: scanUsage}
	}
	if fs.NArg() != 0 {
		return usageError{msg: scanUsage}
	}
	if *fixturePath != "" && *send {
		return fmt.Errorf("fixture scans cannot send Telegram notifications")
	}
	c, err := loadScanConfig(*configPath)
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
	var p scanProvider
	var accountID string
	source := "production"
	var fixture []scanner.Snapshot
	if *fixturePath != "" {
		source = "fixture"
		c.StatePath += ".fixture"
		fixture, err = readFixture(*fixturePath)
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
		accountID = settings.AccountID
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
	report := scanReport{RunID: uuid.NewString(), Started: time.Now().UTC(), Source: source, Execution: "mock_only", Results: []scanner.Result{}, Skipped: []string{}}
	profiles := scanner.Profiles()
	for i := range profiles {
		profiles[i].FutureTurnover = c.FutureTurnover
	}
	if source == "fixture" {
		for _, snap := range fixture {
			r, e := scanner.Evaluate(snap, c.assumptions(), profiles)
			if e != nil {
				report.Skipped = append(report.Skipped, snap.Pair.Future.Ticker+": "+e.Error())
				continue
			}
			r.Preview = scanner.Preview{Status: "fixture_unverified", Message: "Synthetic data; not a broker fee preview or executable quote"}
			report.Results = append(report.Results, r)
		}
	} else {
		pairs, e := p.Discover(ctx, c.Tickers, c.MaxDays)
		if e != nil {
			report.Errors = append(report.Errors, "discover: "+e.Error())
		}
		if w, ok := p.(interface{ Warnings() []string }); ok {
			report.Skipped = append(report.Skipped, w.Warnings()...)
		}
		for _, pair := range pairs {
			r, e := evaluateQuotes(ctx, p, pair, c, profiles, accountID, source == "production")
			if e != nil {
				report.Skipped = append(report.Skipped, pair.Future.Ticker+": "+e.Error())
				var dataError scanDataError
				if errors.As(e, &dataError) {
					report.Errors = append(report.Errors, pair.Future.Ticker+": market data request failed")
				}
				continue
			}
			if r.Preview.Status == "mismatch" || r.Preview.Status == "unavailable" {
				report.Errors = append(report.Errors, pair.Future.Ticker+": "+r.Preview.Message)
			}
			report.Results = append(report.Results, r)
		}
	}
	if source != "fixture" {
		for i := range report.Results {
			if !quotesFreshAt(report.Results[i], time.Now().UTC(), c) {
				disableCandidate(&report.Results[i], "Quotes aged beyond the limit while scanning; suppressed before mock entry/notification")
			}
		}
	}
	// Persist the evidence before recording a mock entry or attempting a notification.
	if err = store.SaveRun(ctx, report.RunID, report.Started, source, report.Results, report.Skipped); err != nil {
		return err
	}
	clock := time.Now().UTC()
	if source == "fixture" {
		clock = fixture[0].ObservedAt
	}
	loader := positionSnapshotLoader(p, fixture, c.Depth)
	touched, e := updatePositions(ctx, store, &report, clock, loader, c)
	if e != nil {
		return e
	}
	for _, r := range report.Results {
		if _, ok := bestExcess(r); !ok || touched[pairKey(r.Snapshot.Pair)] {
			continue
		}
		if source != "fixture" && !quotesFreshAt(r, time.Now().UTC(), c) {
			continue
		}
		_, inserted, e := store.OpenPosition(ctx, report.RunID, r, c.ExitCaptureFraction)
		if e != nil {
			report.Errors = append(report.Errors, "open position: "+e.Error())
			continue
		}
		if inserted {
			report.PositionsOpened++
		}
	}
	report.Positions, err = store.ListPositions(ctx, source)
	if err != nil {
		return err
	}
	if telegram != nil {
		notifyPositions(ctx, store, telegram, &report, c)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err = enc.Encode(report); err != nil {
		return err
	}
	open, awaiting := positionActivity(scanStatuses(report.Positions))
	result, first := "ok", ""
	if len(report.Errors) > 0 {
		result, first = "error", report.Errors[0]
	}
	recordHeartbeat("scan", source, result, fmt.Sprintf("evaluated=%d skipped=%d opened=%d marked=%d closed=%d awaiting=%d notifications=%d open=%d held_awaiting=%d errors=%d", len(report.Results), len(report.Skipped), report.PositionsOpened, report.PositionsMarked, report.PositionsClosed, report.PositionsAwaiting, report.Notifications, open, awaiting, len(report.Errors)), first)
	if len(report.Errors) > 0 {
		return fmt.Errorf("scan completed with %d operational errors; inspect the JSON report", len(report.Errors))
	}
	return nil
}

func validateScanConnection(s config.Settings) error {
	if s.Endpoint != "invest-public-api.tbank.ru:443" && s.Endpoint != "sandbox-invest-public-api.tbank.ru:443" {
		return fmt.Errorf("scan requires a supported production or sandbox TBANK_ENDPOINT; use -fixture for offline tests")
	}
	if strings.TrimSpace(s.AccountID) == "" {
		return fmt.Errorf("scan requires TBANK_ACCOUNT_ID; account creation is not part of this command")
	}
	return nil
}

func readFixture(path string) ([]scanner.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var v []scanner.Snapshot
	d := json.NewDecoder(io.LimitReader(f, 5<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("fixture must contain one JSON array")
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("empty fixture")
	}
	for _, snap := range v {
		if snap.ObservedAt.IsZero() || !snap.ObservedAt.Equal(v[0].ObservedAt) {
			return nil, fmt.Errorf("one fixture must contain one observation time; replay separate files chronologically")
		}
	}
	return v, nil
}

func evaluateLive(ctx context.Context, p scanProvider, pair scanner.Pair, c scanConfig, profiles []scanner.Profile, accountID string) (scanner.Result, error) {
	return evaluateQuotes(ctx, p, pair, c, profiles, accountID, true)
}

func evaluateQuotes(ctx context.Context, p scanProvider, pair scanner.Pair, c scanConfig, profiles []scanner.Profile, accountID string, requirePreview bool) (scanner.Result, error) {
	snap, err := p.Snapshot(ctx, pair, c.Depth)
	if err != nil {
		return scanner.Result{}, scanDataError{err}
	}
	r, err := scanner.Evaluate(snap, c.assumptions(), profiles)
	if err != nil {
		return r, err
	}
	r.Preview.Status = "not_requested"
	if _, ok := bestExcess(r); !ok {
		return r, nil
	}
	// A second independent snapshot avoids alerting on a gap that disappeared while scanning.
	snap, err = p.Snapshot(ctx, pair, c.Depth)
	if err != nil {
		return scanner.Result{}, scanDataError{fmt.Errorf("recheck: %w", err)}
	}
	r, err = scanner.Evaluate(snap, c.assumptions(), profiles)
	if err != nil {
		return r, fmt.Errorf("recheck: %w", err)
	}
	r.Preview.Status = "not_requested"
	if _, ok := bestExcess(r); !ok {
		return r, nil
	}
	if !requirePreview {
		r.Preview = scanner.Preview{Status: "sandbox_modeled", Message: "Sandbox quotes; commissions are simulated published tariffs, not account-verified fees"}
		return r, nil
	}
	preview, err := p.Preview(ctx, r, accountID)
	if err != nil {
		r.Preview = scanner.Preview{Status: "unavailable", Message: "Broker commission preview failed"}
		disableCandidate(&r, "Commission preview unavailable; no mock entry or alert")
		return r, nil
	}
	r.Preview = preview
	if preview.RequiredInitialMargin.GreaterThan(r.InitialMargin) {
		snap.Pair.Future.InitialMargin = preview.RequiredInitialMargin.Div(decimal.NewFromInt(r.Contracts))
		r, err = scanner.Evaluate(snap, c.assumptions(), profiles)
		if err != nil {
			return r, fmt.Errorf("account margin recheck: %w", err)
		}
		r.Preview = preview
		r.Warnings = append(r.Warnings, "Capital increased to cover the broker's account-specific margin preview")
	}
	if err = verifyPreview(r); err != nil {
		r.Preview.Status = "mismatch"
		r.Preview.Message = err.Error()
		disableCandidate(&r, "Broker fee validation failed; no mock entry or alert")
	} else {
		r.Preview.Status = "matched"
	}
	return r, nil
}

func disableCandidate(r *scanner.Result, reason string) {
	for i := range r.Scenarios {
		r.Scenarios[i].Qualifies = false
	}
	r.Warnings = append(r.Warnings, reason)
}

func verifyPreview(r scanner.Result) error {
	if r.Preview.Status != "ok" {
		return fmt.Errorf("broker preview did not succeed")
	}
	tariff := strings.ToLower(strings.TrimSpace(r.Preview.ActualTariff))
	name := ""
	switch tariff {
	case "investor", "инвестор":
		name = "investor"
	case "trader", "трейдер":
		name = "trader"
	case "premium", "премиум":
		name = "premium"
	default:
		return fmt.Errorf("unrecognized account tariff %q", tariff)
	}
	var selected *scanner.Profile
	// Account turnover can differ from hypothetical scenario turnover. Accept only
	// an exact match to a published band; never invent a rate from the returned fee.
	for _, profile := range scanner.Profiles() {
		if profile.Name == name {
			copy := profile
			selected = &copy
			break
		}
	}
	if selected == nil {
		return errors.New("account tariff has no local fee profile")
	}
	stockValue := r.StockLimit.Mul(r.Snapshot.Pair.Stock.PriceFactor).Mul(decimal.NewFromInt(r.Shares))
	futureValue := r.FutureLimit.Mul(r.Snapshot.Pair.Future.PriceFactor).Mul(decimal.NewFromInt(r.Contracts))
	stockFee := selected.StockFee(stockValue)
	tolerance := decimal.RequireFromString("0.02")
	if stockFee.Sub(r.Preview.StockCommission).Abs().GreaterThan(tolerance) {
		return fmt.Errorf("stock entry preview differs: model=%s broker=%s RUB", stockFee, r.Preview.StockCommission)
	}
	for _, turnover := range []string{"0", "5000000.02", "10000000.02", "12000000.02", "17000000.02"} {
		selected.FutureTurnover = decimal.RequireFromString(turnover)
		futureFee := selected.FutureFee(futureValue, r.Contracts, r.Snapshot.Pair.Future.ExchangeFee)
		if futureFee.Sub(r.Preview.FutureCommission).Abs().LessThanOrEqual(tolerance) {
			return nil
		}
	}
	return fmt.Errorf("future entry preview %s RUB does not match a published rate/floor for %s", r.Preview.FutureCommission, tariff)
}

func quotesFreshAt(r scanner.Result, now time.Time, c scanConfig) bool {
	for _, ts := range []time.Time{r.Snapshot.StockBook.Time, r.Snapshot.FutureBook.Time} {
		age := now.Sub(ts)
		if ts.IsZero() || age < 0 || age > time.Duration(c.QuoteAgeSeconds)*time.Second {
			return false
		}
	}
	return true
}

func bestExcess(r scanner.Result) (decimal.Decimal, bool) {
	var best decimal.Decimal
	found := false
	for _, s := range r.Scenarios {
		if s.Qualifies && (!found || s.ExcessProfit.GreaterThan(best)) {
			best = s.ExcessProfit
			found = true
		}
	}
	return best, found
}
