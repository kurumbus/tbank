package market

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
	"tbank/internal/scanner"
)

type relativeCatalogue struct {
	shares            map[string]*pb.Share
	funds             map[string]*pb.Etf
	futures           map[string]*pb.Future
	fees              map[string]decimal.Decimal
	loaded            map[string]curve.Instrument
	trackedUnderlying map[string]string
	diagnostics       []string
	diagnosed         map[string]bool
}

// RelativeSnapshot resolves configured economic relationships to immutable
// broker UIDs. Supplemental references remain explicitly sourced observations;
// absent NAV, ownership, tender or funding facts are never manufactured.
func (p *Provider) RelativeSnapshot(ctx context.Context, selection curve.RelativeSelection) (curve.Snapshot, error) {
	ctx = context.WithValue(ctx, relativeCacheKey{}, &relativeReferenceCache{funding: map[string]cachedFunding{}, fx: map[string]cachedFX{}})
	if selection.MaxPairsPerFamily < 1 || selection.MaxPairsPerFamily > 3 || selection.MaxDays < 1 || selection.MaxDays > 730 || (selection.Depth != 1 && selection.Depth != 10 && selection.Depth != 20 && selection.Depth != 30 && selection.Depth != 40 && selection.Depth != 50) {
		return curve.Snapshot{}, fmt.Errorf("invalid relative selection horizon, count or depth")
	}
	result := curve.Snapshot{Books: map[string]scanner.Book{}}
	now := p.now().UTC()
	catalogue := relativeCatalogue{shares: map[string]*pb.Share{}, funds: map[string]*pb.Etf{}, futures: map[string]*pb.Future{}, loaded: map[string]curve.Instrument{}}
	catalogue.trackedUnderlying = map[string]string{}
	for _, saved := range selection.Tracked {
		if saved.Kind == "future" && saved.UnderlyingUID != "" {
			catalogue.trackedUnderlying[saved.Family] = saved.UnderlyingUID
		}
	}
	fees, err := p.fetchFees(ctx)
	catalogue.fees = fees
	if err != nil {
		result.Warnings = append(result.Warnings, "relative futures exchange fees unavailable: "+err.Error())
	}
	request := &pb.InstrumentsRequest{InstrumentStatus: pb.InstrumentStatus_INSTRUMENT_STATUS_BASE.Enum()}
	shares, e := p.instruments.Shares(ctx, request)
	if e != nil {
		result.Warnings = append(result.Warnings, "relative share catalogue: "+e.Error())
	} else {
		for _, s := range shares.GetInstruments() {
			if s != nil && s.GetClassCode() == "TQBR" {
				catalogue.shares[s.GetUid()] = s
			}
		}
	}
	funds, e := p.instruments.Etfs(ctx, request)
	if e != nil {
		result.Warnings = append(result.Warnings, "relative fund catalogue: "+e.Error())
	} else {
		for _, f := range funds.GetInstruments() {
			if f != nil && (f.GetClassCode() == "SPBRU" || f.GetClassCode() == "TQTF") {
				catalogue.funds[f.GetUid()] = f
			}
		}
	}
	futures, e := p.instruments.Futures(ctx, request)
	if e != nil {
		result.Warnings = append(result.Warnings, "relative futures catalogue: "+e.Error())
	} else {
		for _, f := range futures.GetInstruments() {
			if f != nil {
				catalogue.futures[f.GetUid()] = f
			}
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	for _, definition := range selection.Relationships {
		resolved, e := p.resolveRelative(ctx, &catalogue, definition, selection, now)
		if e != nil {
			result.Warnings = append(result.Warnings, definition.ID+": "+e.Error())
			continue
		}
		if len(resolved) == 0 {
			result.Warnings = append(result.Warnings, definition.ID+": no current supported instruments satisfy the relationship")
		}
		result.Relationships = append(result.Relationships, resolved...)
	}
	result.Warnings = append(result.Warnings, catalogue.diagnostics...)
	// Refresh every saved UID independently of the configurable discovery list.
	for _, saved := range selection.Tracked {
		current, e := p.loadRelativeTracked(ctx, saved, &catalogue, now)
		if e == nil {
			e = sameRelativeUnits(saved, current)
		}
		if e != nil {
			delete(catalogue.loaded, saved.UID)
			result.Warnings = append(result.Warnings, "tracked "+saved.Ticker+": "+e.Error())
			continue
		}
		catalogue.loaded[current.UID] = current
	}
	relationshipIDs := map[string]bool{}
	for _, r := range result.Relationships {
		relationshipIDs[r.ID] = true
	}
	for _, saved := range selection.TrackedRelationships {
		copy, e := copyRelative(saved)
		if e != nil {
			return result, e
		}
		saved = copy
		if saved.Strategy == curve.StandardMini || saved.Strategy == curve.ShareClass || saved.Strategy == curve.PerpetualDated {
			a, aOK := catalogue.loaded[saved.PrimaryUID]
			b, bOK := catalogue.loaded[saved.HedgeUID]
			if aOK && bOK {
				saved.AsOf = now
				saved.ValidUntil = now.Add(24 * time.Hour)
				for _, i := range []curve.Instrument{a, b} {
					if !i.Perpetual && !i.LastTrade.IsZero() && i.LastTrade.Before(saved.ValidUntil) {
						saved.ValidUntil = i.LastTrade
					}
				}
			}
		}
		// The position owns its original sizing and references. A changed default
		// relationship must not silently resize or redefine that position.
		for i := range result.Relationships {
			if result.Relationships[i].ID == saved.ID {
				current := result.Relationships[i]
				if current.PrimaryUID == saved.PrimaryUID && current.HedgeUID == saved.HedgeUID && current.Strategy == saved.Strategy {
					if current.Borrow != nil {
						saved.Borrow = current.Borrow
					}
					if saved.Holding != nil && current.Holding != nil && current.Holding.Borrow != nil {
						saved.Holding.Borrow = current.Holding.Borrow
					}
					if saved.Fund != nil && current.Fund != nil && current.Fund.Borrow != nil {
						saved.Fund.Borrow = current.Fund.Borrow
					}
				}
				result.Relationships[i] = saved
			}
		}
		if !relationshipIDs[saved.ID] {
			result.Relationships = append(result.Relationships, saved)
			relationshipIDs[saved.ID] = true
		}
	}
	for i := range result.Relationships {
		p.refreshRelativeReferences(ctx, &result.Relationships[i], catalogue.loaded, selection.HistoryFrom, p.now().UTC(), &result.Warnings)
	}
	// FX-dependent tick values are refreshed after slower supplemental sources,
	// close to the books they will price. They are never frozen at snapshot start.
	for uid, instrument := range catalogue.loaded {
		if instrument.Kind == "future" && instrument.PriceCurrency == "usd" {
			current, e := p.mapRelativeFuture(ctx, catalogue.futures[uid], &catalogue, p.now().UTC())
			if e != nil {
				delete(catalogue.loaded, uid)
				result.Warnings = append(result.Warnings, "current USD/RUB conversion "+uid+": "+e.Error())
			} else {
				catalogue.loaded[uid] = current
			}
		}
	}
	for _, instrument := range catalogue.loaded {
		result.Instruments = append(result.Instruments, instrument)
	}
	sort.Slice(result.Instruments, func(i, j int) bool { return result.Instruments[i].UID < result.Instruments[j].UID })
	sort.Slice(result.Relationships, func(i, j int) bool { return result.Relationships[i].ID < result.Relationships[j].ID })
	if len(result.Instruments) == 0 {
		result.Warnings = append(result.Warnings, "relative universe contains no supported current or tracked instruments")
	}
	type bookResult struct {
		uid  string
		book scanner.Book
		err  error
	}
	jobs := make(chan curve.Instrument)
	output := make(chan bookResult, len(result.Instruments))
	var workers sync.WaitGroup
	for n := 0; n < 6; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for instrument := range jobs {
				book, e := p.curveBook(ctx, instrument, selection.Depth)
				output <- bookResult{instrument.UID, book, e}
			}
		}()
	}
	for _, instrument := range result.Instruments {
		jobs <- instrument
	}
	close(jobs)
	workers.Wait()
	close(output)
	for r := range output {
		if r.err != nil {
			result.Warnings = append(result.Warnings, r.uid+": "+r.err.Error())
			continue
		}
		result.Books[r.uid] = r.book
	}
	for n := range result.Instruments {
		i := &result.Instruments[n]
		if i.Kind != "share" && i.Kind != "etf" {
			continue
		}
		book, ok := result.Books[i.UID]
		if !ok || !book.Consistent {
			continue
		}
		price := decimal.Zero
		for _, level := range book.Asks {
			price = decimal.Max(price, level.Price)
		}
		for _, level := range book.Bids {
			price = decimal.Max(price, level.Price)
		}
		risk := decimal.NewFromInt(1)
		if s := catalogue.shares[i.UID]; s != nil {
			risk = decimal.Max(risk, quotation(s.GetDshortClient()), quotation(s.GetDshort()), quotation(s.GetDshortMin()))
		}
		if f := catalogue.funds[i.UID]; f != nil {
			risk = decimal.Max(risk, quotation(f.GetDshortClient()), quotation(f.GetDshort()), quotation(f.GetDshortMin()))
		}
		i.InitialMarginSell = price.Mul(i.PriceFactor).Mul(risk)
		if i.ShortEnabled {
			result.Warnings = append(result.Warnings, i.Ticker+": cash-short margin is a conservative full-notional per-unit reserve, raised for published risk coefficients; actual account buying power is not verified")
		}
	}
	result.ObservedAt = p.now().UTC()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func relativeProduct(f *pb.Future) string {
	if f == nil {
		return ""
	}
	// BasicAsset, settlement type, asset type and live unit metadata accompany
	// every product selection. Display-name/ticker substrings alone never qualify.
	if f.GetTicker() == "CNYRUBF" && (f.GetBasicAsset() == "CNYRUBF" || f.GetBasicAsset() == "CNYRUBTOM" || f.GetBasicAsset() == "CNYRUB") {
		return "CNYRUBF"
	}
	if f.GetTicker() == "GLDRUBF" && (f.GetBasicAsset() == "GLDRUBF" || f.GetBasicAsset() == "GLDRUBTOM" || f.GetBasicAsset() == "GLDRUB") {
		return "GLDRUBF"
	}
	products := map[string]string{"MIX": "MX", "MX": "MX", "MXI": "MM", "MM": "MM", "Si": "Si", "SI": "Si", "USDM": "USDM", "GOLD": "GOLD", "GOLDM": "GOLDM", "CNY": "CR", "CNYRUB": "CR", "GL": "GL", "GLDRUB": "GL", "SBRF": "SBER", "SBER": "SBER", "SBPR": "SBERP", "SBERP": "SBERP", "TATN": "TATN", "TATP": "TATNP", "TATNP": "TATNP"}
	return products[f.GetBasicAsset()]
}

func (p *Provider) mapRelativeFuture(ctx context.Context, f *pb.Future, c *relativeCatalogue, now time.Time) (curve.Instrument, error) {
	if f == nil || f.GetUid() == "" || f.GetClassCode() != "SPBFUT" || f.GetLot() != 1 || !strings.EqualFold(f.GetCurrency(), "rub") {
		return curve.Instrument{}, fmt.Errorf("unsupported future identity, lot or settlement currency")
	}
	product := relativeProduct(f)
	if product == "" {
		return curve.Instrument{}, fmt.Errorf("unrecognized underlying basic asset %q", f.GetBasicAsset())
	}
	perpetual := product == "CNYRUBF" || product == "GLDRUBF"
	expiry, last := timestamp(f.GetExpirationDate()), timestamp(f.GetLastTradeDate())
	if !perpetual && (expiry.IsZero() || last.IsZero() || last.After(expiry)) {
		return curve.Instrument{}, fmt.Errorf("dated future expiry/trading date missing")
	}
	settlement, assetType, reference, priceCurrency := "cash_settlement", "index", "", "rub"
	switch product {
	case "MX", "MM":
		reference = "MOEX:IMOEX:standard-mini-same-final-index-fixing"
	case "Si", "USDM":
		assetType = "currency"
		reference = "MOEX:USD-RUB:standard-mini-same-final-currency-fixing"
	case "GOLD", "GOLDM":
		assetType = "commodity"
		reference = "MOEX:GOLD:USD-per-troy-ounce"
		priceCurrency = "usd"
	case "CR", "CNYRUBF":
		assetType = "currency"
		reference = "MOEX:CNY-RUB"
	case "GL", "GLDRUBF":
		assetType = "commodity"
		reference = "MOEX:GOLD:RUB-per-gram"
	case "SBER", "SBERP", "TATN", "TATNP":
		settlement = "physical_delivery"
		assetType = "security"
		reference = "MOEX:share-delivery:" + f.GetBasicAssetPositionUid()
	}
	if futureSettlementType(f.GetFuturesType()) != settlement || futureAssetType(f.GetAssetType()) != assetType {
		return curve.Instrument{}, fmt.Errorf("future settlement/asset type differs from the selected contract specification")
	}
	if assetType == "security" {
		matched := false
		for _, share := range c.shares {
			if share.GetTicker() == product && share.GetPositionUid() != "" && share.GetPositionUid() == f.GetBasicAssetPositionUid() {
				matched = true
			}
		}
		if !matched {
			matched = c.trackedUnderlying[product] != "" && c.trackedUnderlying[product] == f.GetBasicAssetPositionUid()
		}
		if !matched {
			return curve.Instrument{}, fmt.Errorf("share-class future does not resolve to the exact underlying share position UID")
		}
	}
	units := quotation(f.GetBasicAssetSize())
	if !units.IsPositive() {
		return curve.Instrument{}, fmt.Errorf("current contract underlying unit size unavailable")
	}
	m, err := p.instruments.GetFuturesMargin(ctx, &pb.GetFuturesMarginRequest{InstrumentId: f.GetUid()})
	if err != nil {
		return curve.Instrument{}, err
	}
	step, amount := quotation(m.GetMinPriceIncrement()), quotation(m.GetMinPriceIncrementAmount())
	buy, sell := m.GetInitialMarginOnBuy(), m.GetInitialMarginOnSell()
	if !step.IsPositive() || !amount.IsPositive() || buy == nil || sell == nil || !strings.EqualFold(buy.GetCurrency(), "rub") || !strings.EqualFold(sell.GetCurrency(), "rub") || !money(buy).IsPositive() || !money(sell).IsPositive() {
		return curve.Instrument{}, fmt.Errorf("current RUB multiplier and both margin requirements are unavailable")
	}
	fee, known := c.fees[f.GetTicker()]
	if perpetual {
		expiry = time.Time{}
		last = time.Time{}
	}
	return curve.Instrument{UID: f.GetUid(), Ticker: f.GetTicker(), ClassCode: f.GetClassCode(), Kind: "future", Family: product, Currency: "rub", Lot: 1, PriceFactor: amount.Div(step), UnitSize: units, PriceCurrency: priceCurrency, PriceFactorKnown: true, PriceFactorAt: p.now().UTC(), PriceFactorSource: "T-Bank GetFuturesMargin by UID " + f.GetUid(), Perpetual: perpetual, SettlementType: settlement, SettlementReference: reference, UnderlyingUID: f.GetBasicAssetPositionUid(), Expiry: expiry, LastTrade: last, APIAvailable: f.GetApiTradeAvailableFlag(), BuyAvailable: f.GetBuyAvailableFlag(), SellAvailable: f.GetSellAvailableFlag(), ShortEnabled: f.GetShortEnabledFlag(), QualifiedOnly: f.GetForQualInvestorFlag(), InitialMarginBuy: money(buy), InitialMarginSell: money(sell), ExchangeFee: fee, ExchangeFeeKnown: known, ExchangeFeeSource: p.feeURL}, nil
}

func mapRelativeShare(s *pb.Share, now time.Time) (curve.Instrument, error) {
	if s == nil || s.GetUid() == "" || s.GetClassCode() != "TQBR" || !strings.EqualFold(s.GetCurrency(), "rub") || s.GetLot() <= 0 || s.GetPositionUid() == "" {
		return curve.Instrument{}, fmt.Errorf("unsupported current RUB share")
	}
	return curve.Instrument{UID: s.GetUid(), Ticker: s.GetTicker(), ClassCode: s.GetClassCode(), Kind: "share", Family: s.GetTicker(), Currency: "rub", Lot: int64(s.GetLot()), PriceFactor: decimal.NewFromInt(1), UnitSize: decimal.NewFromInt(1), PriceCurrency: "rub", PriceFactorKnown: true, PriceFactorAt: now, PriceFactorSource: "T-Bank ShareBy", UnderlyingUID: s.GetPositionUid(), APIAvailable: s.GetApiTradeAvailableFlag(), BuyAvailable: s.GetBuyAvailableFlag(), SellAvailable: s.GetSellAvailableFlag(), ShortEnabled: s.GetShortEnabledFlag(), QualifiedOnly: s.GetForQualInvestorFlag()}, nil
}
func mapRelativeFund(f *pb.Etf, now time.Time) (curve.Instrument, error) {
	if f == nil || f.GetUid() == "" || f.GetTicker() != "TGLD@" || f.GetIsin() != "RU000A101X50" || f.GetClassCode() != "SPBRU" || !strings.EqualFold(f.GetCurrency(), "rub") || f.GetLot() <= 0 {
		return curve.Instrument{}, fmt.Errorf("only exact RUB TGLD@ / SPBRU fund listing is supported")
	}
	return curve.Instrument{UID: f.GetUid(), Ticker: f.GetTicker(), ClassCode: f.GetClassCode(), Kind: "etf", Family: "TGLD@", Currency: "rub", Lot: int64(f.GetLot()), PriceFactor: decimal.NewFromInt(1), UnitSize: decimal.NewFromInt(1), PriceCurrency: "rub", PriceFactorKnown: true, PriceFactorAt: now, PriceFactorSource: "T-Bank EtfBy", UnderlyingUID: f.GetPositionUid(), APIAvailable: f.GetApiTradeAvailableFlag(), BuyAvailable: f.GetBuyAvailableFlag(), SellAvailable: f.GetSellAvailableFlag(), ShortEnabled: f.GetShortEnabledFlag(), QualifiedOnly: f.GetForQualInvestorFlag()}, nil
}

func (p *Provider) loadRelativeTracked(ctx context.Context, saved curve.Instrument, c *relativeCatalogue, now time.Time) (curve.Instrument, error) {
	request := &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: saved.UID}
	switch saved.Kind {
	case "future":
		r, e := p.instruments.FutureBy(ctx, request)
		if e != nil {
			return curve.Instrument{}, e
		}
		if r.GetInstrument().GetUid() != saved.UID {
			return curve.Instrument{}, fmt.Errorf("future UID changed")
		}
		c.futures[saved.UID] = r.GetInstrument()
		return p.mapRelativeFuture(ctx, r.GetInstrument(), c, now)
	case "share":
		r, e := p.instruments.ShareBy(ctx, request)
		if e != nil {
			return curve.Instrument{}, e
		}
		if r.GetInstrument().GetUid() != saved.UID {
			return curve.Instrument{}, fmt.Errorf("share UID changed")
		}
		c.shares[saved.UID] = r.GetInstrument()
		return mapRelativeShare(r.GetInstrument(), now)
	case "etf":
		r, e := p.instruments.EtfBy(ctx, request)
		if e != nil {
			return curve.Instrument{}, e
		}
		if r.GetInstrument().GetUid() != saved.UID {
			return curve.Instrument{}, fmt.Errorf("fund UID changed")
		}
		c.funds[saved.UID] = r.GetInstrument()
		return mapRelativeFund(r.GetInstrument(), now)
	}
	return curve.Instrument{}, fmt.Errorf("unsupported tracked relative kind %q", saved.Kind)
}
func sameRelativeUnits(a, b curve.Instrument) error {
	if a.UID == "" || a.UID != b.UID || a.Kind != b.Kind || a.Family != b.Family || a.ClassCode != b.ClassCode || a.Currency != b.Currency || a.Lot != b.Lot || !a.UnitSize.Equal(b.UnitSize) || a.PriceCurrency != b.PriceCurrency || a.Perpetual != b.Perpetual || a.SettlementType != b.SettlementType || a.SettlementReference != b.SettlementReference || a.UnderlyingUID != b.UnderlyingUID || !a.Expiry.Equal(b.Expiry) || !a.LastTrade.Equal(b.LastTrade) {
		return fmt.Errorf("tracked relative contract/underlying units changed; reconciliation required")
	}
	// USD-quoted futures use the current RUB tick value. A change in FX is not
	// a corporate action and must not freeze the entry conversion forever.
	if a.PriceCurrency != "usd" && !a.PriceFactor.Equal(b.PriceFactor) {
		return fmt.Errorf("tracked RUB price multiplier changed")
	}
	return nil
}
