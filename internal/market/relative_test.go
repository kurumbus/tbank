package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

type relativeFake struct {
	instrumentsReader
	shares      map[string]*pb.Share
	funds       map[string]*pb.Etf
	futures     map[string]*pb.Future
	factors     map[string]string
	dividends   map[string][]*pb.Dividend
	marginCalls map[string]int
}

func (f *relativeFake) Shares(context.Context, *pb.InstrumentsRequest, ...grpc.CallOption) (*pb.SharesResponse, error) {
	r := &pb.SharesResponse{}
	for _, v := range f.shares {
		r.Instruments = append(r.Instruments, v)
	}
	return r, nil
}
func (f *relativeFake) Etfs(context.Context, *pb.InstrumentsRequest, ...grpc.CallOption) (*pb.EtfsResponse, error) {
	r := &pb.EtfsResponse{}
	for _, v := range f.funds {
		r.Instruments = append(r.Instruments, v)
	}
	return r, nil
}
func (f *relativeFake) Futures(context.Context, *pb.InstrumentsRequest, ...grpc.CallOption) (*pb.FuturesResponse, error) {
	r := &pb.FuturesResponse{}
	for _, v := range f.futures {
		r.Instruments = append(r.Instruments, v)
	}
	return r, nil
}
func (f *relativeFake) ShareBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.ShareResponse, error) {
	if r.GetIdType() != pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID {
		return nil, fmt.Errorf("not UID")
	}
	return &pb.ShareResponse{Instrument: f.shares[r.GetId()]}, nil
}
func (f *relativeFake) EtfBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.EtfResponse, error) {
	return &pb.EtfResponse{Instrument: f.funds[r.GetId()]}, nil
}
func (f *relativeFake) FutureBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.FutureResponse, error) {
	return &pb.FutureResponse{Instrument: f.futures[r.GetId()]}, nil
}
func (f *relativeFake) GetFuturesMargin(_ context.Context, r *pb.GetFuturesMarginRequest, _ ...grpc.CallOption) (*pb.GetFuturesMarginResponse, error) {
	f.marginCalls[r.GetInstrumentId()]++
	factor := f.factors[r.GetInstrumentId()]
	if factor == "" {
		return nil, fmt.Errorf("no current multiplier")
	}
	return &pb.GetFuturesMarginResponse{InitialMarginOnBuy: rub("2000"), InitialMarginOnSell: rub("3000"), MinPriceIncrement: quote("1"), MinPriceIncrementAmount: quote(factor)}, nil
}
func (f *relativeFake) GetDividends(_ context.Context, r *pb.GetDividendsRequest, _ ...grpc.CallOption) (*pb.GetDividendsResponse, error) {
	return &pb.GetDividendsResponse{Dividends: f.dividends[r.GetInstrumentId()]}, nil
}

func newRelativeProvider(t *testing.T) (*Provider, *relativeFake, time.Time, *map[string]int) {
	t.Helper()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	f := &relativeFake{shares: map[string]*pb.Share{}, funds: map[string]*pb.Etf{}, futures: map[string]*pb.Future{}, factors: map[string]string{}, dividends: map[string][]*pb.Dividend{}, marginCalls: map[string]int{}}
	for _, ticker := range []string{"SBER", "SBERP", "TATN", "TATNP", "ENPG", "RUAL", "UGLD"} {
		f.shares[ticker] = &pb.Share{Uid: ticker, Ticker: ticker, PositionUid: ticker + "-position", ClassCode: "TQBR", Currency: "rub", Lot: 1, ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true, ShortEnabledFlag: true, Dshort: quote("0.5")}
	}
	f.funds["fund"] = &pb.Etf{Uid: "fund", Ticker: "TGLD@", Isin: "RU000A101X50", PositionUid: "fund-position", ClassCode: "SPBRU", Currency: "rub", Lot: 1, ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true, ShortEnabledFlag: true}
	add := func(uid, basic, asset, units, factor string) {
		v := &pb.Future{Uid: uid, Ticker: uid, ClassCode: "SPBFUT", Currency: "rub", Lot: 1, BasicAsset: basic, AssetType: asset, BasicAssetSize: quote(units), FuturesType: "cash_settlement", ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true, ShortEnabledFlag: true, ExpirationDate: timestamppb.New(now.AddDate(0, 0, 60)), LastTradeDate: timestamppb.New(now.AddDate(0, 0, 59))}
		if asset == "security" {
			v.FuturesType = "physical_delivery"
			v.BasicAssetPositionUid = relativeProduct(v) + "-position"
		}
		f.futures[uid] = v
		f.factors[uid] = factor
	}
	add("MXZ6", "MIX", "index", "100", "1")
	add("MMZ6", "MXI", "index", "10", "10")
	add("SiZ6", "Si", "currency", "1000", "1")
	add("UMZ6", "USDM", "currency", "100", "100")
	add("GDZ6", "GOLD", "commodity", "1", "80")
	add("GNZ6", "GOLDM", "commodity", "0.1", "8")
	add("SRZ6", "SBRF", "security", "100", "1")
	add("SPZ6", "SBPR", "security", "100", "1")
	add("TTZ6", "TATN", "security", "100", "1")
	add("TPZ6", "TATP", "security", "10", "1")
	add("CNYRUBF", "CNYRUBTOM", "currency", "1000", "1000")
	add("CRZ6", "CNY", "currency", "1000", "1")
	add("GLDRUBF", "GLDRUBTOM", "commodity", "1", "1")
	add("GLZ6", "GL", "commodity", "1", "1")
	hits := map[string]int{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch {
		case r.URL.Path == "/fees":
			rows := [][]any{}
			for uid := range f.futures {
				rows = append(rows, []any{uid, "RFUD", 2})
			}
			json.NewEncoder(w).Encode(map[string]any{"securities": map[string]any{"columns": []string{"SECID", "BOARDID", "BUYSELLFEE"}, "data": rows}})
		case strings.HasPrefix(r.URL.Path, "/funding/"):
			ticker := "CNYRUBF"
			asset := "CNYRUBTOM"
			if strings.Contains(r.URL.Path, "GLDRUBF") {
				ticker = "GLDRUBF"
				asset = "GLDRUBTOM"
			}
			fmt.Fprintf(w, `{"history":{"columns":["BOARDID","TRADEDATE","SECID","SWAPRATE","ASSETCODE"],"data":[["RFUD","2026-10-23","%s",0.01,"%s"]]}}`, ticker, asset)
		case r.URL.Path == "/inav":
			fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","CURRENCYID"],"data":[["TGLDB","INAV","RUB"]]},"marketdata":{"columns":["SECID","BOARDID","CURRENTVALUE","TRADEDATE","TIME","LASTVALUE"],"data":[["TGLDB","INAV",12.6,"2026-10-25","14:59:59",999]]}}`)
		case r.URL.Path == "/nav":
			fmt.Fprint(w, `<script id="__REACT_QUERY_STATE__capital" type="application/json">{"queries":[{"queryKey":["unrelated",{}],"state":{"data":[]}},{"queryKey":["getPortfolioDynamic"],"state":{"data":{"funds":[{"ticker":"TGLD","isin":"RU000A101X50","currency":"USD","metrics":[{"type":"fairPrice","lastClosedDay":{"date":"2026-10-22","value":"0.1574"}}]}]}}}]}</script>`)
		case r.URL.Path == "/fx":
			fmt.Fprint(w, `<ValCurs Date="22.10.2026"><Valute><CharCode>USD</CharCode><Nominal>1</Nominal><Value>81,9739</Value></Valute></ValCurs>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	p := &Provider{instruments: f, data: &curveDataFake{at: now.Add(-time.Second)}, http: server.Client(), feeURL: server.URL + "/fees", fundingURL: server.URL + "/funding", fundINAVURL: server.URL + "/inav", fundURL: server.URL + "/nav", fxURL: server.URL + "/fx", now: func() time.Time { return now }}
	return p, f, now, &hits
}
func relativeSelection(now time.Time) curve.RelativeSelection {
	return curve.RelativeSelection{Depth: 10, MaxPairsPerFamily: 3, MaxDays: 365, HistoryFrom: now.AddDate(0, 0, -5)}
}
func relativeDefinition(id, strategy, a, b string) curve.Relationship {
	return curve.Relationship{ID: id, Strategy: strategy, PrimarySelector: a, HedgeSelector: b, PrimaryLots: 1}
}

func TestRelativeResolvesAllFamiliesFromExactCurrentMetadata(t *testing.T) {
	p, _, now, hits := newRelativeProvider(t)
	selection := relativeSelection(now)
	for _, v := range [][4]string{{"mx", curve.StandardMini, "MX", "MM"}, {"usd", curve.StandardMini, "Si", "USDM"}, {"gold", curve.StandardMini, "GOLD", "GOLDM"}, {"sber", curve.ShareClass, "SBER", "SBERP"}, {"tat", curve.ShareClass, "TATN", "TATNP"}, {"cny", curve.PerpetualDated, "CNYRUBF", "CR"}, {"gl", curve.PerpetualDated, "GLDRUBF", "GL"}, {"holding", curve.HoldingStub, "ENPG", "RUAL"}, {"fund", curve.FundNAV, "TGLD@", "GL"}, {"offer", curve.CashOffer, "UGLD", ""}} {
		r := relativeDefinition(v[0], v[1], v[2], v[3])
		if v[1] == curve.HoldingStub {
			r.Holding = &curve.HoldingReference{SharesPerParent: dec("7")}
		}
		if v[1] == curve.FundNAV {
			r.Fund = &curve.FundReference{DistributionsKnown: true, DistributionsSource: "accumulating rules"}
		}
		if v[1] == curve.CashOffer {
			r.Offer = &curve.OfferReference{}
		}
		selection.Relationships = append(selection.Relationships, r)
	}
	s, err := p.RelativeSnapshot(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Relationships) != 10 || len(s.Books) != 18 {
		t.Fatalf("missing relative universe: rel=%d books=%d warnings=%v", len(s.Relationships), len(s.Books), s.Warnings)
	}
	byID := map[string]curve.Relationship{}
	for _, r := range s.Relationships {
		byID[strings.Split(r.ID, "@")[0]] = r
	}
	if !byID["mx"].HedgeLotsPerPrimaryLot.Equal(dec("10")) || !byID["mx"].PrimaryPriceScale.Equal(dec("0.01")) || !byID["mx"].HedgePriceScale.Equal(dec("1")) {
		t.Fatal("MX unit normalization wrong", byID["mx"])
	}
	if !byID["usd"].PrimaryPriceScale.Equal(dec("0.001")) || !byID["usd"].HedgePriceScale.Equal(dec("1")) {
		t.Fatal("currency normalization wrong")
	}
	if !byID["cny"].PrimaryPriceScale.Equal(dec("1")) || !byID["cny"].HedgePriceScale.Equal(dec("0.001")) {
		t.Fatal("perpetual/dated raw conventions guessed instead of converted")
	}
	if !byID["cny"].Funding.Known || !byID["cny"].Funding.Events[0].AmountPerLongContract.Equal(dec("-10")) || !byID["cny"].Funding.Events[0].DateOnly {
		t.Fatal("funding sign/units/date coverage wrong")
	}
	if !byID["fund"].Fund.NAVIsRUB || !byID["fund"].Fund.NAVPerUnitRUB.Equal(dec("12.6")) || byID["fund"].Verified || byID["offer"].Verified {
		t.Fatal("cash references were certified by lookup", byID["fund"])
	}
	for _, i := range s.Instruments {
		if i.Kind == "share" && !i.InitialMarginSell.Equal(dec("100")) {
			t.Fatal("cash-short full-notional reserve missing", i)
		}
	}
	if (*hits)["/inav"] != 1 {
		t.Fatal("NAV fetched repeatedly", *hits)
	}
	if selection.Relationships[8].Fund.NAVIsRUB || !selection.Relationships[8].Fund.NAVPerUnitRUB.IsZero() {
		t.Fatal("input relationship reference mutated")
	}
}

func TestRelativeRejectsDifferentFixingExpiryAndUnsupportedClassPair(t *testing.T) {
	for _, kind := range []string{"expiry", "settlement", "underlying", "classpair", "zero_unit"} {
		t.Run(kind, func(t *testing.T) {
			p, f, now, _ := newRelativeProvider(t)
			selection := relativeSelection(now)
			r := relativeDefinition("pair", curve.StandardMini, "MX", "MM")
			switch kind {
			case "expiry":
				f.futures["MMZ6"].ExpirationDate = timestamppb.New(now.AddDate(0, 0, 61))
			case "settlement":
				f.futures["MMZ6"].FuturesType = "physical_delivery"
			case "underlying":
				f.futures["MXZ6"].BasicAssetPositionUid = "one"
				f.futures["MMZ6"].BasicAssetPositionUid = "two"
			case "classpair":
				r = relativeDefinition("pair", curve.ShareClass, "SBER", "TATNP")
			case "zero_unit":
				f.futures["MMZ6"].BasicAssetSize = nil
			}
			selection.Relationships = []curve.Relationship{r}
			s, e := p.RelativeSnapshot(context.Background(), selection)
			if e != nil {
				t.Fatal(e)
			}
			if len(s.Relationships) != 0 {
				t.Fatal("incompatible relationship accepted", s.Relationships)
			}
		})
	}
}

func TestRelativeTrackedRefreshKeepsDefinitionAndUpdatesFXAndExpiryProvenance(t *testing.T) {
	p, f, now, _ := newRelativeProvider(t)
	selection := relativeSelection(now)
	selection.Relationships = []curve.Relationship{relativeDefinition("gold", curve.StandardMini, "GOLD", "GOLDM")}
	first, e := p.RelativeSnapshot(context.Background(), selection)
	if e != nil {
		t.Fatal(e)
	}
	tracked := relativeSelection(now)
	tracked.Tracked = first.Instruments
	tracked.TrackedRelationships = first.Relationships
	old := first.Relationships[0]
	f.factors["GDZ6"] = "90"
	f.factors["GNZ6"] = "9"
	f.futures["GDZ6"].ShortEnabledFlag = false
	p.now = func() time.Time { return now.Add(48 * time.Hour) }
	p.data = &curveDataFake{at: p.now().Add(-time.Second)}
	second, e := p.RelativeSnapshot(context.Background(), tracked)
	if e != nil {
		t.Fatal(e)
	}
	if len(second.Relationships) != 1 || second.Relationships[0].ID != old.ID || !second.Relationships[0].ValidUntil.After(p.now()) || !second.Relationships[0].HedgeLotsPerPrimaryLot.Equal(old.HedgeLotsPerPrimaryLot) {
		t.Fatal("tracked definition lost or provenance expired", second.Relationships)
	}
	for _, i := range second.Instruments {
		if i.UID == "GDZ6" && (!i.PriceFactor.Equal(dec("90")) || i.ShortEnabled) {
			t.Fatal("current FX/permission was frozen", i)
		}
	}
	if !first.Instruments[0].PriceFactorAt.Equal(now) || !first.Relationships[0].AsOf.Equal(now) {
		t.Fatal("entry snapshot mutated")
	}
}

func TestRelativeReferenceParsersFailClosedAndKeepUnits(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	xml := `<ValCurs Date="22.10.2026"><Valute><CharCode>USD</CharCode><Nominal>10</Nominal><Value>819,739</Value></Valute></ValCurs>`
	fx, e := parseRelativeFX(strings.NewReader(xml), time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	if e != nil || !fx.Equal(dec("81.9739")) {
		t.Fatal("CBR nominal/date conversion", fx, e)
	}
	if _, e = parseRelativeFX(strings.NewReader(xml), now); e == nil {
		t.Fatal("wrong FX valuation date accepted")
	}
	for _, raw := range []string{`{"securities":{"columns":["SECID","BOARDID","CURRENCYID"],"data":[["TGLDB","INAV","USD"]]}}`, `{"securities":{"columns":["SECID","BOARDID","CURRENCYID"],"data":[["TGLDB","INAV","RUB"]]},"marketdata":{"columns":["SECID","BOARDID","LASTVALUE","TRADEDATE","TIME"],"data":[["TGLDB","INAV",12,"2026-10-25","14:59:59"]]}}`} {
		if _, e := parseRelativeINAV(strings.NewReader(raw), now); e == nil {
			t.Fatal("unverified/wrong NAV value accepted")
		}
	}
}

func TestRelativeNAVFallbackUsesSameDateCBRAndIgnoresUnrelatedJSON(t *testing.T) {
	p, _, now, _ := newRelativeProvider(t)
	p.fundINAVURL = p.fundURL + "/missing"
	r := curve.Relationship{ID: "fund", Strategy: curve.FundNAV, Fund: &curve.FundReference{DistributionsKnown: true}}
	warnings := []string{}
	p.refreshRelativeReferences(context.Background(), &r, map[string]curve.Instrument{}, now, now, &warnings)
	if !r.Fund.NAVPerUnitRUB.Equal(dec("0.1574").Mul(dec("81.9739"))) || r.Fund.NAVIsRUB || !sameRelativeDate(r.Fund.FXAsOf, r.Fund.NAVAsOf) {
		t.Fatal("closed NAV conversion lost provenance", r.Fund, warnings)
	}
}

func TestRelativeDividendCalendarKeepsFullShortGrossAndConservativeLongNet(t *testing.T) {
	p, f, now, _ := newRelativeProvider(t)
	f.dividends["RUAL"] = []*pb.Dividend{{DividendType: "Regular Cash", DividendNet: rub("4"), RecordDate: timestamppb.New(now.Add(24 * time.Hour)), PaymentDate: timestamppb.New(now.Add(48 * time.Hour))}}
	events, e := p.relativeDividends(context.Background(), "RUAL", now, now.Add(72*time.Hour))
	if e != nil || len(events) != 1 || !events[0].GrossPerUnit.Equal(dec("4")) || !events[0].NetPerUnit.IsZero() || !events[0].NetKnown {
		t.Fatal("distribution cost convention wrong", events, e)
	}
	f.dividends["RUAL"][0].DividendNet.Currency = "usd"
	if _, e = p.relativeDividends(context.Background(), "RUAL", now, now.Add(72*time.Hour)); e == nil {
		t.Fatal("unknown dividend FX accepted")
	}
}

func TestRelativeTrackedBorrowFactsRefreshWithoutChangingOwnership(t *testing.T) {
	p, _, now, _ := newRelativeProvider(t)
	r := relativeDefinition("holding", curve.HoldingStub, "ENPG", "RUAL")
	r.Verified = true
	r.AsOf = now
	r.ValidUntil = now.AddDate(0, 1, 0)
	r.Holding = &curve.HoldingReference{SharesPerParent: dec("7"), RatioSource: "old ownership", Borrow: &curve.BorrowReference{AsOf: now, Source: "old borrowing"}}
	selection := relativeSelection(now)
	selection.Relationships = []curve.Relationship{r}
	first, e := p.RelativeSnapshot(context.Background(), selection)
	if e != nil {
		t.Fatal(e)
	}
	updated, e := copyRelative(r)
	if e != nil {
		t.Fatal(e)
	}
	updated.Holding.SharesPerParent = dec("8")
	updated.Holding.Borrow = &curve.BorrowReference{AsOf: now.Add(time.Hour), Source: "reviewed borrowing"}
	selection.Relationships = []curve.Relationship{updated}
	selection.Tracked = first.Instruments
	selection.TrackedRelationships = first.Relationships
	second, e := p.RelativeSnapshot(context.Background(), selection)
	if e != nil {
		t.Fatal(e)
	}
	got := second.Relationships[0]
	if !got.Holding.SharesPerParent.Equal(dec("7")) || got.Holding.Borrow.Source != "reviewed borrowing" || first.Relationships[0].Holding.Borrow.Source != "old borrowing" {
		t.Fatal("borrow refresh changed frozen ownership or entry", got)
	}
}

func TestRelativeINAVUsesResponseClockNotCatalogueStart(t *testing.T) {
	p, _, now, _ := newRelativeProvider(t)
	value, e := p.fetchRelativeINAV(context.Background(), now.Add(-time.Hour))
	if e != nil || !value.At.Equal(now.Add(-time.Second)) {
		t.Fatal("current iNAV rejected using stale catalogue-start clock", value, e)
	}
}

type relativePriceFake struct {
	priceReader
	requests []*pb.GetOrderPriceRequest
	zero     bool
	usd      bool
}

func (f *relativePriceFake) GetOrderPrice(_ context.Context, r *pb.GetOrderPriceRequest, _ ...grpc.CallOption) (*pb.GetOrderPriceResponse, error) {
	f.requests = append(f.requests, r)
	value := quotation(r.Price).Mul(decimal.NewFromInt(r.Quantity))
	response := &pb.GetOrderPriceResponse{LotsRequested: r.Quantity, InitialOrderAmount: rub(value.String()), ExecutedCommissionRub: rub(value.Mul(dec("0.0005")).Round(2).String())}
	if f.zero {
		response.ExecutedCommissionRub = rub("0")
	}
	if f.usd {
		response.InitialOrderAmount.Currency = "usd"
		response.ExecutedCommissionRub = rub(value.Mul(dec("80")).Mul(dec("0.0004")).Round(2).String())
		response.InstrumentExtra = &pb.GetOrderPriceResponse_ExtraFuture_{ExtraFuture: &pb.GetOrderPriceResponse_ExtraFuture{InitialMargin: rub("3000")}}
	}
	return response, nil
}
func TestRelativePreviewRecognizesFundPrivilegeWithoutGrantingStockFreeFees(t *testing.T) {
	f := &relativePriceFake{zero: true}
	p := &Provider{prices: f, users: &previewFake{}}
	leg := curve.Leg{Instrument: curve.Instrument{UID: "fund", Ticker: "TGLD@", ClassCode: "SPBRU", Kind: "etf", Lot: 1, PriceFactor: dec("1")}, Lots: 100, Side: "buy", Limit: dec("12")}
	r, e := p.RelativePreview(context.Background(), []curve.Leg{leg}, "account")
	if e != nil || len(r) != 1 || r[0].Status != "matched" || !strings.Contains(r[0].Message, "conservatively retain") {
		t.Fatal("recognized actual zero privilege failed", r, e)
	}
	leg.Instrument.Kind = "share"
	leg.Instrument.Ticker = "ENPG"
	if _, e = p.RelativePreview(context.Background(), []curve.Leg{leg}, "account"); e == nil {
		t.Fatal("unexplained free stock fee accepted")
	}
}
func TestRelativePreviewValidatesUSDNotionalAgainstLiveRUBCommission(t *testing.T) {
	f := &relativePriceFake{usd: true}
	p := &Provider{prices: f, users: &previewFake{}}
	leg := curve.Leg{Instrument: curve.Instrument{UID: "gold", Ticker: "GDZ6", Kind: "future", Lot: 1, PriceFactor: dec("80"), UnitSize: dec("1"), PriceCurrency: "usd", ExchangeFeeKnown: true, ExchangeFee: dec("2"), ExchangeFeeSource: "MOEX"}, Lots: 2, Side: "sell", Limit: dec("3000")}
	r, e := p.RelativePreview(context.Background(), []curve.Leg{leg}, "account")
	if e != nil || len(r) != 1 || !r[0].InitialMargin.Equal(dec("6000")) || !r[0].Commission.Equal(dec("192")) {
		t.Fatal("USD/RUB cost conversion", r, e)
	}
}
