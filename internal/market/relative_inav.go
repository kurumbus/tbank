package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

func (p *Provider) fetchRelativeINAV(ctx context.Context, now time.Time) (relativeNAV, error) {
	endpoint := p.fundINAVURL
	if endpoint == "" {
		endpoint = relativeINAVURL
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return relativeNAV{}, e
	}
	resp, e := p.http.Do(req)
	if e != nil {
		return relativeNAV{}, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return relativeNAV{}, fmt.Errorf("MOEX iNAV HTTP %d", resp.StatusCode)
	}
	value, e := parseRelativeINAV(io.LimitReader(resp.Body, 2<<20), p.now().UTC())
	value.Source = endpoint
	return value, e
}
func parseRelativeINAV(reader io.Reader, now time.Time) (relativeNAV, error) {
	type table struct {
		Columns []string            `json:"columns"`
		Data    [][]json.RawMessage `json:"data"`
	}
	var body struct {
		Securities table `json:"securities"`
		Marketdata table `json:"marketdata"`
	}
	if e := json.NewDecoder(reader).Decode(&body); e != nil {
		return relativeNAV{}, e
	}
	columns := func(t table, names ...string) (map[string]int, error) {
		m := map[string]int{}
		for n, c := range t.Columns {
			m[c] = n
		}
		for _, name := range names {
			if _, ok := m[name]; !ok {
				return nil, fmt.Errorf("iNAV missing %s", name)
			}
		}
		return m, nil
	}
	s, e := columns(body.Securities, "SECID", "BOARDID", "CURRENCYID")
	if e != nil {
		return relativeNAV{}, e
	}
	verified := false
	for _, row := range body.Securities.Data {
		if len(row) != len(body.Securities.Columns) {
			return relativeNAV{}, fmt.Errorf("malformed iNAV security")
		}
		var id, board, currency string
		if json.Unmarshal(row[s["SECID"]], &id) != nil || json.Unmarshal(row[s["BOARDID"]], &board) != nil || json.Unmarshal(row[s["CURRENCYID"]], &currency) != nil {
			return relativeNAV{}, fmt.Errorf("invalid iNAV identity")
		}
		if id == "TGLDB" && board == "INAV" && currency == "RUB" {
			verified = true
		}
	}
	if !verified {
		return relativeNAV{}, fmt.Errorf("TGLDB/INAV/RUB identity not verified")
	}
	m, e := columns(body.Marketdata, "SECID", "BOARDID", "CURRENTVALUE", "TRADEDATE", "TIME")
	if e != nil {
		return relativeNAV{}, e
	}
	var result relativeNAV
	for _, row := range body.Marketdata.Data {
		if len(row) != len(body.Marketdata.Columns) {
			return relativeNAV{}, fmt.Errorf("malformed iNAV market row")
		}
		var id, board, date, clock string
		if json.Unmarshal(row[m["SECID"]], &id) != nil || json.Unmarshal(row[m["BOARDID"]], &board) != nil || json.Unmarshal(row[m["TRADEDATE"]], &date) != nil || json.Unmarshal(row[m["TIME"]], &clock) != nil {
			return relativeNAV{}, fmt.Errorf("invalid iNAV quote metadata")
		}
		if id != "TGLDB" || board != "INAV" {
			continue
		}
		at, e := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, time.FixedZone("MSK", 3*60*60))
		if e != nil || at.After(now) {
			return relativeNAV{}, fmt.Errorf("invalid or future iNAV quote time")
		}
		value, e := decimal.NewFromString(strings.Trim(string(row[m["CURRENTVALUE"]]), "\""))
		if e != nil || !value.IsPositive() {
			return relativeNAV{}, fmt.Errorf("current iNAV unavailable")
		}
		if !result.At.IsZero() {
			return relativeNAV{}, fmt.Errorf("ambiguous iNAV quote")
		}
		result = relativeNAV{At: at.UTC(), Value: value, Currency: "rub"}
	}
	if result.At.IsZero() {
		return relativeNAV{}, fmt.Errorf("current TGLDB iNAV row missing")
	}
	return result, nil
}
