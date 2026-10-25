package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/scanner"
)

type refreshFake struct {
	instrumentsReader
	stock   *pb.Share
	future  *pb.Future
	lookups []string
}

func (f *refreshFake) ShareBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.ShareResponse, error) {
	if r.GetIdType() != pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID || r.GetId() != "stock-uid" {
		return nil, fmt.Errorf("stock lookup must use recorded UID")
	}
	f.lookups = append(f.lookups, r.GetId())
	return &pb.ShareResponse{Instrument: f.stock}, nil
}
func (f *refreshFake) FutureBy(_ context.Context, r *pb.InstrumentRequest, _ ...grpc.CallOption) (*pb.FutureResponse, error) {
	if r.GetIdType() != pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID || r.GetId() != "future-uid" {
		return nil, fmt.Errorf("future lookup must use recorded UID")
	}
	f.lookups = append(f.lookups, r.GetId())
	return &pb.FutureResponse{Instrument: f.future}, nil
}

func refreshInputs() (scanner.Pair, *refreshFake) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	stock, future := eligibleShare(), eligibleFuture(now)
	future.MinPriceIncrement = quote("2.5")
	future.MinPriceIncrementAmount = quote("5")
	saved := scanner.Pair{
		Stock:  scanner.Instrument{UID: stock.Uid, PositionUID: stock.PositionUid, Ticker: stock.Ticker, ClassCode: stock.ClassCode, Kind: "share", Currency: "rub", Lot: int64(stock.Lot), PriceFactor: dec("1"), APIAvailable: true, BuyAvailable: true, SellAvailable: true},
		Future: scanner.Instrument{UID: future.Uid, Ticker: future.Ticker, ClassCode: future.ClassCode, Kind: "future", Currency: "rub", Lot: 1, UnderlyingPositionUID: stock.PositionUid, AssetSize: dec("100"), PriceFactor: dec("2"), Settlement: "physical", LastTrade: timestamp(future.LastTradeDate), Expiry: timestamp(future.ExpirationDate), APIAvailable: true, BuyAvailable: true, SellAvailable: true, ExchangeFee: dec("5.88"), ExchangeFeeKnown: true, ExchangeFeeSource: "old-entry-fee"},
	}
	return saved, &refreshFake{stock: stock, future: future}
}

func TestRefreshTrackedPairIndependentOfNewEntryFilters(t *testing.T) {
	saved, fake := refreshInputs()
	// Tracked positions survive removed entry permissions and expired discovery
	// horizons. Reading them must not call a base catalogue or require a watchlist.
	fake.stock.BuyAvailableFlag = false
	fake.future.SellAvailableFlag = false
	fake.future.ShortEnabledFlag = false
	fake.future.LastTradeDate = timestamppb.New(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	fake.future.ExpirationDate = timestamppb.New(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprintf(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["SRZ6","RFUD",%d]]}}`, 10+hits)
	}))
	defer srv.Close()
	p := &Provider{instruments: fake, http: srv.Client(), feeURL: srv.URL}
	first, err := p.RefreshPair(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stock.BuyAvailable || first.Future.SellAvailable || !first.Stock.SellAvailable || !first.Future.BuyAvailable || !first.Future.Expiry.Equal(fake.future.ExpirationDate.AsTime()) {
		t.Fatalf("current directions or dates were not preserved: %+v", first)
	}
	if !first.Future.ExchangeFeeKnown || !first.Future.ExchangeFee.Equal(dec("11")) || first.Future.ExchangeFeeSource != srv.URL {
		t.Fatalf("entry fee was not refreshed: %+v", first.Future)
	}
	// Closing flags are returned even when false, so the lifecycle can record a
	// blocked mark instead of silently forgetting the position.
	fake.stock.SellAvailableFlag = false
	fake.future.BuyAvailableFlag = false
	second, err := p.RefreshPair(context.Background(), saved)
	if err != nil || second.Stock.SellAvailable || second.Future.BuyAvailable || !second.Future.ExchangeFee.Equal(dec("12")) {
		t.Fatalf("closing permissions/current fee were not refreshed: %+v, %v", second, err)
	}
	if len(fake.lookups) != 4 || hits != 2 || !saved.Future.ExchangeFee.Equal(dec("5.88")) {
		t.Fatalf("unexpected requests or mutation of saved entry: %+v, fee hits %d", fake.lookups, hits)
	}
}

func TestRefreshNeverReusesUnknownCurrentFee(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			saved, fake := refreshInputs()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if unavailable {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[]}}`)
			}))
			defer srv.Close()
			p := &Provider{instruments: fake, http: srv.Client(), feeURL: srv.URL}
			pair, err := p.RefreshPair(context.Background(), saved)
			if err != nil || pair.Future.ExchangeFeeKnown || !pair.Future.ExchangeFee.IsZero() || len(p.Warnings()) == 0 {
				t.Fatalf("old fee masqueraded as current: %+v, %v", pair.Future, err)
			}
		})
	}
}

func TestRefreshRejectsChangedIdentityAndEconomicUnits(t *testing.T) {
	for name, change := range map[string]func(*refreshFake){
		"stock uid":           func(f *refreshFake) { f.stock.Uid = "another-stock" },
		"future uid":          func(f *refreshFake) { f.future.Uid = "another-future" },
		"underlying":          func(f *refreshFake) { f.future.BasicAssetPositionUid = "another-position" },
		"position":            func(f *refreshFake) { f.stock.PositionUid = "another-position" },
		"stock lot":           func(f *refreshFake) { f.stock.Lot = 100 },
		"contract size":       func(f *refreshFake) { f.future.BasicAssetSize = quote("200") },
		"quote units":         func(f *refreshFake) { f.future.MinPriceIncrementAmount = quote("10") },
		"missing quote units": func(f *refreshFake) { f.future.MinPriceIncrementAmount = nil },
		"settlement":          func(f *refreshFake) { f.future.FuturesType = "cash_settlement" },
	} {
		t.Run(name, func(t *testing.T) {
			saved, fake := refreshInputs()
			change(fake)
			p := &Provider{instruments: fake}
			if _, err := p.RefreshPair(context.Background(), saved); err == nil {
				t.Fatal("changed identity/economic units accepted")
			}
		})
	}
	saved, fake := refreshInputs()
	saved.Future.UnderlyingPositionUID = "unverified"
	if _, err := (&Provider{instruments: fake}).RefreshPair(context.Background(), saved); err == nil || !strings.Contains(err.Error(), "unverified") || len(fake.lookups) != 0 {
		t.Fatalf("unverified saved pair reached network: %v", err)
	}
}
