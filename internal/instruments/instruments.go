package instruments

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"opensource.tbank.ru/invest/invest-go/investgo"
	pb "opensource.tbank.ru/invest/invest-go/proto"
)

const (
	KindShare    = "share"
	KindBond     = "bond"
	KindETF      = "etf"
	KindCurrency = "currency"
	KindFuture   = "future"
	KindOption   = "option"
)

// AllKinds is the full instrument catalog, in export order.
var AllKinds = []string{KindShare, KindBond, KindETF, KindCurrency, KindFuture, KindOption}

// Summary is the basic instrument card used by list, search, and info.
type Summary struct {
	Ticker            string `json:"ticker"`
	Name              string `json:"name"`
	FIGI              string `json:"figi"`
	UID               string `json:"uid"`
	ClassCode         string `json:"class_code"`
	ISIN              string `json:"isin,omitempty"`
	Type              string `json:"type"`
	Currency          string `json:"currency,omitempty"`
	Lot               int32  `json:"lot,omitempty"`
	Exchange          string `json:"exchange,omitempty"`
	TradingStatus     string `json:"trading_status,omitempty"`
	APITradeAvailable *bool  `json:"api_trade_available,omitempty"`
	BuyAvailable      *bool  `json:"buy_available,omitempty"`
	SellAvailable     *bool  `json:"sell_available,omitempty"`
	ShortEnabled      *bool  `json:"short_enabled,omitempty"`
	ForQualified      *bool  `json:"for_qualified_investor,omitempty"`
	MinPriceIncrement string `json:"min_price_increment,omitempty"`
	// Futures catalogue metadata is kept verbatim for discovery diagnostics.
	// These fields are absent on generic search/info responses and other kinds.
	FuturesType             string `json:"futures_type,omitempty"`
	AssetType               string `json:"asset_type,omitempty"`
	BasicAsset              string `json:"basic_asset,omitempty"`
	BasicAssetSize          string `json:"basic_asset_size,omitempty"`
	BasicAssetPositionUID   string `json:"basic_asset_position_uid,omitempty"`
	MinPriceIncrementAmount string `json:"min_price_increment_amount,omitempty"`
	FirstTradeDate          string `json:"first_trade_date,omitempty"`
	LastTradeDate           string `json:"last_trade_date,omitempty"`
	ExpirationDate          string `json:"expiration_date,omitempty"`
}

// Result is a filtered page of instruments plus the pre-limit match count.
type Result struct {
	Matched     int       `json:"matched"`
	Instruments []Summary `json:"instruments"`
}

// Filter narrows a catalog listing. Kind defaults to shares.
// Limit <= 0 returns every match.
type Filter struct {
	Kind     string
	Query    string
	Currency string
	Exchange string
	APIOnly  bool
	Limit    int
}

// Service reads instruments through the official invest-go client.
type Service struct {
	api *investgo.InstrumentsServiceClient
}

func New(client *investgo.Client) *Service {
	return &Service{api: client.NewInstrumentsServiceClient()}
}

// List loads the base catalog for a kind and applies Filter locally.
func (s *Service) List(f Filter) (Result, error) {
	kind, err := NormalizeKind(f.Kind)
	if err != nil {
		return Result{}, err
	}
	f.Kind = kind

	items, err := s.load(kind, false)
	if err != nil {
		return Result{}, err
	}
	return page(items, func(item Summary) bool { return matches(item, f) }, f.Limit, compareSummary), nil
}

// Search calls FindInstrument. The query matches ticker or name on the API side.
func (s *Service) Search(query string, limit int) (Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, fmt.Errorf("search query is empty")
	}
	resp, err := s.api.FindInstrument(query)
	if err != nil {
		return Result{}, fmt.Errorf("find %q: %w", query, err)
	}
	items := make([]Summary, 0, len(resp.GetInstruments()))
	for _, inst := range resp.GetInstruments() {
		items = append(items, fromShort(inst))
	}
	return page(items, func(Summary) bool { return true }, limit, func(a, b Summary) int {
		if c := cmp.Compare(searchRank(query, a), searchRank(query, b)); c != 0 {
			return c
		}
		return compareSummary(a, b)
	}), nil
}

// Info returns full cards for an exact ticker. Pass classCode when several boards match.
func (s *Service) Info(ticker, classCode string) (Result, error) {
	ticker = strings.TrimSpace(ticker)
	classCode = strings.TrimSpace(classCode)
	if ticker == "" {
		return Result{}, fmt.Errorf("ticker is empty")
	}

	resp, err := s.api.FindInstrument(ticker)
	if err != nil {
		return Result{}, fmt.Errorf("find %q: %w", ticker, err)
	}

	type ref struct {
		ticker    string
		classCode string
	}
	var refs []ref
	for _, inst := range resp.GetInstruments() {
		if !strings.EqualFold(inst.GetTicker(), ticker) {
			continue
		}
		if classCode != "" && !strings.EqualFold(inst.GetClassCode(), classCode) {
			continue
		}
		refs = append(refs, ref{ticker: inst.GetTicker(), classCode: inst.GetClassCode()})
	}
	if len(refs) == 0 {
		return Result{}, fmt.Errorf("no instrument with ticker %s", ticker)
	}

	items := make([]Summary, 0, len(refs))
	for _, item := range refs {
		card, err := s.byTicker(item.ticker, item.classCode)
		if err != nil {
			return Result{}, err
		}
		items = append(items, card)
	}
	slices.SortFunc(items, compareSummary)
	return Result{Matched: len(items), Instruments: items}, nil
}

func (s *Service) byTicker(ticker, classCode string) (Summary, error) {
	resp, err := s.api.InstrumentByTicker(ticker, classCode)
	if err != nil {
		return Summary{}, fmt.Errorf("instrument %s/%s: %w", ticker, classCode, err)
	}
	inst := resp.GetInstrument()
	if inst == nil {
		return Summary{}, fmt.Errorf("instrument %s/%s: empty response", ticker, classCode)
	}
	return fromInstrument(inst), nil
}

// Load returns every instrument of a kind. all uses the full catalog;
// otherwise the API's base list of instruments tradable through T-Invest.
func (s *Service) Load(kind string, all bool) ([]Summary, error) {
	normalized, err := NormalizeKind(kind)
	if err != nil {
		return nil, err
	}
	return s.load(normalized, all)
}

// Catalog is the full instrument list across every kind and class code.
type Catalog struct {
	FetchedAt        string         `json:"fetched_at,omitempty"`
	Endpoint         string         `json:"endpoint,omitempty"`
	InstrumentStatus string         `json:"instrument_status"`
	Count            int            `json:"count"`
	CountsByType     map[string]int `json:"counts_by_type"`
	Instruments      []Summary      `json:"instruments"`
}

// Catalog loads every kind with instrument status ALL, including every class code.
// onKind, when set, is called after each kind is fetched.
func (s *Service) Catalog(onKind func(kind string, n int)) (Catalog, error) {
	items := make([]Summary, 0)
	for _, kind := range AllKinds {
		batch, err := s.loadKind(kind)
		if err != nil {
			return Catalog{}, err
		}
		if onKind != nil {
			onKind(kind, len(batch))
		}
		items = append(items, batch...)
	}
	items = dedupe(items)
	slices.SortFunc(items, compareSummary)
	counts := make(map[string]int, len(AllKinds))
	for _, kind := range AllKinds {
		counts[kind] = 0
	}
	for _, item := range items {
		counts[item.Type]++
	}
	return Catalog{
		InstrumentStatus: "all",
		Count:            len(items),
		CountsByType:     counts,
		Instruments:      items,
	}, nil
}

func dedupe(items []Summary) []Summary {
	seen := make(map[string]struct{}, len(items))
	out := make([]Summary, 0, len(items))
	for _, item := range items {
		key := item.UID
		if key == "" {
			key = item.Type + "|" + item.ClassCode + "|" + item.Ticker + "|" + item.FIGI
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func (s *Service) loadKind(kind string) ([]Summary, error) {
	const attempts = 4
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		batch, err := s.load(kind, true)
		if err == nil {
			return batch, nil
		}
		last = err
		if !rateLimited(err) || attempt == attempts {
			return nil, err
		}
		wait := time.Duration(attempt) * 30 * time.Second
		fmt.Fprintf(os.Stderr, "rate limited on %s, retrying in %s\n", kind, wait)
		time.Sleep(wait)
	}
	return nil, last
}

func rateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "message larger than max") {
		return false
	}
	if status.Code(err) == codes.ResourceExhausted && strings.Contains(msg, "ratelimit") {
		return true
	}
	return strings.Contains(msg, "ratelimit")
}

func (s *Service) load(kind string, all bool) ([]Summary, error) {
	status := pb.InstrumentStatus_INSTRUMENT_STATUS_BASE
	if all {
		status = pb.InstrumentStatus_INSTRUMENT_STATUS_ALL
	}
	switch kind {
	case KindShare:
		resp, err := s.api.Shares(status)
		if err != nil {
			return nil, fmt.Errorf("list shares: %w", err)
		}
		return mapListed(kind, resp.GetInstruments()), nil
	case KindBond:
		resp, err := s.api.Bonds(status)
		if err != nil {
			return nil, fmt.Errorf("list bonds: %w", err)
		}
		return mapListed(kind, resp.GetInstruments()), nil
	case KindETF:
		resp, err := s.api.Etfs(status)
		if err != nil {
			return nil, fmt.Errorf("list etfs: %w", err)
		}
		return mapListed(kind, resp.GetInstruments()), nil
	case KindCurrency:
		resp, err := s.api.Currencies(status)
		if err != nil {
			return nil, fmt.Errorf("list currencies: %w", err)
		}
		return mapListed(kind, resp.GetInstruments()), nil
	case KindFuture:
		resp, err := s.api.Futures(status)
		if err != nil {
			return nil, fmt.Errorf("list futures: %w", err)
		}
		return mapListed(kind, resp.GetInstruments()), nil
	case KindOption:
		resp, err := s.api.Options(status)
		if err != nil {
			return nil, fmt.Errorf("list options: %w", err)
		}
		options := resp.GetInstruments()
		out := make([]Summary, 0, len(options))
		for _, opt := range options {
			out = append(out, fromOption(opt))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// NormalizeKind accepts singular and plural catalog names.
func NormalizeKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "share", "shares":
		return KindShare, nil
	case "bond", "bonds":
		return KindBond, nil
	case "etf", "etfs":
		return KindETF, nil
	case "currency", "currencies":
		return KindCurrency, nil
	case "future", "futures":
		return KindFuture, nil
	case "option", "options":
		return KindOption, nil
	default:
		return "", fmt.Errorf("unknown kind %q (use shares, bonds, etfs, currencies, futures, or options)", kind)
	}
}

type listed interface {
	GetTicker() string
	GetName() string
	GetFigi() string
	GetUid() string
	GetClassCode() string
	GetCurrency() string
	GetLot() int32
	GetExchange() string
	GetTradingStatus() pb.SecurityTradingStatus
	GetApiTradeAvailableFlag() bool
	GetBuyAvailableFlag() bool
	GetSellAvailableFlag() bool
	GetShortEnabledFlag() bool
	GetForQualInvestorFlag() bool
	GetMinPriceIncrement() *pb.Quotation
}

type withISIN interface {
	GetIsin() string
}

func mapListed[T listed](kind string, items []T) []Summary {
	out := make([]Summary, 0, len(items))
	for _, item := range items {
		card := fromListed(kind, item)
		if identified, ok := any(item).(withISIN); ok {
			card.ISIN = identified.GetIsin()
		}
		if future, ok := any(item).(*pb.Future); ok {
			card.FuturesType = future.GetFuturesType()
			card.AssetType = future.GetAssetType()
			card.BasicAsset = future.GetBasicAsset()
			card.BasicAssetSize = formatFutureQuotation(future.GetBasicAssetSize())
			card.BasicAssetPositionUID = future.GetBasicAssetPositionUid()
			card.MinPriceIncrementAmount = formatFutureQuotation(future.GetMinPriceIncrementAmount())
			card.FirstTradeDate = formatFutureTimestamp(future.GetFirstTradeDate())
			card.LastTradeDate = formatFutureTimestamp(future.GetLastTradeDate())
			card.ExpirationDate = formatFutureTimestamp(future.GetExpirationDate())
		}
		out = append(out, card)
	}
	return out
}

func formatFutureQuotation(q *pb.Quotation) string {
	if q == nil {
		return ""
	}
	return decimal.NewFromInt(q.GetUnits()).Add(decimal.New(int64(q.GetNano()), -9)).String()
}

func formatFutureTimestamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	if ts.CheckValid() != nil {
		// Invalid upstream dates are diagnostic evidence; do not normalize them
		// into apparently valid dates or make them indistinguishable from nil.
		return fmt.Sprintf("invalid(seconds=%d,nanos=%d)", ts.GetSeconds(), ts.GetNanos())
	}
	return ts.AsTime().UTC().Format(time.RFC3339Nano)
}

func fromOption(opt *pb.Option) Summary {
	step := ""
	if q := opt.GetMinPriceIncrement(); q != nil {
		step = formatIncrement(q)
	}
	return Summary{
		Ticker:            opt.GetTicker(),
		Name:              opt.GetName(),
		UID:               opt.GetUid(),
		ClassCode:         opt.GetClassCode(),
		Type:              KindOption,
		Currency:          opt.GetCurrency(),
		Lot:               opt.GetLot(),
		Exchange:          opt.GetExchange(),
		TradingStatus:     trimEnum(opt.GetTradingStatus().String(), "SECURITY_TRADING_STATUS_"),
		APITradeAvailable: boolPtr(opt.GetApiTradeAvailableFlag()),
		BuyAvailable:      boolPtr(opt.GetBuyAvailableFlag()),
		SellAvailable:     boolPtr(opt.GetSellAvailableFlag()),
		ShortEnabled:      boolPtr(opt.GetShortEnabledFlag()),
		ForQualified:      boolPtr(opt.GetForQualInvestorFlag()),
		MinPriceIncrement: step,
	}
}

func fromListed(kind string, item listed) Summary {
	step := ""
	if q := item.GetMinPriceIncrement(); q != nil {
		step = formatIncrement(q)
	}
	return Summary{
		Ticker:            item.GetTicker(),
		Name:              item.GetName(),
		FIGI:              item.GetFigi(),
		UID:               item.GetUid(),
		ClassCode:         item.GetClassCode(),
		Type:              kind,
		Currency:          item.GetCurrency(),
		Lot:               item.GetLot(),
		Exchange:          item.GetExchange(),
		TradingStatus:     trimEnum(item.GetTradingStatus().String(), "SECURITY_TRADING_STATUS_"),
		APITradeAvailable: boolPtr(item.GetApiTradeAvailableFlag()),
		BuyAvailable:      boolPtr(item.GetBuyAvailableFlag()),
		SellAvailable:     boolPtr(item.GetSellAvailableFlag()),
		ShortEnabled:      boolPtr(item.GetShortEnabledFlag()),
		ForQualified:      boolPtr(item.GetForQualInvestorFlag()),
		MinPriceIncrement: step,
	}
}

func fromInstrument(inst *pb.Instrument) Summary {
	kind := inst.GetInstrumentType()
	if kind == "" {
		kind = trimEnum(inst.GetInstrumentKind().String(), "INSTRUMENT_TYPE_")
	}
	card := fromListed(kind, inst)
	card.ISIN = inst.GetIsin()
	return card
}

func fromShort(inst *pb.InstrumentShort) Summary {
	kind := inst.GetInstrumentType()
	if kind == "" {
		kind = trimEnum(inst.GetInstrumentKind().String(), "INSTRUMENT_TYPE_")
	}
	return Summary{
		Ticker:            inst.GetTicker(),
		Name:              inst.GetName(),
		FIGI:              inst.GetFigi(),
		UID:               inst.GetUid(),
		ClassCode:         inst.GetClassCode(),
		ISIN:              inst.GetIsin(),
		Type:              kind,
		Lot:               inst.GetLot(),
		APITradeAvailable: boolPtr(inst.GetApiTradeAvailableFlag()),
		ForQualified:      boolPtr(inst.GetForQualInvestorFlag()),
	}
}

func matches(item Summary, f Filter) bool {
	if f.APIOnly && (item.APITradeAvailable == nil || !*item.APITradeAvailable) {
		return false
	}
	if f.Currency != "" && !strings.EqualFold(item.Currency, f.Currency) {
		return false
	}
	if f.Exchange != "" && !strings.Contains(strings.ToLower(item.Exchange), strings.ToLower(f.Exchange)) {
		return false
	}
	if f.Query == "" {
		return true
	}
	q := strings.ToLower(f.Query)
	return strings.Contains(strings.ToLower(item.Ticker), q) || strings.Contains(strings.ToLower(item.Name), q)
}

func searchRank(query string, item Summary) int {
	q := strings.ToLower(strings.TrimSpace(query))
	ticker := strings.ToLower(item.Ticker)
	switch {
	case ticker == q && strings.EqualFold(item.ClassCode, "TQBR"):
		return 0
	case ticker == q:
		return 1
	case strings.HasPrefix(ticker, q):
		return 2
	case strings.Contains(ticker, q):
		return 3
	default:
		return 4
	}
}

func page(items []Summary, keep func(Summary) bool, limit int, less func(a, b Summary) int) Result {
	matched := make([]Summary, 0)
	for _, item := range items {
		if keep(item) {
			matched = append(matched, item)
		}
	}
	slices.SortFunc(matched, less)
	shown := matched
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	return Result{Matched: len(matched), Instruments: shown}
}

func compareSummary(a, b Summary) int {
	if c := cmp.Compare(a.Ticker, b.Ticker); c != 0 {
		return c
	}
	return cmp.Compare(a.ClassCode, b.ClassCode)
}

func trimEnum(value, prefix string) string {
	value = strings.TrimPrefix(value, prefix)
	return strings.ToLower(value)
}

func formatIncrement(q *pb.Quotation) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.9f", q.ToFloat()), "0"), ".")
}

func boolPtr(v bool) *bool {
	return &v
}
