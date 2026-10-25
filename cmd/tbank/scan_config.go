package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

type scanConfig struct {
	ExitCaptureFraction decimal.Decimal `json:"exit_capture_fraction"`
	Tickers             []string        `json:"tickers"`
	Contracts           int64           `json:"contracts"`
	CapitalLimit        decimal.Decimal `json:"capital_limit_rub"`
	CashRate            decimal.Decimal `json:"cash_rate_annual"`
	MarginBuffer        decimal.Decimal `json:"margin_buffer_rate"`
	ExecutionBuffer     decimal.Decimal `json:"execution_buffer_bps"`
	SettlementBuffer    decimal.Decimal `json:"settlement_price_buffer_rate"`
	FutureTurnover      decimal.Decimal `json:"assumed_prior_futures_turnover_rub"`
	MinProfit           decimal.Decimal `json:"min_profit_rub"`
	MinExcessProfit     decimal.Decimal `json:"min_excess_profit_rub"`
	MaxDays             int             `json:"max_days"`
	Depth               int             `json:"order_book_depth"`
	QuoteAgeSeconds     int             `json:"max_quote_age_seconds"`
	QuoteSkewSeconds    int             `json:"max_quote_skew_seconds"`
	TimeoutSeconds      int             `json:"timeout_seconds"`
	StatePath           string          `json:"state_path"`
	CooldownMinutes     int             `json:"alert_cooldown_minutes"`
	AlertImprovement    decimal.Decimal `json:"alert_improvement_rub"`
}

func loadScanConfig(path string) (scanConfig, error) {
	c := scanConfig{ExitCaptureFraction: decimal.RequireFromString("0.9")}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("scanner config: %w", err)
	}
	var trailing any
	if err = d.Decode(&trailing); err != io.EOF {
		return c, fmt.Errorf("scanner config must contain one JSON object")
	}
	return c, c.validate()
}

func (c scanConfig) validate() error {
	if !c.ExitCaptureFraction.IsPositive() || c.ExitCaptureFraction.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("exit_capture_fraction must be greater than zero and at most 1")
	}
	if len(c.Tickers) == 0 || len(c.Tickers) > 30 {
		return fmt.Errorf("configure 1 to 30 stock tickers")
	}
	seen := map[string]bool{}
	for _, s := range c.Tickers {
		if s == "" || s != strings.ToUpper(strings.TrimSpace(s)) || seen[s] {
			return fmt.Errorf("tickers must be unique uppercase symbols")
		}
		if s == "T" {
			return fmt.Errorf("T shares are excluded in v1: Premium's free-trade allowance needs separate accounting")
		}
		seen[s] = true
	}
	if c.Contracts < 1 || c.Contracts > 100000 {
		return fmt.Errorf("contracts must be 1..100000")
	}
	if !c.CapitalLimit.IsPositive() {
		return fmt.Errorf("capital_limit_rub must be positive")
	}
	for name, v := range map[string]decimal.Decimal{"cash_rate_annual": c.CashRate, "margin_buffer_rate": c.MarginBuffer, "execution_buffer_bps": c.ExecutionBuffer, "settlement_price_buffer_rate": c.SettlementBuffer, "assumed_prior_futures_turnover_rub": c.FutureTurnover, "min_profit_rub": c.MinProfit, "min_excess_profit_rub": c.MinExcessProfit, "alert_improvement_rub": c.AlertImprovement} {
		if v.IsNegative() {
			return fmt.Errorf("%s cannot be negative", name)
		}
	}
	if c.CashRate.GreaterThan(decimal.NewFromInt(1)) || c.MarginBuffer.GreaterThan(decimal.NewFromInt(10)) || c.SettlementBuffer.GreaterThan(decimal.NewFromInt(10)) || c.ExecutionBuffer.GreaterThan(decimal.NewFromInt(10000)) {
		return fmt.Errorf("rates outside supported ranges; rates use fractions, execution buffer uses basis points")
	}
	if c.MaxDays < 1 || c.MaxDays > 365 {
		return fmt.Errorf("max_days must be 1..365")
	}
	allowedDepth := map[int]bool{1: true, 10: true, 20: true, 30: true, 40: true, 50: true}
	if !allowedDepth[c.Depth] {
		return fmt.Errorf("order_book_depth must be 1, 10, 20, 30, 40 or 50")
	}
	if c.QuoteAgeSeconds < 1 || c.QuoteAgeSeconds > 300 || c.QuoteSkewSeconds < 1 || c.QuoteSkewSeconds > c.QuoteAgeSeconds {
		return fmt.Errorf("quote age/skew must be positive, skew <= age <= 300 seconds")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 {
		return fmt.Errorf("timeout_seconds must be 1..600")
	}
	if c.StatePath == "" || c.CooldownMinutes < 1 || c.CooldownMinutes > 10080 {
		return fmt.Errorf("state_path and 1..10080 minute alert cooldown required")
	}
	return nil
}

func (c scanConfig) assumptions() scanner.Assumptions {
	return scanner.Assumptions{Contracts: c.Contracts, CapitalLimit: c.CapitalLimit, CashRate: c.CashRate, MarginBufferRate: c.MarginBuffer, ExecutionBufferBPS: c.ExecutionBuffer, SettlementPriceBufferRate: c.SettlementBuffer, FutureTurnover: c.FutureTurnover, MinProfit: c.MinProfit, MinExcessProfit: c.MinExcessProfit, MaxQuoteAge: time.Duration(c.QuoteAgeSeconds) * time.Second, MaxQuoteSkew: time.Duration(c.QuoteSkewSeconds) * time.Second, MaxDays: c.MaxDays}
}
