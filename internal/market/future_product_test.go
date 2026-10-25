package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

// These basic_asset strings were reported from the sandbox BASE catalogue on
// 2026-10-26. Dates, prices, sizes and UIDs below remain synthetic test data.
var observedRelativeAssets = map[string]string{
	"MXZ6": "IMOEX", "MMZ6": "Индекс московской биржи",
	"SiZ6": "USD/RUB", "UMZ6": "USD/RUB",
	"GDZ6": "Золото в долларах", "GNZ6": "Золото",
	"CRZ6": "CNY/RUB", "CNYRUBF": "CNY/RUB",
	"GLZ6": "Золото в рублях", "GLDRUBF": "Золото в рублях",
	"SRZ6": "SBER", "SPZ6": "SBERP", "TTZ6": "TATN", "TPZ6": "TATNP",
}

type productCurveCatalogue struct {
	*curveCatalogueFake
	factors map[string]string
}

func (f *productCurveCatalogue) GetFuturesMargin(_ context.Context, request *pb.GetFuturesMarginRequest, _ ...grpc.CallOption) (*pb.GetFuturesMarginResponse, error) {
	factor := f.factors[request.GetInstrumentId()]
	if factor == "" {
		return nil, fmt.Errorf("missing synthetic multiplier for %s", request.GetInstrumentId())
	}
	return &pb.GetFuturesMarginResponse{InitialMarginOnBuy: rub("2500"), InitialMarginOnSell: rub("3000"), MinPriceIncrement: quote("1"), MinPriceIncrementAmount: quote(factor)}, nil
}

func productRelativeSelection() []curve.Relationship {
	var result []curve.Relationship
	for _, r := range [][4]string{
		{"index", curve.StandardMini, "MX", "MM"},
		{"usd", curve.StandardMini, "Si", "USDM"},
		{"gold", curve.StandardMini, "GOLD", "GOLDM"},
		{"cny", curve.PerpetualDated, "CNYRUBF", "CR"},
		{"rub-gold", curve.PerpetualDated, "GLDRUBF", "GL"},
		{"sber", curve.ShareClass, "SBER", "SBERP"},
		{"tatneft", curve.ShareClass, "TATN", "TATNP"},
	} {
		result = append(result, relativeDefinition(r[0], r[1], r[2], r[3]))
	}
	fund := relativeDefinition("fund", curve.FundNAV, "TGLD@", "GL")
	fund.Fund = &curve.FundReference{DistributionsKnown: true, DistributionsSource: "synthetic accumulating rules"}
	return append(result, fund)
}

func TestCurveObservedBasicAssetsReachBooksAndKeepCanonicalFamilies(t *testing.T) {
	p, catalogue, _, now := newCurveProvider(t)
	factors := map[string]string{}
	p.instruments = &productCurveCatalogue{curveCatalogueFake: catalogue, factors: factors}
	fees := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"securities":{"columns":["SECID","BOARDID","BUYSELLFEE"],"data":[["RBZ6","RFUD",2],["RBH7","RFUD",2],["KKZ6","RFUD",3],["KKH7","RFUD",3],["RFZ6","RFUD",4],["RFH7","RFUD",4]]}}`)
	}))
	t.Cleanup(fees.Close)
	p.feeURL = fees.URL
	aliases := map[string]string{
		"RGBI":    "Индекс государственных облигаций",
		"KEYRATE": "Индекс ключевой ставки",
		"RUONIA":  "Индекс RUONIA",
	}
	for _, row := range [][3]string{{"RGBI", "RBZ6", "RBH7"}, {"KEYRATE", "KKZ6", "KKH7"}, {"RUONIA", "RFZ6", "RFH7"}} {
		for n, ticker := range row[1:] {
			f := curveFutureFixture(now, ticker, row[0], 30+n*90)
			catalogue.futures = append(catalogue.futures, f)
			catalogue.byFuture[f.Uid] = f
			factors[f.Uid] = map[string]string{"RGBI": "1", "KEYRATE": "1000", "RUONIA": "10000"}[row[0]]
		}
	}
	before, err := p.CurveSnapshot(context.Background(), curveSelect())
	if err != nil || len(before.Instruments) != 7 {
		t.Fatalf("baseline failed: instruments=%d err=%v warnings=%v", len(before.Instruments), err, before.Warnings)
	}
	for _, f := range catalogue.futures {
		f.BasicAsset = aliases[f.BasicAsset]
	}
	after, err := p.CurveSnapshot(context.Background(), curveSelect())
	if err != nil || len(after.Instruments) != 7 || len(after.Books) != 7 {
		t.Fatalf("observed API descriptions rejected: instruments=%d books=%d err=%v warnings=%v", len(after.Instruments), len(after.Books), err, after.Warnings)
	}
	for n, current := range after.Instruments {
		if err := sameCurveUnits(before.Instruments[n], current); err != nil {
			t.Fatalf("basic_asset alias changed canonical persisted economics: %s: %v", current.Ticker, err)
		}
	}
	scan, err := curve.Evaluate(after, curve.DefaultPolicy(), nil)
	if err != nil || len(scan.Metrics) != 3 {
		t.Fatalf("all three calendars should reach pricing before history warmup: metrics=%d err=%v skips=%v", len(scan.Metrics), err, scan.Skips)
	}

	// Active positions refresh by UID even when discovery no longer selects them.
	catalogue.futures = nil
	tracked := curveSelect()
	tracked.Tracked = before.Instruments
	tracked.MaxFutureDays = 1
	refreshed, err := p.CurveSnapshot(context.Background(), tracked)
	if err != nil || len(refreshed.Books) != 7 || len(catalogue.futureUIDs) != 6 {
		t.Fatalf("tracked aliases failed to refresh: books=%d UID loads=%d err=%v warnings=%v", len(refreshed.Books), len(catalogue.futureUIDs), err, refreshed.Warnings)
	}
	for n, current := range refreshed.Instruments {
		if err := sameCurveUnits(before.Instruments[n], current); err != nil {
			t.Fatal("tracked contract redefined by upstream alias", current.Ticker, err)
		}
	}
}

func TestRelativeObservedBasicAssetsReachBooksAndRefreshTrackedPositions(t *testing.T) {
	p, catalogue, now, _ := newRelativeProvider(t)
	selection := relativeSelection(now)
	selection.Relationships = productRelativeSelection()
	before, err := p.RelativeSnapshot(context.Background(), selection)
	if err != nil || len(before.Relationships) != 8 || len(before.Books) != 15 {
		t.Fatalf("baseline failed: relationships=%d books=%d err=%v warnings=%v", len(before.Relationships), len(before.Books), err, before.Warnings)
	}
	for ticker, asset := range observedRelativeAssets {
		catalogue.futures[ticker].BasicAsset = asset
	}
	after, err := p.RelativeSnapshot(context.Background(), selection)
	if err != nil || len(after.Relationships) != 8 || len(after.Books) != 15 {
		t.Fatalf("observed API descriptions rejected: relationships=%d books=%d err=%v warnings=%v", len(after.Relationships), len(after.Books), err, after.Warnings)
	}
	for n, current := range after.Instruments {
		if err := sameRelativeUnits(before.Instruments[n], current); err != nil {
			t.Fatalf("alias changed canonical units, product or settlement: %s: %v", current.Ticker, err)
		}
	}
	for n, current := range after.Relationships {
		old := before.Relationships[n]
		if current.ID != old.ID || current.PrimaryUID != old.PrimaryUID || current.HedgeUID != old.HedgeUID || !current.HedgeLotsPerPrimaryLot.Equal(old.HedgeLotsPerPrimaryLot) || !current.PrimaryPriceScale.Equal(old.PrimaryPriceScale) || !current.HedgePriceScale.Equal(old.HedgePriceScale) {
			t.Fatal("underlying alias changed pairing or hedge economics", old, current)
		}
	}
	tracked := relativeSelection(now)
	tracked.Tracked, tracked.TrackedRelationships = before.Instruments, before.Relationships
	tracked.MaxDays = 1
	refreshed, err := p.RelativeSnapshot(context.Background(), tracked)
	if err != nil || len(refreshed.Relationships) != 8 || len(refreshed.Books) != 15 {
		t.Fatalf("tracked aliases failed to refresh: relationships=%d books=%d err=%v warnings=%v", len(refreshed.Relationships), len(refreshed.Books), err, refreshed.Warnings)
	}
	for n, current := range refreshed.Instruments {
		if err := sameRelativeUnits(before.Instruments[n], current); err != nil {
			t.Fatal("tracked contract redefined by upstream alias", current.Ticker, err)
		}
	}
}

func TestProductAliasesRejectConflictingTickersAndGoldCurrency(t *testing.T) {
	for _, tc := range []struct{ fixture, ticker, asset string }{
		{"MXZ6", "MXZ6extra", "IMOEX"},
		{"MXZ6", "MXZ6", "MXI"},
		{"MMZ6", "MMZ6", "MIX"},
		{"SiZ6", "SiZ6", "USDM"},
		{"SiZ6", "SiZ6", "CNY/RUB"},
		{"UMZ6", "UMZ6", "Si"},
		{"GDZ6", "GDZ6", "Золото в рублях"},
		{"GNZ6", "GNZ6", "GL"},
		{"GLZ6", "GLZ6", "Золото в долларах"},
		{"GLDRUBF", "GLDRUBF", "GOLD"},
		{"CNYRUBF", "CNYRUBFextra", "CNY/RUB"},
	} {
		t.Run(tc.ticker+"/"+tc.asset, func(t *testing.T) {
			p, fake, now, _ := newRelativeProvider(t)
			f := fake.futures[tc.fixture]
			f.Ticker, f.BasicAsset = tc.ticker, tc.asset
			catalogue := &relativeCatalogue{shares: fake.shares, futures: fake.futures, fees: map[string]decimal.Decimal{}}
			if _, err := p.mapRelativeFuture(context.Background(), f, catalogue, now); err == nil || fake.marginCalls[f.Uid] != 0 {
				t.Fatal("conflicting or malformed product reached margin lookup", tc, err)
			}
		})
	}
	for _, tc := range []struct{ ticker, asset string }{
		{"RBZ6", "Индекс ключевой ставки"},
		{"KKZ6", "RGBI"},
		{"RFZ6extra", "Индекс RUONIA"},
		{"MXZ6", "Индекс государственных облигаций"},
	} {
		p, _, _, now := newCurveProvider(t)
		f := curveFutureFixture(now, tc.ticker, tc.asset, 30)
		if _, err := p.mapCurveFuture(context.Background(), f, map[string]decimal.Decimal{}); err == nil {
			t.Fatal("conflicting curve product accepted", tc)
		}
	}
}

func TestObservedAliasDoesNotMakeUnknownOrUnsupportedContractEligible(t *testing.T) {
	p, fake, now, _ := newRelativeProvider(t)
	for ticker, asset := range observedRelativeAssets {
		fake.futures[ticker].BasicAsset = asset
	}
	// A shared SBER underlying does not turn the unsupported SBERF product into a dated SR contract.
	fake.futures["SBERF"] = &pb.Future{Uid: "SBERF", Ticker: "SBERF", BasicAsset: "SBER"}
	if relativeProduct(fake.futures["SBERF"]) != "" {
		t.Fatal("unsupported SBERF was recognized as dated SBER")
	}
	fake.futures["MXZ6"].BasicAsset = "Неизвестный индекс"
	selection := relativeSelection(now)
	selection.Relationships = []curve.Relationship{relativeDefinition("index", curve.StandardMini, "MX", "MM")}
	snapshot, err := p.RelativeSnapshot(context.Background(), selection)
	if err != nil || len(snapshot.Relationships) != 0 || fake.marginCalls["MXZ6"] != 0 || !strings.Contains(strings.Join(snapshot.Warnings, "\n"), "Неизвестный индекс") {
		t.Fatal("unknown alias was admitted or disappeared from diagnostics", err, snapshot.Warnings)
	}
}
