package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// FormatCurvePositionMessage is a short paper alert. An opening shows the
// target-scenario net after costs. A later mark replaces it. A close is
// labeled realized. The cash yield is not shown. All labels are untrusted.
func FormatCurvePositionMessage(source string, p curve.Position) string {
	var b strings.Builder
	e := p.Entry
	if p.Source != "" {
		source = p.Source
	}
	status := strings.ToUpper(strings.ReplaceAll(p.Status, "_", " "))
	kind := "curve"
	if curve.IsRelativeStrategy(e.Strategy) {
		kind = "relative"
	}
	fmt.Fprintf(&b, "MOCK %s #%d %s | %s | %s\n", label(status, 24), p.ID, kind, curveStrategy(e.Strategy), label(source, 32))
	fmt.Fprintf(&b, "%s | %s | %s | %s RUB\n", shortDate(p.OpenedAt), label(e.MinimumTariff, 24), shortHorizon(e.Horizon), money(e.CapitalRequired))
	for i, leg := range e.Legs {
		if i == 4 {
			fmt.Fprintf(&b, "+%d legs\n", len(e.Legs)-i)
			break
		}
		fmt.Fprintf(&b, "%s %s %d @ %s = %s\n", label(strings.ToUpper(leg.Side), 8), label(leg.Instrument.Ticker, 24), leg.Lots, truncate(leg.Price.String(), 16), money(leg.Value))
	}
	if p.Latest == nil {
		fmt.Fprintf(&b, "Gross %s\n", money(e.ExpectedGross))
		fmt.Fprintln(&b, "Entry net:")
		writeScenarioNet(&b, e.Scenarios)
		return truncate(b.String(), 4096)
	}
	v := p.Latest
	switch p.Status {
	case "closed":
		closedAt := time.Time{}
		if p.ClosedAt != nil {
			closedAt = *p.ClosedAt
		}
		fmt.Fprintf(&b, "Closed %s | %s\n", shortDate(closedAt), label(p.CloseReason, 80))
		fmt.Fprintln(&b, "Realized net:")
	case "open":
		fmt.Fprintf(&b, "Now %s net:\n", shortDate(v.ObservedAt))
	default:
		fmt.Fprintf(&b, "Last mark %s, not a fill:\n", shortDate(v.ObservedAt))
	}
	writeScenarioNet(&b, v.Scenarios)
	fmt.Fprintf(&b, "income %s | funding %s\n", money(v.CouponIncome.Add(v.CashIncome)), money(v.FundingIncome))
	return truncate(b.String(), 4096)
}

func shortDate(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

func shortHorizon(d time.Duration) string {
	if d <= 0 {
		return "unknown"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.Round(time.Minute).String()
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
		return "Standard / mini"
	case curve.ShareClass:
		return "Ordinary / preferred"
	case curve.PerpetualDated:
		return "Perpetual / dated"
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
