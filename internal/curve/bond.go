package curve

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type Cashflow struct {
	Date       time.Time       `json:"date"`
	Amount     decimal.Decimal `json:"amount_rub"`
	Kind       string          `json:"kind,omitempty"`
	RecordDate time.Time       `json:"record_date,omitempty"`
}

// BondTerms describes one vanilla fixed-coupon bullet bond. Accrued is the
// amount per bond on AccruedAt, rather than a percentage of nominal value.
type BondTerms struct {
	Nominal     decimal.Decimal `json:"nominal_rub"`
	Accrued     decimal.Decimal `json:"accrued_rub"`
	Maturity    time.Time       `json:"maturity"`
	AccruedAt   time.Time       `json:"accrued_at"`
	FixedCoupon bool            `json:"fixed_coupon"`
	Amortizing  bool            `json:"amortizing"`
	Floating    bool            `json:"floating"`
	Indexed     bool            `json:"indexed"`
	Cashflows   []Cashflow      `json:"cashflows"`
	Source      string          `json:"source"`
}

type BondObservation struct {
	ID         string          `json:"id"`
	Cashflows  []Cashflow      `json:"cashflows"`
	DirtyPrice decimal.Decimal `json:"dirty_price_rub"`
	Maturity   time.Time       `json:"maturity"`
}

// BondCurve is a local, two-parameter continuously compounded zero curve:
// z(t) = Level + Slope*t/ScaleYears. Each peer contributes one relative dirty-
// price residual, regardless of its coupon count or nominal amount. This is an
// explicitly estimated local curve, not the official MOEX curve or a forecast.
// The caller must apply its fit-quality thresholds before acting on a residual.
type BondCurve struct {
	AsOf                  time.Time       `json:"as_of"`
	Level                 decimal.Decimal `json:"level"`
	Slope                 decimal.Decimal `json:"slope"`
	ScaleYears            decimal.Decimal `json:"scale_years"`
	Peers                 []string        `json:"peers"`
	RMSEPriceFraction     decimal.Decimal `json:"rmse_price_fraction"`
	MaxPriceErrorFraction decimal.Decimal `json:"max_price_error_fraction"`
	Method                string          `json:"method"`
}

// ValidateBondTerms rejects cash flows that would require a different pricing
// model. The observation date must be the relevant settlement date. Coupon
// entitlement, when supplied, is an explicit record date and not an estimate.
func ValidateBondTerms(b BondTerms, asOf time.Time) error {
	if asOf.IsZero() || b.Maturity.IsZero() || !bondDate(b.Maturity).After(bondDate(asOf)) || !b.Nominal.IsPositive() || b.Accrued.IsNegative() || b.AccruedAt.IsZero() || !bondDate(b.AccruedAt).Equal(bondDate(asOf)) || strings.TrimSpace(b.Source) == "" {
		return fmt.Errorf("bond requires positive nominal, current accrued interest, future maturity and a source")
	}
	if !b.FixedCoupon || b.Amortizing || b.Floating || b.Indexed {
		return fmt.Errorf("only fixed-coupon non-amortizing non-indexed bullet bonds are supported")
	}
	redemptions := 0
	for _, flow := range b.Cashflows {
		if flow.Date.IsZero() || !flow.Amount.IsPositive() || bondDate(flow.Date).After(bondDate(b.Maturity)) {
			return fmt.Errorf("invalid bond cash-flow amount or date")
		}
		switch flow.Kind {
		case "redemption":
			redemptions++
			if !bondDate(flow.Date).Equal(bondDate(b.Maturity)) || !flow.Amount.Equal(b.Nominal) || !flow.RecordDate.IsZero() {
				return fmt.Errorf("bond must repay its full nominal exactly at maturity")
			}
		case "coupon":
			if !flow.RecordDate.IsZero() && bondDate(flow.RecordDate).After(bondDate(flow.Date)) {
				return fmt.Errorf("coupon record date follows its payment date")
			}
		default:
			return fmt.Errorf("unknown cash-flow kind %q", flow.Kind)
		}
	}
	if redemptions != 1 {
		return fmt.Errorf("bond requires exactly one full nominal redemption")
	}
	_, err := bondFlows(b.Cashflows, asOf)
	return err
}

// FitBondCurve excludes every observation with candidateID before validating or
// fitting its peers. It uses at most eight nearby bonds, with at least two on
// each side of candidate maturity. This avoids both self-fitting and maturity
// extrapolation. minPeers must be between four and eight.
func FitBondCurve(asOf time.Time, candidateID string, candidateMaturity time.Time, peers []BondObservation, minPeers int) (BondCurve, error) {
	if asOf.IsZero() || strings.TrimSpace(candidateID) == "" || candidateMaturity.IsZero() || minPeers < 4 || minPeers > 8 {
		return BondCurve{}, fmt.Errorf("curve requires a date, candidate identity/maturity and 4..8 minimum peers")
	}
	target := bondYears(asOf, candidateMaturity)
	if target <= 0 || target > 100 {
		return BondCurve{}, fmt.Errorf("candidate maturity is outside the supported horizon")
	}
	var all, shorter, longer []bondFitPeer
	seen := map[string]bool{}
	for _, peer := range peers {
		if peer.ID == candidateID {
			continue
		}
		if strings.TrimSpace(peer.ID) == "" || seen[peer.ID] || !peer.DirtyPrice.IsPositive() {
			return BondCurve{}, fmt.Errorf("curve peer identity or dirty price is invalid")
		}
		seen[peer.ID] = true
		flows, err := bondFlows(peer.Cashflows, asOf)
		if err != nil {
			return BondCurve{}, fmt.Errorf("peer %s: %w", peer.ID, err)
		}
		maturity := flows[len(flows)-1].years
		if !peer.Maturity.IsZero() && math.Abs(bondYears(asOf, peer.Maturity)-maturity) > 1e-10 {
			return BondCurve{}, fmt.Errorf("peer %s maturity disagrees with its final cash flow", peer.ID)
		}
		price, _ := peer.DirtyPrice.Float64()
		if !bondFinite(price) || price <= 0 {
			return BondCurve{}, fmt.Errorf("peer %s dirty price is not a finite supported value", peer.ID)
		}
		p := bondFitPeer{id: peer.ID, flows: flows, price: price, maturity: maturity, distance: math.Abs(maturity - target)}
		all = append(all, p)
		if maturity < target {
			shorter = append(shorter, p)
		}
		if maturity > target {
			longer = append(longer, p)
		}
	}
	if len(shorter) < 2 || len(longer) < 2 || len(all) < minPeers {
		return BondCurve{}, fmt.Errorf("curve requires at least %d peers including two shorter and two longer maturities", minPeers)
	}
	order := func(items []bondFitPeer) {
		sort.Slice(items, func(i, j int) bool {
			if items[i].distance == items[j].distance {
				return items[i].id < items[j].id
			}
			return items[i].distance < items[j].distance
		})
	}
	order(all)
	order(shorter)
	order(longer)
	selected := []bondFitPeer{shorter[0], shorter[1], longer[0], longer[1]}
	used := map[string]bool{}
	for _, peer := range selected {
		used[peer.id] = true
	}
	for _, peer := range all {
		if len(selected) == 8 {
			break
		}
		if !used[peer.id] {
			selected = append(selected, peer)
			used[peer.id] = true
		}
	}
	if len(selected) < minPeers {
		return BondCurve{}, fmt.Errorf("insufficient local peers")
	}
	order(selected)
	scale, seed := 0.0, 0.0
	for _, peer := range selected {
		scale = math.Max(scale, peer.maturity)
		y, err := bondSolveSpread(peer.flows, peer.price, 0, 0, 1)
		if err != nil {
			return BondCurve{}, fmt.Errorf("peer %s yield: %w", peer.id, err)
		}
		seed += y / float64(len(selected))
	}
	level, slope, err := bondFit(selected, scale, seed)
	if err != nil {
		return BondCurve{}, err
	}
	result := BondCurve{AsOf: bondDate(asOf), Level: decimal.NewFromFloat(level), Slope: decimal.NewFromFloat(slope), ScaleYears: decimal.NewFromFloat(scale), Method: "local_linear_continuous_zero_equal_weight_dirty_price_act365f"}
	squared, largest := 0.0, 0.0
	for _, peer := range selected {
		price, _, _ := bondValue(peer.flows, level, slope, scale, 0)
		residual := price/peer.price - 1
		squared += residual * residual
		largest = math.Max(largest, math.Abs(residual))
		result.Peers = append(result.Peers, peer.id)
	}
	result.RMSEPriceFraction = decimal.NewFromFloat(math.Sqrt(squared / float64(len(selected))))
	result.MaxPriceErrorFraction = decimal.NewFromFloat(largest)
	return result, nil
}

// CurvePrice discounts the entitled future cash flows at the fitted zero curve
// plus a parallel continuously compounded annual spread (0.01 means 100 bp).
func CurvePrice(curve BondCurve, flows []Cashflow, spread decimal.Decimal) (decimal.Decimal, error) {
	f, level, slope, scale, err := bondCurveInputs(curve, flows)
	if err != nil {
		return decimal.Zero, err
	}
	s, _ := spread.Float64()
	if !bondFinite(s) {
		return decimal.Zero, fmt.Errorf("spread is not finite")
	}
	price, _, _ := bondValue(f, level, slope, scale, s)
	if !bondFinite(price) || price <= 0 {
		return decimal.Zero, fmt.Errorf("curve price overflows or underflows")
	}
	return decimal.NewFromFloat(price), nil
}

// CurveSpread is the unique parallel zero-curve spread fitting the dirty price
// of positive fixed cash flows. Its result is an annual fraction, not basis points.
func CurveSpread(curve BondCurve, flows []Cashflow, dirty decimal.Decimal) (decimal.Decimal, error) {
	if !dirty.IsPositive() {
		return decimal.Zero, fmt.Errorf("dirty price must be positive")
	}
	f, level, slope, scale, err := bondCurveInputs(curve, flows)
	if err != nil {
		return decimal.Zero, err
	}
	price, _ := dirty.Float64()
	if !bondFinite(price) || price <= 0 {
		return decimal.Zero, fmt.Errorf("dirty price is not a finite supported value")
	}
	s, err := bondSolveSpread(f, price, level, slope, scale)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromFloat(s), nil
}

// BondDV01 returns positive RUB per basis point for one bond, repriced symmetrically
// around the supplied current spread. Both shifted prices are fully repriced;
// neutralizing this first-order measure does not remove convexity or curve risk.
func BondDV01(curve BondCurve, flows []Cashflow, spread decimal.Decimal) (decimal.Decimal, error) {
	f, level, slope, scale, err := bondCurveInputs(curve, flows)
	if err != nil {
		return decimal.Zero, err
	}
	s, _ := spread.Float64()
	if !bondFinite(s) {
		return decimal.Zero, fmt.Errorf("spread is not finite")
	}
	down, _, _ := bondValue(f, level, slope, scale, s-0.0001)
	up, _, _ := bondValue(f, level, slope, scale, s+0.0001)
	value := (down - up) / 2
	if !bondFinite(value) || value <= 0 {
		return decimal.Zero, fmt.Errorf("DV01 is not a positive finite value")
	}
	return decimal.NewFromFloat(value), nil
}

type bondFlow struct{ years, amount float64 }
type bondFitPeer struct {
	id                        string
	flows                     []bondFlow
	price, maturity, distance float64
}

func bondDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
func bondYears(from, to time.Time) float64 {
	return bondDate(to).Sub(bondDate(from)).Hours() / (24 * 365)
}
func bondFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func bondFlows(flows []Cashflow, asOf time.Time) ([]bondFlow, error) {
	if asOf.IsZero() {
		return nil, fmt.Errorf("cash-flow valuation date is missing")
	}
	var result []bondFlow
	for _, flow := range flows {
		if flow.Date.IsZero() || !flow.Amount.IsPositive() {
			return nil, fmt.Errorf("cash flow needs a date and positive amount")
		}
		if !flow.RecordDate.IsZero() && bondDate(flow.RecordDate).After(bondDate(flow.Date)) {
			return nil, fmt.Errorf("record date follows payment date")
		}
		years := bondYears(asOf, flow.Date)
		if years <= 0 {
			continue
		}
		if years > 100 {
			return nil, fmt.Errorf("cash flow exceeds 100-year horizon")
		}
		if flow.Kind == "coupon" && !flow.RecordDate.IsZero() && bondDate(flow.RecordDate).Before(bondDate(asOf)) {
			continue
		}
		amount, _ := flow.Amount.Float64()
		if !bondFinite(amount) || amount <= 0 {
			return nil, fmt.Errorf("cash-flow amount is not a finite supported value")
		}
		result = append(result, bondFlow{years, amount})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no entitled future cash flows")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].years < result[j].years })
	return result, nil
}

func bondCurveInputs(c BondCurve, flows []Cashflow) ([]bondFlow, float64, float64, float64, error) {
	level, _ := c.Level.Float64()
	slope, _ := c.Slope.Float64()
	scale, _ := c.ScaleYears.Float64()
	if c.AsOf.IsZero() || !bondFinite(level) || !bondFinite(slope) || !bondFinite(scale) || scale <= 0 {
		return nil, 0, 0, 0, fmt.Errorf("invalid curve coefficients, scale or valuation date")
	}
	f, err := bondFlows(flows, c.AsOf)
	return f, level, slope, scale, err
}

// Returns value and derivatives with respect to level and scaled slope.
func bondValue(flows []bondFlow, level, slope, scale, spread float64) (float64, float64, float64) {
	value, dl, ds := 0.0, 0.0, 0.0
	for _, f := range flows {
		pv := f.amount * math.Exp(-(level+spread)*f.years-slope*f.years*f.years/scale)
		value += pv
		dl -= f.years * pv
		ds -= f.years * f.years / scale * pv
	}
	return value, dl, ds
}

func bondSolveSpread(flows []bondFlow, target, level, slope, scale float64) (float64, error) {
	low, high := -1.0, 5.0
	left, _, _ := bondValue(flows, level, slope, scale, low)
	right, _, _ := bondValue(flows, level, slope, scale, high)
	if !bondFinite(left) || !bondFinite(right) || target > left || target < right {
		return 0, fmt.Errorf("dirty price has no finite spread root within -100%%..500%%")
	}
	for i := 0; i < 120; i++ {
		mid := (low + high) / 2
		price, _, _ := bondValue(flows, level, slope, scale, mid)
		if !bondFinite(price) {
			return 0, fmt.Errorf("spread solver overflow")
		}
		if math.Abs(price-target) <= math.Max(1e-10, target*1e-12) {
			return mid, nil
		}
		if price > target {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2, nil
}

func bondFit(peers []bondFitPeer, scale, seed float64) (float64, float64, error) {
	level, slope, lambda := seed, 0.0, 0.001
	objective := func(a, b float64) float64 {
		total := 0.0
		for _, p := range peers {
			value, _, _ := bondValue(p.flows, a, b, scale, 0)
			r := value/p.price - 1
			total += r * r
		}
		return total
	}
	current := objective(level, slope)
	for iteration := 0; iteration < 200; iteration++ {
		h00, h01, h11, g0, g1 := 0.0, 0.0, 0.0, 0.0, 0.0
		for _, p := range peers {
			value, dl, ds := bondValue(p.flows, level, slope, scale, 0)
			r, j0, j1 := value/p.price-1, dl/p.price, ds/p.price
			h00 += j0 * j0
			h01 += j0 * j1
			h11 += j1 * j1
			g0 += j0 * r
			g1 += j1 * r
		}
		if !bondFinite(current) || !bondFinite(h00) || !bondFinite(h11) || h00 <= 0 || h11 <= 0 || h00*h11-h01*h01 <= 1e-12*h00*h11 {
			return 0, 0, fmt.Errorf("peer curve fit is numerically singular")
		}
		if math.Abs(g0)+math.Abs(g1) < 1e-10 || current < 1e-22 {
			return level, slope, nil
		}
		a, b := h00*(1+lambda), h11*(1+lambda)
		det := a*b - h01*h01
		dl, ds := (b*g0-h01*g1)/det, (a*g1-h01*g0)/det
		step := math.Max(math.Abs(dl), math.Abs(ds))
		if step > 0.5 {
			dl *= 0.5 / step
			ds *= 0.5 / step
		}
		nextLevel, nextSlope := level-dl, slope-ds
		next := objective(nextLevel, nextSlope)
		if bondFinite(next) && next < current {
			improvement := current - next
			level, slope, current = nextLevel, nextSlope, next
			lambda = math.Max(lambda/3, 1e-12)
			if math.Abs(dl)+math.Abs(ds) < 1e-12 || improvement < 1e-18 {
				return level, slope, nil
			}
		} else {
			lambda *= 10
			if lambda > 1e15 {
				return 0, 0, fmt.Errorf("peer curve fit did not converge")
			}
		}
	}
	return 0, 0, fmt.Errorf("peer curve fit exceeded iteration limit")
}
