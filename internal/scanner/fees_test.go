package scanner

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(v string) decimal.Decimal { return decimal.RequireFromString(v) }

func equalDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(dec(want)) {
		t.Fatalf("%s: got %s, want %s", name, got, want)
	}
}

func TestCommissionRoundingMinimumAndExchangeFloor(t *testing.T) {
	p := Profiles()[0]
	equalDecimal(t, "minimum stock fee", p.StockFee(dec("0.01")), "0.01")
	equalDecimal(t, "half kopek mathematical rounding", p.StockFee(dec("5")), "0.02")
	equalDecimal(t, "round below half", p.StockFee(dec("4.99")), "0.01")
	equalDecimal(t, "exchange floor, not additional charge", p.FutureFee(dec("1000"), 2, dec("5.90")), "11.80")
	equalDecimal(t, "percentage greater than exchange floor", p.FutureFee(dec("30000"), 2, dec("5.90")), "30")
}

func TestCompletedTurnoverTierBoundaries(t *testing.T) {
	for _, tc := range []struct {
		profile        int
		turnover, rate string
	}{
		{1, "0", "0.0004"}, {1, "5000000", "0.0004"}, {1, "5000000.01", "0.0003"},
		{1, "10000000", "0.0003"}, {1, "10000000.01", "0.0003"}, {1, "10000000.02", "0.00025"},
		{2, "12000000", "0.00025"}, {2, "12000000.01", "0.0002"},
		{2, "17000000.01", "0.0002"}, {2, "17000000.02", "0.00015"},
	} {
		p := Profiles()[tc.profile]
		p.FutureTurnover = dec(tc.turnover)
		equalDecimal(t, p.Name+" turnover "+tc.turnover, p.EffectiveFutureRate(), tc.rate)
	}
	p := Profiles()[1]
	p.FutureTurnover = dec("4999999")
	// The candidate would cross the threshold. Do not assume a lower rate for it.
	equalDecimal(t, "candidate does not generate its own discount", p.FutureFee(dec("100000"), 1, dec("1")), "40")
}

func TestExactlyThreeTradingSchedules(t *testing.T) {
	profiles := Profiles()
	if len(profiles) != 3 {
		t.Fatalf("got %d profiles", len(profiles))
	}
	for i, name := range []string{"investor", "trader", "premium"} {
		if profiles[i].Name != name {
			t.Fatalf("wrong tariff order: %s", profiles[i].Name)
		}
	}
	equalDecimal(t, "premium shares", profiles[2].StockFee(dec("100000")), "40")
	equalDecimal(t, "premium future", profiles[2].FutureFee(dec("100000"), 1, dec("5")), "25")
}
