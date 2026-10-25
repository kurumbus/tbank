// Package scanner evaluates fully cash-funded stock/future carry candidates.
// It has no order-placement capability. All prices and money use decimals.
package scanner

import (
	"github.com/shopspring/decimal"
	"time"
)

type Instrument struct {
	UID                   string          `json:"uid"`
	Ticker                string          `json:"ticker"`
	ClassCode             string          `json:"class_code"`
	Kind                  string          `json:"kind"`
	Currency              string          `json:"currency"`
	Lot                   int64           `json:"lot"`
	PositionUID           string          `json:"position_uid,omitempty"`
	UnderlyingPositionUID string          `json:"underlying_position_uid,omitempty"`
	AssetSize             decimal.Decimal `json:"asset_size"`
	PriceFactor           decimal.Decimal `json:"price_factor"`
	Expiry                time.Time       `json:"expiry,omitempty"`
	LastTrade             time.Time       `json:"last_trade,omitempty"`
	Settlement            string          `json:"settlement,omitempty"`
	APIAvailable          bool            `json:"api_available"`
	BuyAvailable          bool            `json:"buy_available"`
	SellAvailable         bool            `json:"sell_available"`
	QualifiedOnly         bool            `json:"qualified_only"`
	InitialMargin         decimal.Decimal `json:"initial_margin"`
	ExchangeFee           decimal.Decimal `json:"exchange_fee_per_contract"`
	ExchangeFeeKnown      bool            `json:"exchange_fee_known"`
	ExchangeFeeSource     string          `json:"exchange_fee_source,omitempty"`
}

type Pair struct {
	Stock  Instrument `json:"stock"`
	Future Instrument `json:"future"`
}

type Level struct {
	Price decimal.Decimal `json:"price"`
	Lots  int64           `json:"lots"`
}

type Book struct {
	UID            string    `json:"uid"`
	Time           time.Time `json:"time"`
	Consistent     bool      `json:"consistent"`
	TradeAvailable bool      `json:"trade_available"`
	Bids           []Level   `json:"bids"`
	Asks           []Level   `json:"asks"`
}

type Snapshot struct {
	Pair       Pair      `json:"pair"`
	StockBook  Book      `json:"stock_book"`
	FutureBook Book      `json:"future_book"`
	ObservedAt time.Time `json:"observed_at"`
}

type Assumptions struct {
	FutureTurnover            decimal.Decimal `json:"assumed_prior_futures_turnover_rub"`
	Contracts                 int64           `json:"contracts"`
	CapitalLimit              decimal.Decimal `json:"capital_limit_rub"`
	CashRate                  decimal.Decimal `json:"cash_rate_annual"`
	MarginBufferRate          decimal.Decimal `json:"margin_buffer_rate"`
	ExecutionBufferBPS        decimal.Decimal `json:"execution_buffer_bps"`
	SettlementPriceBufferRate decimal.Decimal `json:"settlement_price_buffer_rate"`
	DividendPerShare          decimal.Decimal `json:"dividend_per_share_rub"`
	MinProfit                 decimal.Decimal `json:"min_profit_rub"`
	MinExcessProfit           decimal.Decimal `json:"min_excess_profit_rub"`
	MaxQuoteAge               time.Duration   `json:"max_quote_age"`
	MaxQuoteSkew              time.Duration   `json:"max_quote_skew"`
	MaxDays                   int             `json:"max_days"`
}

type FeeLine struct {
	Name   string          `json:"name"`
	Amount decimal.Decimal `json:"amount_rub"`
}

type Scenario struct {
	Profile          string          `json:"profile"`
	StockRate        decimal.Decimal `json:"stock_rate"`
	FutureRate       decimal.Decimal `json:"future_rate"`
	FeeSource        string          `json:"fee_source"`
	Fees             []FeeLine       `json:"fees"`
	TradingFees      decimal.Decimal `json:"trading_fees_rub"`
	NetProfit        decimal.Decimal `json:"net_profit_rub"`
	ExcessProfit     decimal.Decimal `json:"excess_profit_rub"`
	AnnualizedReturn decimal.Decimal `json:"annualized_return"`
	Qualifies        bool            `json:"qualifies"`
}

type Preview struct {
	Status                string          `json:"status"`
	ActualTariff          string          `json:"actual_tariff,omitempty"`
	StockCommission       decimal.Decimal `json:"stock_commission_rub"`
	FutureCommission      decimal.Decimal `json:"future_commission_rub"`
	RequiredInitialMargin decimal.Decimal `json:"required_initial_margin_rub"`
	Message               string          `json:"message,omitempty"`
}

type Result struct {
	Key             string          `json:"key"`
	Snapshot        Snapshot        `json:"snapshot"`
	Assumptions     Assumptions     `json:"assumptions"`
	Contracts       int64           `json:"contracts"`
	Shares          int64           `json:"shares"`
	StockLots       int64           `json:"stock_lots"`
	StockCost       decimal.Decimal `json:"stock_cost_rub"`
	FutureValue     decimal.Decimal `json:"future_value_rub"`
	StockLimit      decimal.Decimal `json:"stock_limit_price"`
	FutureLimit     decimal.Decimal `json:"future_limit_price"`
	GrossBasis      decimal.Decimal `json:"gross_basis_rub"`
	Dividends       decimal.Decimal `json:"dividends_rub"`
	InitialMargin   decimal.Decimal `json:"initial_margin_rub"`
	CapitalRequired decimal.Decimal `json:"capital_required_rub"`
	CashBenchmark   decimal.Decimal `json:"cash_benchmark_rub"`
	ExecutionBuffer decimal.Decimal `json:"execution_buffer_rub"`
	Days            decimal.Decimal `json:"days"`
	Scenarios       []Scenario      `json:"scenarios"`
	Preview         Preview         `json:"preview"`
	Warnings        []string        `json:"warnings,omitempty"`
}
