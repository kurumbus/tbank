package market

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
				continue
			}
			switch definition.Strategy {
			case curve.StandardMini:
				if !((a.Family == "MX" && b.Family == "MM") || (a.Family == "Si" && b.Family == "USDM") || (a.Family == "GOLD" && b.Family == "GOLDM")) {
					continue
				}
			case curve.ShareClass:
				if !((a.Family == "SBER" && b.Family == "SBERP") || (a.Family == "TATN" && b.Family == "TATNP")) {
					continue
				}
			case curve.HoldingStub:
				if a.Ticker != "ENPG" || b.Ticker != "RUAL" {
					continue
				}
			case curve.CashOffer:
				if a.Ticker != "UGLD" {
					continue
				}
			}
			if definition.Strategy == curve.StandardMini || definition.Strategy == curve.ShareClass {
				if a.Perpetual || b.Perpetual || !a.Expiry.Equal(b.Expiry) || !a.LastTrade.Equal(b.LastTrade) {
					continue
				}
				if definition.Strategy == curve.StandardMini && (a.SettlementType != "cash_settlement" || a.SettlementReference != b.SettlementReference || a.PriceCurrency != b.PriceCurrency || (a.UnderlyingUID != "" && b.UnderlyingUID != "" && a.UnderlyingUID != b.UnderlyingUID)) {
					continue
				}
				if definition.Strategy == curve.ShareClass && (a.SettlementType != "physical_delivery" || b.SettlementType != "physical_delivery" || a.UnderlyingUID == "" || b.UnderlyingUID == "" || a.UnderlyingUID == b.UnderlyingUID) {
					continue
				}
			}
			if definition.Strategy == curve.PerpetualDated && (!a.Perpetual || b.Perpetual || a.SettlementReference != b.SettlementReference) {
				continue
			}
			if definition.Strategy == curve.FundNAV && (a.Ticker != "TGLD@" || b.Family != "GL" || b.Perpetual) {
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
				if relativeProduct(f) == selector {
					choices = append(choices, f)
				}
			}
		}
		sort.Slice(choices, func(i, j int) bool {
			a, b := timestamp(choices[i].GetExpirationDate()), timestamp(choices[j].GetExpirationDate())
			if a.Equal(b) {
				return choices[i].GetUid() < choices[j].GetUid()
			}
			return a.Before(b)
		})
		result := []curve.Instrument{}
		for _, f := range choices {
			if !f.GetApiTradeAvailableFlag() || !f.GetBuyAvailableFlag() || !f.GetSellAvailableFlag() || !f.GetShortEnabledFlag() {
				continue
			}
			product := relativeProduct(f)
			perpetual := product == "CNYRUBF" || product == "GLDRUBF"
			if first := timestamp(f.GetFirstTradeDate()); !first.IsZero() && first.After(now) {
				continue
			}
			if !perpetual && (!timestamp(f.GetLastTradeDate()).After(now) || timestamp(f.GetExpirationDate()).After(now.Add(time.Duration(selection.MaxDays)*24*time.Hour))) {
				continue
			}
			mapped, e := p.mapRelativeFuture(ctx, f, c, now)
			if e != nil {
				if uid != "" {
					return nil, e
				}
				continue
			}
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
