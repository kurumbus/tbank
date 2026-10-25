package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
	"tbank/internal/state"
)

func resultForMessage() scanner.Result {
	now := time.Date(2026, 10, 25, 12, 30, 0, 0, time.UTC)
	r := scanner.Result{
		Snapshot:  scanner.Snapshot{ObservedAt: now, Pair: scanner.Pair{Stock: scanner.Instrument{Ticker: "SBER"}, Future: scanner.Instrument{Ticker: "SRZ6", Expiry: now.Add(30 * 24 * time.Hour)}}, StockBook: scanner.Book{Time: now}, FutureBook: scanner.Book{Time: now}},
		Contracts: 1, Shares: 100, StockLots: 100,
		StockLimit: decimal.NewFromInt(250), FutureLimit: decimal.NewFromInt(26000),
		Preview:       scanner.Preview{ActualTariff: "trader", Status: "available"},
		InitialMargin: decimal.NewFromInt(4000), ExecutionBuffer: decimal.NewFromInt(60),
		GrossBasis: decimal.NewFromInt(1000), CapitalRequired: decimal.NewFromInt(30000),
		Assumptions: scanner.Assumptions{FutureTurnover: decimal.NewFromInt(5000000), CapitalLimit: decimal.NewFromInt(100000)},
	}
	for i, p := range scanner.Profiles() {
		r.Scenarios = append(r.Scenarios, scanner.Scenario{
			Profile: p.Name, NetProfit: decimal.NewFromInt(int64(100 + i)), ExcessProfit: decimal.NewFromInt(int64(50 + i)), Qualifies: i > 0,
			Fees: []scanner.FeeLine{
				{Name: "stock_entry", Amount: decimal.NewFromInt(int64(10 + i))},
				{Name: "future_entry_including_exchange_floor", Amount: decimal.NewFromInt(int64(20 + i))},
				{Name: "stock_delivery_estimate", Amount: decimal.NewFromInt(int64(30 + i))},
			},
		})
	}
	return r
}

func TestSendPlainTextWithAllScenarios(t *testing.T) {
	r := resultForMessage()
	r.Snapshot.Pair.Stock.Ticker = "<b>SBER</b>_[x]"
	r.Preview.ActualTariff = "trader\nforged heading"
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/bottest-secret/sendMessage" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	c, err := New("test-secret", "123")
	if err != nil {
		t.Fatal(err)
	}
	if c.httpClient.Timeout != 10*time.Second {
		t.Fatalf("timeout %s", c.httpClient.Timeout)
	}
	c.baseURL = srv.URL
	c.Source = "production"
	if err := c.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["parse_mode"]; ok {
		t.Fatal("parse_mode would allow markup injection")
	}
	text := payload["text"].(string)
	if payload["chat_id"] != "123" || !strings.Contains(text, "<b>SBER</b>_[x]") || strings.Contains(text, "trader\nforged heading") {
		t.Fatalf("bad plain-text message: %s", text)
	}
	for i, s := range r.Scenarios {
		want := fmt.Sprintf("%s: %d.00 / %d.00 | fees %d.00 / %d.00 / %d.00", s.Profile, 100+i, 50+i, 10+i, 20+i, 30+i)
		if !strings.Contains(text, want) {
			t.Fatalf("missing scenario %q", want)
		}
	}
	for _, want := range []string{"MOCK / carry candidate", "Source: production", "Actual tariff: trader forged heading", "Minimum eligible tariff: trader", "Premium levels use the same trading fees", "Expiry:", "100 shares", "Quote times:", "Initial margin: 4000.00 RUB", "execution buffer: 60.00 RUB", "prior futures turnover 5000000.00 RUB", "Fees: stock entry / future entry / estimated delivery"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(text, "monthly") || strings.Contains(text, "Subscriptions") {
		t.Fatal("subscription charges should not be shown")
	}
}

func TestSendErrorsDoNotLeakToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"http", 401, `test-secret`}, {"rejected", 200, `{"ok":false,"error_code":400,"description":"test-secret"}`}, {"invalid", 200, `test-secret`}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c, _ := New("test-secret", "123")
			c.baseURL = srv.URL
			err := c.Send(context.Background(), resultForMessage())
			if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
	c, _ := New("test-secret", "123")
	c.baseURL = "http://127.0.0.1:1"
	err := c.Send(context.Background(), resultForMessage())
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestMessageBoundedForUnicodeAndUntrustedLabels(t *testing.T) {
	r := resultForMessage()
	r.Warnings = []string{strings.Repeat("🙂<>[]\n", 2000)}
	r.Snapshot.Pair.Stock.Ticker = strings.Repeat("🙂", 1000)
	for i := range r.Scenarios {
		r.Scenarios[i].Profile = strings.Repeat(fmt.Sprintf("profile%d🙂", i), 100)
	}
	text := FormatMessage(strings.Repeat("fixture🙂", 1000), r)
	if !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > 4096 {
		t.Fatalf("invalid Telegram length %d", len(utf16.Encode([]rune(text))))
	}
	if !strings.Contains(text, "snapshot only") || !strings.Contains(text, "Minimum eligible tariff:") {
		t.Fatal("essential message content was truncated")
	}
	for i := range r.Scenarios {
		if !strings.Contains(text, fmt.Sprintf("fees %d.00 / %d.00 / %d.00", 10+i, 20+i, 30+i)) {
			t.Fatalf("scenario %d fee breakdown truncated", i)
		}
	}
}

func TestMissingFeeIsUnknownNotZero(t *testing.T) {
	r := resultForMessage()
	r.Scenarios[0].Fees = nil
	if !strings.Contains(FormatMessage("production", r), "investor: 100.00 / 50.00 | fees unknown / unknown / unknown") {
		t.Fatal("missing fee inferred as zero")
	}
}

func TestMinimumEligibleTariffUsesFamilyOrder(t *testing.T) {
	scenarios := []scanner.Scenario{{Profile: "premium", Qualifies: true}, {Profile: "trader", Qualifies: true}, {Profile: "investor", Qualifies: false}}
	r := resultForMessage()
	r.Scenarios = scenarios
	if got := minimumTariff(r); got != "trader" {
		t.Fatalf("minimum tariff %q", got)
	}
	scenarios[2].Qualifies = true
	if got := minimumTariff(r); got != "investor" {
		t.Fatalf("minimum tariff %q", got)
	}
	if got := minimumTariff(scanner.Result{}); got != "none" {
		t.Fatalf("missing qualification inferred: %q", got)
	}
}

func positionForMessage() scanner.Position {
	e := resultForMessage()
	e.StockCost = decimal.NewFromInt(25000)
	e.FutureValue = decimal.NewFromInt(26000)
	e.CapitalRequired = decimal.NewFromInt(30000)
	e.Snapshot.Pair.Future.PriceFactor = decimal.NewFromInt(1)
	return scanner.Position{
		ID: 42, Source: "fixture", Status: "open", Entry: e,
		MinimumTariff: "trader", OpenedAt: e.Snapshot.ObservedAt,
		CaptureTarget: decimal.RequireFromString("0.80"),
		Latest: &scanner.Valuation{
			ObservedAt: e.Snapshot.ObservedAt.Add(24 * time.Hour), Kind: "mark",
			ResidualBasis: decimal.NewFromInt(200), CapturedFraction: decimal.RequireFromString("0.80"),
			CashBenchmark: decimal.NewFromInt(25), Days: decimal.NewFromInt(1),
			Scenarios: []scanner.Scenario{
				{Profile: "investor", NetProfit: decimal.NewFromInt(400), ExcessProfit: decimal.NewFromInt(375), Qualifies: true},
				{Profile: "trader", NetProfit: decimal.NewFromInt(600), ExcessProfit: decimal.NewFromInt(575), Qualifies: true},
				{Profile: "premium", NetProfit: decimal.NewFromInt(650), ExcessProfit: decimal.NewFromInt(625), Qualifies: true},
			},
		},
	}
}

func TestPositionMessageShowsFixedTariffAndPairedFillEconomics(t *testing.T) {
	p := positionForMessage()
	text := FormatPositionMessage("production", p)
	for _, want := range []string{
		"MOCK / OPEN | position #42", "Source: fixture", "opened: 2026-10-25 12:30:00Z",
		"Minimum eligible tariff at entry: trader (fixed)",
		"Entry BUY SBER: 100 shares; 25000.00 RUB; avg 250 RUB/share",
		"Entry SELL SRZ6: 1 contracts; 26000.00 RUB; avg 26000 quote units",
		"Latest executable close gap: 200.00 RUB | captured: 80.00%",
		"Valuation: 2026-10-26 12:30:00Z | mark",
		"Elapsed cash benchmark: 25.00 RUB (1.00 days)",
		"investor: 400.00 / 375.00", "trader: 600.00 / 575.00", "premium: 650.00 / 625.00",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "at entry: investor") || strings.Contains(text, "monthly") || strings.Contains(text, "premium_bronze") {
		t.Fatalf("tariff or fee policy wrong: %s", text)
	}
}

func TestOpeningMessageShowsEntryProjectionAndFeeValidation(t *testing.T) {
	for _, validation := range []string{"matched", "sandbox_modeled", "fixture_unverified"} {
		t.Run(validation, func(t *testing.T) {
			p := positionForMessage()
			p.Latest = nil
			p.Entry.Preview.Status = validation
			p.Entry.Preview.ActualTariff = "trader"
			text := FormatPositionMessage("fixture", p)
			for _, want := range []string{
				"Entry fee validation: " + validation + " | actual tariff: trader",
				"Entry gross basis: 1000.00 RUB",
				"current P&L: unavailable",
				"Elapsed cash benchmark: unknown",
				"Entry projection through delivery (estimate): net / excess over cash (RUB)",
				"investor: 100.00 / 50.00", "trader: 101.00 / 51.00", "premium: 102.00 / 52.00",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in %s", want, text)
				}
			}
			if len(utf16.Encode([]rune(text))) > 4096 {
				t.Fatal("opening message exceeds Telegram limit")
			}
			p.Entry.Preview.ActualTariff = ""
			if strings.Contains(FormatPositionMessage("fixture", p), "actual tariff:") {
				t.Fatal("missing actual tariff presented as known")
			}
		})
	}
}

func TestPositionAwaitingAndClosedLabelsDoNotInventExecutions(t *testing.T) {
	p := positionForMessage()
	p.Status = "awaiting_settlement"
	text := FormatPositionMessage("fixture", p)
	for _, want := range []string{"MOCK / AWAITING SETTLEMENT", "Last valid executable close gap", "Last valid mark net P&L", "previous marks are not a closing fill"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	closedAt := p.Latest.ObservedAt.Add(time.Hour)
	p.Status = "closed"
	p.ClosedAt = &closedAt
	p.CloseReason = "capture_target"
	text = FormatPositionMessage("fixture", p)
	for _, want := range []string{"MOCK / CLOSED", "Closed net P&L", "Executed mock close gap", "Closed: 2026-10-26 13:30:00Z | reason: capture_target"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	p.Latest.Kind = "settlement"
	p.CloseReason = "verified_settlement"
	text = FormatPositionMessage("fixture", p)
	if !strings.Contains(text, "Settlement gap:") || strings.Contains(text, "Latest executable close gap") {
		t.Fatalf("settlement described as executable book: %s", text)
	}
	p.Status = "open"
	p.Latest = nil
	text = FormatPositionMessage("fixture", p)
	if !strings.Contains(text, "current P&L: unavailable") || !strings.Contains(text, "investor: unknown") {
		t.Fatal("missing valuation presented as known current profit")
	}
}

func TestSendPositionReusesSafePlainTextTransport(t *testing.T) {
	var payload map[string]any
	var reject atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if reject.Load() {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"test-secret"}`)
			return
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c, _ := New("test-secret", "123")
	c.baseURL = srv.URL
	p := positionForMessage()
	p.MinimumTariff = "trader\nforged"
	p.Entry.Snapshot.Pair.Stock.Ticker = "<b>SBER</b>_[x]"
	if err := c.SendPosition(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["parse_mode"]; ok {
		t.Fatal("position message enabled markup")
	}
	text := payload["text"].(string)
	if !strings.Contains(text, "<b>SBER</b>_[x]") || strings.Contains(text, "trader\nforged") {
		t.Fatalf("unsafe labels: %s", text)
	}
	reject.Store(true)
	if err := c.SendPosition(context.Background(), p); err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("unsafe position send error: %v", err)
	}
}

func TestPositionMessageBoundedWithUntrustedLabels(t *testing.T) {
	p := positionForMessage()
	p.Source = strings.Repeat("🙂\n", 2000)
	p.MinimumTariff = strings.Repeat("🙂\n", 2000)
	p.Entry.Snapshot.Pair.Stock.Ticker = strings.Repeat("🙂", 2000)
	p.Entry.Snapshot.Pair.Future.Ticker = strings.Repeat("🙂", 2000)
	text := FormatPositionMessage("fixture", p)
	if !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > 4096 {
		t.Fatalf("invalid position message length %d", len(utf16.Encode([]rune(text))))
	}
	for _, want := range []string{"position #42", "(fixed)", "investor: 400.00", "trader: 600.00", "premium: 650.00", "Elapsed cash benchmark"} {
		if !strings.Contains(text, want) {
			t.Fatalf("important content truncated: %q", want)
		}
	}
}

func TestNewFromEnvRequiresBothValues(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "123")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("accepted missing token")
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-secret")
	if _, err := NewFromEnv(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedDeliveryDoesNotConsumeDedup(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "alerts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var accepted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":%t,"error_code":400}`, accepted.Load())
	}))
	defer srv.Close()
	c, _ := New("test-secret", "123")
	c.baseURL = srv.URL
	ctx := context.Background()
	now := time.Now().UTC()
	profit := decimal.NewFromInt(100)
	should := func() bool {
		t.Helper()
		ok, err := store.ShouldNotify(ctx, "pair", now, profit, time.Hour, decimal.NewFromInt(10))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !should() {
		t.Fatal("first alert suppressed")
	}
	if err := c.Send(ctx, resultForMessage()); err == nil {
		t.Fatal("rejection considered success")
	}
	if !should() {
		t.Fatal("failed delivery consumed dedup")
	}
	accepted.Store(true)
	if err := c.Send(ctx, resultForMessage()); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkNotified(ctx, "pair", now, profit); err != nil {
		t.Fatal(err)
	}
	if should() {
		t.Fatal("successful delivery was not deduplicated")
	}
}
