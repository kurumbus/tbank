package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"

	"tbank/internal/client"
	"tbank/internal/config"
	"tbank/internal/instruments"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var usage usageError
		if errors.As(err, &usage) {
			fmt.Fprintln(os.Stderr, usage.Error())
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "tbank: %s\n", err)
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func run(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return usageError{msg: usageText}
	}
	if args[0] == "positions" {
		return cmdPositions(args[1:])
	}
	if args[0] == "scan" {
		return cmdScan(args[1:])
	}
	if args[0] == "curve-scan" {
		return cmdCurveScan(args[1:])
	}
	if args[0] == "curve-positions" {
		return cmdCurvePositions(args[1:])
	}
	switch args[0] {
	case "search", "list", "info", "export":
	default:
		return usageError{msg: usageText}
	}
	for _, arg := range args[1:] {
		if isHelp(arg) {
			return usageError{msg: usageText}
		}
	}

	settings, err := config.Load()
	if err != nil {
		return err
	}

	timeout := 60 * time.Second
	if args[0] == "export" {
		timeout = 10 * time.Minute
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	api, err := client.Dial(ctx, settings)
	if err != nil {
		return err
	}
	defer api.Stop()
	fmt.Fprintf(os.Stderr, "connected %s account %s\n", settings.Endpoint, api.Config.AccountId)

	svc := instruments.New(api)
	switch args[0] {
	case "search":
		return cmdSearch(svc, args[1:])
	case "list":
		return cmdList(svc, args[1:])
	case "info":
		return cmdInfo(svc, args[1:])
	case "export":
		return cmdExport(svc, settings.Endpoint, args[1:])
	default:
		return usageError{msg: usageText}
	}
}

func cmdSearch(svc *instruments.Service, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 20, "max rows, 0 for all matches")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: usageText}
	}
	if fs.NArg() != 1 {
		return usageError{msg: "usage: tbank search [-limit N] [-json] <query>"}
	}
	result, err := svc.Search(fs.Arg(0), *limit)
	if err != nil {
		return err
	}
	return printResult(result, *asJSON)
}

func cmdList(svc *instruments.Service, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "shares", "shares, bonds, etfs, currencies, or futures")
	query := fs.String("q", "", "substring of ticker or name")
	currency := fs.String("currency", "", "settlement currency, for example rub")
	exchange := fs.String("exchange", "", "substring of exchange id")
	apiOnly := fs.Bool("api", false, "only instruments tradable through the API")
	limit := fs.Int("limit", 20, "max rows, 0 for all matches")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: usageText}
	}
	if fs.NArg() != 0 {
		return usageError{msg: "usage: tbank list [-kind shares] [-q text] [-currency rub] [-exchange text] [-api] [-limit N] [-json]"}
	}
	result, err := svc.List(instruments.Filter{
		Kind:     *kind,
		Query:    *query,
		Currency: *currency,
		Exchange: *exchange,
		APIOnly:  *apiOnly,
		Limit:    *limit,
	})
	if err != nil {
		return err
	}
	return printResult(result, *asJSON)
}

func cmdInfo(svc *instruments.Service, args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	classCode := fs.String("class", "", "class code, required when a ticker trades on several boards")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: usageText}
	}
	if fs.NArg() != 1 {
		return usageError{msg: "usage: tbank info [-class CODE] [-json] <ticker>"}
	}
	result, err := svc.Info(fs.Arg(0), *classCode)
	if err != nil {
		return err
	}
	if *asJSON {
		return printResult(result, true)
	}
	for i, item := range result.Instruments {
		if i > 0 {
			fmt.Println()
		}
		printInfo(item)
	}
	fmt.Fprintf(os.Stderr, "showing %d of %d\n", len(result.Instruments), result.Matched)
	return nil
}

func cmdExport(svc *instruments.Service, endpoint string, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	outPath := fs.String("o", filepath.Join("data", "instruments.json"), "output JSON file")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: usageText}
	}
	if fs.NArg() != 0 {
		return usageError{msg: "usage: tbank export [-o data/instruments.json]"}
	}

	catalog, err := svc.Catalog(func(kind string, n int) {
		fmt.Fprintf(os.Stderr, "loaded %d %s\n", n, kind)
	})
	if err != nil {
		return err
	}
	catalog.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	catalog.Endpoint = endpoint
	if err := writeJSON(*outPath, catalog); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d instruments to %s\n", catalog.Count, *outPath)
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instruments-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	encodeErr := enc.Encode(value)
	closeErr := tmp.Close()
	if encodeErr != nil {
		os.Remove(tmpName)
		return encodeErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func printResult(result instruments.Result, asJSON bool) error {
	if result.Matched == 0 {
		return fmt.Errorf("no instruments matched")
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		printTable(result.Instruments)
	}
	fmt.Fprintf(os.Stderr, "showing %d of %d\n", len(result.Instruments), result.Matched)
	return nil
}

func printTable(items []instruments.Summary) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKER\tCLASS\tTYPE\tCUR\tLOT\tSTATUS\tNAME")
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			item.Ticker,
			item.ClassCode,
			item.Type,
			item.Currency,
			item.Lot,
			item.TradingStatus,
			item.Name,
		)
	}
	w.Flush()
}

func printInfo(item instruments.Summary) {
	rows := [][2]string{
		{"ticker", item.Ticker},
		{"name", item.Name},
		{"type", item.Type},
		{"class", item.ClassCode},
		{"figi", item.FIGI},
		{"uid", item.UID},
		{"isin", item.ISIN},
		{"currency", item.Currency},
		{"lot", fmt.Sprintf("%d", item.Lot)},
		{"exchange", item.Exchange},
		{"status", item.TradingStatus},
		{"min step", item.MinPriceIncrement},
		{"api trade", yesNo(item.APITradeAvailable)},
		{"buy", yesNo(item.BuyAvailable)},
		{"sell", yesNo(item.SellAvailable)},
		{"short", yesNo(item.ShortEnabled)},
		{"qualified only", yesNo(item.ForQualified)},
	}
	for _, row := range rows {
		if row[1] == "" {
			continue
		}
		fmt.Printf("%-16s %s\n", row[0], row[1])
	}
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func yesNo(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "yes"
	}
	return "no"
}

const usageText = `tbank reads tickers and basic instrument info from the T-Bank Invest API.

Usage:
  tbank search [-limit N] [-json] <query>
  tbank list [-kind shares|bonds|etfs|currencies|futures|options] [-q text] [-currency rub] [-exchange text] [-api] [-limit N] [-json]
  tbank info [-class CODE] [-json] <ticker>
  tbank export [-o data/instruments.json]
  tbank positions <list|show|settle> [-state var/scanner.sqlite] [options]
  tbank scan [-config config/scanner.json] [-fixture testdata/carry.json] [-state path] [-notify]
  tbank curve-scan [-config config/curves.json] [-fixture testdata/curves.json] [-state path] [-notify]
  tbank curve-positions <list|show> [-state var/scanner.sqlite] [options]

The token is TBANK_API_TOKEN from the environment or .env.
TBANK_ENDPOINT defaults to the sandbox host. An empty TBANK_ACCOUNT_ID makes the SDK reuse or open a sandbox account.
TLS uses the bundled Russian Trusted Root CA. Set TBANK_TLS_CA_FILE to replace that bundle.`
