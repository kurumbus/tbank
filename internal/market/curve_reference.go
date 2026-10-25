package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const moexRGBIHistoryURL = "https://iss.moex.com/iss/history/engines/stock/markets/index/boards/SNDX/securities/RGBI.json"

// The exchange publishes Macaulay duration in days and effective yield in
// percent. These observations are not an official modified-duration series.
type curveIndexObservation struct {
	Date         time.Time
	MacaulayDays decimal.Decimal
	YieldPercent decimal.Decimal
	Source       string
}

func (p *Provider) fetchCurveIndex(ctx context.Context, now time.Time) (curveIndexObservation, error) {
	endpoint := p.rgbiURL
	if endpoint == "" {
		endpoint = moexRGBIHistoryURL
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return curveIndexObservation{}, err
	}
	query := u.Query()
	query.Set("iss.meta", "off")
	query.Set("iss.only", "history")
	query.Set("from", now.UTC().AddDate(0, 0, -14).Format("2006-01-02"))
	query.Set("till", now.UTC().Format("2006-01-02"))
	query.Set("history.columns", "BOARDID,SECID,TRADEDATE,DURATION,YIELD")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return curveIndexObservation{}, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return curveIndexObservation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return curveIndexObservation{}, fmt.Errorf("RGBI history HTTP %d", resp.StatusCode)
	}
	result, err := parseCurveIndex(io.LimitReader(resp.Body, 2<<20), now)
	result.Source = u.String()
	return result, err
}

func parseCurveIndex(reader io.Reader, now time.Time) (curveIndexObservation, error) {
	var body struct {
		History struct {
			Columns []string            `json:"columns"`
			Data    [][]json.RawMessage `json:"data"`
		} `json:"history"`
	}
	if err := json.NewDecoder(reader).Decode(&body); err != nil {
		return curveIndexObservation{}, fmt.Errorf("RGBI history JSON: %w", err)
	}
	columns := map[string]int{}
	for i, name := range body.History.Columns {
		columns[name] = i
	}
	for _, name := range []string{"BOARDID", "SECID", "TRADEDATE", "DURATION", "YIELD"} {
		if _, ok := columns[name]; !ok {
			return curveIndexObservation{}, fmt.Errorf("RGBI history missing %s", name)
		}
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	var result curveIndexObservation
	for _, row := range body.History.Data {
		if len(row) != len(body.History.Columns) {
			return curveIndexObservation{}, fmt.Errorf("RGBI history malformed row")
		}
		var board, security, date string
		if json.Unmarshal(row[columns["BOARDID"]], &board) != nil || json.Unmarshal(row[columns["SECID"]], &security) != nil || json.Unmarshal(row[columns["TRADEDATE"]], &date) != nil {
			return curveIndexObservation{}, fmt.Errorf("RGBI history invalid identity/date")
		}
		if board != "SNDX" || security != "RGBI" {
			continue
		}
		at, err := time.Parse("2006-01-02", date)
		if err != nil || at.After(today) {
			return curveIndexObservation{}, fmt.Errorf("RGBI history has an invalid or future date")
		}
		duration, dErr := decimal.NewFromString(strings.Trim(string(row[columns["DURATION"]]), "\""))
		yield, yErr := decimal.NewFromString(strings.Trim(string(row[columns["YIELD"]]), "\""))
		if dErr != nil || yErr != nil || !duration.IsPositive() || yield.IsNegative() {
			continue // Missing published statistics are not filled with assumptions.
		}
		if at.Equal(result.Date) && (!duration.Equal(result.MacaulayDays) || !yield.Equal(result.YieldPercent)) {
			return curveIndexObservation{}, fmt.Errorf("RGBI history has conflicting same-day statistics")
		}
		if result.Date.IsZero() || at.After(result.Date) {
			result = curveIndexObservation{Date: at, MacaulayDays: duration, YieldPercent: yield}
		}
	}
	if result.Date.IsZero() || today.Sub(result.Date) > 7*24*time.Hour {
		return curveIndexObservation{}, fmt.Errorf("current RGBI duration/yield observation unavailable (maximum age seven calendar days)")
	}
	return result, nil
}
