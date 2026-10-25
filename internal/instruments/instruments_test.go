package instruments

import (
	"cmp"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
)

func TestNormalizeKind(t *testing.T) {
	kind, err := NormalizeKind("ETFs")
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindETF {
		t.Fatalf("kind = %s", kind)
	}
	if kind, err = NormalizeKind("options"); err != nil || kind != KindOption {
		t.Fatalf("options: kind=%s err=%v", kind, err)
	}
}

func TestMatchesQueryCurrencyAndAPI(t *testing.T) {
	yes := true
	no := false
	items := []Summary{
		{Ticker: "SBER", Name: "Сбер Банк", Currency: "rub", Exchange: "MOEX", APITradeAvailable: &yes},
		{Ticker: "AAPL", Name: "Apple", Currency: "usd", Exchange: "SPBXM", APITradeAvailable: &no},
	}

	got := page(items, func(item Summary) bool {
		return matches(item, Filter{Query: "сбер", Currency: "RUB", APIOnly: true})
	}, 10, compareSummary)
	if got.Matched != 1 || got.Instruments[0].Ticker != "SBER" {
		t.Fatalf("matched = %#v", got)
	}
}

func TestPageLimit(t *testing.T) {
	items := []Summary{{Ticker: "B"}, {Ticker: "A"}, {Ticker: "C"}}
	got := page(items, func(Summary) bool { return true }, 2, compareSummary)
	if got.Matched != 3 {
		t.Fatalf("matched = %d", got.Matched)
	}
	if len(got.Instruments) != 2 || got.Instruments[0].Ticker != "A" || got.Instruments[1].Ticker != "B" {
		t.Fatalf("page = %#v", got.Instruments)
	}
}

func TestSearchRankPrefersExactTicker(t *testing.T) {
	items := []Summary{
		{Ticker: "RU000A101C89", Name: "Сбербанк 001Р-SBER15"},
		{Ticker: "SBERP", Name: "Сбербанк - привилегированные акции"},
		{Ticker: "SBER", ClassCode: "SMAL", Name: "Сбербанк"},
		{Ticker: "SBER", ClassCode: "TQBR", Name: "Сбербанк"},
	}
	got := page(items, func(Summary) bool { return true }, 2, func(a, b Summary) int {
		if c := cmp.Compare(searchRank("SBER", a), searchRank("SBER", b)); c != 0 {
			return c
		}
		return compareSummary(a, b)
	})
	if got.Instruments[0].ClassCode != "TQBR" || got.Instruments[1].Ticker != "SBER" {
		t.Fatalf("order = %#v", got.Instruments)
	}
}

func TestDedupeKeepsFirstUID(t *testing.T) {
	items := []Summary{
		{Ticker: "SBER", UID: "1", ClassCode: "TQBR"},
		{Ticker: "SBER", UID: "1", ClassCode: "TQBR"},
		{Ticker: "SBERP", UID: "2", ClassCode: "TQBR"},
	}
	got := dedupe(items)
	if len(got) != 2 || got[1].Ticker != "SBERP" {
		t.Fatalf("deduped = %#v", got)
	}
}

func TestTrimEnum(t *testing.T) {
	got := trimEnum("SECURITY_TRADING_STATUS_NORMAL_TRADING", "SECURITY_TRADING_STATUS_")
	if got != "normal_trading" {
		t.Fatalf("status = %s", got)
	}
}

func TestFutureCatalogueJSONPreservesDiscoveryMetadata(t *testing.T) {
	// Deliberately unfamiliar wire strings must survive the export unchanged;
	// diagnostics must not share the scanner's normalization assumptions.
	first := time.Date(2026, 1, 15, 7, 0, 0, 123456789, time.UTC)
	last := time.Date(2026, 12, 17, 15, 50, 0, 0, time.UTC)
	expires := last.Add(10 * time.Minute)
	f := &pb.Future{
		Uid: "future-uid", Ticker: "TESTZ6", ClassCode: "SPBFUT", Currency: "rub", Lot: 1,
		FuturesType: "new_delivery_wire_value", AssetType: "new_asset_wire_value", BasicAsset: "underlying-label",
		BasicAssetSize: &pb.Quotation{Units: 9007199254740993, Nano: 123456789}, BasicAssetPositionUid: "underlying-uid",
		MinPriceIncrementAmount: &pb.Quotation{Nano: 1},
		FirstTradeDate:          timestamppb.New(first), LastTradeDate: timestamppb.New(last), ExpirationDate: timestamppb.New(expires),
	}
	catalogue := Catalog{Instruments: mapListed(KindFuture, []*pb.Future{f})}
	raw, err := json.Marshal(catalogue)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Instruments []map[string]any `json:"instruments"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	card := exported.Instruments[0]
	for key, want := range map[string]string{
		"uid": "future-uid", "futures_type": "new_delivery_wire_value", "asset_type": "new_asset_wire_value",
		"basic_asset": "underlying-label", "basic_asset_size": "9007199254740993.123456789",
		"basic_asset_position_uid": "underlying-uid", "min_price_increment_amount": "0.000000001",
		"first_trade_date": "2026-01-15T07:00:00.123456789Z", "last_trade_date": "2026-12-17T15:50:00Z", "expiration_date": "2026-12-17T16:00:00Z",
	} {
		if got := card[key]; got != want {
			t.Errorf("%s = %v, want %q", key, got, want)
		}
	}
}

func TestFutureCatalogueDistinguishesMissingAndInvalidMetadata(t *testing.T) {
	futures := mapListed(KindFuture, []*pb.Future{
		{Uid: "missing"},
		{Uid: "zero", BasicAssetSize: &pb.Quotation{}, LastTradeDate: &timestamppb.Timestamp{}},
		{Uid: "invalid", ExpirationDate: &timestamppb.Timestamp{Seconds: 123, Nanos: -1}},
	})
	if futures[0].BasicAssetSize != "" || futures[0].LastTradeDate != "" || futures[0].ExpirationDate != "" {
		t.Fatal("missing fields acquired fabricated values", futures[0])
	}
	if futures[1].BasicAssetSize != "0" || futures[1].LastTradeDate != "1970-01-01T00:00:00Z" {
		t.Fatal("reported zero values were lost", futures[1])
	}
	if futures[2].ExpirationDate != "invalid(seconds=123,nanos=-1)" {
		t.Fatal("invalid timestamp was normalized or omitted", futures[2])
	}
	share := mapListed(KindShare, []*pb.Share{{Uid: "share", Ticker: "SBER"}})[0]
	raw, err := json.Marshal(share)
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"futures_type", "asset_type", "basic_asset", "basic_asset_size", "basic_asset_position_uid", "min_price_increment_amount", "first_trade_date", "last_trade_date", "expiration_date"} {
		if _, exists := card[key]; exists {
			t.Errorf("share unexpectedly includes futures field %s", key)
		}
	}
}
