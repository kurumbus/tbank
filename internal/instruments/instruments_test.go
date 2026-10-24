package instruments

import (
	"cmp"
	"testing"
)

func TestNormalizeKind(t *testing.T) {
	kind, err := NormalizeKind("ETFs")
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindETF {
		t.Fatalf("kind = %s", kind)
	}
	if kind, err = NormalizeKind("options"); err != nil || kind != KindOption {
		t.Fatalf("options: kind=%s err=%v", kind, err)
	}
}

func TestMatchesQueryCurrencyAndAPI(t *testing.T) {
	yes := true
	no := false
	items := []Summary{
		{Ticker: "SBER", Name: "Сбер Банк", Currency: "rub", Exchange: "MOEX", APITradeAvailable: &yes},
		{Ticker: "AAPL", Name: "Apple", Currency: "usd", Exchange: "SPBXM", APITradeAvailable: &no},
	}

	got := page(items, func(item Summary) bool {
		return matches(item, Filter{Query: "сбер", Currency: "RUB", APIOnly: true})
	}, 10, compareSummary)
	if got.Matched != 1 || got.Instruments[0].Ticker != "SBER" {
		t.Fatalf("matched = %#v", got)
	}
}

func TestPageLimit(t *testing.T) {
	items := []Summary{{Ticker: "B"}, {Ticker: "A"}, {Ticker: "C"}}
	got := page(items, func(Summary) bool { return true }, 2, compareSummary)
	if got.Matched != 3 {
		t.Fatalf("matched = %d", got.Matched)
	}
	if len(got.Instruments) != 2 || got.Instruments[0].Ticker != "A" || got.Instruments[1].Ticker != "B" {
		t.Fatalf("page = %#v", got.Instruments)
	}
}

func TestSearchRankPrefersExactTicker(t *testing.T) {
	items := []Summary{
		{Ticker: "RU000A101C89", Name: "Сбербанк 001Р-SBER15"},
		{Ticker: "SBERP", Name: "Сбербанк - привилегированные акции"},
		{Ticker: "SBER", ClassCode: "SMAL", Name: "Сбербанк"},
		{Ticker: "SBER", ClassCode: "TQBR", Name: "Сбербанк"},
	}
	got := page(items, func(Summary) bool { return true }, 2, func(a, b Summary) int {
		if c := cmp.Compare(searchRank("SBER", a), searchRank("SBER", b)); c != 0 {
			return c
		}
		return compareSummary(a, b)
	})
	if got.Instruments[0].ClassCode != "TQBR" || got.Instruments[1].Ticker != "SBER" {
		t.Fatalf("order = %#v", got.Instruments)
	}
}

func TestDedupeKeepsFirstUID(t *testing.T) {
	items := []Summary{
		{Ticker: "SBER", UID: "1", ClassCode: "TQBR"},
		{Ticker: "SBER", UID: "1", ClassCode: "TQBR"},
		{Ticker: "SBERP", UID: "2", ClassCode: "TQBR"},
	}
	got := dedupe(items)
	if len(got) != 2 || got[1].Ticker != "SBERP" {
		t.Fatalf("deduped = %#v", got)
	}
}

func TestTrimEnum(t *testing.T) {
	got := trimEnum("SECURITY_TRADING_STATUS_NORMAL_TRADING", "SECURITY_TRADING_STATUS_")
	if got != "normal_trading" {
		t.Fatalf("status = %s", got)
	}
}
