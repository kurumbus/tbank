// Package curve models paper relative-value positions. Signals are conditional
// convergence scenarios, never contractual arbitrage or broker order placement.
package curve

import (
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

const (
	OFZRGBI         = "ofz_rgbi"
	KEYRATECalendar = "keyrate_calendar"
	RUONIACalendar  = "ruonia_calendar"
	RGBICalendar    = "rgbi_calendar"
)

type Instrument struct {
	UID                 string          `json:"uid"`
	Ticker              string          `json:"ticker"`
	ClassCode           string          `json:"class_code"`
	Kind                string          `json:"kind"`
	Family              string          `json:"family"`
	Currency            string          `json:"currency"`
	Lot                 int64           `json:"lot"`
	PriceFactor         decimal.Decimal `json:"price_factor"`
	UnitSize            decimal.Decimal `json:"unit_size"`
	PriceCurrency       string          `json:"price_currency,omitempty"`
	PriceFactorKnown    bool            `json:"price_factor_known"`
	PriceFactorAt       time.Time       `json:"price_factor_at,omitempty"`
	PriceFactorSource   string          `json:"price_factor_source,omitempty"`
	Perpetual           bool            `json:"perpetual"`
	SettlementReference string          `json:"settlement_reference,omitempty"`
	UnderlyingUID       string          `json:"underlying_uid,omitempty"`
	SettlementType      string          `json:"settlement_type,omitempty"`
	Expiry              time.Time       `json:"expiry,omitempty"`
	LastTrade           time.Time       `json:"last_trade,omitempty"`
	APIAvailable        bool            `json:"api_available"`
	BuyAvailable        bool            `json:"buy_available"`
	SellAvailable       bool            `json:"sell_available"`
	ShortEnabled        bool            `json:"short_enabled"`
	QualifiedOnly       bool            `json:"qualified_only"`
	InitialMarginBuy    decimal.Decimal `json:"initial_margin_buy_rub"`
	InitialMarginSell   decimal.Decimal `json:"initial_margin_sell_rub"`
	ExchangeFee         decimal.Decimal `json:"exchange_fee_rub"`
	ExchangeFeeKnown    bool            `json:"exchange_fee_known"`
	ExchangeFeeSource   string          `json:"exchange_fee_source,omitempty"`
	Bond                *BondTerms      `json:"bond,omitempty"`
}

type RGBIReference struct {
	ModifiedDuration decimal.Decimal `json:"modified_duration_years"`
	MacaulayDays     decimal.Decimal `json:"macaulay_days"`
	YieldPercent     decimal.Decimal `json:"yield_percent"`
	Method           string          `json:"method"`
	AsOf             time.Time       `json:"as_of"`
	Source           string          `json:"source"`
	Known            bool            `json:"known"`
}

type Selection struct {
	BondTickers   []string
	FutureCount   int
	MaxFutureDays int
	Depth         int
	Tracked       []Instrument
}

type Snapshot struct {
	ObservedAt    time.Time               `json:"observed_at"`
	Instruments   []Instrument            `json:"instruments"`
	Books         map[string]scanner.Book `json:"books"`
	RGBIReference RGBIReference           `json:"rgbi_reference"`
	Warnings      []string                `json:"warnings,omitempty"`
	Relationships []Relationship          `json:"relationships,omitempty"`
}

type Leg struct {
	Instrument Instrument      `json:"instrument"`
	Side       string          `json:"side"`
	Lots       int64           `json:"lots"`
	Units      decimal.Decimal `json:"units"`
	Price      decimal.Decimal `json:"price"`
	Limit      decimal.Decimal `json:"limit"`
	Value      decimal.Decimal `json:"value_rub"`
	FeeValue   decimal.Decimal `json:"fee_notional_rub"`
	DV01       decimal.Decimal `json:"dv01_rub"`
	Execution  string          `json:"execution,omitempty"`
}

type LegPreview struct {
	UID           string          `json:"uid"`
	Status        string          `json:"status"`
	Message       string          `json:"message,omitempty"`
	Commission    decimal.Decimal `json:"commission_rub"`
	InitialMargin decimal.Decimal `json:"initial_margin_rub"`
}

type Metric struct {
	Key            string          `json:"key"`
	Strategy       string          `json:"strategy"`
	Unit           string          `json:"unit"`
	ModelSource    string          `json:"model_source"`
	InstrumentUIDs []string        `json:"instrument_uids"`
	ObservedAt     time.Time       `json:"observed_at"`
	QuoteTimes     []time.Time     `json:"quote_times"`
	Value          decimal.Decimal `json:"value"`
}

type Policy struct {
	BondLots             int64           `json:"bond_lots"`
	FutureContracts      int64           `json:"future_contracts"`
	CapitalLimit         decimal.Decimal `json:"capital_limit_rub"`
	CashRate             decimal.Decimal `json:"cash_rate_annual"`
	MarginBufferRate     decimal.Decimal `json:"margin_buffer_rate"`
	ExecutionBufferBPS   decimal.Decimal `json:"execution_buffer_bps"`
	CarryReserveRate     decimal.Decimal `json:"carry_uncertainty_annual"`
	FutureTurnover       decimal.Decimal `json:"assumed_prior_future_turnover_rub"`
	MinProfit            decimal.Decimal `json:"min_profit_rub"`
	MinExcessProfit      decimal.Decimal `json:"min_excess_profit_rub"`
	MinDeviationBPS      decimal.Decimal `json:"min_deviation_bps"`
	MaxResidualDV01Ratio decimal.Decimal `json:"max_residual_dv01_ratio"`
	AllowDurationProxy   bool            `json:"allow_duration_proxy"`
	MaxCurveRMSE         decimal.Decimal `json:"max_curve_rmse_fraction"`
	MaxCurveError        decimal.Decimal `json:"max_curve_error_fraction"`
	MinPeers             int             `json:"min_peers"`
	MinHistory           int             `json:"min_history"`
	MinHistorySpan       time.Duration   `json:"min_history_span"`
	HistoryMaxAge        time.Duration   `json:"history_max_age"`
	MaxQuoteAge          time.Duration   `json:"max_quote_age"`
	MaxQuoteSkew         time.Duration   `json:"max_quote_skew"`
	MaxReferenceAge      time.Duration   `json:"max_reference_age"`
	MaxHolding           time.Duration   `json:"max_holding"`
	ExpiryBuffer         time.Duration   `json:"expiry_buffer"`
}

func DefaultPolicy() Policy {
	d := decimal.RequireFromString
	return Policy{BondLots: 100, FutureContracts: 1, CapitalLimit: d("1000000"), CashRate: d("0.15"), MarginBufferRate: d("0.5"), ExecutionBufferBPS: d("10"), CarryReserveRate: d("0.05"), MinProfit: d("100"), MinExcessProfit: d("100"), MinDeviationBPS: d("5"), MaxResidualDV01Ratio: d("0.10"), MaxCurveRMSE: d("0.005"), MaxCurveError: d("0.01"), MinPeers: 4, MinHistory: 30, MinHistorySpan: 24 * time.Hour, HistoryMaxAge: 30 * 24 * time.Hour, MaxQuoteAge: 30 * time.Second, MaxQuoteSkew: 5 * time.Second, MaxReferenceAge: 96 * time.Hour, MaxHolding: 14 * 24 * time.Hour, ExpiryBuffer: 24 * time.Hour}
}

type Candidate struct {
	Key               string                       `json:"key"`
	Strategy          string                       `json:"strategy"`
	Direction         string                       `json:"direction"`
	Snapshot          Snapshot                     `json:"snapshot"`
	Policy            Policy                       `json:"policy"`
	Legs              []Leg                        `json:"legs"`
	Metric            Metric                       `json:"metric"`
	Target            decimal.Decimal              `json:"target"`
	TargetSource      string                       `json:"target_source"`
	ExpectedGross     decimal.Decimal              `json:"expected_gross_rub"`
	CapitalRequired   decimal.Decimal              `json:"capital_required_rub"`
	CashBenchmark     decimal.Decimal              `json:"cash_benchmark_rub"`
	ExecutionReserve  decimal.Decimal              `json:"execution_reserve_rub"`
	CarryReserve      decimal.Decimal              `json:"carry_reserve_rub"`
	ResidualDV01      decimal.Decimal              `json:"residual_dv01_rub"`
	ResidualDV01Ratio decimal.Decimal              `json:"residual_dv01_ratio"`
	DV01Known         bool                         `json:"dv01_known"`
	HedgeMeasure      string                       `json:"hedge_measure"`
	Horizon           time.Duration                `json:"scenario_horizon"`
	Scenarios         []scanner.Scenario           `json:"scenarios"`
	MinimumTariff     string                       `json:"minimum_tariff"`
	Warnings          []string                     `json:"warnings,omitempty"`
	Previews          []LegPreview                 `json:"previews,omitempty"`
	ReferenceUIDs     []string                     `json:"reference_uids,omitempty"`
	Relationship      *Relationship                `json:"relationship,omitempty"`
	ExtraCosts        map[string][]scanner.FeeLine `json:"extra_costs,omitempty"`
	ExpectedIncome    decimal.Decimal              `json:"expected_income_rub"`
}

type Scan struct {
	ObservedAt time.Time   `json:"observed_at"`
	Metrics    []Metric    `json:"metrics"`
	Candidates []Candidate `json:"candidates"`
	Skips      []string    `json:"skips,omitempty"`
}

type Position struct {
	ID           int64             `json:"id"`
	Source       string            `json:"source"`
	Status       string            `json:"status"`
	Entry        Candidate         `json:"entry"`
	Policy       Policy            `json:"policy"`
	ExitProfiles []scanner.Profile `json:"exit_profiles"`
	OpenedAt     time.Time         `json:"opened_at"`
	Latest       *Valuation        `json:"latest,omitempty"`
	ClosedAt     *time.Time        `json:"closed_at,omitempty"`
	CloseReason  string            `json:"close_reason,omitempty"`
}

type Valuation struct {
	ObservedAt       time.Time           `json:"observed_at"`
	Kind             string              `json:"kind"`
	Snapshot         Snapshot            `json:"snapshot"`
	Metric           Metric              `json:"metric"`
	Legs             []Leg               `json:"legs"`
	LegProfits       []decimal.Decimal   `json:"leg_profits_rub,omitempty"`
	GrossProfit      decimal.Decimal     `json:"gross_profit_rub"`
	CouponIncome     decimal.Decimal     `json:"coupon_income_rub"`
	CashIncome       decimal.Decimal     `json:"cash_income_rub"`
	FundingIncome    decimal.Decimal     `json:"funding_income_rub"`
	Settlement       *RelativeSettlement `json:"settlement,omitempty"`
	CashBenchmark    decimal.Decimal     `json:"cash_benchmark_rub"`
	ExecutionReserve decimal.Decimal     `json:"execution_reserve_rub"`
	CarryReserve     decimal.Decimal     `json:"carry_reserve_rub"`
	Scenarios        []scanner.Scenario  `json:"scenarios"`
	ShouldClose      bool                `json:"should_close"`
	CloseReason      string              `json:"close_reason,omitempty"`
	Warnings         []string            `json:"warnings,omitempty"`
}
