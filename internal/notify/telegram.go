// Package notify sends plain-text snapshot alerts. It cannot place orders.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

type Client struct {
	Source     string
	token      string
	chatID     string
	baseURL    string
	httpClient *http.Client
}

func New(token, chatID string) (*Client, error) {
	token, chatID = strings.TrimSpace(token), strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return nil, errors.New("Telegram requires TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID")
	}
	return &Client{Source: "unknown", token: token, chatID: chatID, baseURL: "https://api.telegram.org", httpClient: &http.Client{Timeout: 10 * time.Second}}, nil
}

func NewFromEnv() (*Client, error) {
	return New(os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"))
}

// Send succeeds only after Telegram confirms ok=true. Errors deliberately omit
// request URLs and response descriptions, either of which could expose a token.
func (c *Client) Send(ctx context.Context, result scanner.Result) error {
	if c == nil {
		return errors.New("Telegram client is not configured")
	}
	return c.sendText(ctx, FormatMessage(c.Source, result))
}

// SendPosition reports a persisted mock position, never a real broker order.
func (c *Client) SendPosition(ctx context.Context, position scanner.Position) error {
	if c == nil {
		return errors.New("Telegram client is not configured")
	}
	return c.sendText(ctx, FormatPositionMessage(c.Source, position))
}

func (c *Client) sendText(ctx context.Context, text string) error {
	if c == nil {
		return errors.New("Telegram client is not configured")
	}
	payload := struct {
		ChatID                string `json:"chat_id"`
		Text                  string `json:"text"`
		DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	}{c.chatID, truncate(text, 4096), true}
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("cannot encode Telegram message")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Telegram request canceled: %w", ctx.Err())
		}
		return errors.New("Telegram request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram rejected message (HTTP %d)", resp.StatusCode)
	}
	var response struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&response); err != nil {
		return errors.New("Telegram returned an invalid response")
	}
	if !response.OK {
		return fmt.Errorf("Telegram did not accept message (code %d)", response.ErrorCode)
	}
	return nil
}

// FormatMessage uses no parse_mode: ticker, tariff and profile text cannot inject
// Markdown/HTML. Dynamic labels are single-line and bounded before formatting.
func FormatMessage(source string, r scanner.Result) string {
	var b strings.Builder
	fmt.Fprintln(&b, "MOCK / carry candidate — snapshot only")
	fmt.Fprintf(&b, "Source: %s\n", label(source, 80))
	fmt.Fprintf(&b, "BUY %s: %d shares (%d lots) at ≤ %s RUB\n", label(r.Snapshot.Pair.Stock.Ticker, 36), r.Shares, r.StockLots, truncate(r.StockLimit.String(), 40))
	fmt.Fprintf(&b, "SELL %s: %d contracts at ≥ %s quote units\n", label(r.Snapshot.Pair.Future.Ticker, 36), r.Contracts, truncate(r.FutureLimit.String(), 40))
	fmt.Fprintf(&b, "Expiry: %s | observed: %s\n", date(r.Snapshot.Pair.Future.Expiry), date(r.Snapshot.ObservedAt))
	fmt.Fprintf(&b, "Quote times: stock %s / future %s\n", date(r.Snapshot.StockBook.Time), date(r.Snapshot.FutureBook.Time))
	fmt.Fprintf(&b, "Actual tariff: %s | commission preview: %s\n", label(r.Preview.ActualTariff, 60), label(r.Preview.Status, 60))
	fmt.Fprintf(&b, "Minimum eligible tariff: %s\n", minimumTariff(r))
	fmt.Fprintf(&b, "Capital: %s RUB | gross basis: %s RUB | cash benchmark: %s RUB\n", money(r.CapitalRequired), money(r.GrossBasis), money(r.CashBenchmark))
	fmt.Fprintf(&b, "Initial margin: %s RUB | execution buffer: %s RUB\n", money(r.InitialMargin), money(r.ExecutionBuffer))
	fmt.Fprintln(&b, "Scenarios (RUB): net after modeled costs / excess over cash")
	fmt.Fprintln(&b, "Fees: stock entry / future entry / estimated delivery")
	for _, s := range r.Scenarios {
		fmt.Fprintf(&b, "%s: %s / %s | fees %s / %s / %s\n", label(s.Profile, 48), money(s.NetProfit), money(s.ExcessProfit), feeAmount(s, "stock_entry"), feeAmount(s, "future_entry_including_exchange_floor"), feeAmount(s, "stock_delivery_estimate"))
	}
	fmt.Fprintf(&b, "Assumptions: cash %s%%/yr; dividend %s RUB/share; prior futures turnover %s RUB.\n", money(r.Assumptions.CashRate.Mul(decimal.NewFromInt(100))), money(r.Assumptions.DividendPerShare), money(r.Assumptions.FutureTurnover))
	fmt.Fprintln(&b, "Pre-tax. Delivery commission estimated. Premium levels use the same trading fees.")
	return truncate(b.String(), 4096)
}

func minimumTariff(result scanner.Result) string {
	if family := scanner.MinimumEligibleTariff(result); family != "" {
		return family
	}
	return "none"
}

// FormatPositionMessage keeps the entry tariff fixed. A previous valid mark is
// explicitly dated when the position is awaiting verified settlement data.
func FormatPositionMessage(source string, p scanner.Position) string {
	var b strings.Builder
	status := "UNKNOWN"
	switch p.Status {
	case "open":
		status = "OPEN"
	case "awaiting_settlement":
		status = "AWAITING SETTLEMENT"
	case "closed":
		status = "CLOSED"
	}
	if p.Source != "" {
		source = p.Source
	}
	fmt.Fprintf(&b, "MOCK / %s | position #%d\n", status, p.ID)
	fmt.Fprintf(&b, "Source: %s | opened: %s\n", label(source, 80), date(p.OpenedAt))
	fmt.Fprintf(&b, "Minimum eligible tariff at entry: %s (fixed)\n", label(p.MinimumTariff, 48))
	e := p.Entry
	fmt.Fprintf(&b, "Entry fee validation: %s", label(e.Preview.Status, 48))
	if strings.TrimSpace(e.Preview.ActualTariff) != "" {
		fmt.Fprintf(&b, " | actual tariff: %s", label(e.Preview.ActualTariff, 60))
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Entry BUY %s: %d shares; %s RUB; avg %s RUB/share\n", label(e.Snapshot.Pair.Stock.Ticker, 36), e.Shares, money(e.StockCost), average(e.StockCost, decimal.NewFromInt(e.Shares)))
	futureUnits := decimal.NewFromInt(e.Contracts).Mul(e.Snapshot.Pair.Future.PriceFactor)
	fmt.Fprintf(&b, "Entry SELL %s: %d contracts; %s RUB; avg %s quote units\n", label(e.Snapshot.Pair.Future.Ticker, 36), e.Contracts, money(e.FutureValue), average(e.FutureValue, futureUnits))
	fmt.Fprintf(&b, "Capital: %s RUB | capture target: %s%%\n", money(e.CapitalRequired), money(p.CaptureTarget.Mul(decimal.NewFromInt(100))))
	fmt.Fprintf(&b, "Entry gross basis: %s RUB\n", money(e.GrossBasis))
	if p.Latest == nil {
		fmt.Fprintln(&b, "Executable close gap / current P&L: unavailable; awaiting valid paired books.")
		fmt.Fprintln(&b, "investor: unknown | trader: unknown | premium: unknown")
		fmt.Fprintln(&b, "Elapsed cash benchmark: unknown")
		fmt.Fprintln(&b, "Entry projection through delivery (estimate): net / excess over cash (RUB)")
		writeScenarioPnL(&b, e.Scenarios)
	} else {
		v := p.Latest
		gapLabel, pnlLabel := "Latest executable close gap", "Current net P&L"
		if p.Status == "awaiting_settlement" {
			gapLabel, pnlLabel = "Last valid executable close gap", "Last valid mark net P&L"
		} else if p.Status == "closed" {
			pnlLabel = "Closed net P&L"
			if v.Kind == "settlement" {
				gapLabel = "Settlement gap"
			} else {
				gapLabel = "Executed mock close gap"
			}
		}
		fmt.Fprintf(&b, "%s: %s RUB | captured: %s%%\n", gapLabel, money(v.ResidualBasis), money(v.CapturedFraction.Mul(decimal.NewFromInt(100))))
		fmt.Fprintf(&b, "Valuation: %s | %s\n", date(v.ObservedAt), label(v.Kind, 24))
		if v.Kind == "settlement" && v.Settlement != nil {
			fmt.Fprintf(&b, "Settlement source: %s\n", label(v.Settlement.Reference, 160))
		}
		fmt.Fprintf(&b, "Elapsed cash benchmark: %s RUB (%s days)\n", money(v.CashBenchmark), money(v.Days))
		fmt.Fprintf(&b, "%s / excess over cash (RUB):\n", pnlLabel)
		writeScenarioPnL(&b, v.Scenarios)
	}
	if p.Status == "closed" {
		closedAt := time.Time{}
		if p.ClosedAt != nil {
			closedAt = *p.ClosedAt
		}
		fmt.Fprintf(&b, "Closed: %s | reason: %s\n", date(closedAt), label(p.CloseReason, 180))
	} else if p.Status == "awaiting_settlement" {
		fmt.Fprintln(&b, "Awaiting verified settlement; previous marks are not a closing fill.")
	}
	fmt.Fprintln(&b, "Paired mock fills; pre-tax. Premium levels share one trading-fee profile.")
	return truncate(b.String(), 4096)
}

func writeScenarioPnL(b *strings.Builder, scenarios []scanner.Scenario) {
	for _, family := range []string{"investor", "trader", "premium"} {
		found := false
		for _, s := range scenarios {
			if s.Profile == family {
				fmt.Fprintf(b, "%s: %s / %s\n", family, money(s.NetProfit), money(s.ExcessProfit))
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(b, "%s: unknown / unknown\n", family)
		}
	}
}

func average(value, quantity decimal.Decimal) string {
	if !quantity.IsPositive() {
		return "unknown"
	}
	return truncate(value.Div(quantity).String(), 40)
}

func feeAmount(s scanner.Scenario, name string) string {
	for _, fee := range s.Fees {
		if fee.Name == name {
			return money(fee.Amount)
		}
	}
	return "unknown"
}

func money(v decimal.Decimal) string { return truncate(v.StringFixed(2), 40) }
func date(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}
func label(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "unknown"
	}
	return truncate(s, max)
}
func truncate(s string, limit int) string {
	if len(utf16.Encode([]rune(s))) <= limit {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if n+size > limit-1 {
			break
		}
		b.WriteRune(r)
		n += size
	}
	b.WriteRune('…')
	return b.String()
}
