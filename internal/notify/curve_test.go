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
			for _, want := range []string{"MOCK OPEN #7 curve | " + curveStrategy(strategy) + " | fixture", "trader", "BUY KKZ6", "SELL KKG7", "Gross 200.00", "Entry net:", "investor: 100.00", "trader: 130.00", "premium: 150.00"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in %s", want, text)
				}
			}
			for _, absent := range []string{"Realized", "Note:", "Pre-tax", "fixture_unverified", "frozen target", "Proxy warning", "Historical baseline", "60.00", "90.00", "110.00"} {
				if strings.Contains(text, absent) {
					t.Errorf("opening alert still contains %q:\n%s", absent, text)
				}
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
	for _, want := range []string{"MOCK OPEN #7 relative | Mandatory cash offer | fixture", "BUY UGLD", "Now 2026-10-25 13:00Z net:", "income 7.00", "funding -3.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	for _, absent := range []string{"Historical baseline", "DV01", "SELL", "Realized", "Note:"} {
		if strings.Contains(text, absent) {
			t.Errorf("inapplicable %q in cash-offer message:\n%s", absent, text)
		}
	}
}

func TestCurveCurrentAndClosedPnLStaySeparateFromEntryProjection(t *testing.T) {
	p := curvePositionForMessage(curve.RUONIACalendar)
	p.Latest = &curve.Valuation{ObservedAt: p.OpenedAt.Add(time.Hour), Kind: "mark", Metric: curve.Metric{Value: decimal.RequireFromString("0.5"), Unit: "percentage_points"}, CashBenchmark: decimal.NewFromInt(5), Scenarios: []scanner.Scenario{{Profile: "investor", NetProfit: decimal.NewFromInt(80), ExcessProfit: decimal.NewFromInt(75)}, {Profile: "trader", NetProfit: decimal.NewFromInt(100), ExcessProfit: decimal.NewFromInt(95)}, {Profile: "premium", NetProfit: decimal.NewFromInt(110), ExcessProfit: decimal.NewFromInt(105)}}}
	text := FormatCurvePositionMessage("fixture", p)
	for _, want := range []string{"Now 2026-10-25 13:00Z net:", "investor: 80.00", "trader: 100.00", "premium: 110.00", "income 0.00", "funding 0.00", "trader | 14d"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "investor: 100.00") || strings.Contains(text, "75.00") || strings.Contains(text, "Realized") || strings.Contains(text, "Entry net:") || strings.Contains(text, "cash ") {
		t.Fatalf("current mark mixed with entry projection or cash yield: %s", text)
	}
	p.Status = "closed"
	p.ClosedAt = &p.Latest.ObservedAt
	p.CloseReason = "target_reached"
	text = FormatCurvePositionMessage("fixture", p)
	for _, want := range []string{"MOCK CLOSED #7", "Realized net:", "Closed 2026-10-25 13:00Z | target_reached"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	p.Status = "awaiting_exit"
	text = FormatCurvePositionMessage("fixture", p)
	if !strings.Contains(text, "Last mark 2026-10-25 13:00Z, not a fill:") || strings.Contains(text, "Realized") {
		t.Fatalf("waiting state treated as realized exit: %s", text)
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
	for _, want := range []string{"SELL KKG7", "14d", "<b>OFZ</b>_[x] forged", "Entry net:", "investor: 100.00"} {
		if !strings.Contains(text, want) {
			t.Fatalf("essential content truncated: %q", want)
		}
	}
	if strings.Contains(text, "duplicate warning") || strings.Contains(text, "Note:") {
		t.Fatal("method notes belong in the report, not the alert")
	}
}

func TestCurveUnknownDV01IsNotShownAsZeroRisk(t *testing.T) {
	p := curvePositionForMessage(curve.RGBICalendar)
	p.Entry.HedgeMeasure = "index_notional"
	p.Entry.DV01Known = false
	p.Entry.ResidualDV01Ratio = decimal.RequireFromString("0.02")
	text := FormatCurvePositionMessage("fixture", p)
	if strings.Contains(text, "DV01") || strings.Contains(text, "0 RUB/bp") || strings.Contains(text, "mismatch") {
		t.Fatalf("hedge diagnostics belong in the report: %s", text)
	}
	p.Entry.DV01Known = true
	p.Entry.ResidualDV01 = decimal.RequireFromString("0.001")
	text = FormatCurvePositionMessage("fixture", p)
	if strings.Contains(text, "0.001") || strings.Contains(text, "RUB/bp") {
		t.Fatalf("known DV01 dumped into the alert: %s", text)
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
	if text, _ := payload["text"].(string); !strings.Contains(text, "MOCK OPEN") {
		t.Fatal("missing curve text")
	}
	var missing *Client
	if err = missing.SendCurvePosition(context.Background(), curve.Position{}); err == nil {
		t.Fatal("nil client accepted")
	}
}
