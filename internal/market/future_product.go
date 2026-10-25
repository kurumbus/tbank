package market

import (
	"slices"
	"strings"

	pb "opensource.tbank.ru/invest/invest-go/proto"
)

// BasicAsset identifies an underlying, not necessarily an exchange product.
// In the sandbox catalogue, USD/RUB describes both Si and USDM; CNY/RUB and
// RUB gold also describe both dated and perpetual contracts. Combine the exact
// ticker product with its known underlying aliases before mapping contract terms.
var futureUnderlyingAliases = map[string][]string{
	"RGBI":    {"RGBI", "Индекс государственных облигаций"},
	"KEYRATE": {"KEYRATE", "Индекс ключевой ставки"},
	"RUONIA":  {"RUONIA", "Индекс RUONIA"},
	"MX":      {"MIX", "MX", "IMOEX"},
	"MM":      {"MXI", "MM", "Индекс московской биржи"},
	"Si":      {"Si", "SI", "USD/RUB"},
	"USDM":    {"USDM", "USD/RUB"},
	"GOLD":    {"GOLD", "Золото в долларах"},
	"GOLDM":   {"GOLDM", "Золото"},
	"CR":      {"CNY", "CNYRUB", "CNY/RUB"},
	"CNYRUBF": {"CNYRUBF", "CNYRUBTOM", "CNYRUB", "CNY/RUB"},
	"GL":      {"GL", "GLDRUB", "Золото в рублях"},
	"GLDRUBF": {"GLDRUBF", "GLDRUBTOM", "GLDRUB", "Золото в рублях"},
	"SBER":    {"SBRF", "SBER"},
	"SBERP":   {"SBPR", "SBERP"},
	"TATN":    {"TATN"},
	"TATNP":   {"TATP", "TATNP"},
}

func futureTickerProduct(ticker string) string {
	if ticker == "CNYRUBF" || ticker == "GLDRUBF" {
		return ticker
	}
	// Supported dated products use a two-character code, an expiry month code,
	// and a year digit. Prefix matches must not admit perpetuals or other names.
	if len(ticker) != 4 || !strings.ContainsRune("FGHJKMNQUVXZ", rune(ticker[2])) || ticker[3] < '0' || ticker[3] > '9' {
		return ""
	}
	return map[string]string{
		"RB": "RGBI", "KK": "KEYRATE", "RF": "RUONIA",
		"MX": "MX", "MM": "MM", "Si": "Si", "UM": "USDM",
		"GD": "GOLD", "GN": "GOLDM", "CR": "CR", "GL": "GL",
		"SR": "SBER", "SP": "SBERP", "TT": "TATN", "TP": "TATNP",
	}[ticker[:2]]
}

// This classifies identity only. Callers still verify exchange, settlement,
// asset type, currency, units, permissions, dates, margins and executable books.
func futureProduct(f *pb.Future) string {
	product := futureTickerProduct(f.GetTicker())
	if product != "" && slices.Contains(futureUnderlyingAliases[product], f.GetBasicAsset()) {
		return product
	}
	return ""
}

func curveFutureFamily(f *pb.Future) string {
	product := futureProduct(f)
	if curveFamily(product) {
		return product
	}
	return ""
}
