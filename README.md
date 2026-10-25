# T-Bank research scanner

Go CLI using the official Invest SDK. The original `search`, `list`, `info` and `export` commands remain available. `scan` tracks stock/future carry; `curve-scan` tracks four bond and interest-rate strategies; `relative-scan` adds six more relative-value families. All keep persistent paper positions, compare three trading-fee profiles and optionally send Telegram alerts. **They never place broker orders.**

## Stock/future strategy and position lifecycle

The scanner models buying fully funded RUB shares and selling matching **dated, physically delivered futures**. It verifies underlying identity, contract size, share lots, RUB conversion, margin, dates and executable order-book depth. Default stocks are SBER, SBERP, TATN, TATNP, GAZP and LKOH. Each stock is matched to its own future. Ordinary/preferred futures spreads use the separate `relative-scan` command.

A qualifying entry consumes stock asks and futures bids for the full quantity. The saved entry is immutable: quantities, prices, entry fees/rates, capital assumptions, profit hurdles and close target remain fixed. The first qualifying tariff in **Investor → Trader → Premium** order becomes the position's fixed minimum eligible tariff. Capital limits apply to each independent candidate, not an aggregate portfolio or wallet reservation.

Every scan revisits active positions using executable **stock bids and futures asks**. Each tariff scenario reports P&L after frozen entry fees, estimated exit fees and one fixed execution reserve taken at entry. Exit fees use a base tariff schedule saved at opening, zero assumed exit-day turnover and the current exchange floor; an entry-day turnover discount is not carried into a later day. The cash benchmark accrues over elapsed time using the entry capital and annual cash rate. Projected dividends are not credited to these marks.

The default paper close requires both:

- At least **90% of the opening gross basis** captured, using the saved `exit_capture_fraction`.
- The entry's minimum eligible tariff clears its saved net-profit and excess-profit hurdles after all modeled costs.

At the future's `LastTrade`, an unclosed position becomes `awaiting_settlement`. Expired quotes cannot close it. Settlement requires explicit verified terminal data; the scanner does not manufacture convergence or a zero-price exit. These remain paper positions: simultaneous fills, available account funds and future variation-margin capacity are not guaranteed.

## Four curve strategies

`curve-scan` uses separate models and position records. These are conditional relative-value trades: their target is a fitted or historical relationship, and their prices need not converge by contract.

| Strategy | Paired position and measured relationship | Target and remaining exposure |
| --- | --- | --- |
| `ofz_rgbi` | Buy a selected fixed-coupon OFZ and sell a dated RGBI index future, with quantities sized to approximate equal RUB sensitivity to a 1 bp rate move (DV01). The OFZ is priced against a local curve fitted to other bonds' complete remaining cash flows. | The fitted peer curve is the fair-value hypothesis. A zero fitted spread is a model target, not a promised outcome. Issue-specific spread, curve twists and index basis remain. |
| `keyrate_calendar` | Opposite positions in two KEYRATE expiries. The metric is the far-minus-near quoted key rate, in basis points. KEYRATE quotes the rate directly: `14.1` means 14.1%; one contract changes by RUB 10 per bp. | Reversion toward the pair's historical median. Equal contracts offset an equal revision to both rate expectations, while the difference between the two meeting outcomes remains exposed. |
| `ruonia_calendar` | Opposite positions in two futures on the compounded RUONIA index. The metric is the annualized log ratio of far/near index futures, in basis points; sizing accounts for maturity and index-level sensitivity. | Reversion toward a historical forward-compounding relationship. The index accumulates overnight interest, so a positive raw far/near gap is normal. Parallel-rate neutrality is a first-order approximation. |
| `rgbi_calendar` | Opposite positions in two dated futures on the same broad clean-price OFZ index. The metric is their annualized log-price ratio, in basis points. | Reversion toward the historical financing/basis relationship. This is an expiry/carry spread, not a trade between two government-bond maturity buckets. |

With the supplied configuration, the three calendar strategies require **at least 30 distinct, strictly earlier observations spanning at least 24 hours**. A new database normally needs more than a day of hourly collection before it can produce a calendar entry. History is specific to the instrument pair/model; a rolled contract pair must establish its own baseline. The current observation is not included in its target. Neither an empty history nor a new pair gets a zero-spread target.

OFZ fitting instead requires at least four eligible peer bonds, including two maturing before and two after the candidate. It uses up to eight nearby peers, excludes the candidate from its own fit and rejects poor fits. Missing coupon, redemption, accrued-interest or risk-reference data does not become a fabricated zero.

**OFZ/RGBI mock entry is disabled by default.** The available RGBI duration is an aggregate proxy, so `policy.allow_duration_proxy: false` leaves those signals visible without opening paper positions. Setting it to `true` opts into the approximation; the residual DV01 gate still applies. Duration matching offsets an assumed parallel curve shift, not every interest-rate movement. Perpetual `RGBIF` is excluded because it tracks a different index/funding structure.

Curve entries save their legs, target, policy, cost assumptions and minimum eligible tariff. For calendars, the expected-profit scenario holds the near leg unchanged and moves the far leg to the target while preserving the modeled book shape. The OFZ scenario holds the peer curve and hedge price fixed while repricing the selected bond. These are scenario estimates, not accrued P&L. Later marks consume the executable opposite sides for both full quantities and report net P&L separately. RGBI calendars match index notional directly; when a current duration reference is unavailable, absolute DV01 is reported as unknown rather than zero risk.

The normal paper close requires the saved target to be reached and the saved minimum tariff to pass both profit hurdles. The defaults also require an executable exit after **14 days** or **24 hours before the earliest leg's last trading time**, even at a loss. If no valid exit is available, the scanner records the failure instead of inventing a fill. Hedge quantities stay fixed; there is no dynamic rehedging or separate stop-loss rule. A position reaching the earliest last-trading time becomes `awaiting_settlement`. Multi-expiry futures settlement remains unresolved; `curve-positions settle` supports verified cash offers only.

OFZ marks also pause when the position has earned a coupon that remains unpaid after its entitlement record date: v1 does not track a receivable surviving a bond sale. The explicit mark error prevents a false loss or paper close in that window. Marks resume when the earned payment can be included; unknown entitlement is an error.

## Six additional relative-value families

`relative-scan` discovers current contracts from the exact selectors in `config/relative.json`. It resolves up to three matching expiries within 365 days, verifies current broker permissions and contract metadata, and retains instruments belonging to active positions after a watchlist change. It shares the curve position tables; each scan command only marks its own strategy families.

| Strategy | Default instruments | Target and sizing |
| --- | --- | --- |
| `standard_mini` | MX/MM, Si/USDM, GOLD/GOLDM | Equal normalized prices for contracts with the same underlying, fixing and expiry. One standard contract normally hedges ten minis; current metadata determines both quantity and quote conversion. Both directions are evaluated. |
| `share_class` | SBER/SBERP futures (SR/SP), TATN/TATNP futures (TT/TP) | Earlier median normalized ordinary/preferred price ratio, not equal share prices. SR/SP is normally 1:1; TT/TP is 1:10 because their delivery sizes differ. |
| `perpetual_dated` | CNYRUBF/CR, GLDRUBF/GL | Earlier median normalized perpetual/dated basis, with equal underlying quantities and an explicit uncertain-carry reserve. Today's funding is not extrapolated over the holding period. |
| `holding_stub` | ENPG/RUAL | Parent price less its ownership-equivalent RUAL value, reverting toward its own earlier median. The supplied ownership figures imply about 13.5271 RUAL shares per ENPG share. The residual includes the power business and parent financing. |
| `fund_nav` | TGLD@ / dated GL | Fund price discount/premium to sourced RUB NAV, hedged with its reported grams of gold per unit. Quantities round to executable lots and must pass the residual-exposure gate. |
| `cash_offer` | UGLD mandatory offer | One fully funded long share position versus the sourced RUB 0.7425 offer price. This is an event trade without a market hedge. Broker participation, purchase eligibility, cutoff, payout and terminal charges must be supplied before it can qualify. |

Share-class, perpetual/dated and holding-company models require the same 30 distinct earlier observations spanning 24 hours as the calendar models. Targets, quantities and entry costs freeze when a position opens. Revised ownership facts or model definitions start a separate history regime. Standard/mini and NAV relationships use their explicit economic reference instead of a historical target.

```sh
./bin/tbank relative-scan -config config/relative.json
./bin/tbank relative-scan -config config/relative.json -notify
./bin/tbank relative-scan -state var/relative-demo.sqlite -fixture testdata/relative.json
./bin/tbank curve-positions list -state var/relative-demo.sqlite.fixture
```

Reference inputs have sources and dates. The supplied holding ratio has a review deadline; fund gold exposure comes from the September 2026 manager report, including physical gold and gold-denominated NCC claims. Its monthly exposure review date is separate from fresh NAV requirements. Current RUB iNAV comes from MOEX's `TGLDB` indicator and must be synchronized with the traded books. A delayed or closed-day NAV can be displayed as a dated monitor but cannot qualify a trade. USD manager NAV requires a matching-date FX conversion; an already-RUB iNAV is not converted again. Missing or expired inputs appear in scan skips or blocked candidates. The supplied UGLD offer intentionally lacks unverified broker-processing terms, so it remains a monitor until those fields are completed. Extending a review deadline means checking the underlying source, not just changing a date.

Cash-share and fund short legs include the published daily uncovered-balance charge for each tariff. The default paper assumption is zero pre-existing uncovered balance, clearly marked as an assumption; the free small-balance allowance is conservatively disabled. Borrowing estimates accrue across Moscow calendar dates from observed balances, preserve prior accrued intervals, and can be replaced by a verified charge ledger. Sale proceeds do not reduce the modeled capital reserve. Announced distributions use full gross amounts for short liabilities; long receipts require an explicitly known net amount.

Perpetual marks use historical MOEX funding, with positive published funding charged to a long and credited to a short, multiplied by the contract's underlying size. Date-only observations cover fully held completed Moscow dates. Ambiguous entry/mark-day payments are reported as unbooked, with the carry reserve retained; exact timestamped reference events can resolve them. USD-quoted gold futures accrue quote changes at each observed RUB conversion, freezing earlier RUB amounts. This is explicitly an observation-based FX estimate, not a reconstruction of every clearing payment; unchanged quotes cannot rewrite prior profit merely because FX changed.

TGLD uses ordinary tariff fees conservatively, assuming the shared 15-free-orders daily allowance is exhausted. A qualifying zero broker preview is recognized but does not grant free fees to every simulated tariff. The fund's sourced annual expense cap is included in the entry scenario; it is not charged again as a cash debit on marks when already reflected in NAV.

Cash offers are marked at executable exchange bids before the verified broker cutoff. At cutoff they become `awaiting_settlement`; this does not assert that an instruction was submitted or accepted. Record full accepted quantity and actual payment with:

```sh
./bin/tbank curve-positions settle -state var/scanner.sqlite -id 1 -input offer-settlement.json
```

The input fields are `observed_at`, `instrument_uid`, `accepted_at`, `accepted_units`, `paid_at`, `gross_proceeds_rub`, `terminal_fee_rub`, `fee_known`, `net_distributions_rub`, `distributions_known`, `verified`, and `reference`. Acceptance must satisfy the saved terms; the full quantity and payment must reconcile. Partial acceptance is rejected. A tender charges its verified terminal fee instead of an invented exchange-sale commission. Payment time ends the capital charge; later confirmation remains in the audit history.

## Diagnosing empty futures scans

`curve-scan` and `relative-scan` include per-family discovery counts in `scan.skips`: catalogue size, recognized metadata matches, ticker-only hints, and selected contracts. Rejections include the failing rule and up to two metadata examples per reason, capped at eight reasons per selector. A ticker alone does not qualify a contract: its product code must match an approved `basic_asset` alias, including the API's Russian descriptions and currency-pair names. This distinguishes Si/UM despite both reporting `USD/RUB`, and dated/perpetual contracts sharing `CNY/RUB` or RUB gold. Settlement, units, permissions, dates and book checks still apply. Saved family identifiers remain canonical across metadata aliases. Pairing failures report expiry, fixing or underlying mismatches. Curve output also reports contracts excluded by the configured expiry buffer.

To preserve the current BASE catalogue used by discovery, including raw future types, underlying identifiers, contract size and dates:

```sh
./bin/tbank list -kind futures -limit 0 -json > futures-diagnostics.json
```

The full `export` command includes these fields too, but uses the ALL catalogue. A closed or stale book is checked after contract discovery; an empty discovery result needs its own metadata diagnosis. Successful authentication alone does not verify the scanner's contract mappings.

## Build and offline demonstrations

Requires Go 1.24 or later. SQLite is embedded and pure Go.

```sh
go mod download
CGO_ENABLED=0 go build -o bin/tbank ./cmd/tbank
go test -race ./...
go vet ./...
```

Fixtures contain **invented prices, identities and fees**. Each file supplies one simulation clock. They never access the broker or send Telegram; `-notify` is rejected. A base state path receives a `.fixture` suffix. Use fresh state basenames when repeating a demonstration.

Entry followed by an executable paper close:

```sh
./bin/tbank scan -state var/close-demo.sqlite -fixture testdata/carry.json
./bin/tbank scan -state var/close-demo.sqlite -fixture testdata/carry-close.json
./bin/tbank positions list -state var/close-demo.sqlite.fixture -source fixture
./bin/tbank positions show -state var/close-demo.sqlite.fixture -id 1
```

A separate entry followed by expiry and explicitly supplied settlement:

```sh
./bin/tbank scan -state var/settlement-demo.sqlite -fixture testdata/carry.json
./bin/tbank scan -state var/settlement-demo.sqlite -fixture testdata/carry-awaiting.json
./bin/tbank positions list -state var/settlement-demo.sqlite.fixture -status awaiting_settlement
./bin/tbank positions settle -state var/settlement-demo.sqlite.fixture -id 1 -input testdata/carry-settlement.json
./bin/tbank positions show -state var/settlement-demo.sqlite.fixture -id 1
```

Positive fixture results demonstrate the calculation and lifecycle, not a current market opportunity or a validated live execution.

## Production and sandbox observations

Copy `.env.example` to `.env`, set an explicit account ID and choose an endpoint:

```dotenv
TBANK_API_TOKEN=your_read_only_token
TBANK_ENDPOINT=invest-public-api.tbank.ru:443
TBANK_ACCOUNT_ID=your_brokerage_account_id
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_CHAT_ID=your_chat_id
```

For sandbox use `sandbox-invest-public-api.tbank.ru:443` and an existing sandbox account ID. The scanner does not create accounts. Production retains the actual-account `GetOrderPrice` commission check; sandbox uses shared market data and explicitly unverified hypothetical fees, without an actual broker fee preview. Sandbox positions are stored separately in `var/scanner.sqlite.sandbox`. Catalogue commands retain their existing sandbox default.

```sh
./bin/tbank scan -config config/scanner.json
./bin/tbank scan -config config/scanner.json -notify
```

Both commands only manage paper positions. The second also sends Telegram; missing Telegram configuration is an error with `-notify`. Keep `.env` private (`chmod 600 .env`); it and `var/` are ignored by Git. `scan -state PATH` overrides the configured base path; sandbox and fixture suffixes still apply. Position commands use the actual database path, including its suffix.

Review `config/scanner.json` before running. Rates are decimal fractions: `0.15` means 15% annually. **The default cash rate is illustrative, not fetched from the market.** Defaults include one future contract, RUB 100,000 capital per candidate, 25% extra initial-margin reserve, 5 bps execution reserve on both entry legs, a 20% delivery-price allowance and `exit_capture_fraction: "0.9"`. The entry reserve is charged once in lifecycle P&L, not on every observation. Price and funding needs can exceed these allowances. Results exclude personal tax and all monthly subscription costs.

## Running and configuring curve scans

```sh
./bin/tbank curve-scan -config config/curves.json
./bin/tbank curve-scan -config config/curves.json -state var/scanner.sqlite -notify
./bin/tbank curve-positions list -state var/scanner.sqlite -source production -status open
./bin/tbank curve-positions show -state var/scanner.sqlite -id 1
```

Curve scans use the same explicit-account and production/sandbox settings as stock scans. Both can share `var/scanner.sqlite` because their tables are separate. Sandbox and fixture scans append `.sandbox` and `.fixture` to the configured base path. Inspection commands require that exact resulting path. `curve-positions list` supports `-source production|sandbox|fixture` and `-status open|awaiting_settlement|closed`; `show` requires `-id` and includes observation history.

`curve-scan` emits a JSON array of scan reports. A production or sandbox call has one report. With `-fixture PATH`, the input is a chronological JSON array of snapshots and each snapshot produces a report; a single fixture can contain baseline warm-up, entry and exit observations. Fixtures make no broker requests and reject `-notify`.

The supplied KEYRATE fixture contains 32 invented snapshots covering baseline warm-up and subsequent spread changes:

```sh
./bin/tbank curve-scan -config config/curves.json -state var/curve-demo.sqlite -fixture testdata/curves.json
./bin/tbank curve-positions list -state var/curve-demo.sqlite.fixture -source fixture
./bin/tbank curve-positions show -state var/curve-demo.sqlite.fixture -id 1
```

`config/curves.json` contains these controls. Decimal rates and money are JSON strings; durations are Go duration strings such as `24h` or `30s`.

| Field | Supplied default and meaning |
| --- | --- |
| `bond_tickers` | Explicit allowlist of 12 fixed-coupon OFZ tickers. The loader accepts 5–40 unique uppercase tickers; incomplete or unsuitable bonds are skipped. |
| `futures_per_family`, `max_future_days` | Up to 3 dated contracts per supported family within 365 days. Active positions retain their own tracked instruments. |
| `order_book_depth`, `timeout_seconds` | 20 levels; 240-second command deadline. |
| `state_path` | `var/scanner.sqlite`, with source suffixes applied by scans. |
| `alert_cooldown_minutes`, `alert_improvement_rub` | 1,440 minutes and RUB 100 for repeated-alert suppression. |
| `policy.bond_lots`, `future_contracts` | 100 bond lots and a one-contract starting scale. The hedge is sized and checked for residual sensitivity; a one-contract configuration does not imply one contract on every leg. |
| `policy.capital_limit_rub` | RUB 1,000,000 per independent candidate, not a shared portfolio balance. |
| `policy.cash_rate_annual` | `0.15`, the illustrative annual cash alternative applied to required funded capital. |
| `policy.margin_buffer_rate`, `execution_buffer_bps` | `0.5` extra initial-margin reserve and a 10 bp execution-cost reserve. |
| `policy.carry_uncertainty_annual` | `0.05`, an annual reserve for uncertain carry over the modeled holding period. |
| `policy.assumed_prior_future_turnover_rub` | `0`; an entry-fee scenario assumption, not a fetched account turnover. |
| `policy.min_profit_rub`, `min_excess_profit_rub`, `min_deviation_bps` | RUB 100 net profit, RUB 100 excess over cash and 5 bp minimum metric deviation. |
| `policy.max_residual_dv01_ratio`, `allow_duration_proxy` | `0.10` residual-sensitivity limit; `false` for opening OFZ/RGBI with the aggregate duration proxy. |
| `policy.min_peers`, `max_curve_rmse_fraction`, `max_curve_error_fraction` | 4 peers; 0.5% RMS and 1% maximum relative bond-price fitting error. |
| `policy.min_history`, `min_history_span`, `history_max_age` | 30 prior observations, at least 24 hours apart overall, using at most 720-hour-old history. |
| `policy.max_quote_age`, `max_quote_skew`, `max_reference_age` | 30-second books, 5-second cross-leg skew and a 96-hour maximum RGBI risk-reference age. |
| `policy.max_holding`, `expiry_buffer` | `336h` maximum hold and `24h` before the earliest last-trading time. |

Curve capital includes the cash-funded bond purchase where applicable, each futures leg's applicable initial margin, the margin reserve and entry charges. Costs include entry and estimated exit fees for all three tariff families, execution/carry reserves and the cash benchmark. Futures notional is not treated as a cash purchase. Finite margin reserves do not eliminate variation-margin funding needs. Curve scans reject an entry if the account preview requires more margin than its modeled instrument metadata, or if fees cannot be reconciled. The separate stock scanner can instead raise its funded-capital estimate from an account preview. Sandbox and fixture estimates are labeled as unverified. Subscription fees are excluded.

## Costs and alerts

See [verified commission rules and sources](docs/commissions.md).

- **Investor, Trader and Premium** are compared. Bronze, Silver, Gold and Diamond have identical Premium trading rates; their monthly prices and eligibility do not enter these calculations.
- Futures commission is the greater of the percentage charge and the MOEX exchange fee floor, rounded to kopecks. The `BUYSELLFEE` source must be present and valid; exchange fees are not added twice. The stock/future `scan` excludes perpetual, cash-settled, special-list and non-RUB contracts. `T` shares are excluded because their Premium allowance requires a daily transaction counter. `curve-scan` separately supports the specified dated, cash-settled RUB index/rate futures.
- `assumed_prior_futures_turnover_rub` defaults to zero. Changing it simulates a discount, not a verified account total. A threshold-crossing order retains its pre-order rate. Entry rates and fees are frozen in each saved position.
- Production candidates are fetched again and their actual-account entry commission previews must match a published tariff/rate/floor. Unknown tariffs, missing previews or unexplained differences suppress new entries and their alerts. Account-specific initial margin can raise required funded capital. Sandbox does not claim this validation.
- Entry eligibility requires both net profit and excess profit above the cash benchmark. Funding includes the stock purchase, initial margin, reserve and entry fees. Quotes must be fresh, synchronized, uncrossed and tradeable, with full depth for both legs. Rejected candidates and unavailable marks remain visible in reports and history.

Telegram identifies the fixed minimum eligible tariff and shows open or closed P&L for all three scenarios. It includes source and fee-validation status. Alert cooldown prevents repeated observations from flooding the chat; failed delivery can retry. No broker trades are executed by an alert or a paper close.

Curve alerts name the strategy and paired sides, show the frozen target and its historical or fitted source, and separate expected target-convergence profits from current executable-close estimates or realized **mock** closes. They include the duration/parallel-curve approximation where relevant. A gap or expected profit alone is not recorded as realized P&L.

## Inspecting and settling positions

Commands return JSON. `show` includes the entry and observation history:

```sh
./bin/tbank positions list -state var/scanner.sqlite
./bin/tbank positions list -state var/scanner.sqlite -source production -status open
./bin/tbank positions show -state var/scanner.sqlite -id 1
./bin/tbank positions settle -state var/scanner.sqlite -id 1 -input settlement.json
```

Source filters are `production`, `sandbox` and `fixture`; status filters are `open`, `awaiting_settlement` and `closed`. Manual settlement input is supported for each source:

```json
{
  "stock_price": "300",
  "future_price": "30000",
  "observed_at": "2026-12-18T16:00:00Z",
  "reference": "source identifying the verified terminal prices and distributions",
  "dividends_rub": "0",
  "verified": true
}
```

This is a schema illustration, not market data. Prices use the instruments' raw quote units; stored price factors convert them to money. `dividends_rub` is the cumulative cash distribution for the whole saved position, not per share. Supply both terminal prices, a nonempty source reference and a verified timestamp at or after expiry and the latest valid price mark. A later awaiting-settlement check does not prevent recording the actual earlier settlement time. The caller must check the evidence; setting `verified` does not fetch or independently verify it. Production and sandbox reject future settlement timestamps; fixtures may use a future simulation clock. Settlement charges the physical-delivery share commission, without adding a futures expiry trade commission.

SQLite stores runs, stock and curve positions, historical curve metrics, observations and notification state. Entries remain immutable; observations record marks and transitions. Existing `mock_records` remain audit-only and are not promoted into managed positions. Production, sandbox and fixtures use separate databases. Keep `var/` on persistent disk and back it up if this history matters.

## Hourly Linux cron

Install at `/opt/tbank`, build there and configure `.env`. Run as the account owning the checkout and state. `flock` and `timeout` are standard Linux utilities.

```sh
mkdir -p var
chmod 700 var
chmod 600 .env
chmod +x deploy/run-hourly.sh
```

Add to that user's crontab:

```cron
0 * * * * /opt/tbank/deploy/run-hourly.sh >> /opt/tbank/var/hourly.log 2>&1
```

The wrapper changes to the install directory and holds one lock while it runs `scan`, `curve-scan`, and `relative-scan` serially. Each command has a five-minute process limit. If one fails, the remaining scans still run, and the wrapper exits unsuccessfully if any failed. Set `TBANK_INSTALL_DIR` for another path. Use the same wrapper/lock for manual production runs and rotate `var/hourly.log`. No cron entry is installed by building or testing. Hourly observations can track persistent gaps but miss opportunities between samples.
