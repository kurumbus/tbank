package curve

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

// RelativeSettlement is an explicit observation of a fully accepted and paid
// cash offer. A published offer or a passed cutoff is not payment evidence.
// GrossProceedsRUB excludes terminal fees; NetDistributionsRUB is the cumulative
// actual after-tax distribution cash received while this position was held.
type RelativeSettlement struct {
	ObservedAt          time.Time       `json:"observed_at"`
	InstrumentUID       string          `json:"instrument_uid"`
	AcceptedAt          time.Time       `json:"accepted_at"`
	AcceptedUnits       decimal.Decimal `json:"accepted_units"`
	PaidAt              time.Time       `json:"paid_at"`
	GrossProceedsRUB    decimal.Decimal `json:"gross_proceeds_rub"`
	TerminalFeeRUB      decimal.Decimal `json:"terminal_fee_rub"`
	FeeKnown            bool            `json:"fee_known"`
	NetDistributionsRUB decimal.Decimal `json:"net_distributions_rub"`
	DistributionsKnown  bool            `json:"distributions_known"`
	Verified            bool            `json:"verified"`
	Reference           string          `json:"reference"`
}

// SettlementDue separates a tender's operational cutoff from an exchange's last
// trading instant. Crossing it means unresolved acceptance/payment, never that
// the simulator submitted a tender. Pure cash spreads have no such boundary.
func SettlementDue(p Position) time.Time {
	boundary := LastTrade(p)
	if p.Entry.Strategy == CashOffer && p.Entry.Relationship != nil && p.Entry.Relationship.Offer != nil {
		offer := p.Entry.Relationship.Offer
		if offer.BrokerCutoffVerified && !offer.BrokerCutoff.IsZero() && (boundary.IsZero() || offer.BrokerCutoff.Before(boundary)) {
			boundary = offer.BrokerCutoff
		}
	}
	return boundary
}

// SettleRelativePosition supports full cash-offer settlements only. Derivative
// settlement and partially accepted tenders require a separate explicit model.
func SettleRelativePosition(p Position, input RelativeSettlement) (Valuation, error) {
	if p.Status != PositionOpen && p.Status != PositionAwaitingSettlement {
		return Valuation{}, fmt.Errorf("only an open or awaiting position can settle")
	}
	if p.Entry.Strategy != CashOffer || p.Entry.Relationship == nil || p.Entry.Relationship.Offer == nil || len(p.Entry.Legs) != 1 || p.Entry.Legs[0].Side != "buy" {
		return Valuation{}, fmt.Errorf("verified relative settlement supports a one-leg long cash offer only")
	}
	if err := validateEntry(p.Entry); err != nil {
		return Valuation{}, err
	}
	if err := ValidatePolicy(p.Policy); err != nil {
		return Valuation{}, err
	}
	if p.OpenedAt.IsZero() || !p.OpenedAt.Equal(p.Entry.Snapshot.ObservedAt) || input.ObservedAt.IsZero() || input.ObservedAt.Before(p.OpenedAt) || (p.Latest != nil && input.ObservedAt.Before(p.Latest.ObservedAt)) {
		return Valuation{}, fmt.Errorf("settlement observation is missing or out of order")
	}
	entry, offer := p.Entry.Legs[0], p.Entry.Relationship.Offer
	if !input.Verified || strings.TrimSpace(input.Reference) == "" || input.InstrumentUID != entry.Instrument.UID || input.AcceptedAt.IsZero() || input.PaidAt.IsZero() || input.AcceptedAt.Before(p.OpenedAt) || input.AcceptedAt.After(input.PaidAt) || input.PaidAt.After(input.ObservedAt) {
		return Valuation{}, fmt.Errorf("verified identity, acceptance and actual payment evidence are required")
	}
	if !input.AcceptedUnits.Equal(entry.Units) {
		return Valuation{}, fmt.Errorf("partial or excess tender acceptance is unsupported; accepted units must equal the full position")
	}
	if !offer.BrokerSupportsTender || !offer.BrokerCutoffVerified || !offer.EligibleSecurity || offer.BrokerCutoff.IsZero() || offer.AcceptanceDeadline.IsZero() || input.AcceptedAt.After(offer.BrokerCutoff) || input.AcceptedAt.After(offer.AcceptanceDeadline) {
		return Valuation{}, fmt.Errorf("acceptance does not satisfy the frozen verified offer eligibility and cutoff")
	}
	expectedProceeds := offer.OfferPriceRUB.Mul(entry.Units).Round(2)
	if !offer.OfferPriceRUB.IsPositive() || !input.GrossProceedsRUB.IsPositive() || !input.GrossProceedsRUB.Equal(expectedProceeds) {
		return Valuation{}, fmt.Errorf("actual gross payment does not match the frozen offer price and full accepted quantity")
	}
	expectedFee := offer.TerminalFeeRUB.Add(input.GrossProceedsRUB.Mul(offer.TerminalFeeRate)).Round(2)
	if !offer.TerminalFeeKnown || strings.TrimSpace(offer.TerminalFeeSource) == "" || !input.FeeKnown || input.TerminalFeeRUB.IsNegative() || !input.TerminalFeeRUB.Equal(expectedFee) {
		return Valuation{}, fmt.Errorf("actual terminal fee must be verified and match the frozen offer fee terms")
	}
	if !input.DistributionsKnown || input.NetDistributionsRUB.IsNegative() {
		return Valuation{}, fmt.Errorf("verified cumulative net distribution receipts are required")
	}
	profiles, err := positionExitProfiles(p)
	if err != nil {
		return Valuation{}, err
	}
	saved, err := clonePositionJSON(input)
	if err != nil {
		return Valuation{}, err
	}
	// The holding period ends when payment is received, not when its evidence is
	// entered later. A payment preceding the last valid mark is inconsistent.
	if p.Latest != nil && input.PaidAt.Before(p.Latest.ObservedAt) {
		return Valuation{}, fmt.Errorf("actual payment precedes the last valid open-position mark")
	}
	elapsed := years(input.PaidAt.Sub(p.OpenedAt))
	quote := offer.OfferPriceRUB.Div(entry.Instrument.PriceFactor)
	exit := Leg{Instrument: entry.Instrument, Side: "sell", Execution: "cash_offer", Lots: entry.Lots, Units: entry.Units, Price: quote, Limit: quote, Value: input.GrossProceedsRUB, FeeValue: input.GrossProceedsRUB}
	v := Valuation{ObservedAt: input.ObservedAt, Kind: "settlement", Settlement: &saved, Legs: []Leg{exit}, GrossProfit: input.GrossProceedsRUB.Sub(entry.Value), CashIncome: input.NetDistributionsRUB, ExecutionReserve: p.Entry.ExecutionReserve, CarryReserve: p.Entry.CapitalRequired.Mul(p.Policy.CarryReserveRate).Mul(elapsed), ShouldClose: true, CloseReason: "verified_cash_offer_payment"}
	v.LegProfits = []decimal.Decimal{v.GrossProfit}
	for _, profile := range profiles {
		fees, err := positionEntryFees(p.Entry, profile.Name)
		if err != nil {
			return Valuation{}, err
		}
		// A tender is not a fictitious market sale. Charge the verified terminal
		// action instead of applying another broker exchange-sale commission.
		fees = append(fees, scanner.FeeLine{Name: "cash_offer_terminal", Amount: input.TerminalFeeRUB})
		total := decimal.Zero
		for _, fee := range fees {
			total = total.Add(fee.Amount)
		}
		net := v.GrossProfit.Add(v.CashIncome).Sub(total).Sub(v.ExecutionReserve).Sub(v.CarryReserve)
		excess := net.Sub(v.CashBenchmark)
		annualized := decimal.Zero
		if elapsed.IsPositive() {
			annualized = net.Div(p.Entry.CapitalRequired).Div(elapsed)
		}
		v.Scenarios = append(v.Scenarios, scanner.Scenario{Profile: profile.Name, StockRate: profile.StockRate, FutureRate: profile.FutureRate, FeeSource: profile.Source, Fees: fees, TradingFees: total, NetProfit: net, ExcessProfit: excess, AnnualizedReturn: annualized, Qualifies: !net.LessThan(p.Policy.MinProfit) && !excess.LessThan(p.Policy.MinExcessProfit)})
	}
	return v, nil
}
