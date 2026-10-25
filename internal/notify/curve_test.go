package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

func curvePositionForMessage(strategy string) curve.Position {
	d := decimal.RequireFromString
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	policy := curve.DefaultPolicy()
	entryScenarios := []scanner.Scenario{{Profile: "investor", NetProfit: d("100"), ExcessProfit: d("60")}, {Profile: "trader", NetProfit: d("130"), ExcessProfit: d("90")}, {Profile: "premium", NetProfit: d("150"), ExcessProfit: d("110")}}
	return curve.Position{ID: 7, Source: "fixture", Status: "open", OpenedAt: now, Policy: policy, Entry: curve.Candidate{
		Strategy: strategy, MinimumTariff: "trader", Policy: policy, CapitalRequired: d("50000"), ExpectedGross: d("200"), CashBenchmark: d("40"), Horizon: 14 * 24 * time.Hour,
		Metric: curve.Metric{Value: d("0.75"), Unit: "percentage_points"}, Target: d("0.4"), TargetSource: "31 earlier observations, median 0.4, spanning 30h",
		Legs:     []curve.Leg{{Instrument: curve.Instrument{Ticker: "KKZ6"}, Side: "buy", Lots: 1, Price: d("14.75"), Value: d("14750")}, {Instrument: curve.Instrument{Ticker: "KKG7"}, Side: "sell", Lots: 1, Price: d("14"), Value: d("14000")}},
		Previews: []curve.LegPreview{{Status: "fixture_unverified"}, {Status: "fixture_unverified"}}, Scenarios: entryScenarios,
	}}
}

func TestCurveOpeningMessageShowsScenarioNotRealizedProfit(t *testing.T) {
	for _, strategy := range []string{curve.OFZRGBI, curve.KEYRATECalendar, curve.RUONIACalendar, curve.RGBICalendar} {
		t.Run(strategy, func(t *testing.T) {
			p := curvePositionForMessage(strategy)
			text := FormatCurvePositionMessage("production", p)
			for _, want := range []string{"MOCK / OPEN | curve position #7", "Strategy: " + curveStrategy(strategy), "source: fixture", "minimum eligible tariff: trader (fixed)", "Entry BUY KKZ6", "Entry SELL KKG7", "fixture_unverified", "frozen target: 0.4", "31 earlier observations", "Entry target-convergence scenario (estimate)", "investor: 100.00 / 60.00", "trader: 130.00 / 90.00", "premium: 150.00 / 110.00", "Current P&L: unavailable", "Target convergence is conditional"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in %s", want, text)
				}
			}
			if strings.Contains(text, "Realized mock-close") {
				t.Fatal("entry projection called realized profit")
			}
			if strategy == curve.OFZRGBI && (!strings.Contains(text, "Proxy warning:") || !strings.Contains(text, "Proxy opt-in: false")) {
				t.Fatal("missing duration-proxy limitation")
			}
			if strategy != curve.OFZRGBI && !strings.Contains(text, "30 samples spanning 24h0m0s") {
				t.Fatal("missing calendar history guard")
			}
			if strategy == curve.OFZRGBI && !strings.Contains(text, "Peer-curve minimum: 4 bonds") {
				t.Fatal("missing OFZ fitted-curve distinction")
			}
		})
	}
}

func TestRelativeAlertsDistinguishCashIncomeFundingAndOneLegOffer(t *testing.T) {
	p := curvePositionForMessage(curve.CashOffer)
	p.Entry.Legs = p.Entry.Legs[:1]
	p.Entry.Legs[0].Instrument.Ticker = "UGLD"
	p.Entry.HedgeMeasure = "unhedged_event_risk"
	p.Latest = &curve.Valuation{ObservedAt: p.OpenedAt.Add(time.Hour), CashIncome: decimal.NewFromInt(7), FundingIncome: decimal.NewFromInt(-3), Scenarios: p.Entry.Scenarios}
	text := FormatCurvePositionMessage("fixture", p)
	for _, want := range []string{"relative-value position #7", "Mandatory cash offer", "Entry BUY UGLD", "coupons/distributions: 7.00 RUB", "funding: -3.00 RUB", "Current executable-close estimate"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	for _, absent := range []string{"Historical baseline minimum", "Absolute DV01 unavailable", "Entry SELL", "Realized mock-close P&L"} {
		if strings.Contains(text, absent) {
			t.Errorf("inapplicable %q in cash-offer message", absent)
		}
	}
}

func TestCurveCurrentAndClosedPnLStaySeparateFromEntryProjection(t *testing.T) {
	p := curvePositionForMessage(curve.RUONIACalendar)
	p.Latest = &curve.Valuation{ObservedAt: p.OpenedAt.Add(time.Hour), Kind: "mark", Metric: curve.Metric{Value: decimal.RequireFromString("0.5"), Unit: "percentage_points"}, CashBenchmark: decimal.NewFromInt(5), Scenarios: []scanner.Scenario{{Profile: "investor", NetProfit: decimal.NewFromInt(80), ExcessProfit: decimal.NewFromInt(75)}, {Profile: "trader", NetProfit: decimal.NewFromInt(100), ExcessProfit: decimal.NewFromInt(95)}, {Profile: "premium", NetProfit: decimal.NewFromInt(110), ExcessProfit: decimal.NewFromInt(105)}}}
	text := FormatCurvePositionMessage("fixture", p)
	for _, want := range []string{"Entry target-convergence scenario (estimate)", "Current executable-close estimate", "investor: 100.00 / 60.00", "investor: 80.00 / 75.00", "Elapsed cash benchmark: 5.00 RUB", "marked: 2026-10-25 13:00:00Z", "minimum eligible tariff: trader (fixed)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	p.Status = "closed"
	p.ClosedAt = &p.Latest.ObservedAt
	p.CloseReason = "target_reached"
	text = FormatCurvePositionMessage("fixture", p)
	for _, want := range []string{"MOCK / CLOSED", "Realized mock-close P&L", "Closed: 2026-10-25 13:00:00Z | reason: target_reached"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	p.Status = "awaiting_exit"
	if text = FormatCurvePositionMessage("fixture", p); !strings.Contains(text, "Last valid close-value estimate (not a fill)") {
		t.Fatal("waiting state treated as realized exit")
	}
}

func TestCurveMessageBoundsUntrustedLabelsAndPreservesWarnings(t *testing.T) {
	p := curvePositionForMessage(curve.OFZRGBI)
	p.Source = strings.Repeat("🙂\n", 2000)
	p.Entry.TargetSource = strings.Repeat("baseline🙂\n", 2000)
	p.Entry.Legs[0].Instrument.Ticker = "<b>OFZ</b>_[x]\nforged"
	p.Entry.Warnings = []string{strings.Repeat("risk🙂\n", 2000), "duplicate warning", "duplicate warning"}
	text := FormatCurvePositionMessage("fixture", p)
	if !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > 4096 {
		t.Fatal("invalid Telegram text length")
	}
	if strings.Contains(text, "_[x]\nforged") || !strings.Contains(text, "<b>OFZ</b>_[x] forged") {
		t.Fatal("labels not plain-text single line")
	}
	for _, want := range []string{"Proxy warning:", "investor: 100.00", "trader: 130.00", "premium: 150.00", "Target convergence is conditional"} {
		if !strings.Contains(text, want) {
			t.Fatalf("essential content truncated: %q", want)
		}
	}
	if strings.Count(text, "duplicate warning") != 1 {
		t.Fatal("duplicate warnings repeated")
	}
}

func TestCurveUnknownDV01IsNotShownAsZeroRisk(t *testing.T) {
	p := curvePositionForMessage(curve.RGBICalendar)
	p.Entry.HedgeMeasure = "index_notional"
	p.Entry.DV01Known = false
	p.Entry.ResidualDV01Ratio = decimal.RequireFromString("0.02")
	text := FormatCurvePositionMessage("fixture", p)
	if !strings.Contains(text, "Hedge: index_notional | residual mismatch: 2.00%") || !strings.Contains(text, "Absolute DV01 unavailable") || strings.Contains(text, "0 RUB/bp") {
		t.Fatalf("unknown DV01 shown as known neutrality: %s", text)
	}
	p.Entry.DV01Known = true
	p.Entry.ResidualDV01 = decimal.RequireFromString("0.001")
	text = FormatCurvePositionMessage("fixture", p)
	if !strings.Contains(text, "Residual DV01 (duration proxy): 0.001 RUB/bp") {
		t.Fatalf("duration approximation or precision lost: %s", text)
	}
}

func TestSendCurvePositionUsesPlainTextAndRedactsFailure(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"test-secret"}`)
	}))
	defer srv.Close()
	c, _ := New("test-secret", "123")
	c.baseURL = srv.URL
	err := c.SendCurvePosition(context.Background(), curvePositionForMessage(curve.KEYRATECalendar))
	if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("unsafe error %v", err)
	}
	if _, ok := payload["parse_mode"]; ok {
		t.Fatal("curve message enables markup")
	}
	if text, _ := payload["text"].(string); !strings.Contains(text, "MOCK / OPEN") {
		t.Fatal("missing curve text")
	}
	var missing *Client
	if err = missing.SendCurvePosition(context.Background(), curve.Position{}); err == nil {
		t.Fatal("nil client accepted")
	}
}
