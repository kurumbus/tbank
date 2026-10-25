package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

func TestFutureMetadataAcceptsDocumentedAndWireAliases(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"physical_delivery", "physical_delivery"},
		{"DELIVERY_TYPE_PHYSICAL_DELIVERY", "physical_delivery"},
		{"cash_settlement", "cash_settlement"},
		{"DELIVERY_TYPE_CASH_SETTLEMENT", "cash_settlement"},
		{"", ""}, {"DELIVERY_TYPE_UNSPECIFIED", ""},
		{"DELIVERY_TYPE_OTHER", ""}, {"not_physical_delivery", ""},
		{"DELIVERY_TYPE_PHYSICAL_DELIVERY_UNKNOWN", ""},
	} {
		if got := futureSettlementType(tc.raw); got != tc.want {
			t.Errorf("settlement %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
	for _, tc := range []struct{ raw, want string }{
		{"security", "security"}, {"TYPE_SECURITY", "security"},
		{"index", "index"}, {"TYPE_INDEX", "index"},
		{"currency", "currency"}, {"TYPE_CURRENCY", "currency"},
		{"commodity", "commodity"}, {"TYPE_COMMODITY", "commodity"},
		{"", ""}, {"TYPE_UNSPECIFIED", ""}, {"TYPE_OTHER", ""},
		{"not_security", ""}, {"TYPE_SECURITY_UNKNOWN", ""},
	} {
		if got := futureAssetType(tc.raw); got != tc.want {
			t.Errorf("asset %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// These are two independently encoded string fields in the broker response.
// Cover mixed responses as well as entirely documented or enum-shaped values.
func metadataFixtureEncoding(f *pb.Future, settlementEnum, assetEnum bool) {
	f.FuturesType = strings.ToLower(strings.TrimPrefix(f.FuturesType, "DELIVERY_TYPE_"))
	f.AssetType = strings.ToLower(strings.TrimPrefix(f.AssetType, "TYPE_"))
	if settlementEnum {
		f.FuturesType = "DELIVERY_TYPE_" + strings.ToUpper(f.FuturesType)
	}
	if assetEnum {
		f.AssetType = "TYPE_" + strings.ToUpper(f.AssetType)
	}
}

func TestCarryDiscoveryAndTrackedRefreshSupportBothMetadataEncodings(t *testing.T) {
	for _, settlementEnum := range []bool{false, true} {
		for _, assetEnum := range []bool{false, true} {
			t.Run(fmt.Sprintf("settlement_enum=%t/asset_enum=%t", settlementEnum, assetEnum), func(t *testing.T) {
				now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
				stock, future := eligibleShare(), eligibleFuture(now)
				metadataFixtureEncoding(future, settlementEnum, assetEnum)
				future.MinPriceIncrement, future.MinPriceIncrementAmount = quote("2.5"), quote("5")
				fake := &catalogueFake{shares: []*pb.Share{stock}, futures: []*pb.Future{future}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["SRZ6","RFUD",5.88]]}}`)
				}))
				defer server.Close()
				p := &Provider{instruments: fake, http: server.Client(), feeURL: server.URL, now: func() time.Time { return now }}
				pairs, err := p.Discover(context.Background(), []string{"SBER"}, 30)
				if err != nil || len(pairs) != 1 {
					t.Fatalf("valid broker metadata rejected in discovery: pairs=%d err=%v", len(pairs), err)
				}
				if pairs[0].Future.Settlement != "physical" || !pairs[0].Future.AssetSize.Equal(dec("100")) {
					t.Fatal("wire encoding leaked into persisted settlement or contract sizing")
				}
				p.instruments = &refreshFake{stock: stock, future: future}
				refreshed, err := p.RefreshPair(context.Background(), pairs[0])
				if err != nil || refreshed.Future.Settlement != "physical" || !refreshed.Future.PriceFactor.Equal(pairs[0].Future.PriceFactor) {
					t.Fatalf("tracked metadata refresh rejected the same contract: %+v %v", refreshed.Future, err)
				}
			})
		}
	}
}

func TestCurveAndRelativeDiscoverySupportBothMetadataEncodings(t *testing.T) {
	for _, settlementEnum := range []bool{false, true} {
		for _, assetEnum := range []bool{false, true} {
			t.Run(fmt.Sprintf("settlement_enum=%t/asset_enum=%t", settlementEnum, assetEnum), func(t *testing.T) {
				p, catalogue, _, now := newCurveProvider(t)
				for i, family := range []string{"RGBI", "KEYRATE", "RUONIA"} {
					f := curveFutureFixture(now, []string{"RBZ6", "KKX6", "RFZ6"}[i], family, 30)
					metadataFixtureEncoding(f, settlementEnum, assetEnum)
					catalogue.futures = append(catalogue.futures, f)
				}
				snapshot, err := p.CurveSnapshot(context.Background(), curveSelect())
				if err != nil || len(snapshot.Instruments) != 4 || len(snapshot.Books) != 4 {
					t.Fatalf("curve metadata rejection: instruments=%d books=%d err=%v warnings=%v", len(snapshot.Instruments), len(snapshot.Books), err, snapshot.Warnings)
				}

				rp, relative, now, _ := newRelativeProvider(t)
				for _, f := range relative.futures {
					metadataFixtureEncoding(f, settlementEnum, assetEnum)
				}
				selection := relativeSelection(now)
				selection.Relationships = []curve.Relationship{
					relativeDefinition("index", curve.StandardMini, "MX", "MM"),
					relativeDefinition("currency", curve.StandardMini, "Si", "USDM"),
					relativeDefinition("commodity", curve.StandardMini, "GOLD", "GOLDM"),
					relativeDefinition("security", curve.ShareClass, "SBER", "SBERP"),
				}
				snapshot, err = rp.RelativeSnapshot(context.Background(), selection)
				if err != nil || len(snapshot.Relationships) != 4 || len(snapshot.Books) != 8 {
					t.Fatalf("relative metadata rejection: relationships=%d books=%d err=%v warnings=%v", len(snapshot.Relationships), len(snapshot.Books), err, snapshot.Warnings)
				}
				for _, i := range snapshot.Instruments {
					want := "cash_settlement"
					if i.Family == "SBER" || i.Family == "SBERP" {
						want = "physical_delivery"
					}
					if i.SettlementType != want {
						t.Fatalf("noncanonical persisted relative type %q for %s", i.SettlementType, i.Ticker)
					}
				}
			})
		}
	}
}

func TestFutureMetadataNormalizationDoesNotBroadenEligibleContracts(t *testing.T) {
	for _, tc := range []struct{ name, settlement, asset string }{
		{"unknown_delivery", "DELIVERY_TYPE_UNKNOWN", "TYPE_SECURITY"},
		{"cash_instead_of_physical", "DELIVERY_TYPE_CASH_SETTLEMENT", "TYPE_SECURITY"},
		{"index_instead_of_share", "DELIVERY_TYPE_PHYSICAL_DELIVERY", "TYPE_INDEX"},
		{"unknown_asset", "DELIVERY_TYPE_PHYSICAL_DELIVERY", "TYPE_UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, refresh := refreshInputs()
			refresh.future.FuturesType, refresh.future.AssetType = tc.settlement, tc.asset
			fake := &catalogueFake{shares: []*pb.Share{refresh.stock}, futures: []*pb.Future{refresh.future}}
			p := &Provider{instruments: fake, now: func() time.Time { return saved.Future.LastTrade.Add(-24 * time.Hour) }}
			if pairs, err := p.Discover(context.Background(), []string{"SBER"}, 30); err == nil || len(pairs) != 0 || fake.marginCalls != 0 {
				t.Fatalf("unsupported future advanced past discovery: pairs=%d margin_calls=%d err=%v", len(pairs), fake.marginCalls, err)
			}
			p.instruments = refresh
			if _, err := p.RefreshPair(context.Background(), saved); err == nil {
				t.Fatal("tracked future accepted a changed/unknown economic specification")
			}
		})
	}

	for _, tc := range []struct{ settlement, asset string }{
		{"DELIVERY_TYPE_PHYSICAL_DELIVERY", "TYPE_INDEX"},
		{"DELIVERY_TYPE_UNSPECIFIED", "TYPE_INDEX"},
		{"DELIVERY_TYPE_CASH_SETTLEMENT", "TYPE_SECURITY"},
		{"DELIVERY_TYPE_CASH_SETTLEMENT", "TYPE_UNSPECIFIED"},
	} {
		p, _, _, now := newCurveProvider(t)
		f := curveFutureFixture(now, "KKX6", "KEYRATE", 30)
		f.FuturesType, f.AssetType = tc.settlement, tc.asset
		if _, err := p.mapCurveFuture(context.Background(), f, map[string]decimal.Decimal{}); err == nil {
			t.Fatalf("curve future accepted wrong specifications %+v", tc)
		}
		rp, relative, now, _ := newRelativeProvider(t)
		future := relative.futures["MXZ6"]
		future.FuturesType, future.AssetType = tc.settlement, tc.asset
		catalogue := &relativeCatalogue{shares: relative.shares, futures: relative.futures, fees: map[string]decimal.Decimal{}}
		if _, err := rp.mapRelativeFuture(context.Background(), future, catalogue, now); err == nil {
			t.Fatalf("relative future accepted wrong specifications %+v", tc)
		}
	}
}
