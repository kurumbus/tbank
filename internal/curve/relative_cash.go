package curve

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

// BorrowTier describes the published fee for the entire uncovered cash-short
// balance in a band. UpToRUB is inclusive; zero is permitted only for the final,
// unlimited band. DailyRate is a documented daily fraction, not an assumed APR.
type BorrowTier struct {
	UpToRUB     decimal.Decimal `json:"up_to_rub"`
	DailyFeeRUB decimal.Decimal `json:"daily_fee_rub"`
	DailyRate   decimal.Decimal `json:"daily_rate"`
}

type BorrowReference struct {
	Verified               bool                    `json:"verified"`
	AsOf                   time.Time               `json:"as_of"`
	ValidUntil             time.Time               `json:"valid_until"`
	Source                 string                  `json:"source"`
	ExistingShortRUB       decimal.Decimal         `json:"existing_short_rub"`
	ExistingBalanceKnown   bool                    `json:"existing_balance_known"`
	ExistingBalanceAssumed bool                    `json:"existing_balance_assumed"`
	BalanceAsOf            time.Time               `json:"balance_as_of"`
	FreeTierEligibleKnown  bool                    `json:"free_tier_eligible_known"`
	FreeTierEligible       bool                    `json:"free_tier_eligible"`
	ProfileTiers           map[string][]BorrowTier `json:"profile_tiers"`
	ChargesKnown           bool                    `json:"charges_known"`
	ChargesFrom            time.Time               `json:"charges_from"`
	ChargesThrough         time.Time               `json:"charges_through"`
	ChargesSource          string                  `json:"charges_source"`
	Charges                []BorrowChargeEvent     `json:"charges,omitempty"`
}

type BorrowChargeEvent struct {
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Profile string          `json:"profile"`
	Amount  decimal.Decimal `json:"amount_rub"`
}

// A dividend is credited only after payment and when this paper holding spans
// its verified entitlement date. NetPerUnit is required for a long receipt;
// a short owes GrossPerUnit without using the long investor's tax deduction.
type DividendEvent struct {
	InstrumentUID string          `json:"instrument_uid"`
	RecordDate    time.Time       `json:"record_date"`
	PayDate       time.Time       `json:"pay_date"`
	GrossPerUnit  decimal.Decimal `json:"gross_per_unit_rub"`
	NetPerUnit    decimal.Decimal `json:"net_per_unit_rub"`
	NetKnown      bool            `json:"net_known"`
	Verified      bool            `json:"verified"`
	Source        string          `json:"source"`
}

type HoldingReference struct {
	SharesPerParent         decimal.Decimal  `json:"shares_per_parent"`
	RatioSource             string           `json:"ratio_source"`
	RatioAsOf               time.Time        `json:"ratio_as_of"`
	Borrow                  *BorrowReference `json:"borrow,omitempty"`
	DistributionsKnown      bool             `json:"distributions_known"`
	DistributionsAsOf       time.Time        `json:"distributions_as_of"`
	DistributionsValidUntil time.Time        `json:"distributions_valid_until"`
	DistributionsSource     string           `json:"distributions_source"`
	Dividends               []DividendEvent  `json:"dividends,omitempty"`
}

type FundReference struct {
	NAVPerUnitRUB           decimal.Decimal  `json:"nav_per_unit_rub"`
	NAVIsRUB                bool             `json:"nav_is_rub"`
	NAVAsOf                 time.Time        `json:"nav_as_of"`
	NAVSource               string           `json:"nav_source"`
	GoldGramsPerUnit        decimal.Decimal  `json:"gold_grams_per_unit"`
	GoldAsOf                time.Time        `json:"gold_as_of"`
	GoldValidUntil          time.Time        `json:"gold_valid_until"`
	GoldSource              string           `json:"gold_source"`
	GoldGramsPerContract    decimal.Decimal  `json:"gold_grams_per_contract"`
	ContractSource          string           `json:"contract_source"`
	FXRUBPerUSD             decimal.Decimal  `json:"fx_rub_per_usd"`
	FXAsOf                  time.Time        `json:"fx_as_of"`
	FXSource                string           `json:"fx_source"`
	ExpenseAnnualCap        decimal.Decimal  `json:"expense_annual_cap"`
	ExpenseKnown            bool             `json:"expense_known"`
	ExpenseSource           string           `json:"expense_source"`
	Borrow                  *BorrowReference `json:"borrow,omitempty"`
	DistributionsKnown      bool             `json:"distributions_known"`
	NoDistributions         bool             `json:"no_distributions"`
	DistributionsAsOf       time.Time        `json:"distributions_as_of"`
	DistributionsValidUntil time.Time        `json:"distributions_valid_until"`
	DistributionsSource     string           `json:"distributions_source"`
	Dividends               []DividendEvent  `json:"dividends,omitempty"`
}

// BrokerSupportsTender establishes feasibility only. Neither this reference nor
// a paper candidate is evidence that a tender instruction has been submitted.
type OfferReference struct {
	OfferPriceRUB           decimal.Decimal `json:"offer_price_rub"`
	PriceSource             string          `json:"price_source"`
	AcceptanceDeadline      time.Time       `json:"acceptance_deadline"`
	BrokerCutoff            time.Time       `json:"broker_cutoff"`
	BrokerCutoffVerified    bool            `json:"broker_cutoff_verified"`
	BrokerSupportsTender    bool            `json:"broker_supports_tender"`
	BrokerSource            string          `json:"broker_source"`
	EligibleSecurity        bool            `json:"eligible_security"`
	EligibilitySource       string          `json:"eligibility_source"`
	PayoutDate              time.Time       `json:"payout_date"`
	PayoutSource            string          `json:"payout_source"`
	TerminalFeeKnown        bool            `json:"terminal_fee_known"`
	TerminalFeeRUB          decimal.Decimal `json:"terminal_fee_rub"`
	TerminalFeeRate         decimal.Decimal `json:"terminal_fee_rate"`
	TerminalFeeSource       string          `json:"terminal_fee_source"`
	DistributionsKnown      bool            `json:"distributions_known"`
	DistributionsAsOf       time.Time       `json:"distributions_as_of"`
	DistributionsValidUntil time.Time       `json:"distributions_valid_until"`
	DistributionsSource     string          `json:"distributions_source"`
	Dividends               []DividendEvent `json:"dividends,omitempty"`
}

// evaluateCashRelationships retains priced monitors with explicit blocked
// eligibility when execution facts are incomplete. Unknown NAV is never zero.
func evaluateCashRelationships(s Snapshot, p Policy, history []Metric) Scan {
	out := Scan{ObservedAt: s.ObservedAt}
	for _, r := range s.Relationships {
		if r.Strategy != HoldingStub && r.Strategy != FundNAV && r.Strategy != CashOffer {
			continue
		}
		m, err := cashRelationshipMetric(s, p, r)
		if err != nil {
			out.Skips = append(out.Skips, r.ID+": "+err.Error())
			continue
		}
		out.Metrics = append(out.Metrics, m)
		target, source := decimal.Zero, "verified fund NAV; conditional discount normalization"
		if r.Strategy == HoldingStub {
			target, source, err = historyTarget(m, history, p)
			if err != nil {
				out.Skips = append(out.Skips, r.ID+": "+err.Error())
				continue
			}
		}
		directions := []bool{true, false}
		if r.Strategy == CashOffer {
			directions = []bool{true}
			target, source = r.Offer.OfferPriceRUB, r.Offer.PriceSource
		}
		for _, longPrimary := range directions {
			c, e := cashCandidate(s, p, r, m, target, source, longPrimary)
			if e != nil {
				out.Skips = append(out.Skips, r.ID+": "+e.Error())
				continue
			}
			out.Candidates = append(out.Candidates, c)
		}
	}
	return out
}

func cashReferenceValid(s Snapshot, r Relationship) error {
	if r.ID == "" || !r.Verified || strings.TrimSpace(r.Source) == "" || r.AsOf.IsZero() || r.AsOf.After(s.ObservedAt) || r.ValidUntil.Before(s.ObservedAt) {
		return fmt.Errorf("verified, sourced relationship facts within their validity dates are required")
	}
	return nil
}

func cashFindInstrument(s Snapshot, uid string) (Instrument, error) {
	for _, i := range s.Instruments {
		if i.UID == uid && uid != "" {
			return i, nil
		}
	}
	return Instrument{}, fmt.Errorf("relationship instrument %q is unavailable", uid)
}

// The holding metric deliberately retains the non-aluminium residual business.
// Its historical target is comparable only under the same ownership facts.
func cashRelationshipMetric(s Snapshot, p Policy, r Relationship) (Metric, error) {
	if err := cashReferenceValid(s, r); err != nil {
		return Metric{}, err
	}
	primary, err := cashFindInstrument(s, r.PrimaryUID)
	if err != nil {
		return Metric{}, err
	}
	if primary.Kind != "share" && primary.Kind != "etf" {
		return Metric{}, fmt.Errorf("cash relationship primary must be a share or fund")
	}
	pm, err := mid(s, p, primary)
	if err != nil {
		return Metric{}, err
	}
	pm = pm.Mul(primary.PriceFactor)
	m := Metric{Key: r.Strategy + ":" + r.ID + ":" + primary.UID, Strategy: r.Strategy, ObservedAt: s.ObservedAt, InstrumentUIDs: []string{primary.UID}, QuoteTimes: []time.Time{s.Books[primary.UID].Time}}
	var fingerprint string
	switch r.Strategy {
	case HoldingStub:
		if r.Holding == nil || !r.Holding.SharesPerParent.IsPositive() || r.Holding.RatioSource == "" || r.Holding.RatioAsOf.IsZero() || r.Holding.RatioAsOf.After(s.ObservedAt) {
			return Metric{}, fmt.Errorf("verified parent/subsidiary share-count ratio required")
		}
		hedge, e := cashFindInstrument(s, r.HedgeUID)
		if e != nil || hedge.Kind != "share" {
			return Metric{}, fmt.Errorf("subsidiary share instrument unavailable")
		}
		hm, e := mid(s, p, hedge)
		if e != nil {
			return Metric{}, e
		}
		if e = checkSkew(s, p, primary.UID, hedge.UID); e != nil {
			return Metric{}, e
		}
		m.Key += ":" + hedge.UID
		m.InstrumentUIDs = append(m.InstrumentUIDs, hedge.UID)
		m.QuoteTimes = append(m.QuoteTimes, s.Books[hedge.UID].Time)
		m.Unit, m.Value = "rub_per_parent_share", pm.Sub(hm.Mul(hedge.PriceFactor).Mul(r.Holding.SharesPerParent))
		fingerprint = r.Holding.SharesPerParent.String() + "|" + r.Holding.RatioSource + "|" + r.Holding.RatioAsOf.UTC().Format(time.RFC3339Nano)
	case FundNAV:
		if err := validateFundReference(s, p, r.Fund); err != nil {
			return Metric{}, err
		}
		hedge, e := cashFindInstrument(s, r.HedgeUID)
		if e != nil || hedge.Kind != "future" || hedge.Family != "GL" {
			return Metric{}, fmt.Errorf("dated RUB gold GL hedge required")
		}
		if hedge.Perpetual || !hedge.UnitSize.Equal(r.Fund.GoldGramsPerContract) || !hedge.PriceFactor.Equal(one) || !strings.EqualFold(hedge.PriceCurrency, "rub") {
			return Metric{}, fmt.Errorf("GL contract metadata must confirm one gram and one RUB per quote point")
		}
		if _, e = mid(s, p, hedge); e != nil {
			return Metric{}, e
		}
		if e = checkSkew(s, p, primary.UID, hedge.UID); e != nil {
			return Metric{}, e
		}
		m.Key += ":" + hedge.UID
		m.InstrumentUIDs = append(m.InstrumentUIDs, hedge.UID)
		m.QuoteTimes = append(m.QuoteTimes, s.Books[hedge.UID].Time)
		m.Unit, m.Value = "fraction", pm.Div(r.Fund.NAVPerUnitRUB).Sub(one)
		fingerprint = r.Fund.NAVSource + "|" + r.Fund.GoldSource + "|" + r.Fund.ContractSource + "|rub_nav_physical_gold_grams_v1"
	case CashOffer:
		if r.Offer == nil || !r.Offer.OfferPriceRUB.IsPositive() || r.Offer.PriceSource == "" {
			return Metric{}, fmt.Errorf("verified positive cash offer price required")
		}
		m.Unit, m.Value = "rub_per_share", pm
		offerTerms := *r.Offer
		offerTerms.DistributionsKnown = false
		offerTerms.DistributionsAsOf, offerTerms.DistributionsValidUntil = time.Time{}, time.Time{}
		offerTerms.DistributionsSource, offerTerms.Dividends = "", nil
		terms, _ := json.Marshal(offerTerms)
		fingerprint = string(terms) + "|" + r.ID
	default:
		return Metric{}, fmt.Errorf("unsupported cash relationship")
	}
	sum := sha256.Sum256([]byte(r.ID + "|" + r.Source + "|" + fingerprint))
	m.ModelSource = fmt.Sprintf("%s_v1_%x", r.Strategy, sum[:12])
	return m, nil
}

func validateFundReference(s Snapshot, p Policy, f *FundReference) error {
	if f == nil || !f.NAVPerUnitRUB.IsPositive() || !f.GoldGramsPerUnit.IsPositive() || !f.GoldGramsPerContract.Equal(one) || f.NAVSource == "" || f.GoldSource == "" || f.ContractSource == "" {
		return fmt.Errorf("sourced positive RUB NAV, gold grams per unit, one-gram GL contract and FX reference required")
	}
	timestamps := []time.Time{f.NAVAsOf}
	if !f.NAVIsRUB {
		if !f.FXRUBPerUSD.IsPositive() || f.FXSource == "" {
			return fmt.Errorf("USD-origin NAV requires a sourced positive FX conversion")
		}
		timestamps = append(timestamps, f.FXAsOf)
	}
	for _, at := range timestamps {
		if at.IsZero() || at.After(s.ObservedAt) || s.ObservedAt.Sub(at) > p.MaxReferenceAge {
			return fmt.Errorf("fund NAV, holdings or FX reference is stale or missing")
		}
	}
	if !f.NAVIsRUB && f.NAVAsOf.UTC().Format("2006-01-02") != f.FXAsOf.UTC().Format("2006-01-02") {
		return fmt.Errorf("fund NAV and its FX conversion must use the same dated observation")
	}
	if f.GoldAsOf.IsZero() || f.GoldAsOf.After(s.ObservedAt) || f.GoldValidUntil.Before(s.ObservedAt) {
		return fmt.Errorf("dated gold holdings report requires an explicit unexpired review date")
	}
	if !f.ExpenseKnown || f.ExpenseSource == "" || f.ExpenseAnnualCap.IsNegative() || f.ExpenseAnnualCap.GreaterThan(one) {
		return fmt.Errorf("sourced fund annual expense cap is required")
	}
	return nil
}

// A closed-day NAV can remain visible as a monitor, but is not a current
// valuation target. Entry and target-driven exits must both use this check.
func validateFundNAVAlignment(s Snapshot, p Policy, r Relationship) error {
	if r.Fund == nil {
		return fmt.Errorf("fund NAV is missing")
	}
	if r.Fund.NAVAsOf.IsZero() || r.Fund.NAVAsOf.After(s.ObservedAt) || s.ObservedAt.Sub(r.Fund.NAVAsOf) > p.MaxQuoteAge {
		return fmt.Errorf("fund indicative NAV is stale for an executable target")
	}
	for _, uid := range []string{r.PrimaryUID, r.HedgeUID} {
		book, ok := s.Books[uid]
		if !ok || book.Time.IsZero() {
			return fmt.Errorf("fund target lacks synchronized leg quotes")
		}
		delta := r.Fund.NAVAsOf.Sub(book.Time)
		if delta < 0 {
			delta = -delta
		}
		if delta > p.MaxQuoteSkew {
			return fmt.Errorf("fund indicative NAV is not synchronized with traded quotes")
		}
	}
	return nil
}

func validateCashEntryAt(c Candidate, now time.Time) error {
	if c.Relationship == nil {
		return fmt.Errorf("cash relationship reference missing")
	}
	s := c.Snapshot
	s.ObservedAt = now
	r := *c.Relationship
	if err := cashReferenceValid(s, r); err != nil {
		return err
	}
	if r.Strategy == FundNAV {
		if err := validateFundReference(s, c.Policy, r.Fund); err != nil {
			return err
		}
		if err := validateFundNAVAlignment(s, c.Policy, r); err != nil {
			return err
		}
	}
	if r.Strategy == CashOffer {
		if err := validateOffer(s, c.Policy, r); err != nil {
			return err
		}
		c.Horizon = r.Offer.PayoutDate.Sub(now)
	}
	c.Snapshot = s
	_, _, err := cashForecastAdjustments(c)
	return err
}

func cashCandidate(s Snapshot, p Policy, r Relationship, m Metric, target decimal.Decimal, targetSource string, longPrimary bool) (Candidate, error) {
	if r.PrimaryLots <= 0 {
		return Candidate{}, fmt.Errorf("positive relationship primary_lots required")
	}
	primary, _ := cashFindInstrument(s, r.PrimaryUID)
	primarySide, direction := "buy", "long_primary_short_hedge"
	if !longPrimary {
		primarySide, direction = "sell", "short_primary_long_hedge"
	}
	l1, err := fill(s, p, primary, primarySide, r.PrimaryLots, decimal.Zero)
	if err != nil {
		return Candidate{}, err
	}
	c := Candidate{Key: m.Key, Strategy: r.Strategy, Direction: direction, Snapshot: s, Policy: p, Legs: []Leg{l1}, Metric: m, Target: target, TargetSource: targetSource, Relationship: &r, Horizon: p.MaxHolding, ExtraCosts: map[string][]scanner.FeeLine{}}
	var exits []Leg
	var blockers []string
	if r.Strategy == CashOffer {
		c.Direction, c.HedgeMeasure = "long_share_cash_offer", "unhedged_event_risk"
		c.Horizon = r.Offer.PayoutDate.Sub(s.ObservedAt)
		if c.Horizon <= 0 {
			c.Horizon = p.MaxHolding
		}
		if err := validateOffer(s, p, r); err != nil {
			blockers = append(blockers, err.Error())
		}
		exit := l1
		exit.Side, exit.Execution = "sell", "cash_offer"
		exit.Price, exit.Limit = r.Offer.OfferPriceRUB.Div(primary.PriceFactor), r.Offer.OfferPriceRUB.Div(primary.PriceFactor)
		exit.Value, exit.FeeValue = r.Offer.OfferPriceRUB.Mul(l1.Units), decimal.Zero
		exits = []Leg{exit}
		terminal := r.Offer.TerminalFeeRUB.Add(exit.Value.Mul(r.Offer.TerminalFeeRate))
		addCashCost(c.ExtraCosts, "cash_offer_terminal_fee", terminal)
		c.Warnings = append(c.Warnings, "Single-leg corporate-event exposure; no market hedge. Payment depends on valid eligibility, broker processing and completion of the offer.", "This models a paper cash payment only; no tender instruction is submitted or presumed accepted.")
	} else {
		hedge, _ := cashFindInstrument(s, r.HedgeUID)
		var hedgeLots, shift decimal.Decimal
		if r.Strategy == HoldingStub {
			wanted := l1.Units.Mul(r.Holding.SharesPerParent)
			hedgeLots = wanted.Div(decimal.NewFromInt(hedge.Lot))
			shift = target.Sub(m.Value).Div(primary.PriceFactor)
			c.HedgeMeasure = "subsidiary_share_count"
			c.Warnings = append(c.Warnings, "The holding-company stub includes the power business, debt, taxes and holding discount. Target is a prior historical median, not zero or asset parity.")
		} else {
			hedgeLots = l1.Units.Mul(r.Fund.GoldGramsPerUnit).Div(r.Fund.GoldGramsPerContract)
			pm, _ := mid(s, p, primary)
			shift = r.Fund.NAVPerUnitRUB.Div(primary.PriceFactor).Sub(pm)
			c.HedgeMeasure = "physical_gold_grams"
			if err := validateFundNAVAlignment(s, p, r); err != nil {
				blockers = append(blockers, "dated NAV monitor only: "+err.Error()+"; a verified current indicative NAV or sourced roll-forward is required")
			}
			remaining := hedge.LastTrade.Sub(s.ObservedAt) - p.ExpiryBuffer
			if remaining < c.Horizon {
				c.Horizon = remaining
			}
			if c.Horizon <= 0 {
				return Candidate{}, fmt.Errorf("gold future has insufficient time before expiry buffer")
			}
			if longPrimary {
				addCashCost(c.ExtraCosts, "forecast_fund_expense_drift", r.Fund.NAVPerUnitRUB.Mul(l1.Units).Mul(r.Fund.ExpenseAnnualCap).Mul(years(c.Horizon)))
			}
			c.Warnings = append(c.Warnings, "Discount-to-NAV normalization is conditional; the fund and retail investor have no guaranteed redemption arbitrage. Gold hedge leaves cash, tracking, expenses and futures basis risk.", "The GL hedge is one gram quoted in RUB. USD-origin NAV needs matching dated FX; an officially RUB-denominated indicative NAV requires no extra currency multiplier.")
		}
		quantity, e := integerQuantity(hedgeLots)
		if e != nil {
			return Candidate{}, e
		}
		c.ResidualDV01Ratio = decimal.NewFromInt(quantity).Sub(hedgeLots).Abs().Div(hedgeLots)
		if c.ResidualDV01Ratio.GreaterThan(p.MaxResidualDV01Ratio) {
			return Candidate{}, fmt.Errorf("rounded share/gram hedge mismatch exceeds configured limit")
		}
		l2, e := fill(s, p, hedge, opposite(primarySide), quantity, decimal.Zero)
		if e != nil {
			return Candidate{}, e
		}
		c.Legs = append(c.Legs, l2)
		e1, e := closeFill(s, p, l1, shift)
		if e != nil {
			return Candidate{}, e
		}
		e2, e := closeFill(s, p, l2, decimal.Zero)
		if e != nil {
			return Candidate{}, e
		}
		exits = []Leg{e1, e2}
		c.Warnings = append(c.Warnings, "Scenario keeps hedge price and order-book shape unchanged while the primary moves to target; all entry and exit spreads and commissions apply.")
	}
	income, costs, err := cashForecastAdjustments(c)
	if err != nil {
		blockers = append(blockers, err.Error())
	} else {
		c.ExpectedIncome = income
		mergeCashCosts(c.ExtraCosts, costs)
	}
	if facts := cashFacts(r); facts.borrow != nil {
		c.Warnings = append(c.Warnings, "Borrow forecast charges the full published calendar-day tier on the candidate plus existing uncovered balance; it is a conservative paper estimate, not a broker charge or a flat annual rate.")
		if facts.borrow.ExistingBalanceAssumed {
			c.Warnings = append(c.Warnings, "The pre-existing uncovered account balance is an explicit paper assumption; production account borrowing may differ.")
		}
	}
	if r.Strategy == FundNAV {
		c.Warnings = append(c.Warnings, "Gold exposure comes from the dated holdings report and is accepted only until its configured review date; this is an approximation between published reports.")
	}
	c.Policy.MinDeviationBPS = decimal.Zero
	c, err = priceCandidate(c, exits)
	c.Policy = p
	if err != nil {
		return Candidate{}, err
	}
	// The common threshold is in basis points; these strategy metrics are RUB
	// or fractions, so apply an economically comparable deviation explicitly.
	deviationBPS := m.Value.Sub(target).Abs().Mul(bps)
	if r.Strategy != FundNAV {
		pm, _ := mid(s, p, primary)
		deviationBPS = deviationBPS.Div(pm.Mul(primary.PriceFactor))
	}
	if deviationBPS.LessThan(p.MinDeviationBPS) {
		blockers = append(blockers, "normalized deviation is below minimum basis-point threshold")
	}
	if len(blockers) > 0 {
		blockCashCandidate(&c, blockers...)
	}
	return c, nil
}

func validateOffer(s Snapshot, p Policy, r Relationship) error {
	f := r.Offer
	if f == nil || !f.OfferPriceRUB.IsPositive() || f.PriceSource == "" || f.AcceptanceDeadline.IsZero() || !f.AcceptanceDeadline.After(s.ObservedAt) || f.BrokerCutoff.IsZero() || !f.BrokerCutoff.After(s.ObservedAt) || f.BrokerCutoff.After(f.AcceptanceDeadline) || !f.BrokerCutoffVerified || !f.BrokerSupportsTender || f.BrokerSource == "" || !f.EligibleSecurity || f.EligibilitySource == "" {
		return fmt.Errorf("cash offer monitor only: current deadline, verified broker cutoff/support and share eligibility are required")
	}
	if !f.PayoutDate.After(f.AcceptanceDeadline) || f.PayoutSource == "" || f.PayoutDate.Sub(s.ObservedAt) > p.MaxHolding || f.PayoutDate.After(r.ValidUntil) {
		return fmt.Errorf("cash offer monitor only: verified payout date must lie within reference validity and maximum holding policy")
	}
	if !f.TerminalFeeKnown || f.TerminalFeeSource == "" || f.TerminalFeeRUB.IsNegative() || f.TerminalFeeRate.IsNegative() || f.TerminalFeeRate.GreaterThan(one) {
		return fmt.Errorf("cash offer monitor only: sourced terminal fee schedule is required")
	}
	return nil
}

func blockCashCandidate(c *Candidate, reasons ...string) {
	c.MinimumTariff = ""
	for n := range c.Scenarios {
		c.Scenarios[n].Qualifies = false
	}
	c.Warnings = append(c.Warnings, reasons...)
}

func addCashCost(costs map[string][]scanner.FeeLine, name string, amount decimal.Decimal) {
	for _, profile := range scanner.Profiles() {
		costs[profile.Name] = append(costs[profile.Name], scanner.FeeLine{Name: name, Amount: amount})
	}
}

func mergeCashCosts(dst, src map[string][]scanner.FeeLine) {
	for profile, lines := range src {
		dst[profile] = append(dst[profile], lines...)
	}
}

type cashDistributionFacts struct {
	known                bool
	asOf, until          time.Time
	source               string
	events               []DividendEvent
	borrow               *BorrowReference
	noDistributionPolicy bool
}

func cashFacts(r Relationship) cashDistributionFacts {
	switch {
	case r.Strategy == HoldingStub && r.Holding != nil:
		f := r.Holding
		return cashDistributionFacts{f.DistributionsKnown, f.DistributionsAsOf, f.DistributionsValidUntil, f.DistributionsSource, f.Dividends, f.Borrow, false}
	case r.Strategy == FundNAV && r.Fund != nil:
		f := r.Fund
		return cashDistributionFacts{f.DistributionsKnown, f.DistributionsAsOf, f.DistributionsValidUntil, f.DistributionsSource, f.Dividends, f.Borrow, f.NoDistributions}
	case r.Strategy == CashOffer && r.Offer != nil:
		f := r.Offer
		return cashDistributionFacts{f.DistributionsKnown, f.DistributionsAsOf, f.DistributionsValidUntil, f.DistributionsSource, f.Dividends, nil, false}
	}
	return cashDistributionFacts{}
}

func cashForecastAdjustments(c Candidate) (decimal.Decimal, map[string][]scanner.FeeLine, error) {
	facts := cashFacts(*c.Relationship)
	end := c.Snapshot.ObservedAt.Add(c.Horizon)
	income, costs, err := cashDistributions(facts, c.Legs, c.Snapshot.ObservedAt, end, c.Snapshot.ObservedAt, c.Policy)
	if err != nil {
		return decimal.Zero, nil, err
	}
	short := decimal.Zero
	for _, l := range c.Legs {
		if l.Side == "sell" && (l.Instrument.Kind == "share" || l.Instrument.Kind == "etf") {
			short = short.Add(l.Value)
		}
	}
	if short.IsZero() {
		return income, costs, nil
	}
	if err := validateBorrow(facts.borrow, c.Snapshot.ObservedAt, end, c.Policy); err != nil {
		return decimal.Zero, nil, err
	}
	days := decimal.NewFromInt(int64(c.Horizon / time.Second)).Div(decimal.NewFromInt(86400)).Ceil()
	for _, profile := range scanner.Profiles() {
		fee, err := cashBorrowDaily(facts.borrow, profile.Name, short)
		if err != nil {
			return decimal.Zero, nil, err
		}
		costs[profile.Name] = append(costs[profile.Name], scanner.FeeLine{Name: "forecast_borrow_calendar_days", Amount: fee.Mul(days)})
	}
	return income, costs, nil
}

func cashDistributions(f cashDistributionFacts, legs []Leg, from, through, observed time.Time, p Policy) (decimal.Decimal, map[string][]scanner.FeeLine, error) {
	costs := map[string][]scanner.FeeLine{}
	if !f.known || f.source == "" || f.asOf.IsZero() || f.asOf.After(observed) || (!f.noDistributionPolicy && observed.Sub(f.asOf) > p.MaxReferenceAge) || f.until.Before(through) {
		return decimal.Zero, nil, fmt.Errorf("current sourced announced-distribution calendar covering the holding interval is required")
	}
	if f.noDistributionPolicy && len(f.events) > 0 {
		return decimal.Zero, nil, fmt.Errorf("no-distribution fund policy conflicts with distribution events")
	}
	income := decimal.Zero
	seen := map[string]bool{}
	for _, event := range f.events {
		if !event.Verified || event.Source == "" || event.InstrumentUID == "" || event.RecordDate.IsZero() || event.PayDate.Before(event.RecordDate) || event.GrossPerUnit.IsNegative() || event.NetPerUnit.IsNegative() || event.NetPerUnit.GreaterThan(event.GrossPerUnit) {
			return decimal.Zero, nil, fmt.Errorf("distribution amount, entitlement/payment date or source is unverified")
		}
		id := cashDividendID(event)
		if seen[id] {
			return decimal.Zero, nil, fmt.Errorf("duplicate distribution event")
		}
		seen[id] = true
		held := false
		for _, l := range legs {
			if l.Instrument.UID == event.InstrumentUID {
				held = true
				break
			}
		}
		if !held {
			continue
		}
		if cashMoscowDay(event.RecordDate).Equal(cashMoscowDay(from)) {
			return decimal.Zero, nil, fmt.Errorf("same-record-day cash entry requires verified settlement/entitlement reconciliation")
		}
		if event.RecordDate.Before(from) || event.RecordDate.After(through) {
			continue
		}
		for _, l := range legs {
			if l.Instrument.UID != event.InstrumentUID {
				continue
			}
			if l.Side == "sell" {
				// Liability exists once the short spans entitlement, even if
				// reimbursement is paid after the mark/exit date.
				addCashCost(costs, "short_distribution_"+id, event.GrossPerUnit.Mul(l.Units))
			} else if event.PayDate.After(through) {
				return decimal.Zero, nil, fmt.Errorf("earned but unpaid long distribution requires receivable reconciliation before marking or closing")
			} else {
				if !event.NetKnown {
					return decimal.Zero, nil, fmt.Errorf("net long-distribution receipt is not verified")
				}
				income = income.Add(event.NetPerUnit.Mul(l.Units))
			}
		}
	}
	return income, costs, nil
}

func validateBorrow(b *BorrowReference, now, through time.Time, p Policy) error {
	if b == nil || !b.Verified || b.Source == "" || b.AsOf.IsZero() || b.AsOf.After(now) || b.ValidUntil.Before(through) || !b.ExistingBalanceKnown || b.ExistingShortRUB.IsNegative() {
		return fmt.Errorf("current sourced borrow tiers and known aggregate pre-existing uncovered balance are required")
	}
	if !b.ExistingBalanceAssumed {
		at := b.BalanceAsOf
		if at.IsZero() {
			at = b.AsOf
		}
		if at.After(now) || now.Sub(at) > p.MaxReferenceAge {
			return fmt.Errorf("existing uncovered-account balance needs a fresh observation or an explicit paper assumption")
		}
	}
	for _, profile := range scanner.Profiles() {
		tiers, ok := b.ProfileTiers[profile.Name]
		if !ok || len(tiers) == 0 {
			return fmt.Errorf("borrow tiers missing for %s", profile.Name)
		}
		previous := decimal.Zero
		for n, tier := range tiers {
			if tier.UpToRUB.IsNegative() || tier.DailyFeeRUB.IsNegative() || tier.DailyRate.IsNegative() || tier.DailyRate.GreaterThan(one) || (tier.UpToRUB.IsZero() && n != len(tiers)-1) || (!tier.UpToRUB.IsZero() && !tier.UpToRUB.GreaterThan(previous)) {
				return fmt.Errorf("invalid non-marginal daily borrow tier schedule")
			}
			previous = tier.UpToRUB
		}
		if !tiers[len(tiers)-1].UpToRUB.IsZero() {
			return fmt.Errorf("borrow schedule needs an explicit unlimited final band")
		}
	}
	return nil
}

func cashBorrowDaily(b *BorrowReference, profile string, short decimal.Decimal) (decimal.Decimal, error) {
	if b == nil || short.IsNegative() {
		return decimal.Zero, fmt.Errorf("invalid borrow balance")
	}
	balance := short.Add(b.ExistingShortRUB)
	for _, tier := range b.ProfileTiers[profile] {
		if tier.UpToRUB.IsZero() || balance.LessThanOrEqual(tier.UpToRUB) {
			fee := tier.DailyFeeRUB.Add(balance.Mul(tier.DailyRate))
			if fee.IsZero() && balance.IsPositive() {
				if !b.FreeTierEligibleKnown {
					return decimal.Zero, fmt.Errorf("free borrow tier requires known concurrent uncovered-account eligibility")
				}
				if !b.FreeTierEligible {
					fee = decimal.NewFromInt(5)
				}
			}
			return fee, nil
		}
	}
	return decimal.Zero, fmt.Errorf("no published borrow band covers balance for %s", profile)
}

// PublishedBorrowReference contains the 2026 schedules published in the three
// broker tariff PDFs. The caller must supply/review the existing uncovered
// account balance; zero is deliberately not marked known by this constructor.
// Fees are whole-account calendar-day bands, not marginal rates or annual APRs.
func PublishedBorrowReference(asOf time.Time) BorrowReference {
	ceilings := []string{"5000", "50000", "100000", "250000", "500000", "1000000", "2500000", "5000000", "10000000", "25000000", "50000000", "0"}
	fixed := map[string][]string{
		"investor": {"0", "40", "80", "190", "375", "750", "1850", "3700", "7200"},
		"trader":   {"0", "40", "75", "180", "350", "700", "1750", "3500", "6900"},
		"premium":  {"0", "35", "70", "175", "340", "680", "1700", "3400", "6800"},
	}
	rates := map[string][]string{"investor": {"0.00070", "0.00067", "0.00059"}, "trader": {"0.00068", "0.00065", "0.00057"}, "premium": {"0.00066", "0.00063", "0.00055"}}
	b := BorrowReference{Verified: true, AsOf: asOf, ValidUntil: asOf.AddDate(0, 1, 0), Source: "https://acdn.t-static.ru/static/documents/invest-tariff-fees.pdf ; https://acdn.t-static.ru/static/documents/invest-tariff-trader.pdf ; https://acdn.t-static.ru/static/documents/invest-tariff-premium.pdf (2026 editions; calendar-day whole uncovered-balance bands)", ProfileTiers: map[string][]BorrowTier{}}
	for _, profile := range scanner.Profiles() {
		for n, upper := range ceilings {
			tier := BorrowTier{UpToRUB: decimal.RequireFromString(upper)}
			if n < 9 {
				tier.DailyFeeRUB = decimal.RequireFromString(fixed[profile.Name][n])
			} else {
				tier.DailyRate = decimal.RequireFromString(rates[profile.Name][n-9])
			}
			b.ProfileTiers[profile.Name] = append(b.ProfileTiers[profile.Name], tier)
		}
	}
	return b
}

// CashPositionAdjustments computes cumulative distributions and dated paper
// borrowing accrual. Earlier intervals stay frozen; later prices never reprice
// the entire borrowing history. Explicit verified charge ledgers override the
// estimate. Fund expenses are already in marked NAV/price and are not recharged.
func CashPositionAdjustments(p Position, s Snapshot) (decimal.Decimal, map[string][]scanner.FeeLine, error) {
	if p.Entry.Relationship == nil {
		return decimal.Zero, map[string][]scanner.FeeLine{}, nil
	}
	frozen := *p.Entry.Relationship
	if frozen.Strategy != HoldingStub && frozen.Strategy != FundNAV && frozen.Strategy != CashOffer {
		return decimal.Zero, map[string][]scanner.FeeLine{}, nil
	}
	var current *Relationship
	for n := range s.Relationships {
		r := &s.Relationships[n]
		if r.ID == frozen.ID && r.Strategy == frozen.Strategy && r.PrimaryUID == frozen.PrimaryUID && r.HedgeUID == frozen.HedgeUID {
			if current != nil {
				return decimal.Zero, nil, fmt.Errorf("duplicate current cash relationship")
			}
			current = r
		}
	}
	if current == nil {
		return decimal.Zero, nil, fmt.Errorf("current distribution/borrow reference unavailable")
	}
	facts := cashFacts(*current)
	// Published APIs may stop returning paid events. Retain historical economic
	// facts from the frozen entry and last mark, and reject revisions to already
	// accrued events instead of erasing income or reimbursement liabilities.
	previousFacts := cashFacts(frozen)
	if p.Latest != nil {
		for _, r := range p.Latest.Snapshot.Relationships {
			if r.ID == frozen.ID {
				previousFacts = cashFacts(r)
				break
			}
		}
	}
	var err error
	facts.events, err = mergeCashDistributionHistory(facts.events, previousFacts.events, p.OpenedAt, s.ObservedAt)
	if err != nil {
		return decimal.Zero, nil, err
	}
	income, costs, err := cashDistributions(facts, p.Entry.Legs, p.OpenedAt, s.ObservedAt, s.ObservedAt, p.Policy)
	if err != nil {
		return decimal.Zero, nil, err
	}
	if p.Latest != nil {
		income = p.Latest.CashIncome
		for _, event := range facts.events {
			if !event.PayDate.After(p.Latest.ObservedAt) || event.PayDate.After(s.ObservedAt) || event.RecordDate.Before(p.OpenedAt) {
				continue
			}
			for _, l := range p.Entry.Legs {
				if l.Side == "buy" && l.Instrument.UID == event.InstrumentUID {
					income = income.Add(event.NetPerUnit.Mul(l.Units))
				}
			}
		}
		for _, scenario := range p.Latest.Scenarios {
			for _, prior := range scenario.Fees {
				if !strings.HasPrefix(prior.Name, "short_distribution_") {
					continue
				}
				found := false
				for _, current := range costs[scenario.Profile] {
					if current.Name != prior.Name {
						continue
					}
					found = true
					if !current.Amount.Equal(prior.Amount) {
						return decimal.Zero, nil, fmt.Errorf("previously accrued short distribution was revised; reconciliation required")
					}
				}
				if !found {
					costs[scenario.Profile] = append(costs[scenario.Profile], prior)
				}
			}
		}
	}
	previousAt := p.OpenedAt
	previousLegs := p.Entry.Legs
	if p.Latest != nil {
		previousAt, previousLegs = p.Latest.ObservedAt, p.Latest.Legs
	}
	previousShort, currentShort := decimal.Zero, decimal.Zero
	for _, l := range p.Entry.Legs {
		if l.Side != "sell" || (l.Instrument.Kind != "share" && l.Instrument.Kind != "etf") {
			continue
		}
		prior := l.Value
		for _, v := range previousLegs {
			if v.Instrument.UID == l.Instrument.UID {
				prior = v.Value
			}
		}
		previousShort = previousShort.Add(prior)
		i, e := cashFindInstrument(s, l.Instrument.UID)
		if e != nil {
			return decimal.Zero, nil, e
		}
		close, e := fill(s, p.Policy, i, "buy", l.Lots, decimal.Zero)
		if e != nil {
			return decimal.Zero, nil, e
		}
		currentShort = currentShort.Add(close.Value)
	}
	if previousShort.IsZero() && currentShort.IsZero() {
		return income, costs, nil
	}
	if facts.borrow != nil && facts.borrow.ChargesKnown {
		verified, err := cashVerifiedBorrow(facts.borrow, p.OpenedAt, s.ObservedAt)
		if err != nil {
			return decimal.Zero, nil, err
		}
		mergeCashCosts(costs, verified)
		return income, costs, nil
	}
	if err = validateBorrow(facts.borrow, s.ObservedAt, s.ObservedAt, p.Policy); err != nil {
		return decimal.Zero, nil, err
	}
	days := cashBorrowDays(previousAt, s.ObservedAt)
	if days < 0 {
		return decimal.Zero, nil, fmt.Errorf("borrow accrual observation moved backwards")
	}
	for _, profile := range scanner.Profiles() {
		prior := decimal.Zero
		if p.Latest != nil {
			for _, scenario := range p.Latest.Scenarios {
				if scenario.Profile != profile.Name {
					continue
				}
				for _, line := range scenario.Fees {
					if strings.HasPrefix(line.Name, "borrow_estimated_") || strings.HasPrefix(line.Name, "borrow_verified_") {
						prior = prior.Add(line.Amount)
					}
				}
			}
		}
		ref := *facts.borrow
		previousFacts := cashFacts(frozen)
		if p.Latest != nil {
			for _, r := range p.Latest.Snapshot.Relationships {
				if r.ID == frozen.ID {
					previousFacts = cashFacts(r)
					break
				}
			}
		}
		if previousFacts.borrow != nil {
			ref.ExistingShortRUB = decimal.Max(ref.ExistingShortRUB, previousFacts.borrow.ExistingShortRUB)
		}
		daily, e := cashBorrowDaily(&ref, profile.Name, decimal.Max(previousShort, currentShort))
		if e != nil {
			return decimal.Zero, nil, e
		}
		// If the interval straddled a published tariff revision, reserve the
		// larger endpoint fee rather than apply a lower new rate backwards.
		if previousFacts.borrow != nil {
			oldRef := *previousFacts.borrow
			oldRef.ExistingShortRUB = ref.ExistingShortRUB
			if oldFee, e := cashBorrowDaily(&oldRef, profile.Name, decimal.Max(previousShort, currentShort)); e == nil {
				daily = decimal.Max(daily, oldFee)
			}
		}
		costs[profile.Name] = append(costs[profile.Name], scanner.FeeLine{Name: "borrow_estimated_calendar_days", Amount: prior.Add(daily.Mul(decimal.NewFromInt(days)))})
	}
	return income, costs, nil
}

func cashBorrowDays(from, to time.Time) int64 {
	moscow := time.FixedZone("MSK", 3*60*60)
	a, b := from.In(moscow), to.In(moscow)
	a = time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	b = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int64(b.Sub(a) / (24 * time.Hour))
}

func cashMoscowDay(at time.Time) time.Time {
	t := at.In(time.FixedZone("MSK", 3*60*60))
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func cashDividendID(event DividendEvent) string {
	return event.InstrumentUID + "|" + event.RecordDate.UTC().Format(time.RFC3339Nano)
}

func mergeCashDistributionHistory(current, previous []DividendEvent, from, through time.Time) ([]DividendEvent, error) {
	result := append([]DividendEvent(nil), current...)
	seen := map[string]DividendEvent{}
	for _, e := range current {
		seen[cashDividendID(e)] = e
	}
	for _, old := range previous {
		if old.RecordDate.Before(from) || old.RecordDate.After(through) {
			continue
		}
		id := cashDividendID(old)
		if now, exists := seen[id]; exists {
			if !old.GrossPerUnit.Equal(now.GrossPerUnit) || (old.NetKnown && (!now.NetKnown || !old.NetPerUnit.Equal(now.NetPerUnit))) || !old.PayDate.Equal(now.PayDate) {
				return nil, fmt.Errorf("previously recorded distribution terms changed; reconciliation required")
			}
			continue
		}
		result = append(result, old)
		seen[id] = old
	}
	return result, nil
}

func cashVerifiedBorrow(b *BorrowReference, from, through time.Time) (map[string][]scanner.FeeLine, error) {
	if b.ChargesSource == "" || b.ChargesFrom.IsZero() || b.ChargesFrom.After(from) || b.ChargesThrough.Before(through) {
		return nil, fmt.Errorf("verified borrow ledger does not cover the full holding interval")
	}
	out := map[string][]scanner.FeeLine{}
	seen := map[string]bool{}
	for _, event := range b.Charges {
		key := event.Profile + ":" + event.ID
		if event.ID == "" || seen[key] || event.At.IsZero() || event.Amount.IsNegative() || (event.Profile != "investor" && event.Profile != "trader" && event.Profile != "premium") {
			return nil, fmt.Errorf("invalid or duplicate verified borrow charge")
		}
		seen[key] = true
		if !event.At.After(from) || event.At.After(through) {
			continue
		}
		out[event.Profile] = append(out[event.Profile], scanner.FeeLine{Name: "borrow_verified_" + event.ID, Amount: event.Amount})
	}
	return out, nil
}
