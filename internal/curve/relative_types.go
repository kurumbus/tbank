package curve

import (
	"time"

	"github.com/shopspring/decimal"
)

const (
	StandardMini   = "standard_mini"
	ShareClass     = "share_class"
	PerpetualDated = "perpetual_dated"
	HoldingStub    = "holding_stub"
	FundNAV        = "fund_nav"
	CashOffer      = "cash_offer"
)

func IsRelativeStrategy(strategy string) bool {
	switch strategy {
	case StandardMini, ShareClass, PerpetualDated, HoldingStub, FundNAV, CashOffer:
		return true
	}
	return false
}

// Relationship contains sourced economic identities and references. Selectors
// are configuration inputs; the provider resolves UIDs before evaluation.
type Relationship struct {
	ID                     string            `json:"id"`
	Strategy               string            `json:"strategy"`
	PrimarySelector        string            `json:"primary_selector,omitempty"`
	HedgeSelector          string            `json:"hedge_selector,omitempty"`
	PrimaryUID             string            `json:"primary_uid"`
	HedgeUID               string            `json:"hedge_uid,omitempty"`
	PrimaryLots            int64             `json:"primary_lots"`
	HedgeLotsPerPrimaryLot decimal.Decimal   `json:"hedge_lots_per_primary_lot"`
	PrimaryPriceScale      decimal.Decimal   `json:"primary_price_scale"`
	HedgePriceScale        decimal.Decimal   `json:"hedge_price_scale"`
	Verified               bool              `json:"verified"`
	Source                 string            `json:"source"`
	AsOf                   time.Time         `json:"as_of"`
	ValidUntil             time.Time         `json:"valid_until"`
	Funding                *FundingReference `json:"funding,omitempty"`
	Holding                *HoldingReference `json:"holding,omitempty"`
	Fund                   *FundReference    `json:"fund,omitempty"`
	Offer                  *OfferReference   `json:"offer,omitempty"`
	Borrow                 *BorrowReference  `json:"borrow,omitempty"`
	Dividends              []DividendEvent   `json:"dividends,omitempty"`
}

type RelativeSelection struct {
	Relationships        []Relationship
	Tracked              []Instrument
	TrackedRelationships []Relationship
	HistoryFrom          time.Time
	Depth                int
	MaxPairsPerFamily    int
	MaxDays              int
}

// Funding amounts are signed RUB cash amounts for ONE LONG perpetual contract.
// Coverage explicitly includes intervals without events; an empty events slice
// without verified coverage must never be interpreted as free funding.
type FundingReference struct {
	InstrumentUID string         `json:"instrument_uid"`
	Known         bool           `json:"known"`
	Source        string         `json:"source"`
	CoverageStart time.Time      `json:"coverage_start"`
	CoverageEnd   time.Time      `json:"coverage_end"`
	Events        []FundingEvent `json:"events"`
}

type FundingEvent struct {
	At                    time.Time       `json:"at"`
	DateOnly              bool            `json:"date_only"`
	AmountPerLongContract decimal.Decimal `json:"amount_per_long_contract_rub"`
	Reference             string          `json:"reference"`
}
