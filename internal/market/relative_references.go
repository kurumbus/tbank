package market

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding/charmap"
	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

const relativeFundingURL = "https://iss.moex.com/iss/history/engines/futures/markets/forts/securities/"
const relativeFundURL = "https://t-capital-funds.ru/statistics/"
const relativeFXURL = "https://www.cbr.ru/scripts/XML_daily.asp"
const relativeINAVURL = "https://iss.moex.com/iss/engines/stock/markets/index/securities/TGLDB.json?iss.meta=off"

type relativeCacheKey struct{}
type cachedFunding struct {
	value curve.FundingReference
	err   error
}
type cachedFX struct {
	value  decimal.Decimal
	source string
	err    error
}
type relativeReferenceCache struct {
	funding   map[string]cachedFunding
	nav       *relativeNAV
	navErr    error
	navLoaded bool
	fx        map[string]cachedFX
}

func (p *Provider) refreshRelativeReferences(ctx context.Context, r *curve.Relationship, instruments map[string]curve.Instrument, from, now time.Time, warnings *[]string) {
	warn := func(message string) { *warnings = append(*warnings, r.ID+": "+message) }
	if from.IsZero() {
		from = now.AddDate(0, 0, -30)
	}
	if r.Strategy == curve.PerpetualDated {
		instrument, ok := instruments[r.PrimaryUID]
		if ok && (r.Funding == nil || !r.Funding.Known || r.Funding.CoverageStart.After(from) || r.Funding.CoverageEnd.Before(now)) {
			funding, e := p.fetchRelativeFunding(ctx, instrument, from, now)
			if e != nil {
				r.Funding = &curve.FundingReference{InstrumentUID: r.PrimaryUID}
				warn("verified funding coverage unavailable: " + e.Error())
			} else {
				r.Funding = &funding
				warn("MOEX completed daily funding history is a published-ledger model with date-only observations. Partial ownership days remain unresolved; no current-day funding is extrapolated")
			}
		}
	}
	if r.Fund != nil {
		// Manager daily NAV is USD and a closed-day observation. Conversion is
		// allowed only with an explicitly sourced FX observation from that date.
		nav, e := p.fetchRelativeNAV(ctx, now)
		if e == nil && nav.Currency == "usd" && (!r.Fund.FXRUBPerUSD.IsPositive() || r.Fund.FXSource == "" || !sameRelativeDate(r.Fund.FXAsOf, nav.At)) {
			fx, source, fxErr := p.fetchRelativeFX(ctx, nav.At)
			if fxErr == nil {
				r.Fund.FXRUBPerUSD = fx
				r.Fund.FXAsOf = nav.At
				r.Fund.FXSource = source
			} else {
				warn("matching-date official CBR conversion unavailable: " + fxErr.Error())
			}
		}
		if e != nil {
			warn("current manager NAV unavailable: " + e.Error())
		} else if nav.At.Before(r.Fund.NAVAsOf) {
			warn("published NAV predates the supplied reference; retained the newer dated input")
		} else if nav.Currency == "rub" {
			r.Fund.NAVPerUnitRUB = nav.Value
			r.Fund.NAVAsOf = nav.At
			r.Fund.NAVSource = nav.Source
			r.Fund.NAVIsRUB = true
		} else if r.Fund.FXRUBPerUSD.IsPositive() && r.Fund.FXSource != "" && sameRelativeDate(r.Fund.FXAsOf, nav.At) {
			r.Fund.NAVPerUnitRUB = nav.Value.Mul(r.Fund.FXRUBPerUSD)
			r.Fund.NAVAsOf = nav.At
			r.Fund.NAVSource = nav.Source + "; USD NAV converted with " + r.Fund.FXSource
			r.Fund.NAVIsRUB = false
			warn("manager NAV is a dated closed-day value, not intraday NAV; gold exposure and comparison-time risk remain separate required references")
		} else {
			warn("manager NAV is USD on " + nav.At.Format("2006-01-02") + "; matching-date verified USD/RUB conversion is missing, so no RUB NAV was invented")
		}
	}
	if r.Holding == nil && r.Fund == nil && r.Offer == nil {
		return
	}
	until := now.AddDate(1, 0, 0)
	if !r.ValidUntil.IsZero() && r.ValidUntil.After(now) && r.ValidUntil.Before(until) {
		until = r.ValidUntil
	}
	if r.Offer != nil && r.Offer.PayoutDate.After(until) {
		until = r.Offer.PayoutDate
	}
	var events []curve.DividendEvent
	complete := true
	for _, uid := range []string{r.PrimaryUID, r.HedgeUID} {
		i, ok := instruments[uid]
		if !ok || i.Kind != "share" {
			continue
		}
		found, e := p.relativeDividends(ctx, uid, from, until)
		if e != nil {
			complete = false
			warn(i.Ticker + " declared dividend calendar unavailable: " + e.Error())
			continue
		}
		events = append(events, found...)
	}
	// TGLD is an accumulating gold fund. Its distribution policy still requires
	// the supplied reference; unsupported GetDividends(ETF) is not zero evidence.
	if r.Fund != nil {
		if !r.Fund.DistributionsKnown {
			warn("fund distribution policy remains unverified; an empty share dividend calendar is not evidence")
		}
		return
	}
	source := "T-Bank GetDividends by exact UID; announced-calendar forecast as of " + now.Format(time.RFC3339) + "; unannounced future dividends are not ruled out; long net receipts conservatively zero, short gross obligations retained"
	if r.Holding != nil {
		r.Holding.DistributionsKnown = complete
		r.Holding.DistributionsAsOf = now
		r.Holding.DistributionsValidUntil = until
		r.Holding.DistributionsSource = source
		r.Holding.Dividends = events
	}
	if r.Offer != nil {
		r.Offer.DistributionsKnown = complete
		r.Offer.DistributionsAsOf = now
		r.Offer.DistributionsValidUntil = until
		r.Offer.DistributionsSource = source
		r.Offer.Dividends = events
	}
	r.Dividends = events
	if complete {
		warn("dividend coverage is a current announced-calendar forecast; future announcements can change costs. Long net dividends are conservatively zero; shorts owe full announced gross distributions")
	}
}

func (p *Provider) relativeDividends(ctx context.Context, uid string, from, until time.Time) ([]curve.DividendEvent, error) {
	r, e := p.instruments.GetDividends(ctx, &pb.GetDividendsRequest{InstrumentId: uid, From: timestamppb.New(from), To: timestamppb.New(until)})
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, fmt.Errorf("nil declared-calendar response")
	}
	events := []curve.DividendEvent{}
	for _, d := range r.GetDividends() {
		if d == nil {
			return nil, fmt.Errorf("nil dividend event")
		}
		if d.GetDividendType() == "Cancelled" {
			continue
		}
		amount, record, pay := d.GetDividendNet(), timestamp(d.GetRecordDate()), timestamp(d.GetPaymentDate())
		if amount == nil || !strings.EqualFold(amount.GetCurrency(), "rub") || money(amount).IsNegative() || record.IsZero() || pay.IsZero() || pay.Before(record) {
			return nil, fmt.Errorf("dividend amount, currency or entitlement/payment dates unknown")
		}
		if record.Before(from) || record.After(until) {
			return nil, fmt.Errorf("dividend response outside requested coverage")
		}
		events = append(events, curve.DividendEvent{InstrumentUID: uid, RecordDate: record, PayDate: pay, GrossPerUnit: money(amount), NetPerUnit: decimal.Zero, NetKnown: true, Verified: true, Source: "T-Bank GetDividends declared gross per security; zero long net receipt is an explicit conservative tax assumption"})
	}
	return events, nil
}

func (p *Provider) fetchRelativeFunding(ctx context.Context, i curve.Instrument, from, now time.Time) (curve.FundingReference, error) {
	cache, _ := ctx.Value(relativeCacheKey{}).(*relativeReferenceCache)
	if cache != nil {
		if v, ok := cache.funding[i.UID]; ok {
			return v.value, v.err
		}
	}
	value, err := p.fetchRelativeFundingRemote(ctx, i, from, now)
	if cache != nil {
		cache.funding[i.UID] = cachedFunding{value, err}
	}
	return value, err
}
func (p *Provider) fetchRelativeFundingRemote(ctx context.Context, i curve.Instrument, from, now time.Time) (curve.FundingReference, error) {
	if (i.Ticker != "CNYRUBF" && i.Ticker != "GLDRUBF") || !i.UnitSize.IsPositive() {
		return curve.FundingReference{}, fmt.Errorf("unsupported perpetual funding instrument or unit size")
	}
	base := p.fundingURL
	if base == "" {
		base = relativeFundingURL
	}
	u, e := url.Parse(strings.TrimRight(base, "/") + "/" + i.Ticker + ".json")
	if e != nil {
		return curve.FundingReference{}, e
	}
	q := u.Query()
	q.Set("iss.meta", "off")
	q.Set("iss.only", "history")
	msk := time.FixedZone("MSK", 3*60*60)
	q.Set("from", from.In(msk).Format("2006-01-02"))
	q.Set("till", now.In(msk).AddDate(0, 0, -1).Format("2006-01-02"))
	q.Set("history.columns", "BOARDID,TRADEDATE,SECID,SWAPRATE,ASSETCODE")
	result := curve.FundingReference{InstrumentUID: i.UID, Source: u.String(), Known: false}
	complete := false
	seen := map[string]bool{}
	for start := 0; start < 10000; start += 100 {
		q.Set("start", fmt.Sprint(start))
		u.RawQuery = q.Encode()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return result, e
		}
		resp, e := p.http.Do(req)
		if e != nil {
			return result, e
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return result, fmt.Errorf("funding HTTP %d", resp.StatusCode)
		}
		events, count, e := parseRelativeFunding(io.LimitReader(resp.Body, 4<<20), i, from, now, u.String())
		resp.Body.Close()
		if e != nil {
			return result, e
		}
		for _, event := range events {
			key := event.At.Format("2006-01-02")
			if seen[key] {
				return result, fmt.Errorf("funding history duplicated across pages")
			}
			seen[key] = true
			result.Events = append(result.Events, event)
		}
		result.Source = u.String()
		if count < 100 {
			complete = true
			break
		}
	}
	if !complete {
		return result, fmt.Errorf("funding pagination did not complete")
	}
	if len(result.Events) == 0 {
		return result, fmt.Errorf("no completed published funding dates")
	}
	location := time.FixedZone("MSK", 3*60*60)
	start := from.In(location)
	result.CoverageStart = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
	var latest time.Time
	for _, event := range result.Events {
		if event.At.After(latest) {
			latest = event.At
		}
	}
	last := latest.In(location)
	result.CoverageEnd = time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, location).Add(-time.Nanosecond)
	result.Known = true
	result.Source += "; complete paginated published ledger through last returned completed date; missing rows inside that interval are modelled as no published payment; partial days excluded"
	return result, nil
}

func parseRelativeFunding(reader io.Reader, i curve.Instrument, from, now time.Time, source string) ([]curve.FundingEvent, int, error) {
	var body struct {
		History struct {
			Columns []string            `json:"columns"`
			Data    [][]json.RawMessage `json:"data"`
		} `json:"history"`
	}
	if e := json.NewDecoder(reader).Decode(&body); e != nil {
		return nil, 0, e
	}
	columns := map[string]int{}
	for n, c := range body.History.Columns {
		columns[c] = n
	}
	for _, c := range []string{"BOARDID", "TRADEDATE", "SECID", "SWAPRATE", "ASSETCODE"} {
		if _, ok := columns[c]; !ok {
			return nil, 0, fmt.Errorf("funding history missing %s", c)
		}
	}
	events := []curve.FundingEvent{}
	seen := map[string]bool{}
	for _, row := range body.History.Data {
		if len(row) != len(body.History.Columns) {
			return nil, 0, fmt.Errorf("malformed funding row")
		}
		var board, date, ticker, asset string
		if json.Unmarshal(row[columns["BOARDID"]], &board) != nil || json.Unmarshal(row[columns["TRADEDATE"]], &date) != nil || json.Unmarshal(row[columns["SECID"]], &ticker) != nil || json.Unmarshal(row[columns["ASSETCODE"]], &asset) != nil {
			return nil, 0, fmt.Errorf("invalid funding identity")
		}
		if board != "RFUD" || ticker != i.Ticker {
			continue
		}
		expected := "CNYRUBTOM"
		if i.Ticker == "GLDRUBF" {
			expected = "GLDRUBTOM"
		}
		if asset != expected {
			return nil, 0, fmt.Errorf("funding underlying mismatch")
		}
		at, e := time.ParseInLocation("2006-01-02", date, time.FixedZone("MSK", 3*60*60))
		nowMSK := now.In(time.FixedZone("MSK", 3*60*60))
		fromMSK := from.In(nowMSK.Location())
		if e != nil || !at.Before(time.Date(nowMSK.Year(), nowMSK.Month(), nowMSK.Day(), 0, 0, 0, 0, nowMSK.Location())) {
			return nil, 0, fmt.Errorf("funding date is invalid or not a completed day")
		}
		if at.Before(time.Date(fromMSK.Year(), fromMSK.Month(), fromMSK.Day(), 0, 0, 0, 0, fromMSK.Location())) {
			return nil, 0, fmt.Errorf("funding date outside request")
		}
		if seen[date] {
			return nil, 0, fmt.Errorf("duplicate daily funding observation")
		}
		seen[date] = true
		rate, e := decimal.NewFromString(strings.Trim(string(row[columns["SWAPRATE"]]), "\""))
		if e != nil {
			return nil, 0, fmt.Errorf("unknown daily funding rate")
		}
		events = append(events, curve.FundingEvent{At: at, AmountPerLongContract: rate.Mul(i.UnitSize).Neg(), DateOnly: true, Reference: source + "; MOEX October 2026 perpetual guide: positive SWAPRATE paid by long; daily date only"})
	}
	return events, len(body.History.Data), nil
}

type relativeNAV struct {
	At               time.Time
	Value            decimal.Decimal
	Currency, Source string
}

var relativeNAVScript = regexp.MustCompile(`(?s)<script[^>]*id=["']__REACT_QUERY_STATE__capital["'][^>]*>(.*?)</script>`)

func (p *Provider) fetchRelativeNAV(ctx context.Context, now time.Time) (relativeNAV, error) {
	cache, _ := ctx.Value(relativeCacheKey{}).(*relativeReferenceCache)
	if cache != nil && cache.navLoaded {
		if cache.nav == nil {
			return relativeNAV{}, cache.navErr
		}
		return *cache.nav, cache.navErr
	}
	value, err := p.fetchRelativeINAV(ctx, now)
	if err != nil {
		value, err = p.fetchRelativeClosedNAV(ctx, now)
	}
	if cache != nil {
		cache.nav = &value
		cache.navErr = err
		cache.navLoaded = true
	}
	return value, err
}
func (p *Provider) fetchRelativeClosedNAV(ctx context.Context, now time.Time) (relativeNAV, error) {
	endpoint := p.fundURL
	if endpoint == "" {
		endpoint = relativeFundURL
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
		return relativeNAV{}, fmt.Errorf("fund manager HTTP %d", resp.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if e != nil {
		return relativeNAV{}, e
	}
	nav, e := parseRelativeNAV(data, now)
	nav.Source = endpoint
	return nav, e
}
func parseRelativeNAV(data []byte, now time.Time) (relativeNAV, error) {
	match := relativeNAVScript.FindSubmatch(data)
	if len(match) != 2 {
		return relativeNAV{}, fmt.Errorf("manager published NAV state is missing")
	}
	var body struct {
		Queries []struct {
			QueryKey []json.RawMessage `json:"queryKey"`
			State    struct {
				Data json.RawMessage `json:"data"`
			} `json:"state"`
		} `json:"queries"`
	}
	if e := json.Unmarshal([]byte(html.UnescapeString(string(match[1]))), &body); e != nil {
		return relativeNAV{}, fmt.Errorf("manager NAV JSON: %w", e)
	}
	for _, query := range body.Queries {
		var key string
		if len(query.QueryKey) == 0 || json.Unmarshal(query.QueryKey[0], &key) != nil || key != "getPortfolioDynamic" {
			continue
		}
		var payload struct {
			Funds []struct {
				Ticker, Isin, Currency string
				Metrics                []struct {
					Type          string
					LastClosedDay struct {
						Date  string
						Value json.RawMessage
					} `json:"lastClosedDay"`
				}
			} `json:"funds"`
		}
		if e := json.Unmarshal(query.State.Data, &payload); e != nil {
			return relativeNAV{}, e
		}
		for _, fund := range payload.Funds {
			if fund.Ticker != "TGLD" || fund.Isin != "RU000A101X50" || strings.ToUpper(fund.Currency) != "USD" {
				continue
			}
			for _, metric := range fund.Metrics {
				if metric.Type != "fairPrice" {
					continue
				}
				at, e := time.Parse("2006-01-02", metric.LastClosedDay.Date)
				if e != nil || at.After(now) {
					return relativeNAV{}, fmt.Errorf("manager NAV date invalid")
				}
				raw := strings.Trim(string(metric.LastClosedDay.Value), "\"")
				raw = strings.ReplaceAll(strings.ReplaceAll(raw, " ", ""), "\u00a0", "")
				value, e := decimal.NewFromString(raw)
				if e != nil || !value.IsPositive() {
					return relativeNAV{}, fmt.Errorf("manager NAV value invalid")
				}
				return relativeNAV{At: at, Value: value, Currency: "usd"}, nil
			}
		}
	}
	return relativeNAV{}, fmt.Errorf("exact TGLD USD per-unit NAV is unavailable")
}
func sameRelativeDate(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && a.UTC().Format("2006-01-02") == b.UTC().Format("2006-01-02")
}

func (p *Provider) fetchRelativeFX(ctx context.Context, date time.Time) (decimal.Decimal, string, error) {
	cache, _ := ctx.Value(relativeCacheKey{}).(*relativeReferenceCache)
	key := date.UTC().Format("2006-01-02")
	if cache != nil {
		if v, ok := cache.fx[key]; ok {
			return v.value, v.source, v.err
		}
	}
	value, source, err := p.fetchRelativeFXRemote(ctx, date)
	if cache != nil {
		cache.fx[key] = cachedFX{value, source, err}
	}
	return value, source, err
}
func (p *Provider) fetchRelativeFXRemote(ctx context.Context, date time.Time) (decimal.Decimal, string, error) {
	endpoint := p.fxURL
	if endpoint == "" {
		endpoint = relativeFXURL
	}
	u, e := url.Parse(endpoint)
	if e != nil {
		return decimal.Zero, "", e
	}
	q := u.Query()
	q.Set("date_req", date.UTC().Format("02/01/2006"))
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return decimal.Zero, "", e
	}
	resp, e := p.http.Do(req)
	if e != nil {
		return decimal.Zero, "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, "", fmt.Errorf("CBR HTTP %d", resp.StatusCode)
	}
	value, e := parseRelativeFX(io.LimitReader(resp.Body, 2<<20), date)
	return value, u.String(), e
}
func parseRelativeFX(reader io.Reader, date time.Time) (decimal.Decimal, error) {
	var doc struct {
		Date       string `xml:"Date,attr"`
		Currencies []struct {
			CharCode string `xml:"CharCode"`
			Nominal  string `xml:"Nominal"`
			Value    string `xml:"Value"`
		} `xml:"Valute"`
	}
	d := xml.NewDecoder(reader)
	d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(label, "windows-1251") {
			return charmap.Windows1251.NewDecoder().Reader(input), nil
		}
		return nil, fmt.Errorf("unsupported CBR XML encoding %q", label)
	}
	if e := d.Decode(&doc); e != nil {
		return decimal.Zero, e
	}
	if doc.Date != date.UTC().Format("02.01.2006") {
		return decimal.Zero, fmt.Errorf("CBR returned a different valuation date")
	}
	for _, c := range doc.Currencies {
		if c.CharCode != "USD" {
			continue
		}
		nominal, e1 := decimal.NewFromString(strings.TrimSpace(c.Nominal))
		value, e2 := decimal.NewFromString(strings.ReplaceAll(strings.TrimSpace(c.Value), ",", "."))
		if e1 != nil || e2 != nil || !nominal.IsPositive() || !value.IsPositive() {
			return decimal.Zero, fmt.Errorf("invalid CBR USD nominal/rate")
		}
		return value.Div(nominal), nil
	}
	return decimal.Zero, fmt.Errorf("CBR USD rate is missing")
}
