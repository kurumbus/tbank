package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/curve"
)

// Durations are human-readable in configuration; stored position policies use
// the domain's exact time.Duration values so future config edits cannot alter them.
type curvePolicyConfig struct{ curve.Policy }

func (p *curvePolicyConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"min_history_span", "history_max_age", "max_quote_age", "max_quote_skew", "max_reference_age", "max_holding", "expiry_buffer"} {
		if raw, ok := fields[key]; ok {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("%s must be a duration string such as 24h", key)
			}
			duration, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fields[key], _ = json.Marshal(duration)
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(&p.Policy)
}

type curveConfig struct {
	Mode             string               `json:"mode,omitempty"`
	Relationships    []curve.Relationship `json:"relationships,omitempty"`
	BondTickers      []string             `json:"bond_tickers"`
	FutureCount      int                  `json:"futures_per_family"`
	MaxFutureDays    int                  `json:"max_future_days"`
	Depth            int                  `json:"order_book_depth"`
	TimeoutSeconds   int                  `json:"timeout_seconds"`
	StatePath        string               `json:"state_path"`
	CooldownMinutes  int                  `json:"alert_cooldown_minutes"`
	AlertImprovement decimal.Decimal      `json:"alert_improvement_rub"`
	Policy           curvePolicyConfig    `json:"policy"`
}

func loadCurveConfig(path string) (curveConfig, error) {
	c := curveConfig{FutureCount: 3, MaxFutureDays: 365, Depth: 20, TimeoutSeconds: 240, StatePath: "var/scanner.sqlite", CooldownMinutes: 1440, AlertImprovement: decimal.NewFromInt(100), Policy: curvePolicyConfig{curve.DefaultPolicy()}}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("curve config: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("curve config must contain one JSON object")
	}
	if c.Mode != "" && c.Mode != "relative" {
		return c, fmt.Errorf("mode must be empty or relative")
	}
	if c.FutureCount < 2 || c.FutureCount > 3 || c.MaxFutureDays < 1 || c.MaxFutureDays > 730 {
		return c, fmt.Errorf("futures_per_family must be 2..3 and max_future_days 1..730")
	}
	if c.Mode != "relative" && (len(c.BondTickers) < 5 || len(c.BondTickers) > 40) {
		return c, fmt.Errorf("configure 5..40 fixed-coupon OFZ tickers")
	}
	seen := map[string]bool{}
	for _, ticker := range c.BondTickers {
		if ticker == "" || ticker != strings.ToUpper(strings.TrimSpace(ticker)) || seen[ticker] {
			return c, fmt.Errorf("bond tickers must be unique uppercase symbols")
		}
		seen[ticker] = true
	}
	if c.Mode == "relative" {
		if len(c.Relationships) == 0 || len(c.Relationships) > 40 || len(c.BondTickers) != 0 {
			return c, fmt.Errorf("relative mode requires 1..40 relationships and no bond_tickers")
		}
		seen = map[string]bool{}
		for _, r := range c.Relationships {
			if strings.TrimSpace(r.ID) == "" || seen[r.ID] || !curve.IsRelativeStrategy(r.Strategy) {
				return c, fmt.Errorf("relationships require unique ids and a supported relative strategy")
			}
			seen[r.ID] = true
			if (r.PrimaryUID == "" && r.PrimarySelector == "") || (r.Strategy != curve.CashOffer && r.HedgeUID == "" && r.HedgeSelector == "") || r.PrimaryLots <= 0 {
				return c, fmt.Errorf("relationship %s requires selectors or UIDs and positive primary_lots", r.ID)
			}
		}
	} else if len(c.Relationships) != 0 {
		return c, fmt.Errorf("relationships require relative mode")
	}
	if !map[int]bool{1: true, 10: true, 20: true, 30: true, 40: true, 50: true}[c.Depth] {
		return c, fmt.Errorf("unsupported order_book_depth")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 || c.StatePath == "" || c.CooldownMinutes < 1 || c.CooldownMinutes > 10080 || c.AlertImprovement.IsNegative() {
		return c, fmt.Errorf("invalid curve runtime settings")
	}
	return c, curve.ValidatePolicy(c.Policy.Policy)
}
