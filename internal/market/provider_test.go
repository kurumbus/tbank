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
	"tbank/internal/scanner"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }
func quote(s string) *pb.Quotation {
	q, e := toQuotation(dec(s))
	if e != nil {
		panic(e)
	}
	return q
}
func rub(s string) *pb.MoneyValue {
	q := quote(s)
	return &pb.MoneyValue{Currency: "rub", Units: q.Units, Nano: q.Nano}
}

type catalogueFake struct {
	instrumentsReader
	shares                               []*pb.Share
	futures                              []*pb.Future
	shareCalls, futureCalls, marginCalls int
}

func (f *catalogueFake) Shares(_ context.Context, r *pb.InstrumentsRequest, _ ...grpc.CallOption) (*pb.SharesResponse, error) {
	f.shareCalls++
	if r.GetInstrumentStatus() != pb.InstrumentStatus_INSTRUMENT_STATUS_BASE {
		return nil, fmt.Errorf("not base")
	}
	return &pb.SharesResponse{Instruments: f.shares}, nil
}
func (f *catalogueFake) Futures(_ context.Context, r *pb.InstrumentsRequest, _ ...grpc.CallOption) (*pb.FuturesResponse, error) {
	f.futureCalls++
	if r.GetInstrumentStatus() != pb.InstrumentStatus_INSTRUMENT_STATUS_BASE {
		return nil, fmt.Errorf("not base")
	}
	return &pb.FuturesResponse{Instruments: f.futures}, nil
}
func (f *catalogueFake) GetFuturesMargin(_ context.Context, _ *pb.GetFuturesMarginRequest, _ ...grpc.CallOption) (*pb.GetFuturesMarginResponse, error) {
	f.marginCalls++
	return &pb.GetFuturesMarginResponse{InitialMarginOnSell: rub("5475.97"), MinPriceIncrement: quote("2.5"), MinPriceIncrementAmount: quote("5")}, nil
}

func eligibleShare() *pb.Share {
	return &pb.Share{Uid: "stock-uid", PositionUid: "stock-position", Ticker: "SBER", ClassCode: "TQBR", Lot: 10, Currency: "rub", ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true}
}
func eligibleFuture(now time.Time) *pb.Future {
	return &pb.Future{Uid: "future-uid", PositionUid: "future-position", BasicAssetPositionUid: "stock-position", BasicAsset: "SBRF", Ticker: "SRZ6", ClassCode: "SPBFUT", Lot: 1, Currency: "rub", AssetType: "security", FuturesType: "physical_delivery", BasicAssetSize: quote("100"), ApiTradeAvailableFlag: true, BuyAvailableFlag: true, SellAvailableFlag: true, ShortEnabledFlag: true, ExpirationDate: timestamppb.New(now.Add(20 * 24 * time.Hour)), LastTradeDate: timestamppb.New(now.Add(19 * 24 * time.Hour))}
}

func TestDiscoverUsesPositionUIDAndContractSize(t *testing.T) {
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	valid := eligibleFuture(now)
	wrong := eligibleFuture(now)
	wrong.Uid = "wrong-class"
	wrong.BasicAssetPositionUid = "preferred-position"
	cash := eligibleFuture(now)
	cash.Uid = "cash"
	cash.FuturesType = "cash_settlement"
	expired := eligibleFuture(now)
	expired.Uid = "expired"
	expired.LastTradeDate = timestamppb.New(now.Add(-time.Hour))
	fake := &catalogueFake{shares: []*pb.Share{eligibleShare()}, futures: []*pb.Future{wrong, cash, expired, valid}}
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["SRZ6","RFUD",5.88]]}}`)
	}))
	defer srv.Close()
	p := &Provider{instruments: fake, http: srv.Client(), feeURL: srv.URL, now: func() time.Time { return now }}
	pairs, err := p.Discover(context.Background(), []string{"sber"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs", len(pairs))
	}
	f := pairs[0].Future
	if !f.AssetSize.Equal(dec("100")) || f.Lot != 1 || !f.PriceFactor.Equal(dec("2")) || !f.InitialMargin.Equal(dec("5475.97")) {
		t.Fatalf("wrong contract mapping: %+v", f)
	}
	if !f.ExchangeFeeKnown || !f.ExchangeFee.Equal(dec("5.88")) {
		t.Fatalf("wrong fee: %+v", f)
	}
	if f.Settlement != "physical" {
		t.Fatalf("provider did not normalize physical settlement: %s", f.Settlement)
	}
	// Feed a real provider-discovered pair into the engine, so mismatches in
	// normalized names or quantity/price units cannot hide behind isolated tests.
	status := func(uid string) *pb.GetTradingStatusResponse {
		return &pb.GetTradingStatusResponse{InstrumentUid: uid, TradingStatus: pb.SecurityTradingStatus_SECURITY_TRADING_STATUS_NORMAL_TRADING, LimitOrderAvailableFlag: true, ApiTradeAvailableFlag: true}
	}
	stockBook, err := mapBook(pairs[0].Stock, &pb.GetOrderBookResponse{InstrumentUid: pairs[0].Stock.UID, OrderbookTs: timestamppb.New(now), Bids: []*pb.Order{{Price: quote("299"), Quantity: 10}}, Asks: []*pb.Order{{Price: quote("300"), Quantity: 10}}}, status(pairs[0].Stock.UID))
	if err != nil {
		t.Fatal(err)
	}
	futureBook, err := mapBook(f, &pb.GetOrderBookResponse{InstrumentUid: f.UID, OrderbookTs: timestamppb.New(now), Bids: []*pb.Order{{Price: quote("16000"), Quantity: 1}}, Asks: []*pb.Order{{Price: quote("16001"), Quantity: 1}}}, status(f.UID))
	if err != nil {
		t.Fatal(err)
	}
	evaluated, err := scanner.Evaluate(scanner.Snapshot{Pair: pairs[0], StockBook: stockBook, FutureBook: futureBook, ObservedAt: now}, scanner.Assumptions{Contracts: 1, CapitalLimit: dec("100000"), MaxDays: 30, MaxQuoteAge: time.Second, MaxQuoteSkew: time.Second}, scanner.Profiles())
	if err != nil {
		t.Fatalf("provider pair rejected by engine: %v", err)
	}
	if evaluated.Shares != 100 || evaluated.StockLots != 10 || !evaluated.GrossBasis.Equal(dec("2000")) {
		t.Fatalf("provider-to-engine units wrong: %+v", evaluated)
	}
	if fake.shareCalls != 1 || fake.futureCalls != 1 || fake.marginCalls != 1 || hits != 1 {
		t.Fatalf("redundant reads: %+v; HTTP %d", fake, hits)
	}
	if _, err := p.Discover(context.Background(), []string{"SBE"}, 30); err == nil {
		t.Fatal("accepted ticker prefix instead of exact ticker")
	}
}

func TestMissingFeeStaysUnknown(t *testing.T) {
	now := time.Now().UTC()
	fake := &catalogueFake{shares: []*pb.Share{eligibleShare()}, futures: []*pb.Future{eligibleFuture(now)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer srv.Close()
	p := &Provider{instruments: fake, http: srv.Client(), feeURL: srv.URL, now: func() time.Time { return now }}
	pairs, err := p.Discover(context.Background(), []string{"SBER"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].Future.ExchangeFeeKnown || len(p.Warnings()) == 0 {
		t.Fatal("missing source fabricated a known fee")
	}
}

func TestFeeParsingRejectsWrongBoardNullAndMalformed(t *testing.T) {
	fees, err := parseFees(strings.NewReader(`{"securities":{"columns":["BUYSELLFEE","SECID","BOARDID"],"data":[[0.000000123,"EXACT","RFUD"],[99,"OTHER","RFNU"],[null,"MISSING","RFUD"]]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fees) != 1 || !fees["EXACT"].Equal(dec("0.000000123")) {
		t.Fatalf("unexpected fees: %v", fees)
	}
	for _, body := range []string{
		`{"securities":{"columns":["SECID","BOARDID"],"data":[]}}`,
		`{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["X","RFUD",-1]]}}`,
		`{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["X","RFUD",1],["X","RFUD",2]]}}`,
	} {
		if _, err := parseFees(strings.NewReader(body)); err == nil {
			t.Fatalf("accepted malformed payload %s", body)
		}
	}
}

func TestBookPreservesExchangeTimeAndLotQuantities(t *testing.T) {
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	i := scanner.Instrument{UID: "u", Ticker: "SBER", ClassCode: "TQBR", Lot: 10}
	b := &pb.GetOrderBookResponse{InstrumentUid: "u", Ticker: "SBER", ClassCode: "TQBR", OrderbookTs: timestamppb.New(now), Bids: []*pb.Order{{Price: quote("287.123456789"), Quantity: 17}}, Asks: []*pb.Order{{Price: quote("287.133456789"), Quantity: 19}}}
	s := &pb.GetTradingStatusResponse{InstrumentUid: "u", ApiTradeAvailableFlag: true, LimitOrderAvailableFlag: true, TradingStatus: pb.SecurityTradingStatus_SECURITY_TRADING_STATUS_NORMAL_TRADING}
	book, err := mapBook(i, b, s)
	if err != nil {
		t.Fatal(err)
	}
	if !book.Time.Equal(now) || !book.Consistent || !book.TradeAvailable || book.Bids[0].Lots != 17 || !book.Bids[0].Price.Equal(dec("287.123456789")) {
		t.Fatalf("bad mapping: %+v", book)
	}
	b.Asks[0].Price = quote("287")
	book, _ = mapBook(i, b, s)
	if book.Consistent {
		t.Fatal("crossed book accepted")
	}
	b.InstrumentUid = "wrong"
	if _, err = mapBook(i, b, s); err == nil {
		t.Fatal("mismatched UID accepted")
	}
}

type dataFake struct {
	dataReader
	mu                     sync.Mutex
	bookCalls, statusCalls int
	now                    time.Time
}

func (f *dataFake) GetOrderBook(ctx context.Context, r *pb.GetOrderBookRequest, _ ...grpc.CallOption) (*pb.GetOrderBookResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.bookCalls++
	f.mu.Unlock()
	return &pb.GetOrderBookResponse{InstrumentUid: r.GetInstrumentId(), OrderbookTs: timestamppb.New(f.now), Bids: []*pb.Order{{Price: quote("10"), Quantity: 1}}, Asks: []*pb.Order{{Price: quote("11"), Quantity: 1}}}, nil
}
func (f *dataFake) GetTradingStatus(ctx context.Context, r *pb.GetTradingStatusRequest, _ ...grpc.CallOption) (*pb.GetTradingStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.statusCalls++
	f.mu.Unlock()
	return &pb.GetTradingStatusResponse{InstrumentUid: r.GetInstrumentId(), TradingStatus: pb.SecurityTradingStatus_SECURITY_TRADING_STATUS_NORMAL_TRADING, LimitOrderAvailableFlag: true, ApiTradeAvailableFlag: true}, nil
}
func TestSnapshotContextAndConcurrentMapping(t *testing.T) {
	now := time.Now().UTC()
	f := &dataFake{now: now}
	p := &Provider{data: f, now: func() time.Time { return now }}
	pair := scanner.Pair{Stock: scanner.Instrument{UID: "stock"}, Future: scanner.Instrument{UID: "future"}}
	s, err := p.Snapshot(context.Background(), pair, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.StockBook.UID != "stock" || s.FutureBook.UID != "future" || f.bookCalls != 2 || f.statusCalls != 2 {
		t.Fatalf("bad snapshot %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = p.Snapshot(ctx, pair, 10); err == nil {
		t.Fatal("cancellation ignored")
	}
}

type previewFake struct {
	userReader
	priceReader
	requests       []*pb.GetOrderPriceRequest
	qualified      bool
	marginOverride func(int64) *pb.MoneyValue
}

func (f *previewFake) GetInfo(context.Context, *pb.GetInfoRequest, ...grpc.CallOption) (*pb.GetInfoResponse, error) {
	return &pb.GetInfoResponse{Tariff: "trader", QualStatus: f.qualified}, nil
}
func (f *previewFake) GetOrderPrice(_ context.Context, r *pb.GetOrderPriceRequest, _ ...grpc.CallOption) (*pb.GetOrderPriceResponse, error) {
	f.requests = append(f.requests, r)
	response := &pb.GetOrderPriceResponse{LotsRequested: r.Quantity, ExecutedCommissionRub: rub("12.345678901")}
	if r.InstrumentId == "future" {
		margin := rub(decimal.NewFromInt(r.Quantity).Mul(dec("12500.50")).String())
		if f.marginOverride != nil {
			margin = f.marginOverride(r.Quantity)
		}
		response.InstrumentExtra = &pb.GetOrderPriceResponse_ExtraFuture_{ExtraFuture: &pb.GetOrderPriceResponse_ExtraFuture{InitialMargin: margin}}
	}
	return response, nil
}
func TestPreviewUsesExactLimitsDirectionsAndLotQuantities(t *testing.T) {
	f := &previewFake{}
	p := &Provider{users: f, prices: f}
	r := scanner.Result{Snapshot: scanner.Snapshot{Pair: scanner.Pair{Stock: scanner.Instrument{UID: "stock"}, Future: scanner.Instrument{UID: "future"}}}, StockLots: 20, Contracts: 2, StockLimit: dec("287.123456789"), FutureLimit: dec("29724")}
	preview, err := p.Preview(context.Background(), r, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Status != "ok" || preview.ActualTariff != "trader" || !preview.StockCommission.Equal(dec("12.345678901")) || !preview.RequiredInitialMargin.Equal(dec("25001")) {
		t.Fatalf("wrong preview %+v", preview)
	}
	if len(f.requests) != 3 || f.requests[0].Quantity != 20 || f.requests[1].Quantity != 2 || f.requests[2].Quantity != 1 || f.requests[0].Direction != pb.OrderDirection_ORDER_DIRECTION_BUY || f.requests[1].Direction != pb.OrderDirection_ORDER_DIRECTION_SELL || f.requests[2].Direction != pb.OrderDirection_ORDER_DIRECTION_SELL || !quotation(f.requests[0].Price).Equal(r.StockLimit) || !quotation(f.requests[1].Price).Equal(r.FutureLimit) || !quotation(f.requests[2].Price).Equal(r.FutureLimit) {
		t.Fatalf("wrong requests %+v", f.requests)
	}
	if _, err = p.Preview(context.Background(), r, ""); err == nil {
		t.Fatal("missing account accepted")
	}
	if len(f.requests) != 3 {
		t.Fatal("made requests before explicit account validation")
	}
}

func TestPreviewConservativeMarginAndQualification(t *testing.T) {
	r := scanner.Result{Snapshot: scanner.Snapshot{Pair: scanner.Pair{Stock: scanner.Instrument{UID: "stock"}, Future: scanner.Instrument{UID: "future"}}}, StockLots: 20, Contracts: 2, StockLimit: dec("287"), FutureLimit: dec("29724")}
	for name, override := range map[string]func(int64) *pb.MoneyValue{
		"missing":          func(int64) *pb.MoneyValue { return nil },
		"zero":             func(int64) *pb.MoneyValue { return rub("0") },
		"negative":         func(int64) *pb.MoneyValue { return rub("-5") },
		"foreign currency": func(int64) *pb.MoneyValue { return &pb.MoneyValue{Currency: "usd", Units: 12500} },
	} {
		t.Run(name, func(t *testing.T) {
			f := &previewFake{marginOverride: override}
			p := &Provider{users: f, prices: f}
			preview, err := p.Preview(context.Background(), r, "acct")
			if err == nil || preview.Status != "unavailable" {
				t.Fatalf("invalid margin accepted: %+v, %v", preview, err)
			}
		})
	}
	for _, representation := range []string{"total", "per contract"} {
		t.Run(representation, func(t *testing.T) {
			f := &previewFake{}
			if representation == "per contract" {
				f.marginOverride = func(int64) *pb.MoneyValue { return rub("12500.50") }
			}
			p := &Provider{users: f, prices: f}
			preview, err := p.Preview(context.Background(), r, "acct")
			if err != nil || preview.Status != "ok" || !preview.RequiredInitialMargin.Equal(dec("25001")) || !strings.Contains(preview.Message, "quantity convention") {
				t.Fatalf("wrong conservative aggregate: %+v, %v", preview, err)
			}
		})
	}
	for _, qualifiedLeg := range []string{"stock", "future"} {
		t.Run(qualifiedLeg, func(t *testing.T) {
			candidate := r
			if qualifiedLeg == "stock" {
				candidate.Snapshot.Pair.Stock.QualifiedOnly = true
			} else {
				candidate.Snapshot.Pair.Future.QualifiedOnly = true
			}
			f := &previewFake{}
			p := &Provider{users: f, prices: f}
			preview, err := p.Preview(context.Background(), candidate, "acct")
			if err == nil || preview.Status != "unavailable" || len(f.requests) != 0 {
				t.Fatalf("unqualified account accepted: %+v, %v", preview, err)
			}
			f.qualified = true
			if _, err = p.Preview(context.Background(), candidate, "acct"); err != nil {
				t.Fatalf("qualified account rejected: %v", err)
			}
		})
	}
}

func TestQuotationRoundTripDoesNotUseFloatingPoint(t *testing.T) {
	for _, value := range []string{"0.000000001", "123456789012345.123456789", "-1.999999999"} {
		q, err := toQuotation(dec(value))
		if err != nil {
			t.Fatal(err)
		}
		if !quotation(q).Equal(dec(value)) {
			t.Fatalf("round trip changed %s", value)
		}
	}
	if _, err := toQuotation(dec("1.0000000001")); err == nil {
		t.Fatal("subnanounit price silently rounded")
	}
}
