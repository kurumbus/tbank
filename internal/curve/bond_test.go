package curve

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func bondTestCurve() BondCurve {
	return BondCurve{AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Level: decimal.RequireFromString("0.12"), Slope: decimal.RequireFromString("0.04"), ScaleYears: decimal.NewFromInt(10)}
}

func bondTestFlows(asOf time.Time, years int, coupon, nominal decimal.Decimal) []Cashflow {
	var flows []Cashflow
	for period := 1; period <= years*2; period++ {
		date := asOf.AddDate(0, period*6, 0)
		flows = append(flows, Cashflow{Date: date, Amount: coupon.Div(decimal.NewFromInt(2)), Kind: "coupon", RecordDate: date.AddDate(0, 0, -1)})
	}
	return append(flows, Cashflow{Date: asOf.AddDate(years, 0, 0), Amount: nominal, Kind: "redemption"})
}

func bondTestPeers(t *testing.T, years ...int) []BondObservation {
	t.Helper()
	c := bondTestCurve()
	var peers []BondObservation
	for i, y := range years {
		flows := bondTestFlows(c.AsOf, y, decimal.NewFromInt(int64(40+i*9)), decimal.NewFromInt(1000))
		price, err := CurvePrice(c, flows, decimal.Zero)
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, BondObservation{ID: decimal.NewFromInt(int64(i + 1)).String(), Cashflows: flows, DirtyPrice: price, Maturity: c.AsOf.AddDate(y, 0, 0)})
	}
	return peers
}

func assertBondNear(t *testing.T, got decimal.Decimal, want float64, tolerance float64) {
	t.Helper()
	v, _ := got.Float64()
	if math.Abs(v-want) > tolerance {
		t.Fatalf("got %s want %.12f tolerance %.12f", got, want, tolerance)
	}
}

func TestBondCurveFitsPeerCashflowsAndExcludesCandidate(t *testing.T) {
	known := bondTestCurve()
	peers := bondTestPeers(t, 2, 3, 4, 6, 7, 8)
	flows := bondTestFlows(known.AsOf, 5, decimal.NewFromInt(85), decimal.NewFromInt(1000))
	// Candidate's bogus/invalid observation must not influence its own fair curve.
	peers = append(peers, BondObservation{ID: "candidate", DirtyPrice: decimal.NewFromInt(-9000)})
	fit, err := FitBondCurve(known.AsOf, "candidate", known.AsOf.AddDate(5, 0, 0), peers, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range fit.Peers {
		if id == "candidate" {
			t.Fatal("candidate included in fit")
		}
	}
	fair, err := CurvePrice(fit, flows, decimal.Zero)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := CurvePrice(known, flows, decimal.Zero)
	wantFloat, _ := want.Float64()
	assertBondNear(t, fair, wantFloat, 1e-6)
	if fit.RMSEPriceFraction.GreaterThan(decimal.RequireFromString("0.00000001")) {
		t.Fatal("exact peer curve did not fit", fit)
	}
	spread, err := CurveSpread(fit, flows, fair.Sub(decimal.NewFromInt(20)))
	if err != nil {
		t.Fatal(err)
	}
	if !spread.IsPositive() {
		t.Fatal("discounted bond must have a positive curve spread")
	}
	price, err := CurvePrice(fit, flows, spread)
	if err != nil {
		t.Fatal(err)
	}
	wantFloat, _ = fair.Sub(decimal.NewFromInt(20)).Float64()
	assertBondNear(t, price, wantFloat, 1e-6)
}

func TestBondCurveRequiresIndependentMaturityBracket(t *testing.T) {
	c := bondTestCurve()
	for _, years := range [][]int{{2, 6, 7, 8}, {2, 3, 4, 6}, {6, 7, 8, 9}, {2, 3, 6}} {
		if _, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), bondTestPeers(t, years...), 4); err == nil {
			t.Fatal("unbracketed or insufficient fit accepted", years)
		}
	}
	peers := bondTestPeers(t, 2, 3, 6, 7)
	peers[1].ID = peers[0].ID
	if _, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), peers, 4); err == nil {
		t.Fatal("duplicate peer overweighted")
	}
	for _, minimum := range []int{0, 3, 9} {
		if _, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), bondTestPeers(t, 2, 3, 6, 7), minimum); err == nil {
			t.Fatal("invalid peer minimum accepted")
		}
	}
}

func TestBondCurveLocalSelectionAndEqualBondWeights(t *testing.T) {
	c := bondTestCurve()
	peers := bondTestPeers(t, 1, 2, 3, 4, 6, 7, 8, 9)
	a, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), peers, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Scaling one issue's nominal and every cash flow must not increase its fit
	// weight. Splitting a cash flow also must not create another observation.
	factor := decimal.NewFromInt(100)
	for i := range peers[0].Cashflows {
		peers[0].Cashflows[i].Amount = peers[0].Cashflows[i].Amount.Mul(factor)
	}
	peers[0].DirtyPrice = peers[0].DirtyPrice.Mul(factor)
	first := peers[1].Cashflows[0]
	first.Amount = first.Amount.Div(decimal.NewFromInt(2))
	peers[1].Cashflows[0] = first
	peers[1].Cashflows = append(peers[1].Cashflows, first)
	far := bondTestPeers(t, 30)[0]
	far.ID = "far-outlier"
	far.DirtyPrice = far.DirtyPrice.Mul(decimal.RequireFromString("0.5"))
	peers = append(peers, far)
	b, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), peers, 4)
	if err != nil {
		t.Fatal(err)
	}
	level, _ := a.Level.Float64()
	assertBondNear(t, b.Level, level, 1e-8)
	slope, _ := a.Slope.Float64()
	assertBondNear(t, b.Slope, slope, 1e-8)
	if len(b.Peers) != 8 {
		t.Fatal("local fit did not cap peer count")
	}
	for _, id := range b.Peers {
		if id == far.ID {
			t.Fatal("distant outlier included despite enough local peers")
		}
	}
}

func TestBondCurveReportsFitErrorInsteadOfInventingExactFairValue(t *testing.T) {
	c := bondTestCurve()
	peers := bondTestPeers(t, 2, 3, 4, 6, 7, 8)
	peers[2].DirtyPrice = peers[2].DirtyPrice.Mul(decimal.RequireFromString("1.05"))
	fit, err := FitBondCurve(c.AsOf, "candidate", c.AsOf.AddDate(5, 0, 0), peers, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.RMSEPriceFraction.GreaterThan(decimal.RequireFromString("0.005")) || !fit.MaxPriceErrorFraction.GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatal("large peer inconsistency was hidden", fit)
	}
}

func TestCurveSpreadAndDV01UseFullCashflowsAndBasisPointUnits(t *testing.T) {
	c := bondTestCurve()
	date := c.AsOf.AddDate(5, 0, 0)
	flows := []Cashflow{{Date: date, Amount: decimal.NewFromInt(1000), Kind: "redemption"}}
	years := bondYears(c.AsOf, date)
	for _, z := range []string{"0", "0.005", "-0.005"} {
		spread := decimal.RequireFromString(z)
		price, err := CurvePrice(c, flows, spread)
		if err != nil {
			t.Fatal(err)
		}
		inferred, err := CurveSpread(c, flows, price)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := spread.Float64()
		assertBondNear(t, inferred, want, 1e-10)
		dv01, err := BondDV01(c, flows, spread)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := price.Float64()
		assertBondNear(t, dv01, p*math.Sinh(years*0.0001), 1e-8)
	}
	for _, bad := range []decimal.Decimal{decimal.Zero, decimal.NewFromInt(-1)} {
		if _, err := CurveSpread(c, flows, bad); err == nil {
			t.Fatal("invalid dirty price accepted")
		}
	}
}

func TestBondCouponEntitlementAndCalendarDayAccrual(t *testing.T) {
	c := bondTestCurve()
	due := c.AsOf.AddDate(0, 0, 5)
	flows := []Cashflow{
		{Date: due, Amount: decimal.NewFromInt(80), Kind: "coupon", RecordDate: c.AsOf.AddDate(0, 0, -1)},
		{Date: c.AsOf.AddDate(1, 0, 0), Amount: decimal.NewFromInt(1000), Kind: "redemption"},
	}
	without, err := CurvePrice(c, flows, decimal.Zero)
	if err != nil {
		t.Fatal(err)
	}
	redemption, _ := CurvePrice(c, flows[1:], decimal.Zero)
	if !without.Equal(redemption) {
		t.Fatal("new buyer received an ex-record coupon")
	}
	flows[0].RecordDate = c.AsOf
	with, err := CurvePrice(c, flows, decimal.Zero)
	if err != nil {
		t.Fatal(err)
	}
	if !with.GreaterThan(without) {
		t.Fatal("settlement on record date lost entitlement")
	}
	c.AsOf = c.AsOf.Add(20 * time.Hour)
	intraday, err := CurvePrice(c, flows, decimal.Zero)
	if err != nil {
		t.Fatal(err)
	}
	if !intraday.Equal(with) {
		t.Fatal("intraday timestamp changed calendar-day cash-flow pricing")
	}
}

func TestValidateBondTermsRejectsUnsupportedAndStaleTerms(t *testing.T) {
	c := bondTestCurve()
	base := BondTerms{Nominal: decimal.NewFromInt(1000), Accrued: decimal.NewFromInt(20), Maturity: c.AsOf.AddDate(5, 0, 0), AccruedAt: c.AsOf, FixedCoupon: true, Cashflows: bondTestFlows(c.AsOf, 5, decimal.NewFromInt(80), decimal.NewFromInt(1000)), Source: "synthetic"}
	if err := ValidateBondTerms(base, c.AsOf); err != nil {
		t.Fatal(err)
	}
	mutators := []func(*BondTerms){
		func(b *BondTerms) { b.Floating = true }, func(b *BondTerms) { b.Indexed = true }, func(b *BondTerms) { b.Amortizing = true }, func(b *BondTerms) { b.FixedCoupon = false },
		func(b *BondTerms) { b.AccruedAt = b.AccruedAt.AddDate(0, 0, -1) }, func(b *BondTerms) { b.Accrued = decimal.NewFromInt(-1) },
		func(b *BondTerms) { b.Nominal = decimal.NewFromInt(500) }, func(b *BondTerms) { b.Cashflows = append(b.Cashflows, b.Cashflows[len(b.Cashflows)-1]) },
		func(b *BondTerms) { b.Source = "" },
	}
	for _, mutate := range mutators {
		b := base
		mutate(&b)
		if err := ValidateBondTerms(b, c.AsOf); err == nil {
			t.Fatalf("unsupported terms accepted: %+v", b)
		}
	}
}
