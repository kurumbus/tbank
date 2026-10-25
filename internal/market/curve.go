package market

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

// Only the standard OFZ-PD issue series are accepted. Inflation-linked,
// amortizing, foreign-currency and floating-rate OFZ series are excluded even
// when one catalogue boolean is missing or wrong.
var fixedOFZTicker = regexp.MustCompile(`^SU(250|262)[0-9]{2}RMFS[0-9]$`)

// CurveSnapshot returns current metadata and books, independently of trade
// sizing. A failed instrument/reference is omitted and explained, rather than
// replaced with cached entry economics. Tracked UIDs are refreshed even when
// no longer selected by the discovery horizon or current watchlist.
func (p *Provider) CurveSnapshot(ctx context.Context, selection curve.Selection) (curve.Snapshot, error) {
	if selection.FutureCount < 2 || selection.FutureCount > 3 || selection.MaxFutureDays < 1 || selection.MaxFutureDays > 730 || (selection.Depth != 1 && selection.Depth != 10 && selection.Depth != 20 && selection.Depth != 30 && selection.Depth != 40 && selection.Depth != 50) {
		return curve.Snapshot{}, fmt.Errorf("invalid curve future count, horizon or book depth")
	}
	result := curve.Snapshot{Books: map[string]scanner.Book{}}
	now := p.now().UTC()
	warn := func(s string) { result.Warnings = append(result.Warnings, s) }
	fees, err := p.fetchFees(ctx)
	if err != nil {
		warn("Futures exchange fees unavailable: " + err.Error())
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	wanted := map[string]bool{}
	availableRequested := map[string]bool{}
	for _, ticker := range selection.BondTickers {
		if ticker == "" || ticker != strings.ToUpper(strings.TrimSpace(ticker)) || !fixedOFZTicker.MatchString(ticker) || wanted[ticker] {
			return result, fmt.Errorf("bond selection must contain unique exact vanilla fixed-coupon OFZ tickers")
		}
		wanted[ticker] = true
	}
	byUID := map[string]curve.Instrument{}
	bonds, err := p.instruments.Bonds(ctx, &pb.InstrumentsRequest{InstrumentStatus: pb.InstrumentStatus_INSTRUMENT_STATUS_BASE.Enum()})
	if err != nil || bonds == nil {
		warn(fmt.Sprintf("OFZ catalogue unavailable: %v", err))
	} else {
		for _, bond := range bonds.GetInstruments() {
			if bond == nil || bond.GetClassCode() != "TQOB" || !fixedOFZTicker.MatchString(bond.GetTicker()) || !bond.GetApiTradeAvailableFlag() || !bond.GetBuyAvailableFlag() || (len(wanted) > 0 && !wanted[bond.GetTicker()]) {
				continue
			}
			instrument, e := p.loadCurveBond(ctx, bond.GetUid(), now)
			if e != nil {
				warn(bond.GetTicker() + ": " + e.Error())
				continue
			}
			byUID[instrument.UID] = instrument
			availableRequested[instrument.Ticker] = true
		}
	}
	requested := make([]string, 0, len(wanted))
	for ticker := range wanted {
		requested = append(requested, ticker)
	}
	sort.Strings(requested)
	for _, ticker := range requested {
		if !availableRequested[ticker] {
			warn("requested OFZ " + ticker + " is unavailable as an eligible current TQOB bond")
		}
	}
	futures, err := p.instruments.Futures(ctx, &pb.InstrumentsRequest{InstrumentStatus: pb.InstrumentStatus_INSTRUMENT_STATUS_BASE.Enum()})
	if err != nil || futures == nil {
		warn(fmt.Sprintf("Curve futures catalogue unavailable: %v", err))
	} else {
		for _, family := range []string{"RGBI", "KEYRATE", "RUONIA"} {
			diagnostic := futureDiscovery{selector: family, catalogue: len(futures.GetInstruments())}
			var values []*pb.Future
			for _, future := range futures.GetInstruments() {
				if future != nil && (future.GetBasicAsset() == family || futureTickerHint(future.GetTicker(), family)) {
					values = append(values, future)
				}
			}
			sortFutureChoices(values)
			for _, future := range values {
				if future.GetBasicAsset() != family {
					diagnostic.tickerOnly++
					diagnostic.reject(future, "ticker hint matches but basic_asset does not identify the selected family")
					continue
				}
				diagnostic.matched++
				if reason := futureAvailabilityReason(future, now, selection.MaxFutureDays, false); reason != "" {
					diagnostic.reject(future, reason)
					continue
				}
				if diagnostic.selected == selection.FutureCount {
					diagnostic.limitSkipped++
					continue
				}
				instrument, e := p.mapCurveFuture(ctx, future, fees)
				if e != nil {
					diagnostic.reject(future, e.Error())
					continue
				}
				byUID[instrument.UID] = instrument
				diagnostic.selected++
			}
			result.Warnings = append(result.Warnings, diagnostic.messages()...)
		}
	}
	for _, saved := range selection.Tracked {
		var instrument curve.Instrument
		var e error
		switch saved.Kind {
		case "bond":
			instrument, e = p.loadCurveBond(ctx, saved.UID, now)
		case "future":
			var response *pb.FutureResponse
			response, e = p.instruments.FutureBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: saved.UID})
			if e == nil {
				if response.GetInstrument().GetUid() != saved.UID {
					e = fmt.Errorf("tracked future UID changed")
				} else {
					instrument, e = p.mapCurveFuture(ctx, response.GetInstrument(), fees)
				}
			}
		default:
			e = fmt.Errorf("unsupported tracked kind %q", saved.Kind)
		}
		if e == nil {
			e = sameCurveUnits(saved, instrument)
		}
		if e != nil {
			delete(byUID, saved.UID)
			warn("tracked " + saved.Ticker + ": " + e.Error())
			continue
		}
		byUID[instrument.UID] = instrument
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if len(byUID) == 0 {
		warn("curve universe contains no supported current or tracked instruments")
		result.ObservedAt = p.now().UTC()
		return result, nil
	}
	// Supplemental index data is needed only for OFZ/RGBI hedges. Its failure must
	// not disable independently priced rate/calendar families.
	index, err := p.fetchCurveIndex(ctx, now)
	if err != nil {
		warn("RGBI duration reference unavailable: " + err.Error())
	} else {
		result.RGBIReference = curve.RGBIReference{AsOf: index.Date, Source: index.Source}
		// This derived aggregate proxy is opt-in in the domain policy; the raw
		// official statistics remain available for audit and model comparison.
		result.RGBIReference.MacaulayDays = index.MacaulayDays
		result.RGBIReference.YieldPercent = index.YieldPercent
		result.RGBIReference.ModifiedDuration = index.MacaulayDays.Div(decimal.NewFromInt(365)).Div(decimal.NewFromInt(1).Add(index.YieldPercent.Div(decimal.NewFromInt(100))))
		result.RGBIReference.Method = "aggregate_proxy"
		result.RGBIReference.Known = true
		warn("RGBI modified duration is an aggregate proxy derived from official Macaulay duration/yield; it omits the clean/dirty correction and does not establish exact hedge neutrality")
	}
	for _, instrument := range byUID {
		result.Instruments = append(result.Instruments, instrument)
	}
	sort.Slice(result.Instruments, func(i, j int) bool { return result.Instruments[i].UID < result.Instruments[j].UID })
	// Bound concurrency while fetching peer books close together in time. Their
	// exchange timestamps are preserved and checked by the strategy, not retimed.
	type bookResult struct {
		uid  string
		book scanner.Book
		err  error
	}
	jobs := make(chan curve.Instrument)
	output := make(chan bookResult, len(result.Instruments))
	var workers sync.WaitGroup
	for n := 0; n < 6; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for instrument := range jobs {
				book, e := p.curveBook(ctx, instrument, selection.Depth)
				output <- bookResult{instrument.UID, book, e}
			}
		}()
	}
	for _, instrument := range result.Instruments {
		jobs <- instrument
	}
	close(jobs)
	workers.Wait()
	close(output)
	for value := range output {
		if value.err != nil {
			warn(value.uid + ": " + value.err.Error())
			continue
		}
		result.Books[value.uid] = value.book
	}
	result.ObservedAt = p.now().UTC()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func curveFamily(family string) bool {
	return family == "RGBI" || family == "KEYRATE" || family == "RUONIA"
}

func (p *Provider) mapCurveFuture(ctx context.Context, f *pb.Future, fees map[string]decimal.Decimal) (curve.Instrument, error) {
	if f == nil || f.GetUid() == "" || !curveFamily(f.GetBasicAsset()) || f.GetClassCode() != "SPBFUT" || !strings.EqualFold(f.GetCurrency(), "rub") || f.GetLot() != 1 || futureSettlementType(f.GetFuturesType()) != "cash_settlement" || futureAssetType(f.GetAssetType()) != "index" {
		return curve.Instrument{}, fmt.Errorf("unsupported dated cash-settled curve contract")
	}
	expiry, last := timestamp(f.GetExpirationDate()), timestamp(f.GetLastTradeDate())
	if expiry.IsZero() || last.IsZero() || last.After(expiry) {
		return curve.Instrument{}, fmt.Errorf("missing or invalid dated future expiry")
	}
	m, err := p.instruments.GetFuturesMargin(ctx, &pb.GetFuturesMarginRequest{InstrumentId: f.GetUid()})
	if err != nil {
		return curve.Instrument{}, fmt.Errorf("current future margin: %w", err)
	}
	step, amount := quotation(m.GetMinPriceIncrement()), quotation(m.GetMinPriceIncrementAmount())
	buy, sell := m.GetInitialMarginOnBuy(), m.GetInitialMarginOnSell()
	if !step.IsPositive() || !amount.IsPositive() || buy == nil || sell == nil || !strings.EqualFold(buy.GetCurrency(), "rub") || !strings.EqualFold(sell.GetCurrency(), "rub") || !money(buy).IsPositive() || !money(sell).IsPositive() {
		return curve.Instrument{}, fmt.Errorf("current positive RUB margins and quote multiplier are required")
	}
	fee, known := fees[f.GetTicker()]
	return curve.Instrument{UID: f.GetUid(), Ticker: f.GetTicker(), ClassCode: f.GetClassCode(), Kind: "future", Family: f.GetBasicAsset(), Currency: "rub", Lot: 1, PriceFactor: amount.Div(step), Expiry: expiry, LastTrade: last, APIAvailable: f.GetApiTradeAvailableFlag(), BuyAvailable: f.GetBuyAvailableFlag(), SellAvailable: f.GetSellAvailableFlag(), ShortEnabled: f.GetShortEnabledFlag(), QualifiedOnly: f.GetForQualInvestorFlag(), InitialMarginBuy: money(buy), InitialMarginSell: money(sell), ExchangeFee: fee, ExchangeFeeKnown: known, ExchangeFeeSource: p.feeURL}, nil
}

func (p *Provider) loadCurveBond(ctx context.Context, uid string, now time.Time) (curve.Instrument, error) {
	if uid == "" {
		return curve.Instrument{}, fmt.Errorf("bond UID required")
	}
	r, err := p.instruments.BondBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: uid})
	if err != nil {
		return curve.Instrument{}, fmt.Errorf("current bond metadata: %w", err)
	}
	b := r.GetInstrument()
	if b == nil || b.GetUid() != uid || b.GetClassCode() != "TQOB" || !fixedOFZTicker.MatchString(b.GetTicker()) || !strings.HasPrefix(b.GetIsin(), "RU") || !strings.EqualFold(b.GetCurrency(), "rub") || b.GetLot() <= 0 || b.GetFloatingCouponFlag() || b.GetPerpetualFlag() || b.GetAmortizationFlag() || b.GetSubordinatedFlag() || b.GetCallDate() != nil || b.GetBondType() != pb.BondType_BOND_TYPE_UNSPECIFIED {
		return curve.Instrument{}, fmt.Errorf("not a verified vanilla RUB fixed-coupon OFZ")
	}
	nominal, initial, accrued := b.GetNominal(), b.GetInitialNominal(), b.GetAciValue()
	for _, value := range []*pb.MoneyValue{nominal, initial, accrued} {
		if value == nil || !strings.EqualFold(value.GetCurrency(), "rub") {
			return curve.Instrument{}, fmt.Errorf("RUB nominal and accrued interest are required")
		}
	}
	if !money(nominal).IsPositive() || !money(nominal).Equal(money(initial)) || money(accrued).IsNegative() {
		return curve.Instrument{}, fmt.Errorf("indexed/amortized nominal or invalid accrued interest")
	}
	maturity := timestamp(b.GetMaturityDate())
	if !maturity.After(now) || b.GetCouponQuantityPerYear() <= 0 {
		return curve.Instrument{}, fmt.Errorf("future maturity and known coupon schedule required")
	}
	coupons, err := p.instruments.GetBondCoupons(ctx, &pb.GetBondCouponsRequest{InstrumentId: uid, From: timestamppb.New(now), To: timestamppb.New(maturity.AddDate(0, 0, 7))})
	if err != nil {
		return curve.Instrument{}, fmt.Errorf("coupon schedule: %w", err)
	}
	flows, err := curveCoupons(coupons, b.GetFigi(), now, maturity, money(nominal))
	if err != nil {
		return curve.Instrument{}, err
	}
	terms := &curve.BondTerms{Nominal: money(nominal), Accrued: money(accrued), Maturity: maturity, AccruedAt: now, FixedCoupon: true, Cashflows: flows, Source: "T-Bank BondBy/GetBondCoupons by UID " + uid}
	return curve.Instrument{UID: uid, Ticker: b.GetTicker(), ClassCode: b.GetClassCode(), Kind: "bond", Family: "OFZ", Currency: "rub", Lot: int64(b.GetLot()), PriceFactor: money(nominal).Div(decimal.NewFromInt(100)), Expiry: maturity, APIAvailable: b.GetApiTradeAvailableFlag(), BuyAvailable: b.GetBuyAvailableFlag(), SellAvailable: b.GetSellAvailableFlag(), ShortEnabled: false, QualifiedOnly: b.GetForQualInvestorFlag(), Bond: terms}, nil
}

func curveCoupons(response *pb.GetBondCouponsResponse, figi string, now, maturity time.Time, nominal decimal.Decimal) ([]curve.Cashflow, error) {
	if response == nil || len(response.GetEvents()) == 0 {
		return nil, fmt.Errorf("full future fixed coupon schedule is missing")
	}
	values := append([]*pb.Coupon(nil), response.GetEvents()...)
	sort.Slice(values, func(i, j int) bool {
		return timestamp(values[i].GetCouponEndDate()).Before(timestamp(values[j].GetCouponEndDate()))
	})
	flows := []curve.Cashflow{}
	var previous *pb.Coupon
	day := func(t time.Time) string { return t.UTC().Format("2006-01-02") }
	for _, coupon := range values {
		if coupon == nil {
			return nil, fmt.Errorf("nil coupon in schedule")
		}
		date, start, end := timestamp(coupon.GetCouponDate()), timestamp(coupon.GetCouponStartDate()), timestamp(coupon.GetCouponEndDate())
		amount := coupon.GetPayOneBond()
		if date.IsZero() || start.IsZero() || end.IsZero() || !end.After(start) || !date.After(now) || date.Before(end) || end.After(maturity) || amount == nil || !strings.EqualFold(amount.GetCurrency(), "rub") || !money(amount).IsPositive() || (coupon.GetCouponType() != pb.CouponType_COUPON_TYPE_CONSTANT && coupon.GetCouponType() != pb.CouponType_COUPON_TYPE_FIX) || coupon.GetCouponNumber() <= 0 || (coupon.GetFigi() != "" && coupon.GetFigi() != figi) {
			return nil, fmt.Errorf("coupon amount, type, dates or identity is unverified")
		}
		if previous == nil {
			if start.After(now) {
				return nil, fmt.Errorf("coupon schedule does not cover the current coupon period")
			}
		} else if coupon.GetCouponNumber() != previous.GetCouponNumber()+1 || day(start) != day(timestamp(previous.GetCouponEndDate())) || !date.After(timestamp(previous.GetCouponDate())) {
			return nil, fmt.Errorf("coupon schedule is incomplete or duplicated")
		}
		flows = append(flows, curve.Cashflow{Date: date, Amount: money(amount), Kind: "coupon", RecordDate: timestamp(coupon.GetFixDate())})
		previous = coupon
	}
	if previous == nil || day(timestamp(previous.GetCouponEndDate())) != day(maturity) {
		return nil, fmt.Errorf("coupon schedule does not reach maturity")
	}
	flows = append(flows, curve.Cashflow{Date: maturity, Amount: nominal, Kind: "redemption"})
	return flows, nil
}

func sameCurveUnits(saved, current curve.Instrument) error {
	if saved.UID == "" || saved.UID != current.UID || saved.Kind != current.Kind || saved.Family != current.Family || saved.ClassCode != current.ClassCode || saved.Currency != current.Currency || saved.Lot != current.Lot || !saved.PriceFactor.Equal(current.PriceFactor) || !saved.Expiry.Equal(current.Expiry) || !saved.LastTrade.Equal(current.LastTrade) {
		return fmt.Errorf("tracked instrument financial identity changed; reconciliation required")
	}
	if saved.Kind == "bond" && (saved.Bond == nil || current.Bond == nil || !saved.Bond.Nominal.Equal(current.Bond.Nominal) || !saved.Bond.Maturity.Equal(current.Bond.Maturity)) {
		return fmt.Errorf("tracked bond nominal/maturity changed")
	}
	return nil
}

func (p *Provider) curveBook(ctx context.Context, instrument curve.Instrument, depth int) (scanner.Book, error) {
	uid := instrument.UID
	book, err := p.data.GetOrderBook(ctx, &pb.GetOrderBookRequest{InstrumentId: &uid, Depth: int32(depth)})
	if err != nil {
		return scanner.Book{}, fmt.Errorf("book: %w", err)
	}
	status, err := p.data.GetTradingStatus(ctx, &pb.GetTradingStatusRequest{InstrumentId: &uid})
	if err != nil {
		return scanner.Book{}, fmt.Errorf("trading status: %w", err)
	}
	return mapBook(scanner.Instrument{UID: uid, Ticker: instrument.Ticker, ClassCode: instrument.ClassCode}, book, status)
}
