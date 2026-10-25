package market

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
)

// A ticker match alone is only a diagnostic hint. Eligibility also requires
// an approved underlying alias and the contract's verified economic terms.
func futureTickerHint(ticker, family string) bool {
	return family != "" && futureTickerProduct(ticker) == family
}

func futureAvailabilityReason(f *pb.Future, now time.Time, maxDays int, perpetual bool) string {
	if !f.GetApiTradeAvailableFlag() || !f.GetBuyAvailableFlag() || !f.GetSellAvailableFlag() || !f.GetShortEnabledFlag() {
		return "API/buy/sell/short permission unavailable"
	}
	if first := timestamp(f.GetFirstTradeDate()); !first.IsZero() && first.After(now) {
		return "first_trade_date is after scan time"
	}
	if !perpetual && !timestamp(f.GetLastTradeDate()).After(now) {
		return "last_trade_date is missing or not after scan time"
	}
	if !perpetual && timestamp(f.GetExpirationDate()).After(now.Add(time.Duration(maxDays)*24*time.Hour)) {
		return "expiration_date exceeds discovery horizon"
	}
	return ""
}

func futureMetadataDiagnostic(f *pb.Future) string {
	units := "missing"
	if f.GetBasicAssetSize() != nil {
		units = quotation(f.GetBasicAssetSize()).String()
	}
	return fmt.Sprintf("%s uid=%q class=%q basic_asset=%q asset_type=%q futures_type=%q underlying_uid=%q currency=%q lot=%d units=%s api=%t buy=%t sell=%t short=%t first=%s last=%s expiry=%s",
		f.GetTicker(), f.GetUid(), f.GetClassCode(), f.GetBasicAsset(), f.GetAssetType(), f.GetFuturesType(), f.GetBasicAssetPositionUid(), f.GetCurrency(), f.GetLot(), units,
		f.GetApiTradeAvailableFlag(), f.GetBuyAvailableFlag(), f.GetSellAvailableFlag(), f.GetShortEnabledFlag(),
		futureDateDiagnostic(f.GetFirstTradeDate()), futureDateDiagnostic(f.GetLastTradeDate()), futureDateDiagnostic(f.GetExpirationDate()))
}

func futureDateDiagnostic(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "missing"
	}
	if ts.CheckValid() != nil {
		return fmt.Sprintf("invalid(seconds=%d,nanos=%d)", ts.GetSeconds(), ts.GetNanos())
	}
	return ts.AsTime().UTC().Format(time.RFC3339Nano)
}

type futureDiscovery struct {
	selector                       string
	catalogue, matched, tickerOnly int
	selected, limitSkipped         int
	counts                         map[string]int
	samples                        map[string][]string
}

func (d *futureDiscovery) reject(f *pb.Future, reason string) {
	if d.counts == nil {
		d.counts, d.samples = map[string]int{}, map[string][]string{}
	}
	d.counts[reason]++
	if len(d.samples[reason]) < 2 {
		d.samples[reason] = append(d.samples[reason], futureMetadataDiagnostic(f))
	}
}

func (d *futureDiscovery) messages() []string {
	result := []string{fmt.Sprintf("%s futures discovery: catalogue=%d metadata_matches=%d ticker_only=%d selected=%d selection_limit_skipped=%d", d.selector, d.catalogue, d.matched, d.tickerOnly, d.selected, d.limitSkipped)}
	reasons := make([]string, 0, len(d.counts))
	for reason := range d.counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for n, reason := range reasons {
		if n == 8 {
			result = append(result, fmt.Sprintf("%s futures: %d additional rejection reasons omitted; export futures metadata for full inspection", d.selector, len(reasons)-n))
			break
		}
		result = append(result, fmt.Sprintf("%s futures rejected=%d: %s; examples: %s", d.selector, d.counts[reason], reason, strings.Join(d.samples[reason], "; ")))
	}
	return result
}

func sortFutureChoices(values []*pb.Future) {
	sort.Slice(values, func(i, j int) bool {
		a, b := timestamp(values[i].GetExpirationDate()), timestamp(values[j].GetExpirationDate())
		if a.Equal(b) {
			return values[i].GetUid() < values[j].GetUid()
		}
		return a.Before(b)
	})
}
