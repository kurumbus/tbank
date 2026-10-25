package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/shopspring/decimal"
)

// MOEX ISS publishes BUYSELLFEE per futures contract. Read the RFUD trading
// board, never substitute SCALPERFEE or negotiate an assumed percentage.
const moexFeeURL = "https://iss.moex.com/iss/engines/futures/markets/forts/securities.json?iss.meta=off&iss.only=securities&securities.columns=SECID,BOARDID,BUYSELLFEE"

func (p *Provider) fetchFees(ctx context.Context) (map[string]decimal.Decimal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.feeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MOEX ISS HTTP %d", resp.StatusCode)
	}
	return parseFees(io.LimitReader(resp.Body, 8<<20))
}
func parseFees(r io.Reader) (map[string]decimal.Decimal, error) {
	var body struct {
		Securities struct {
			Columns []string            `json:"columns"`
			Data    [][]json.RawMessage `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("MOEX ISS JSON: %w", err)
	}
	cols := map[string]int{}
	for i, c := range body.Securities.Columns {
		cols[c] = i
	}
	for _, c := range []string{"SECID", "BOARDID", "BUYSELLFEE"} {
		if _, ok := cols[c]; !ok {
			return nil, fmt.Errorf("MOEX ISS missing %s column", c)
		}
	}
	out := map[string]decimal.Decimal{}
	for _, row := range body.Securities.Data {
		if len(row) != len(body.Securities.Columns) {
			return nil, fmt.Errorf("MOEX ISS malformed securities row")
		}
		var ticker, board string
		if err := json.Unmarshal(row[cols["SECID"]], &ticker); err != nil {
			return nil, fmt.Errorf("MOEX ISS invalid ticker")
		}
		if err := json.Unmarshal(row[cols["BOARDID"]], &board); err != nil {
			return nil, fmt.Errorf("MOEX ISS invalid board")
		}
		if board != "RFUD" || ticker == "" {
			continue
		}
		raw := strings.TrimSpace(string(row[cols["BUYSELLFEE"]]))
		if raw == "null" || raw == "" {
			continue
		}
		fee, err := decimal.NewFromString(raw)
		if err != nil || fee.IsNegative() {
			return nil, fmt.Errorf("MOEX ISS invalid BUYSELLFEE for %s", ticker)
		}
		if old, ok := out[ticker]; ok && !old.Equal(fee) {
			return nil, fmt.Errorf("MOEX ISS conflicting fee for %s", ticker)
		}
		out[ticker] = fee
	}
	return out, nil
}
