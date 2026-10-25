package market

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/scanner"
)

// RefreshPair reloads the metadata of an already tracked pair by immutable UID.
// Unlike Discover, it applies no watchlist, horizon, last-trade, short-opening or
// buy/sell filter. A position may still be closable after new entries are disabled.
// The caller must check the closing directions (stock sell / future buy), session
// state and current books. Changed economic units require manual reconciliation.
func (p *Provider) RefreshPair(ctx context.Context, saved scanner.Pair) (scanner.Pair, error) {
	if saved.Stock.UID == "" || saved.Future.UID == "" || saved.Stock.PositionUID == "" || saved.Stock.PositionUID != saved.Future.UnderlyingPositionUID {
		return scanner.Pair{}, fmt.Errorf("tracked pair has unverified instrument identities")
	}
	stockResponse, err := p.instruments.ShareBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: saved.Stock.UID})
	if err != nil {
		return scanner.Pair{}, fmt.Errorf("refresh tracked stock: %w", err)
	}
	futureResponse, err := p.instruments.FutureBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: saved.Future.UID})
	if err != nil {
		return scanner.Pair{}, fmt.Errorf("refresh tracked future: %w", err)
	}
	stock, future := stockResponse.GetInstrument(), futureResponse.GetInstrument()
	if stock == nil || future == nil || stock.GetUid() != saved.Stock.UID || future.GetUid() != saved.Future.UID || stock.GetPositionUid() != saved.Stock.PositionUID || future.GetBasicAssetPositionUid() != stock.GetPositionUid() {
		return scanner.Pair{}, fmt.Errorf("tracked pair instrument or underlying identity changed")
	}
	if stock.GetClassCode() != "TQBR" || future.GetClassCode() != "SPBFUT" || !strings.EqualFold(stock.GetCurrency(), "rub") || !strings.EqualFold(future.GetCurrency(), "rub") || future.GetFuturesType() != "physical_delivery" || future.GetAssetType() != "security" {
		return scanner.Pair{}, fmt.Errorf("tracked pair no longer has supported RUB share/physical future specifications")
	}
	size := quotation(future.GetBasicAssetSize())
	step, amount := quotation(future.GetMinPriceIncrement()), quotation(future.GetMinPriceIncrementAmount())
	if !step.IsPositive() || !amount.IsPositive() {
		return scanner.Pair{}, fmt.Errorf("tracked future has no verified current price conversion")
	}
	factor := amount.Div(step)
	if stock.GetLot() <= 0 || int64(stock.GetLot()) != saved.Stock.Lot || future.GetLot() != 1 || saved.Future.Lot != 1 || !size.IsPositive() || !size.Equal(saved.Future.AssetSize) || !factor.Equal(saved.Future.PriceFactor) || !saved.Stock.PriceFactor.Equal(decimal.NewFromInt(1)) {
		return scanner.Pair{}, fmt.Errorf("tracked pair lot, contract size or price conversion changed; reconcile position before marking")
	}
	expiry, last := timestamp(future.GetExpirationDate()), timestamp(future.GetLastTradeDate())
	if expiry.IsZero() || last.IsZero() || last.After(expiry) {
		return scanner.Pair{}, fmt.Errorf("tracked future has invalid current trading or expiry dates")
	}
	pair := saved
	pair.Stock.Ticker = stock.GetTicker()
	pair.Stock.APIAvailable = stock.GetApiTradeAvailableFlag()
	pair.Stock.BuyAvailable = stock.GetBuyAvailableFlag()
	pair.Stock.SellAvailable = stock.GetSellAvailableFlag()
	pair.Stock.QualifiedOnly = stock.GetForQualInvestorFlag()
	pair.Future.Ticker = future.GetTicker()
	pair.Future.APIAvailable = future.GetApiTradeAvailableFlag()
	pair.Future.BuyAvailable = future.GetBuyAvailableFlag()
	pair.Future.SellAvailable = future.GetSellAvailableFlag()
	pair.Future.QualifiedOnly = future.GetForQualInvestorFlag()
	pair.Future.Settlement = "physical"
	pair.Future.LastTrade, pair.Future.Expiry = last, expiry
	// Never let a recorded entry fee masquerade as today's exit fee.
	pair.Future.ExchangeFee = decimal.Zero
	pair.Future.ExchangeFeeKnown = false
	pair.Future.ExchangeFeeSource = p.feeURL
	fees, err := p.fetchFees(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return scanner.Pair{}, ctx.Err()
		}
		p.warn("tracked " + pair.Future.Ticker + ": current exchange fee unavailable: " + err.Error())
		return pair, nil
	}
	if fee, ok := fees[pair.Future.Ticker]; ok {
		pair.Future.ExchangeFee = fee
		pair.Future.ExchangeFeeKnown = true
	} else {
		p.warn("tracked " + pair.Future.Ticker + ": current exchange fee unknown")
	}
	return pair, nil
}
