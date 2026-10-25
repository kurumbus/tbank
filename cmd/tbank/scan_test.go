package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/config"
	"tbank/internal/scanner"
)

func sample(t *testing.T) (scanner.Snapshot, scanConfig) {
	t.Helper()
	s, e := readFixture(filepath.Join("..", "..", "testdata", "carry.json"))
	if e != nil {
		t.Fatal(e)
	}
	c, e := loadScanConfig(filepath.Join("..", "..", "config", "scanner.json"))
	if e != nil {
		t.Fatal(e)
	}
	return s[0], c
}

type fakeMarket struct {
	snaps        []scanner.Snapshot
	calls        int
	previewCalls int
	preview      scanner.Preview
	err          error
}

func (f *fakeMarket) Discover(context.Context, []string, int) ([]scanner.Pair, error) {
	return nil, nil
}
func (f *fakeMarket) Snapshot(context.Context, scanner.Pair, int) (scanner.Snapshot, error) {
	i := f.calls
	f.calls++
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	return f.snaps[i], nil
}
func (f *fakeMarket) Preview(context.Context, scanner.Result, string) (scanner.Preview, error) {
	f.previewCalls++
	return f.preview, f.err
}

func TestScanRequiresExplicitAccountAndSupportedEndpoint(t *testing.T) {
	for _, s := range []config.Settings{{Endpoint: "sandbox-invest-public-api.tbank.ru:443"}, {Endpoint: "invest-public-api.tbank.ru:443"}, {Endpoint: "other:443", AccountID: "a"}} {
		if validateScanConnection(s) == nil {
			t.Fatal("unsafe connection accepted", s.Endpoint)
		}
	}
	for _, endpoint := range []string{"invest-public-api.tbank.ru:443", "sandbox-invest-public-api.tbank.ru:443"} {
		if err := validateScanConnection(config.Settings{Endpoint: endpoint, AccountID: "a"}); err != nil {
			t.Fatal(err)
		}
	}
}

func previewFor(r scanner.Result) scanner.Preview {
	p := scanner.Profiles()[0]
	return scanner.Preview{Status: "ok", ActualTariff: "investor", StockCommission: p.StockFee(r.StockLimit.Mul(decimal.NewFromInt(r.Shares))), FutureCommission: p.FutureFee(r.FutureLimit.Mul(r.Snapshot.Pair.Future.PriceFactor).Mul(decimal.NewFromInt(r.Contracts)), r.Contracts, r.Snapshot.Pair.Future.ExchangeFee), RequiredInitialMargin: r.InitialMargin}
}

func TestRecheckRemovesDisappearingOpportunity(t *testing.T) {
	s, c := sample(t)
	s2 := s
	s2.FutureBook.Bids = []scanner.Level{{Price: decimal.NewFromInt(30001), Lots: 10}}
	p := &fakeMarket{snaps: []scanner.Snapshot{s, s2}}
	r, err := evaluateLive(context.Background(), p, s.Pair, c, scanner.Profiles(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bestExcess(r); ok {
		t.Fatal("vanished gap still qualifies")
	}
	if p.calls != 2 || p.previewCalls != 0 {
		t.Fatalf("calls=%d previews=%d", p.calls, p.previewCalls)
	}
}

func TestUnknownOrMismatchedBrokerFeesSuppressMock(t *testing.T) {
	s, c := sample(t)
	base, err := scanner.Evaluate(s, c.assumptions(), scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"mismatch", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			p := &fakeMarket{snaps: []scanner.Snapshot{s, s}, preview: previewFor(base)}
			if kind == "mismatch" {
				p.preview.StockCommission = decimal.NewFromInt(1)
			} else {
				p.err = errors.New("preview failed")
			}
			r, err := evaluateLive(context.Background(), p, s.Pair, c, scanner.Profiles(), "a")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := bestExcess(r); ok {
				t.Fatal("unverified fees still qualify")
			}
			if r.Preview.Status != kind {
				t.Fatalf("status=%s", r.Preview.Status)
			}
		})
	}
}

func TestAccountSpecificMarginCanInvalidateCandidate(t *testing.T) {
	s, c := sample(t)
	base, err := scanner.Evaluate(s, c.assumptions(), scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeMarket{snaps: []scanner.Snapshot{s, s}, preview: previewFor(base)}
	p.preview.RequiredInitialMargin = decimal.NewFromInt(200000)
	r, err := evaluateLive(context.Background(), p, s.Pair, c, scanner.Profiles(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bestExcess(r); ok {
		t.Fatal("insufficient configured capital still qualifies")
	}
	if !r.InitialMargin.Equal(decimal.NewFromInt(200000)) {
		t.Fatal("account margin not applied", r.InitialMargin)
	}
}

func TestKnownActualTurnoverDiscountCalibratesSeparately(t *testing.T) {
	s, c := sample(t)
	r, err := scanner.Evaluate(s, c.assumptions(), scanner.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	p := scanner.Profiles()[1]
	p.FutureTurnover = decimal.NewFromInt(6000000)
	r.Preview = scanner.Preview{Status: "ok", ActualTariff: "trader", StockCommission: p.StockFee(r.StockCost), FutureCommission: p.FutureFee(r.FutureValue, r.Contracts, s.Pair.Future.ExchangeFee)}
	if err = verifyPreview(r); err != nil {
		t.Fatal(err)
	}
	r.Preview.FutureCommission = decimal.NewFromInt(1)
	if verifyPreview(r) == nil {
		t.Fatal("arbitrary future commission accepted")
	}
}

func TestQuotesMustStillBeFreshBeforeAlert(t *testing.T) {
	s, c := sample(t)
	r := scanner.Result{Snapshot: s}
	if !quotesFreshAt(r, s.ObservedAt, c) {
		t.Fatal("fresh quotes rejected")
	}
	if quotesFreshAt(r, s.ObservedAt.Add(time.Minute), c) {
		t.Fatal("old quotes accepted")
	}
	if quotesFreshAt(r, s.StockBook.Time.Add(-time.Second), c) {
		t.Fatal("future quotes accepted")
	}
}

func TestFixtureCannotSendTelegram(t *testing.T) {
	if err := cmdScan([]string{"-fixture", "does-not-need-to-exist.json", "-notify"}); err == nil {
		t.Fatal("fixture allowed notification")
	}
}

func TestConfigRejectsUnsupportedIssuerAndBadRates(t *testing.T) {
	_, c := sample(t)
	c.Tickers = []string{"T"}
	if c.validate() == nil {
		t.Fatal("issuer privilege not rejected")
	}
	_, c = sample(t)
	c.CashRate = decimal.NewFromInt(15)
	if c.validate() == nil {
		t.Fatal("percent mistaken for fraction accepted")
	}
}

func TestSandboxQuotesDoNotRequireProductionFeePreview(t *testing.T) {
	s, c := sample(t)
	p := &fakeMarket{snaps: []scanner.Snapshot{s, s}, err: errors.New("no production access")}
	r, err := evaluateQuotes(context.Background(), p, s.Pair, c, scanner.Profiles(), "sandbox", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.previewCalls != 0 || p.calls != 2 || r.Preview.Status != "sandbox_modeled" {
		t.Fatalf("wrong sandbox flow: %+v", r.Preview)
	}
	if _, ok := bestExcess(r); !ok {
		t.Fatal("modeled sandbox candidate missing")
	}
}

func TestExitTargetValidation(t *testing.T) {
	_, c := sample(t)
	for _, value := range []string{"0", "-0.1", "1.1"} {
		c.ExitCaptureFraction = decimal.RequireFromString(value)
		if c.validate() == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
