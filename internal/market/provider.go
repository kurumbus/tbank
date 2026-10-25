// Package market retrieves read-only market data and indicative order costs.
// Its service interfaces deliberately expose no order-placement methods.
package market

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	"opensource.tbank.ru/invest/invest-go/investgo"
	pb "opensource.tbank.ru/invest/invest-go/proto"

	"tbank/internal/scanner"
)

type instrumentsReader interface {
	Shares(context.Context, *pb.InstrumentsRequest, ...grpc.CallOption) (*pb.SharesResponse, error)
	Futures(context.Context, *pb.InstrumentsRequest, ...grpc.CallOption) (*pb.FuturesResponse, error)
	ShareBy(context.Context, *pb.InstrumentRequest, ...grpc.CallOption) (*pb.ShareResponse, error)
	FutureBy(context.Context, *pb.InstrumentRequest, ...grpc.CallOption) (*pb.FutureResponse, error)
	GetFuturesMargin(context.Context, *pb.GetFuturesMarginRequest, ...grpc.CallOption) (*pb.GetFuturesMarginResponse, error)
}
type dataReader interface {
	GetOrderBook(context.Context, *pb.GetOrderBookRequest, ...grpc.CallOption) (*pb.GetOrderBookResponse, error)
	GetTradingStatus(context.Context, *pb.GetTradingStatusRequest, ...grpc.CallOption) (*pb.GetTradingStatusResponse, error)
}
type userReader interface {
	GetInfo(context.Context, *pb.GetInfoRequest, ...grpc.CallOption) (*pb.GetInfoResponse, error)
}
type priceReader interface {
	GetOrderPrice(context.Context, *pb.GetOrderPriceRequest, ...grpc.CallOption) (*pb.GetOrderPriceResponse, error)
}

type Provider struct {
	instruments instrumentsReader
	data        dataReader
	users       userReader
	prices      priceReader
	http        *http.Client
	feeURL      string
	now         func() time.Time
	mu          sync.Mutex
	warnings    []string
}

// New reuses the SDK's authenticated connection, but uses generated read-only
// clients so every request respects the caller's context and deadline.
func New(api *investgo.Client) *Provider {
	return &Provider{
		instruments: pb.NewInstrumentsServiceClient(api.Conn),
		data:        pb.NewMarketDataServiceClient(api.Conn),
		users:       pb.NewUsersServiceClient(api.Conn),
		prices:      pb.NewOrdersServiceClient(api.Conn),
		http:        &http.Client{Timeout: 10 * time.Second}, feeURL: moexFeeURL, now: time.Now,
	}
}

// Warnings reports unavailable supplemental data without inventing substitutes.
func (p *Provider) Warnings() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.warnings...)
}
func (p *Provider) warn(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, s)
}

// Discover loads each base catalogue once. tickers are exact stock tickers,
// never substring searches or guessed futures symbols. Empty selects all.
func (p *Provider) Discover(ctx context.Context, tickers []string, maxDays int) ([]scanner.Pair, error) {
	if maxDays <= 0 {
		return nil, fmt.Errorf("max days must be positive")
	}
	p.mu.Lock()
	p.warnings = nil
	p.mu.Unlock()
	shares, err := p.instruments.Shares(ctx, &pb.InstrumentsRequest{InstrumentStatus: pb.InstrumentStatus_INSTRUMENT_STATUS_BASE.Enum()})
	if err != nil {
		return nil, fmt.Errorf("base shares: %w", err)
	}
	futures, err := p.instruments.Futures(ctx, &pb.InstrumentsRequest{InstrumentStatus: pb.InstrumentStatus_INSTRUMENT_STATUS_BASE.Enum()})
	if err != nil {
		return nil, fmt.Errorf("base futures: %w", err)
	}
	if shares == nil || futures == nil {
		return nil, fmt.Errorf("empty catalogue response")
	}
	wanted := make(map[string]bool)
	for _, s := range tickers {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			return nil, fmt.Errorf("empty ticker")
		}
		wanted[s] = false
	}
	byPosition := make(map[string]scanner.Instrument)
	for _, s := range shares.GetInstruments() {
		if s == nil {
			continue
		}
		ticker := strings.ToUpper(s.GetTicker())
		if len(wanted) > 0 {
			if _, ok := wanted[ticker]; !ok {
				continue
			}
		}
		if s.GetClassCode() != "TQBR" || !strings.EqualFold(s.GetCurrency(), "rub") || !s.GetApiTradeAvailableFlag() || !s.GetBuyAvailableFlag() || !s.GetSellAvailableFlag() || s.GetUid() == "" || s.GetPositionUid() == "" || s.GetLot() <= 0 {
			continue
		}
		if _, exists := byPosition[s.GetPositionUid()]; exists {
			return nil, fmt.Errorf("ambiguous TQBR position %s", s.GetPositionUid())
		}
		byPosition[s.GetPositionUid()] = scanner.Instrument{UID: s.GetUid(), Ticker: s.GetTicker(), ClassCode: s.GetClassCode(), Kind: "share", Currency: "rub", Lot: int64(s.GetLot()), PositionUID: s.GetPositionUid(), AssetSize: decimal.NewFromInt(1), PriceFactor: decimal.NewFromInt(1), APIAvailable: true, BuyAvailable: true, SellAvailable: true, QualifiedOnly: s.GetForQualInvestorFlag()}
		if _, ok := wanted[ticker]; ok {
			wanted[ticker] = true
		}
	}
	var missing []string
	for ticker, found := range wanted {
		if !found {
			missing = append(missing, ticker)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no eligible exact RUB TQBR stock ticker: %s", strings.Join(missing, ", "))
	}
	now := p.now().UTC()
	latest := now.AddDate(0, 0, maxDays)
	var pairs []scanner.Pair
	seen := map[string]bool{}
	for _, f := range futures.GetInstruments() {
		if f == nil || f.GetUid() == "" || seen[f.GetUid()] {
			continue
		}
		stock, ok := byPosition[f.GetBasicAssetPositionUid()]
		if !ok {
			continue
		}
		// PositionUID, not BasicAsset (often SBRF rather than SBER), is the
		// authoritative mapping. Never infer share class from a symbol prefix.
		if f.GetClassCode() != "SPBFUT" || f.GetFuturesType() != "physical_delivery" || f.GetAssetType() != "security" || !strings.EqualFold(f.GetCurrency(), "rub") || !f.GetApiTradeAvailableFlag() || !f.GetBuyAvailableFlag() || !f.GetSellAvailableFlag() || !f.GetShortEnabledFlag() || f.GetLot() != 1 {
			continue
		}
		expiry, last := timestamp(f.GetExpirationDate()), timestamp(f.GetLastTradeDate())
		if expiry.IsZero() || last.IsZero() || !last.After(now) || !expiry.After(now) || expiry.After(latest) || expiry.Before(last) {
			continue
		}
		size := quotation(f.GetBasicAssetSize())
		if !size.IsPositive() || !size.Equal(size.Truncate(0)) {
			continue
		}
		margin, err := p.instruments.GetFuturesMargin(ctx, &pb.GetFuturesMarginRequest{InstrumentId: f.GetUid()})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			p.warn(fmt.Sprintf("%s skipped: futures margin: %v", f.GetTicker(), err))
			continue
		}
		if margin == nil {
			p.warn(f.GetTicker() + " skipped: missing futures margin")
			continue
		}
		im := margin.GetInitialMarginOnSell()
		step, amount := quotation(margin.GetMinPriceIncrement()), quotation(margin.GetMinPriceIncrementAmount())
		if im == nil || !strings.EqualFold(im.GetCurrency(), "rub") || !money(im).IsPositive() || !step.IsPositive() || !amount.IsPositive() {
			p.warn(f.GetTicker() + " skipped: invalid RUB margin or price increment")
			continue
		}
		pairs = append(pairs, scanner.Pair{Stock: stock, Future: scanner.Instrument{UID: f.GetUid(), Ticker: f.GetTicker(), ClassCode: f.GetClassCode(), Kind: "future", Currency: "rub", Lot: 1, PositionUID: f.GetPositionUid(), UnderlyingPositionUID: f.GetBasicAssetPositionUid(), AssetSize: size, PriceFactor: amount.Div(step), Expiry: expiry, LastTrade: last, Settlement: "physical", APIAvailable: true, BuyAvailable: true, SellAvailable: true, QualifiedOnly: f.GetForQualInvestorFlag(), InitialMargin: money(im)}})
		seen[f.GetUid()] = true
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no eligible dated physically delivered stock/future pairs")
	}
	fees, err := p.fetchFees(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		p.warn("MOEX exchange fee unavailable: " + err.Error())
	}
	for i := range pairs {
		pairs[i].Future.ExchangeFeeSource = p.feeURL
		if fee, ok := fees[pairs[i].Future.Ticker]; ok {
			pairs[i].Future.ExchangeFee = fee
			pairs[i].Future.ExchangeFeeKnown = true
		} else {
			p.warn(pairs[i].Future.Ticker + ": exchange fee unknown; candidate cannot qualify")
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Stock.Ticker != pairs[j].Stock.Ticker {
			return pairs[i].Stock.Ticker < pairs[j].Stock.Ticker
		}
		return pairs[i].Future.Expiry.Before(pairs[j].Future.Expiry)
	})
	return pairs, nil
}

// Snapshot retrieves both books and statuses concurrently to minimize skew.
// Book.Time is the exchange's orderbook timestamp, never the local fetch time.
func (p *Provider) Snapshot(ctx context.Context, pair scanner.Pair, depth int) (scanner.Snapshot, error) {
	if depth != 1 && depth != 10 && depth != 20 && depth != 30 && depth != 40 && depth != 50 {
		return scanner.Snapshot{}, fmt.Errorf("unsupported order book depth %d", depth)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type response struct {
		index  int
		book   *pb.GetOrderBookResponse
		status *pb.GetTradingStatusResponse
		err    error
	}
	ch := make(chan response, 4)
	ids := []string{pair.Stock.UID, pair.Future.UID}
	for i, uid := range ids {
		go func(i int, uid string) {
			r, e := p.data.GetOrderBook(ctx, &pb.GetOrderBookRequest{InstrumentId: &uid, Depth: int32(depth)})
			ch <- response{index: i, book: r, err: e}
		}(i, uid)
		go func(i int, uid string) {
			r, e := p.data.GetTradingStatus(ctx, &pb.GetTradingStatusRequest{InstrumentId: &uid})
			ch <- response{index: i + 2, status: r, err: e}
		}(i, uid)
	}
	books := make([]*pb.GetOrderBookResponse, 2)
	statuses := make([]*pb.GetTradingStatusResponse, 2)
	for i := 0; i < 4; i++ {
		r := <-ch
		if r.err != nil {
			return scanner.Snapshot{}, fmt.Errorf("snapshot request %d: %w", r.index, r.err)
		}
		if r.index < 2 {
			books[r.index] = r.book
		} else {
			statuses[r.index-2] = r.status
		}
	}
	stock, err := mapBook(pair.Stock, books[0], statuses[0])
	if err != nil {
		return scanner.Snapshot{}, err
	}
	future, err := mapBook(pair.Future, books[1], statuses[1])
	if err != nil {
		return scanner.Snapshot{}, err
	}
	return scanner.Snapshot{Pair: pair, StockBook: stock, FutureBook: future, ObservedAt: p.now().UTC()}, nil
}

func mapBook(instrument scanner.Instrument, b *pb.GetOrderBookResponse, s *pb.GetTradingStatusResponse) (scanner.Book, error) {
	if b == nil || s == nil {
		return scanner.Book{}, fmt.Errorf("%s: missing book or status", instrument.Ticker)
	}
	if b.GetInstrumentUid() != instrument.UID || s.GetInstrumentUid() != instrument.UID {
		return scanner.Book{}, fmt.Errorf("%s: response instrument UID mismatch", instrument.Ticker)
	}
	if b.GetTicker() != "" && b.GetTicker() != instrument.Ticker {
		return scanner.Book{}, fmt.Errorf("%s: response ticker mismatch", instrument.Ticker)
	}
	if b.GetClassCode() != "" && b.GetClassCode() != instrument.ClassCode {
		return scanner.Book{}, fmt.Errorf("%s: response class mismatch", instrument.Ticker)
	}
	book := scanner.Book{UID: instrument.UID, Time: timestamp(b.GetOrderbookTs()), Consistent: true, TradeAvailable: s.GetApiTradeAvailableFlag() && s.GetLimitOrderAvailableFlag() && s.GetTradingStatus() == pb.SecurityTradingStatus_SECURITY_TRADING_STATUS_NORMAL_TRADING}
	if book.Time.IsZero() {
		book.Consistent = false
	}
	mapLevels := func(orders []*pb.Order, bids bool) []scanner.Level {
		out := make([]scanner.Level, 0, len(orders))
		for i, o := range orders {
			price := quotation(o.GetPrice())
			quantity := o.GetQuantity()
			if o == nil || !price.IsPositive() || quantity <= 0 {
				book.Consistent = false
			}
			if i > 0 && ((bids && price.GreaterThanOrEqual(out[i-1].Price)) || (!bids && price.LessThanOrEqual(out[i-1].Price))) {
				book.Consistent = false
			}
			out = append(out, scanner.Level{Price: price, Lots: quantity})
		}
		return out
	}
	book.Bids = mapLevels(b.GetBids(), true)
	book.Asks = mapLevels(b.GetAsks(), false)
	if len(book.Bids) > 0 && len(book.Asks) > 0 && book.Bids[0].Price.GreaterThanOrEqual(book.Asks[0].Price) {
		book.Consistent = false
	}
	return book, nil
}

// Preview is an indicative, read-only commission check. It does not reserve
// prices, collateral or permission, and never submits an order.
func (p *Provider) Preview(ctx context.Context, result scanner.Result, accountID string) (scanner.Preview, error) {
	preview := scanner.Preview{Status: "unavailable"}
	if strings.TrimSpace(accountID) == "" {
		return preview, fmt.Errorf("explicit account ID is required for preview")
	}
	if result.StockLots <= 0 || result.Contracts <= 0 || !result.StockLimit.IsPositive() || !result.FutureLimit.IsPositive() {
		return preview, fmt.Errorf("positive quantities and limits are required for preview")
	}
	info, err := p.users.GetInfo(ctx, &pb.GetInfoRequest{})
	if err != nil {
		return preview, fmt.Errorf("get actual tariff: %w", err)
	}
	if info == nil || info.GetTariff() == "" {
		return preview, fmt.Errorf("actual tariff is missing")
	}
	preview.ActualTariff = info.GetTariff()
	if (result.Snapshot.Pair.Stock.QualifiedOnly || result.Snapshot.Pair.Future.QualifiedOnly) && !info.GetQualStatus() {
		return preview, fmt.Errorf("a proposed instrument requires qualified status, which GetInfo did not confirm")
	}
	stockPrice, err := toQuotation(result.StockLimit)
	if err != nil {
		return preview, err
	}
	futurePrice, err := toQuotation(result.FutureLimit)
	if err != nil {
		return preview, err
	}
	stock, err := p.prices.GetOrderPrice(ctx, &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: result.Snapshot.Pair.Stock.UID, Price: stockPrice, Direction: pb.OrderDirection_ORDER_DIRECTION_BUY, Quantity: result.StockLots})
	if err != nil {
		return preview, fmt.Errorf("stock cost preview: %w", err)
	}
	preview.StockCommission, err = previewCommission(stock, result.StockLots)
	if err != nil {
		return preview, fmt.Errorf("stock cost preview: %w", err)
	}
	future, err := p.prices.GetOrderPrice(ctx, &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: result.Snapshot.Pair.Future.UID, Price: futurePrice, Direction: pb.OrderDirection_ORDER_DIRECTION_SELL, Quantity: result.Contracts})
	if err != nil {
		return preview, fmt.Errorf("future cost preview: %w", err)
	}
	preview.FutureCommission, err = previewCommission(future, result.Contracts)
	if err != nil {
		return preview, fmt.Errorf("future cost preview: %w", err)
	}
	preview.RequiredInitialMargin, err = previewMargin(future, result.Contracts)
	if err != nil {
		return preview, fmt.Errorf("future margin preview: %w", err)
	}
	marginMessage := ""
	if result.Contracts > 1 {
		// The API schema calls initial_margin "margin for the future" without
		// explicitly stating its quantity unit. A one-contract quote gives an
		// unambiguous unit amount; retain the larger funding estimate.
		one, singleErr := p.prices.GetOrderPrice(ctx, &pb.GetOrderPriceRequest{AccountId: accountID, InstrumentId: result.Snapshot.Pair.Future.UID, Price: futurePrice, Direction: pb.OrderDirection_ORDER_DIRECTION_SELL, Quantity: 1})
		if singleErr != nil {
			return preview, fmt.Errorf("one-contract margin verification: %w", singleErr)
		}
		unitMargin, singleErr := previewMargin(one, 1)
		if singleErr != nil {
			return preview, fmt.Errorf("one-contract margin verification: %w", singleErr)
		}
		expected := unitMargin.Mul(decimal.NewFromInt(result.Contracts))
		preview.RequiredInitialMargin = decimal.Max(preview.RequiredInitialMargin, expected)
		marginMessage = " API margin quantity convention is unspecified; funding conservatively uses max(requested-quantity margin, one-contract margin × contracts)."
	}
	preview.Status = "ok"
	preview.Message = "Indicative entry commissions and account-specific futures funding requirement at worst executable book limits; no order or reservation." + marginMessage
	return preview, nil
}

func previewMargin(r *pb.GetOrderPriceResponse, quantity int64) (decimal.Decimal, error) {
	if r == nil || r.GetLotsRequested() != quantity {
		return decimal.Zero, fmt.Errorf("missing response or requested-lot mismatch")
	}
	m := r.GetExtraFuture().GetInitialMargin()
	if m == nil || !strings.EqualFold(m.GetCurrency(), "rub") || !money(m).IsPositive() {
		return decimal.Zero, fmt.Errorf("missing or invalid positive RUB initial margin")
	}
	return money(m), nil
}

func previewCommission(r *pb.GetOrderPriceResponse, quantity int64) (decimal.Decimal, error) {
	if r == nil || r.GetLotsRequested() != quantity {
		return decimal.Zero, fmt.Errorf("missing response or requested-lot mismatch")
	}
	m := r.GetExecutedCommissionRub()
	if m == nil {
		m = r.GetExecutedCommission()
	}
	if m == nil || !strings.EqualFold(m.GetCurrency(), "rub") || money(m).IsNegative() {
		return decimal.Zero, fmt.Errorf("missing or invalid RUB commission")
	}
	return money(m), nil
}

func quotation(q *pb.Quotation) decimal.Decimal {
	if q == nil {
		return decimal.Zero
	}
	return decimal.New(q.GetUnits(), 0).Add(decimal.New(int64(q.GetNano()), -9))
}
func money(m *pb.MoneyValue) decimal.Decimal {
	if m == nil {
		return decimal.Zero
	}
	return decimal.New(m.GetUnits(), 0).Add(decimal.New(int64(m.GetNano()), -9))
}
func timestamp(t *timestamppb.Timestamp) time.Time {
	if t == nil || t.CheckValid() != nil {
		return time.Time{}
	}
	return t.AsTime().UTC()
}
func toQuotation(d decimal.Decimal) (*pb.Quotation, error) {
	if !d.Equal(d.Truncate(9)) {
		return nil, fmt.Errorf("price exceeds quotation precision")
	}
	units := d.Truncate(0)
	if units.GreaterThan(decimal.NewFromInt(9223372036854775807)) || units.LessThan(decimal.NewFromInt(-9223372036854775807-1)) {
		return nil, fmt.Errorf("quotation overflow")
	}
	return &pb.Quotation{Units: units.IntPart(), Nano: int32(d.Sub(units).Shift(9).IntPart())}, nil
}
