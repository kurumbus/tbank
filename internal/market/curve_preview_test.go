package market

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

type curvePriceFake struct {
	priceReader
	requests []*pb.GetOrderPriceRequest
	alter    func(*pb.GetOrderPriceResponse)
}

func (f *curvePriceFake) GetOrderPrice(_ context.Context, r *pb.GetOrderPriceRequest, _ ...grpc.CallOption) (*pb.GetOrderPriceResponse, error) {
	f.requests = append(f.requests, r)
	count := decimal.NewFromInt(r.Quantity)
	response := &pb.GetOrderPriceResponse{LotsRequested: r.Quantity}
	if r.InstrumentId == "bond" {
		clean := quotation(r.Price).Mul(dec("10")).Mul(count)
		response.InitialOrderAmount = rub(clean.String())
		response.ExecutedCommissionRub = rub(clean.Mul(dec("0.0005")).Round(2).String())
		response.InstrumentExtra = &pb.GetOrderPriceResponse_ExtraBond_{ExtraBond: &pb.GetOrderPriceResponse_ExtraBond{AciValue: rub("17.5")}}
	} else {
		clean := quotation(r.Price).Mul(dec("1000")).Mul(count)
		response.InitialOrderAmount = rub(clean.String())
		response.ExecutedCommissionRub = rub(clean.Mul(dec("0.0003")).Round(2).String()) // A known actual-account band, independent of scenario turnover.
		response.InstrumentExtra = &pb.GetOrderPriceResponse_ExtraFuture_{ExtraFuture: &pb.GetOrderPriceResponse_ExtraFuture{InitialMargin: rub("3000")}}
	}
	if f.alter != nil {
		f.alter(response)
	}
	return response, nil
}
func curvePreviewLegs() []curve.Leg {
	return []curve.Leg{
		{Instrument: curve.Instrument{UID: "bond", Ticker: "OFZ", Kind: "bond", Lot: 1, PriceFactor: dec("10"), Bond: &curve.BondTerms{Nominal: dec("1000"), Accrued: dec("17.5")}}, Side: "buy", Lots: 10, Limit: dec("95.125")},
		{Instrument: curve.Instrument{UID: "future", Ticker: "RBZ6", Kind: "future", Lot: 1, PriceFactor: dec("1000"), ExchangeFeeKnown: true, ExchangeFee: dec("2"), ExchangeFeeSource: "exchange"}, Side: "sell", Lots: 2, Limit: dec("105.01")},
	}
}
func TestCurvePreviewPreservesPercentQuoteAndSeparatesACI(t *testing.T) {
	fake := &curvePriceFake{}
	provider := &Provider{users: &previewFake{}, prices: fake}
	legs := curvePreviewLegs()
	results, err := provider.CurvePreview(context.Background(), legs, "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != "matched" || !results[0].Commission.Equal(dec("4.76")) || !results[1].InitialMargin.Equal(dec("6000")) {
		t.Fatal("wrong fee/margin mapping", results)
	}
	if len(fake.requests) != 4 || fake.requests[0].Quantity != 10 || fake.requests[1].Quantity != 1 || fake.requests[2].Quantity != 2 || fake.requests[3].Quantity != 1 || fake.requests[0].Direction != pb.OrderDirection_ORDER_DIRECTION_BUY || fake.requests[2].Direction != pb.OrderDirection_ORDER_DIRECTION_SELL || !quotation(fake.requests[0].Price).Equal(legs[0].Limit) {
		t.Fatal("API quote/direction/lot conversion wrong", fake.requests)
	}
}
func TestCurvePreviewFailsClosedOnFeeAmountACIAndCurrencyMismatch(t *testing.T) {
	for _, name := range []string{"dirty_fee", "price_conversion", "aci", "non_rub", "lots", "missing_extra"} {
		t.Run(name, func(t *testing.T) {
			fake := &curvePriceFake{alter: func(r *pb.GetOrderPriceResponse) {
				switch name {
				case "dirty_fee":
					r.ExecutedCommissionRub = rub("10")
				case "price_conversion":
					r.InitialOrderAmount = rub("95.125")
				case "aci":
					r.GetExtraBond().AciValue = rub("18")
				case "non_rub":
					r.ExecutedCommissionRub.Currency = "usd"
				case "lots":
					r.LotsRequested++
				case "missing_extra":
					r.InstrumentExtra = nil
				}
			}}
			provider := &Provider{users: &previewFake{}, prices: fake}
			if _, err := provider.CurvePreview(context.Background(), curvePreviewLegs()[:1], "account"); err == nil {
				t.Fatal("invalid broker response accepted")
			}
		})
	}
}
func TestCurvePreviewRequiresAccountAndQualifiedStatus(t *testing.T) {
	fake := &curvePriceFake{}
	provider := &Provider{users: &previewFake{}, prices: fake}
	legs := curvePreviewLegs()
	if _, err := provider.CurvePreview(context.Background(), legs, ""); err == nil {
		t.Fatal("no account accepted")
	}
	legs[0].Instrument.QualifiedOnly = true
	if _, err := provider.CurvePreview(context.Background(), legs, "account"); err == nil {
		t.Fatal("unqualified account accepted")
	}
	if len(fake.requests) != 0 {
		t.Fatal("cost preview called before qualification checks")
	}
}
