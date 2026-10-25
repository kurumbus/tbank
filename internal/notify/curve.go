package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
)

// SendCurvePosition reports paper relative-value positions through the same
// plain-text, token-redacting transport as stock/future alerts.
func (c *Client) SendCurvePosition(ctx context.Context, position curve.Position) error {
	if c == nil {
		return errors.New("Telegram client is not configured")
	}
	return c.sendText(ctx, FormatCurvePositionMessage(c.Source, position))
}

// FormatCurvePositionMessage separates a frozen convergence scenario from
// executable-close marks and realized paper closes. All labels are untrusted.
func FormatCurvePositionMessage(source string, p curve.Position) string {
	var b strings.Builder
	e := p.Entry
	if p.Source != "" {
		source = p.Source
	}
	status := strings.ToUpper(strings.ReplaceAll(p.Status, "_", " "))
	positionLabel := "curve"
	if curve.IsRelativeStrategy(e.Strategy) {
		positionLabel = "relative-value"
	}
	fmt.Fprintf(&b, "MOCK / %s | %s position #%d\n", label(status, 36), positionLabel, p.ID)
	fmt.Fprintf(&b, "Strategy: %s | source: %s\n", curveStrategy(e.Strategy), label(source, 64))
	fmt.Fprintf(&b, "Opened: %s | minimum eligible tariff: %s (fixed)\n", date(p.OpenedAt), label(e.MinimumTariff, 36))
	for i, leg := range e.Legs {
		if i == 4 {
			fmt.Fprintf(&b, "Additional legs: %d\n", len(e.Legs)-i)
			break
		}
		fmt.Fprintf(&b, "Entry %s %s: %d lots @ %s quote units; %s RUB\n", label(strings.ToUpper(leg.Side), 12), label(leg.Instrument.Ticker, 32), leg.Lots, truncate(leg.Price.String(), 32), money(leg.Value))
	}
	if len(e.Previews) > 0 {
		fmt.Fprint(&b, "Entry fee checks:")
		for i, preview := range e.Previews {
			if i == 4 {
				break
			}
			fmt.Fprintf(&b, " leg %d=%s;", i+1, label(preview.Status, 32))
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "Entry metric: %s %s | frozen target: %s\n", truncate(e.Metric.Value.String(), 32), label(e.Metric.Unit, 32), truncate(e.Target.String(), 32))
	fmt.Fprintf(&b, "Target / baseline source: %s\n", label(e.TargetSource, 180))
	policy := p.Policy
	if policy.MinHistory == 0 {
		policy = e.Policy
	}
	if e.Strategy == curve.OFZRGBI {
		fmt.Fprintf(&b, "Peer-curve minimum: %d bonds; fitted fair-value target, not contractual convergence.\n", policy.MinPeers)
	} else if e.Strategy != curve.StandardMini && e.Strategy != curve.FundNAV && e.Strategy != curve.CashOffer {
		fmt.Fprintf(&b, "Historical baseline minimum: %d samples spanning %s\n", policy.MinHistory, label(policy.MinHistorySpan.String(), 32))
	}
	fmt.Fprintf(&b, "Capital: %s RUB | scenario horizon: %s\n", money(e.CapitalRequired), label(e.Horizon.String(), 32))
	if e.HedgeMeasure != "" {
		fmt.Fprintf(&b, "Hedge: %s | residual mismatch: %s%%\n", label(e.HedgeMeasure, 32), money(e.ResidualDV01Ratio.Mul(decimal.NewFromInt(100))))
		if e.DV01Known {
			measureLabel := "Residual DV01 estimate"
			if e.Strategy == curve.OFZRGBI || e.Strategy == curve.RGBICalendar {
				measureLabel = "Residual DV01 (duration proxy)"
			}
			fmt.Fprintf(&b, "%s: %s RUB/bp\n", measureLabel, truncate(e.ResidualDV01.String(), 32))
		} else if !curve.IsRelativeStrategy(e.Strategy) {
			fmt.Fprintln(&b, "Absolute DV01 unavailable; notional mismatch is not measured rate risk.")
		}
	}
	fmt.Fprintf(&b, "Entry target-convergence scenario (estimate), net / excess vs cash (RUB):\n")
	writeScenarioPnL(&b, e.Scenarios)
	fmt.Fprintf(&b, "Projected gross: %s RUB | scenario cash benchmark: %s RUB\n", money(e.ExpectedGross), money(e.CashBenchmark))
	if p.Latest == nil {
		fmt.Fprintln(&b, "Current P&L: unavailable; no executable closing mark yet.")
	} else {
		v := p.Latest
		pnlLabel := "Current executable-close estimate"
		if p.Status == "closed" {
			pnlLabel = "Realized mock-close P&L"
		} else if p.Status != "open" {
			pnlLabel = "Last valid close-value estimate (not a fill)"
		}
		fmt.Fprintf(&b, "Latest metric: %s %s | marked: %s\n", truncate(v.Metric.Value.String(), 32), label(v.Metric.Unit, 32), date(v.ObservedAt))
		fmt.Fprintf(&b, "%s, net / excess vs cash (RUB):\n", pnlLabel)
		writeScenarioPnL(&b, v.Scenarios)
		fmt.Fprintf(&b, "Elapsed cash benchmark: %s RUB | coupons/distributions: %s RUB | funding: %s RUB\n", money(v.CashBenchmark), money(v.CouponIncome.Add(v.CashIncome)), money(v.FundingIncome))
	}
	if p.Status == "closed" {
		closedAt := time.Time{}
		if p.ClosedAt != nil {
			closedAt = *p.ClosedAt
		}
		fmt.Fprintf(&b, "Closed: %s | reason: %s\n", date(closedAt), label(p.CloseReason, 120))
	}
	if e.Strategy == curve.OFZRGBI {
		fmt.Fprintf(&b, "Proxy warning: aggregate RGBI duration is a parallel-shift approximation; issue, curve and index-basis risk remain. Proxy opt-in: %t.\n", policy.AllowDurationProxy)
	} else if e.Strategy == curve.RUONIACalendar || e.Strategy == curve.KEYRATECalendar {
		fmt.Fprintln(&b, "Hedge warning: first-order parallel-rate approximation; curve twists and sensitivity changes remain.")
	} else if e.Strategy == curve.RGBICalendar {
		fmt.Fprintln(&b, "Basis warning: different expiries of one bond index; financing and roll risk remain.")
	}
	seen := map[string]bool{}
	warnings := append([]string(nil), e.Warnings...)
	if p.Latest != nil {
		warnings = append(warnings, p.Latest.Warnings...)
	}
	shown := 0
	for _, warning := range warnings {
		warning = label(warning, 140)
		if seen[warning] || warning == "unknown" {
			continue
		}
		seen[warning] = true
		fmt.Fprintf(&b, "Note: %s\n", warning)
		shown++
		if shown == 3 {
			break
		}
	}
	fmt.Fprintln(&b, "Pre-tax mock results. Target convergence is conditional, not guaranteed.")
	return truncate(b.String(), 4096)
}

func curveStrategy(strategy string) string {
	switch strategy {
	case curve.OFZRGBI:
		return "OFZ / RGBI"
	case curve.KEYRATECalendar:
		return "KEYRATE calendar"
	case curve.RUONIACalendar:
		return "RUONIA calendar"
	case curve.RGBICalendar:
		return "RGBI calendar"
	case curve.StandardMini:
		return "Standard / mini futures"
	case curve.ShareClass:
		return "Ordinary / preferred futures"
	case curve.PerpetualDated:
		return "Perpetual / dated futures"
	case curve.HoldingStub:
		return "Holding-company residual"
	case curve.FundNAV:
		return "Gold fund / NAV"
	case curve.CashOffer:
		return "Mandatory cash offer"
	default:
		return label(strategy, 48)
	}
}
