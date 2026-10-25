package market

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

func TestCurveDiscoveryExplainsPreMappingRejections(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*pb.Future, time.Time)
	}{
		{"basic_asset", `basic_asset="unrecognized-index"`, func(f *pb.Future, _ time.Time) { f.BasicAsset = "unrecognized-index" }},
		{"permission", "short=false", func(f *pb.Future, _ time.Time) { f.ShortEnabledFlag = false }},
		{"not_started", "first_trade_date is after", func(f *pb.Future, now time.Time) { f.FirstTradeDate = timestamppb.New(now.Add(time.Hour)) }},
		{"ended", "last_trade_date is missing or not after", func(f *pb.Future, now time.Time) { f.LastTradeDate = timestamppb.New(now) }},
		{"horizon", "expiration_date exceeds", func(f *pb.Future, now time.Time) { f.ExpirationDate = timestamppb.New(now.AddDate(2, 0, 0)) }},
		{"delivery", `futures_type="unexpected-delivery"`, func(f *pb.Future, _ time.Time) { f.FuturesType = "unexpected-delivery" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, catalogue, data, now := newCurveProvider(t)
			f := curveFutureFixture(now, "RBZ6", "RGBI", 30)
			tc.change(f, now)
			catalogue.futures = []*pb.Future{f}
			s, err := p.CurveSnapshot(context.Background(), curveSelect())
			warnings := strings.Join(s.Warnings, "\n")
			if err != nil || len(s.Instruments) != 1 || !strings.Contains(warnings, tc.want) || !strings.Contains(warnings, "RGBI futures discovery:") || !strings.Contains(warnings, "RBZ6 uid=") {
				t.Fatalf("lost rejection evidence or admitted rejected contract: err=%v instruments=%d warnings=%s", err, len(s.Instruments), warnings)
			}
			for _, uid := range data.uids {
				if uid == f.Uid {
					t.Fatal("diagnostic ticker hint requested an ineligible future's book")
				}
			}
		})
	}
}

func TestRelativeDiscoveryExplainsMetadataAndPairingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*relativeFake)
	}{
		{"basic_asset", `basic_asset="unknown-index"`, func(f *relativeFake) { f.futures["MXZ6"].BasicAsset = "unknown-index" }},
		{"asset_type", `asset_type="TYPE_OTHER"`, func(f *relativeFake) { f.futures["MXZ6"].AssetType = "TYPE_OTHER" }},
		{"class", `class="OTHER"`, func(f *relativeFake) { f.futures["MXZ6"].ClassCode = "OTHER" }},
		{"expiry", "expiry or last-trade mismatch", func(f *relativeFake) { f.futures["MMZ6"].ExpirationDate.Seconds += 3600 }},
		{"underlying", `underlying="index-one"/"index-two"`, func(f *relativeFake) {
			f.futures["MXZ6"].BasicAssetPositionUid = "index-one"
			f.futures["MMZ6"].BasicAssetPositionUid = "index-two"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f, now, _ := newRelativeProvider(t)
			tc.change(f)
			selection := relativeSelection(now)
			selection.Relationships = []curve.Relationship{relativeDefinition("index", curve.StandardMini, "MX", "MM")}
			s, err := p.RelativeSnapshot(context.Background(), selection)
			warnings := strings.Join(s.Warnings, "\n")
			if err != nil || len(s.Relationships) != 0 || !strings.Contains(warnings, tc.want) || !strings.Contains(warnings, "MX futures discovery:") {
				t.Fatalf("lost reason or admitted incompatible pair: err=%v relationships=%d warnings=%s", err, len(s.Relationships), warnings)
			}
			if tc.name == "basic_asset" && f.marginCalls["MXZ6"] != 0 {
				t.Fatal("ticker hint was used to admit an unknown underlying")
			}
		})
	}
}

func TestDiscoveryDiagnosticsDistinguishAbsenceAndAreBounded(t *testing.T) {
	p, f, now, _ := newRelativeProvider(t)
	delete(f.futures, "MXZ6")
	selection := relativeSelection(now)
	selection.Relationships = []curve.Relationship{relativeDefinition("index", curve.StandardMini, "MX", "MM")}
	s, err := p.RelativeSnapshot(context.Background(), selection)
	if err != nil || !strings.Contains(strings.Join(s.Warnings, "\n"), "metadata_matches=0 ticker_only=0 selected=0") {
		t.Fatal("catalogue absence is not distinguishable from rejection", err, s.Warnings)
	}
	d := futureDiscovery{selector: "RGBI"}
	for n := 0; n < 20; n++ {
		d.reject(&pb.Future{Ticker: fmt.Sprintf("contract-%d", n)}, fmt.Sprintf("RPC failure %02d", n))
	}
	if messages := d.messages(); len(messages) != 10 || !strings.Contains(messages[9], "12 additional rejection reasons omitted") {
		t.Fatal("distinct instrument errors produced unbounded diagnostics", messages)
	}
	d = futureDiscovery{selector: "RGBI"}
	for n := 0; n < 20; n++ {
		d.reject(&pb.Future{Ticker: fmt.Sprintf("contract-%d", n)}, "same reason")
	}
	if messages := strings.Join(d.messages(), "\n"); !strings.Contains(messages, "rejected=20") || strings.Count(messages, "uid=") != 4 { // UID and underlying UID, two samples.
		t.Fatal("repeated rejection examples were not bounded", messages)
	}
}

func TestFutureDiagnosticPreservesMissingAndInvalidValues(t *testing.T) {
	f := &pb.Future{ExpirationDate: &timestamppb.Timestamp{Seconds: 1, Nanos: -1}}
	got := futureMetadataDiagnostic(f)
	if !strings.Contains(got, "units=missing") || !strings.Contains(got, "first=missing") || !strings.Contains(got, "expiry=invalid(seconds=1,nanos=-1)") {
		t.Fatal("missing or invalid metadata was normalized away", got)
	}
	f.BasicAssetSize = quote("0")
	if got = futureMetadataDiagnostic(f); !strings.Contains(got, "units=0") {
		t.Fatal("explicit zero was mistaken for missing", got)
	}
}
