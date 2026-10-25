package market

import (
	"context"
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

type curveCatalogueFake struct {
	instrumentsReader
	bonds                []*pb.Bond
	futures              []*pb.Future
	byBond               map[string]*pb.Bond
	byFuture             map[string]*pb.Future
	coupons              map[string]*pb.GetBondCouponsResponse
	margin               *pb.GetFuturesMarginResponse
	bondUIDs, futureUIDs []string
}

func (f *curveCatalogueFake) Bonds(_ context.Context, r *pb.InstrumentsRequest, _ ...grpc.CallOption) (*pb.BondsResponse, error) {
	if r.GetInstrumentStatus() != pb.InstrumentStatus_INSTRUMENT_STATUS_BASE {
		return nil, fmt.Errorf("expected base bonds")
	}
	return &pb.BondsResponse{Instruments: f.bonds}, nil
}
func (f *curveCatalogueFake) Futures(_ context.Context, r *pb.InstrumentsRequest, _ ...grpc.CallOption) (*pb.FuturesResponse, error) {
	if r.GetInstrumentStatus() != pb.InstrumentStatus_INSTRUMENT_STATUS_BASE {
		return nil, fmt.Errorf("expected base futures")
	}
	return &pb.FuturesResponse{Instruments: f.futures}, nil
}
func (f *curveCatalogueFake) BondBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.BondResponse, error) {
	if r.GetIdType() != pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID {
		return nil, fmt.Errorf("not UID")
	}
	f.bondUIDs = append(f.bondUIDs, r.GetId())
	return &pb.BondResponse{Instrument: f.byBond[r.GetId()]}, nil
}
func (f *curveCatalogueFake) FutureBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.FutureResponse, error) {
	if r.GetIdType() != pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID {
		return nil, fmt.Errorf("not UID")
	}
	f.futureUIDs = append(f.futureUIDs, r.GetId())
	return &pb.FutureResponse{Instrument: f.byFuture[r.GetId()]}, nil
}
func (f *curveCatalogueFake) GetBondCoupons(_ context.Context, r *pb.GetBondCouponsRequest, _ ...grpc.CallOption) (*pb.GetBondCouponsResponse, error) {
	if r.GetInstrumentId() == "" || r.GetFrom() == nil || r.GetTo() == nil {
		return nil, fmt.Errorf("missing UID or full range")
	}
	return f.coupons[r.GetInstrumentId()], nil
}
func (f *curveCatalogueFake) GetFuturesMargin(context.Context, *pb.GetFuturesMarginRequest, ...grpc.CallOption) (*pb.GetFuturesMarginResponse, error) {
	return f.margin, nil
}

type curveDataFake struct {
	dataReader
	mu   sync.Mutex
	at   time.Time
	uids []string
}

func (f *curveDataFake) GetOrderBook(_ context.Context, r *pb.GetOrderBookRequest, _ ...grpc.CallOption) (*pb.GetOrderBookResponse, error) {
	f.mu.Lock()
	f.uids = append(f.uids, r.GetInstrumentId())
	f.mu.Unlock()
	return &pb.GetOrderBookResponse{InstrumentUid: r.GetInstrumentId(), OrderbookTs: timestamppb.New(f.at), Bids: []*pb.Order{{Price: quote("99"), Quantity: 100}}, Asks: []*pb.Order{{Price: quote("100"), Quantity: 100}}}, nil
}
func (f *curveDataFake) GetTradingStatus(_ context.Context, r *pb.GetTradingStatusRequest, _ ...grpc.CallOption) (*pb.GetTradingStatusResponse, error) {
	return &pb.GetTradingStatusResponse{InstrumentUid: r.GetInstrumentId(), ApiTradeAvailableFlag: true, LimitOrderAvailableFlag: true, TradingStatus: pb.SecurityTradingStatus_SECURITY_TRADING_STATUS_NORMAL_TRADING}, nil
}

func curveBondFixture(now time.Time) (*pb.Bond, *pb.GetBondCouponsResponse) {
	start := now.AddDate(0, -3, 0)
	middle := now.AddDate(0, 3, 0)
	end := now.AddDate(0, 9, 0)
	bond := &pb.Bond{Uid: "ofz-uid", Figi: "ofz-figi", Ticker: "SU26242RMFS6", Isin: "RU000A105RV3", ClassCode: "TQOB", Currency: "rub", Lot: 1, Nominal: rub("1000"), InitialNominal: rub("1000"), AciValue: rub("17.5"), MaturityDate: timestamppb.New(end), CouponQuantityPerYear: 2, ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true}
	coupon := func(number int64, a, b time.Time) *pb.Coupon {
		return &pb.Coupon{Figi: bond.Figi, CouponNumber: number, CouponStartDate: timestamppb.New(a), CouponEndDate: timestamppb.New(b), CouponDate: timestamppb.New(b), FixDate: timestamppb.New(b.AddDate(0, 0, -1)), PayOneBond: rub("50"), CouponType: pb.CouponType_COUPON_TYPE_CONSTANT}
	}
	return bond, &pb.GetBondCouponsResponse{Events: []*pb.Coupon{coupon(10, start, middle), coupon(11, middle, end)}}
}
func curveFutureFixture(now time.Time, uid, family string, days int) *pb.Future {
	return &pb.Future{Uid: uid, Ticker: uid, ClassCode: "SPBFUT", Currency: "rub", Lot: 1, AssetType: "TYPE_INDEX", BasicAsset: family, FuturesType: "DELIVERY_TYPE_CASH_SETTLEMENT", ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true, ShortEnabledFlag: true, ExpirationDate: timestamppb.New(now.AddDate(0, 0, days)), LastTradeDate: timestamppb.New(now.AddDate(0, 0, days-1))}
}
func newCurveProvider(t *testing.T) (*Provider, *curveCatalogueFake, *curveDataFake, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	bond, coupons := curveBondFixture(now)
	fake := &curveCatalogueFake{bonds: []*pb.Bond{bond}, byBond: map[string]*pb.Bond{bond.Uid: bond}, byFuture: map[string]*pb.Future{}, coupons: map[string]*pb.GetBondCouponsResponse{bond.Uid: coupons}, margin: &pb.GetFuturesMarginResponse{InitialMarginOnBuy: rub("2500"), InitialMarginOnSell: rub("3000"), MinPriceIncrement: quote("0.01"), MinPriceIncrementAmount: quote("10")}}
	data := &curveDataFake{at: now.Add(-time.Second)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			fmt.Fprint(w, `{"history":{"columns":["BOARDID","SECID","TRADEDATE","DURATION","YIELD"],"data":[["SNDX","RGBI","2026-10-23",1654,16.01]]}}`)
		} else {
			fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["RBZ6","RFUD",2],["KKX6","RFUD",3],["RFZ6","RFUD",4]]}}`)
		}
	}))
	t.Cleanup(server.Close)
	return &Provider{instruments: fake, data: data, http: server.Client(), feeURL: server.URL + "/fees", rgbiURL: server.URL + "/index", now: func() time.Time { return now }}, fake, data, now
}
func curveSelect() curve.Selection {
	return curve.Selection{FutureCount: 3, MaxFutureDays: 365, Depth: 10}
}

func TestCurveSnapshotMapsCurrentCashflowsAndFamilyUnits(t *testing.T) {
	p, fake, _, now := newCurveProvider(t)
	for i, family := range []string{"RGBI", "KEYRATE", "RUONIA"} {
		ticker := []string{"RBZ6", "KKX6", "RFZ6"}[i]
		f := curveFutureFixture(now, ticker, family, 30)
		fake.futures = append(fake.futures, f)
	}
	fake.futures = append(fake.futures, curveFutureFixture(now, "RGBIF", "RGBIF", 10))
	s, err := p.CurveSnapshot(context.Background(), curveSelect())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Instruments) != 4 || len(s.Books) != 4 {
		t.Fatalf("unexpected universe: %+v", s)
	}
	for _, i := range s.Instruments {
		if i.Kind == "bond" {
			if !i.PriceFactor.Equal(dec("10")) || !i.Bond.Accrued.Equal(dec("17.5")) || len(i.Bond.Cashflows) != 3 || i.ShortEnabled {
				t.Fatal("bond not mapped from complete current metadata", i)
			}
		} else if !i.PriceFactor.Equal(dec("1000")) || !i.InitialMarginBuy.Equal(dec("2500")) || !i.InitialMarginSell.Equal(dec("3000")) || !i.ExchangeFeeKnown {
			t.Fatal("wrong cash future units", i)
		}
		if !s.Books[i.UID].Time.Equal(now.Add(-time.Second)) {
			t.Fatal("exchange time was replaced")
		}
	}
	if !s.RGBIReference.Known || s.RGBIReference.Method != "aggregate_proxy" || !s.RGBIReference.MacaulayDays.Equal(dec("1654")) || !s.RGBIReference.ModifiedDuration.IsPositive() {
		t.Fatal("unlabeled or missing reference approximation", s.RGBIReference)
	}
}

func TestCurveBondRejectsUnverifiedCashflowsAndNonVanillaTerms(t *testing.T) {
	for _, name := range []string{"floating", "indexed", "amortizing", "callable", "unknown_coupon", "missing_first", "missing_last", "missing_middle", "wrong_currency", "wrong_uid"} {
		t.Run(name, func(t *testing.T) {
			p, f, _, now := newCurveProvider(t)
			b := f.byBond["ofz-uid"]
			c := f.coupons[b.Uid]
			switch name {
			case "floating":
				b.FloatingCouponFlag = true
			case "indexed":
				b.Nominal = rub("1010")
			case "amortizing":
				b.AmortizationFlag = true
			case "callable":
				b.CallDate = timestamppb.New(now.AddDate(0, 1, 0))
			case "unknown_coupon":
				c.Events[0].CouponType = pb.CouponType_COUPON_TYPE_UNSPECIFIED
			case "missing_first":
				c.Events = c.Events[1:]
			case "missing_last":
				c.Events = c.Events[:1]
			case "missing_middle":
				c.Events[1].CouponNumber = 12
			case "wrong_currency":
				b.AciValue.Currency = "cny"
			case "wrong_uid":
				b.Uid = "different"
			}
			if _, err := p.loadCurveBond(context.Background(), "ofz-uid", now); err == nil {
				t.Fatal("unverified bond accepted")
			}
		})
	}
}

func TestCurveTrackedUIDSurvivesDiscoveryAndClearsUnavailableFee(t *testing.T) {
	p, f, _, now := newCurveProvider(t)
	future := curveFutureFixture(now, "RBZ6", "RGBI", 30)
	saved, err := p.mapCurveFuture(context.Background(), future, map[string]decimal.Decimal{"RBZ6": dec("2")})
	if err != nil {
		t.Fatal(err)
	}
	future.BuyAvailableFlag = false
	future.ShortEnabledFlag = false
	f.byFuture[saved.UID] = future
	p.feeURL = p.rgbiURL // Valid HTTP but no securities block: must not retain saved known fee.
	selection := curveSelect()
	selection.MaxFutureDays = 1
	selection.Tracked = []curve.Instrument{saved}
	s, err := p.CurveSnapshot(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	var found *curve.Instrument
	for i := range s.Instruments {
		if s.Instruments[i].UID == saved.UID {
			found = &s.Instruments[i]
		}
	}
	if found == nil || found.BuyAvailable || found.ShortEnabled || found.ExchangeFeeKnown || !found.ExchangeFee.IsZero() || len(f.futureUIDs) != 1 {
		t.Fatalf("tracked metadata was filtered or stale: %+v", found)
	}
	if !saved.ExchangeFeeKnown || !saved.BuyAvailable {
		t.Fatal("entry instrument mutated")
	}
	future.ExpirationDate = timestamppb.New(now.AddDate(0, 0, 40))
	s, err = p.CurveSnapshot(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range s.Instruments {
		if i.UID == saved.UID {
			t.Fatal("changed tracked contract retained")
		}
	}
}

func TestCurveFutureSelectionTakesNearestValidContracts(t *testing.T) {
	p, f, _, now := newCurveProvider(t)
	for _, days := range []int{90, 10, 30, 60} {
		f.futures = append(f.futures, curveFutureFixture(now, fmt.Sprintf("K%d", days), "KEYRATE", days))
	}
	bad := curveFutureFixture(now, "wrongdelivery", "KEYRATE", 5)
	bad.FuturesType = "physical_delivery"
	f.futures = append(f.futures, bad)
	notStarted := curveFutureFixture(now, "notstarted", "KEYRATE", 6)
	notStarted.FirstTradeDate = timestamppb.New(now.Add(time.Hour))
	f.futures = append(f.futures, notStarted)
	lastTradeNow := curveFutureFixture(now, "endednow", "KEYRATE", 7)
	lastTradeNow.LastTradeDate = timestamppb.New(now)
	f.futures = append(f.futures, lastTradeNow)
	selection := curveSelect()
	selection.FutureCount = 2
	s, err := p.CurveSnapshot(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	uids := map[string]bool{}
	for _, i := range s.Instruments {
		uids[i.UID] = true
	}
	if !uids["K10"] || !uids["K30"] || uids["K60"] || uids["K90"] || uids["wrongdelivery"] || uids["notstarted"] || uids["endednow"] {
		t.Fatal("wrong contract selection", uids)
	}
}

func TestCurveMissingRequestedBondAndEmptyUniverseAreVisible(t *testing.T) {
	p, f, _, _ := newCurveProvider(t)
	f.bonds = nil
	selection := curveSelect()
	selection.BondTickers = []string{"SU26242RMFS6"}
	s, err := p.CurveSnapshot(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(s.Warnings, "\n")
	if len(s.Instruments) != 0 || s.ObservedAt.IsZero() || !strings.Contains(warnings, "requested OFZ SU26242RMFS6") || !strings.Contains(warnings, "no supported current or tracked instruments") {
		t.Fatal("missing requested universe was silent", s)
	}
}

func TestCurveUnavailableIndexReferencePreservesOtherFamilies(t *testing.T) {
	p, f, _, now := newCurveProvider(t)
	f.futures = []*pb.Future{curveFutureFixture(now, "KKX6", "KEYRATE", 30)}
	p.rgbiURL = p.feeURL // No index-history block.
	s, err := p.CurveSnapshot(context.Background(), curveSelect())
	if err != nil {
		t.Fatal(err)
	}
	if s.RGBIReference.Known || len(s.Instruments) != 2 || len(s.Books) != 2 || !strings.Contains(strings.Join(s.Warnings, "\n"), "RGBI duration reference unavailable") {
		t.Fatal("optional reference failure disabled independent family", s)
	}
}

func TestCurveIndexRejectsMissingStaleFutureAndConflictingStatistics(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, rows := range []string{`[]`, `[["SNDX","RGBI","2026-10-01",1654,16.01]]`, `[["SNDX","RGBI","2026-10-26",1654,16.01]]`, `[["SNDX","RGBI","2026-10-23",null,16.01]]`, `[["SNDX","RGBI","2026-10-23",1654,16.01],["SNDX","RGBI","2026-10-23",1655,16.01]]`} {
		body := `{"history":{"columns":["BOARDID","SECID","TRADEDATE","DURATION","YIELD"],"data":` + rows + `}}`
		if _, err := parseCurveIndex(strings.NewReader(body), now); err == nil {
			t.Fatal("unverified reference accepted", rows)
		}
	}
}
