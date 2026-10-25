package market

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	pb "opensource.tbank.ru/invest/invest-go/proto"
	"tbank/internal/curve"
)

func copyRelative(r curve.Relationship) (curve.Relationship, error) {
	var copy curve.Relationship
	raw, e := json.Marshal(r)
	if e != nil {
		return copy, e
	}
	e = json.Unmarshal(raw, &copy)
	return copy, e
}

func (p *Provider) resolveRelative(ctx context.Context, c *relativeCatalogue, definition curve.Relationship, selection curve.RelativeSelection, now time.Time) ([]curve.Relationship, error) {
	if definition.ID == "" || !curve.IsRelativeStrategy(definition.Strategy) || definition.PrimaryLots <= 0 {
		return nil, fmt.Errorf("relative relationship requires ID, strategy and positive primary lots")
	}
	primaryKind, hedgeKind := "future", "future"
	switch definition.Strategy {
	case curve.HoldingStub:
		primaryKind = "share"
		hedgeKind = "share"
	case curve.FundNAV:
		primaryKind = "etf"
	case curve.CashOffer:
		primaryKind = "share"
		hedgeKind = ""
	}
	primary, e := p.relativeChoices(ctx, c, definition.PrimaryUID, definition.PrimarySelector, primaryKind, selection, now)
	if e != nil {
		return nil, e
	}
	var hedge []curve.Instrument
	if hedgeKind != "" {
		hedge, e = p.relativeChoices(ctx, c, definition.HedgeUID, definition.HedgeSelector, hedgeKind, selection, now)
		if e != nil {
			return nil, e
		}
	}
	resolved := []curve.Relationship{}
	pairRejects := map[string]int{}
	var pairExamples []string
	rejectPair := func(a, b curve.Instrument, reason string) {
		pairRejects[reason]++
		if len(pairExamples) < 2 {
			pairExamples = append(pairExamples, fmt.Sprintf("%s/%s: %s (expiry=%s/%s last_trade=%s/%s underlying=%q/%q settlement=%q/%q reference=%q/%q)", a.Ticker, b.Ticker, reason, a.Expiry.Format(time.RFC3339), b.Expiry.Format(time.RFC3339), a.LastTrade.Format(time.RFC3339), b.LastTrade.Format(time.RFC3339), a.UnderlyingUID, b.UnderlyingUID, a.SettlementType, b.SettlementType, a.SettlementReference, b.SettlementReference))
		}
	}
	for _, a := range primary {
		matches := hedge
		if hedgeKind == "" {
			matches = []curve.Instrument{{}}
		}
		for _, b := range matches {
			if len(resolved) >= selection.MaxPairsPerFamily {
				return resolved, nil
			}
			if a.UID == b.UID {
				rejectPair(a, b, "identical leg UIDs")
				continue
			}
			switch definition.Strategy {
			case curve.StandardMini:
				if !((a.Family == "MX" && b.Family == "MM") || (a.Family == "Si" && b.Family == "USDM") || (a.Family == "GOLD" && b.Family == "GOLDM")) {
					rejectPair(a, b, "unsupported product combination")
					continue
				}
			case curve.ShareClass:
				if !((a.Family == "SBER" && b.Family == "SBERP") || (a.Family == "TATN" && b.Family == "TATNP")) {
					rejectPair(a, b, "unsupported product combination")
					continue
				}
			case curve.HoldingStub:
				if a.Ticker != "ENPG" || b.Ticker != "RUAL" {
					rejectPair(a, b, "unsupported cash/product combination")
					continue
				}
			case curve.CashOffer:
				if a.Ticker != "UGLD" {
					rejectPair(a, b, "unsupported cash/product combination")
					continue
				}
			}
			if definition.Strategy == curve.StandardMini || definition.Strategy == curve.ShareClass {
				if a.Perpetual || b.Perpetual || !a.Expiry.Equal(b.Expiry) || !a.LastTrade.Equal(b.LastTrade) {
					rejectPair(a, b, "expiry or last-trade mismatch, or unexpected perpetual")
					continue
				}
				if definition.Strategy == curve.StandardMini && (a.SettlementType != "cash_settlement" || a.SettlementReference != b.SettlementReference || a.PriceCurrency != b.PriceCurrency || (a.UnderlyingUID != "" && b.UnderlyingUID != "" && a.UnderlyingUID != b.UnderlyingUID)) {
					rejectPair(a, b, "settlement, fixing, quote currency or underlying mismatch")
					continue
				}
				if definition.Strategy == curve.ShareClass && (a.SettlementType != "physical_delivery" || b.SettlementType != "physical_delivery" || a.UnderlyingUID == "" || b.UnderlyingUID == "" || a.UnderlyingUID == b.UnderlyingUID) {
					rejectPair(a, b, "share delivery or underlying mismatch")
					continue
				}
			}
			if definition.Strategy == curve.PerpetualDated && (!a.Perpetual || b.Perpetual || a.SettlementReference != b.SettlementReference) {
				rejectPair(a, b, "perpetual/dated type or fixing mismatch")
				continue
			}
			if definition.Strategy == curve.FundNAV && (a.Ticker != "TGLD@" || b.Family != "GL" || b.Perpetual) {
				rejectPair(a, b, "unsupported cash/product combination")
				continue
			}
			r, e := copyRelative(definition)
			if e != nil {
				return nil, e
			}
			r.PrimaryUID = a.UID
			r.HedgeUID = b.UID
			if a.Kind == "future" && b.Kind == "future" {
				if !a.UnitSize.IsPositive() || !b.UnitSize.IsPositive() {
					rejectPair(a, b, "underlying unit size unavailable")
					continue
				}
				r.HedgeLotsPerPrimaryLot = a.UnitSize.Div(b.UnitSize)
				r.PrimaryPriceScale = relativePriceScale(a)
				r.HedgePriceScale = relativePriceScale(b)
			}
			if b.Kind == "future" {
				r.ID += "@" + b.Expiry.UTC().Format("20060102T150405Z")
			} else if a.Kind == "future" {
				r.ID += "@" + a.Expiry.UTC().Format("20060102T150405Z")
			}
			if r.Source == "" {
				r.Source = "T-Bank current metadata and MOEX contract specifications: " + a.SettlementReference + " / " + b.SettlementReference
			}
			if definition.Strategy == curve.StandardMini || definition.Strategy == curve.ShareClass || definition.Strategy == curve.PerpetualDated {
				r.Verified = true
				r.AsOf = now
				r.ValidUntil = now.Add(24 * time.Hour)
				for _, i := range []curve.Instrument{a, b} {
					if i.Kind == "future" && !i.Perpetual && i.LastTrade.Before(r.ValidUntil) {
						r.ValidUntil = i.LastTrade
					}
				}
			}
			c.loaded[a.UID] = a
			if b.UID != "" {
				c.loaded[b.UID] = b
			}
			resolved = append(resolved, r)
		}
	}
	if len(resolved) == 0 && len(pairRejects) > 0 {
		var reasons []string
		for reason, count := range pairRejects {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
		}
		sort.Strings(reasons)
		return nil, fmt.Errorf("no compatible pair from %d eligible primary and %d eligible hedge contracts: %s; examples: %s", len(primary), len(hedge), strings.Join(reasons, "; "), strings.Join(pairExamples, "; "))
	}
	return resolved, nil
}

func relativePriceScale(i curve.Instrument) decimal.Decimal {
	if i.PriceCurrency == "usd" {
		return decimal.NewFromInt(1)
	} // Both listed GOLD products quote USD per ounce.
	return i.PriceFactor.Div(i.UnitSize)
}

func (p *Provider) relativeChoices(ctx context.Context, c *relativeCatalogue, uid, selector, kind string, selection curve.RelativeSelection, now time.Time) ([]curve.Instrument, error) {
	if uid == "" && selector == "" {
		return nil, fmt.Errorf("missing %s UID or exact selector", kind)
	}
	if kind == "future" {
		var choices []*pb.Future
		if uid != "" {
			f := c.futures[uid]
			if f == nil {
				r, e := p.instruments.FutureBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: uid})
				if e != nil {
					return nil, e
				}
				f = r.GetInstrument()
				if f.GetUid() != uid {
					return nil, fmt.Errorf("explicit future UID mismatch")
				}
				c.futures[uid] = f
			}
			choices = append(choices, f)
		} else {
			for _, f := range c.futures {
				if relativeProduct(f) == selector || futureTickerHint(f.GetTicker(), selector) {
					choices = append(choices, f)
				}
			}
		}
		sortFutureChoices(choices)
		diagnostic := futureDiscovery{selector: selector, catalogue: len(c.futures)}
		if uid != "" {
			diagnostic.selector = uid
		}
		defer func() {
			if c.diagnosed == nil {
				c.diagnosed = map[string]bool{}
			}
			if !c.diagnosed[diagnostic.selector] {
				c.diagnostics = append(c.diagnostics, diagnostic.messages()...)
				c.diagnosed[diagnostic.selector] = true
			}
		}()
		result := []curve.Instrument{}
		for _, f := range choices {
			product := relativeProduct(f)
			if uid == "" && product != selector {
				diagnostic.tickerOnly++
				diagnostic.reject(f, "ticker hint matches but basic_asset does not identify the selected product")
				continue
			}
			diagnostic.matched++
			perpetual := product == "CNYRUBF" || product == "GLDRUBF"
			if reason := futureAvailabilityReason(f, now, selection.MaxDays, perpetual); reason != "" {
				diagnostic.reject(f, reason)
				continue
			}
			mapped, e := p.mapRelativeFuture(ctx, f, c, now)
			if e != nil {
				diagnostic.reject(f, e.Error())
				if uid != "" {
					return nil, e
				}
				continue
			}
			diagnostic.selected++
			result = append(result, mapped)
		}
		return result, nil
	}
	if kind == "share" {
		var choices []*pb.Share
		if uid != "" {
			r, e := p.instruments.ShareBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: uid})
			if e != nil {
				return nil, e
			}
			if r.GetInstrument().GetUid() != uid {
				return nil, fmt.Errorf("share UID mismatch")
			}
			choices = []*pb.Share{r.GetInstrument()}
		} else {
			for _, s := range c.shares {
				if s.GetTicker() == selector {
					choices = append(choices, s)
				}
			}
		}
		if len(choices) > 1 {
			return nil, fmt.Errorf("ambiguous exact share selector %q", selector)
		}
		if len(choices) == 0 {
			return nil, nil
		}
		// Catalogue membership selects a UID; ShareBy supplies current permissions.
		r, e := p.instruments.ShareBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: choices[0].GetUid()})
		if e != nil {
			return nil, e
		}
		i, e := mapRelativeShare(r.GetInstrument(), now)
		if e != nil {
			return nil, e
		}
		if i.UID != choices[0].GetUid() {
			return nil, fmt.Errorf("share identity changed")
		}
		c.shares[i.UID] = r.GetInstrument()
		return []curve.Instrument{i}, nil
	}
	if kind == "etf" {
		var choices []*pb.Etf
		if uid != "" {
			r, e := p.instruments.EtfBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: uid})
			if e != nil {
				return nil, e
			}
			if r.GetInstrument().GetUid() != uid {
				return nil, fmt.Errorf("fund UID mismatch")
			}
			choices = []*pb.Etf{r.GetInstrument()}
		} else {
			for _, f := range c.funds {
				if f.GetTicker() == selector && f.GetClassCode() == "SPBRU" {
					choices = append(choices, f)
				}
			}
		}
		if len(choices) > 1 {
			return nil, fmt.Errorf("ambiguous exact fund selector %q", selector)
		}
		if len(choices) == 0 {
			return nil, nil
		}
		r, e := p.instruments.EtfBy(ctx, &pb.InstrumentRequest{IdType: pb.InstrumentIdType_INSTRUMENT_ID_TYPE_UID, Id: choices[0].GetUid()})
		if e != nil {
			return nil, e
		}
		i, e := mapRelativeFund(r.GetInstrument(), now)
		if e != nil {
			return nil, e
		}
		if i.UID != choices[0].GetUid() {
			return nil, fmt.Errorf("fund identity changed")
		}
		c.funds[i.UID] = r.GetInstrument()
		return []curve.Instrument{i}, nil
	}
	return nil, fmt.Errorf("unsupported relative instrument kind %q", kind)
}
