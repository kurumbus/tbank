# T-Bank research scanner

Go CLI using the official Invest SDK. The original `search`, `list`, `info` and `export` commands remain available. `scan` adds hourly market observations, three commission scenarios, persistent paper positions and optional Telegram alerts. **It never places broker orders.**

## Strategy and position lifecycle

The scanner models buying fully funded RUB shares and selling matching **dated, physically delivered futures**. It verifies underlying identity, contract size, share lots, RUB conversion, margin, dates and executable order-book depth. Default stocks are SBER, SBERP, TATN, TATNP, GAZP and LKOH. Each stock is matched to its own future; ordinary/preferred convergence is outside this version.

A qualifying entry consumes stock asks and futures bids for the full quantity. The saved entry is immutable: quantities, prices, entry fees/rates, capital assumptions, profit hurdles and close target remain fixed. The first qualifying tariff in **Investor → Trader → Premium** order becomes the position's fixed minimum eligible tariff. Capital limits apply to each independent candidate, not an aggregate portfolio or wallet reservation.

Every scan revisits active positions using executable **stock bids and futures asks**. Each tariff scenario reports P&L after frozen entry fees, estimated exit fees and one fixed execution reserve taken at entry. Exit fees use a base tariff schedule saved at opening, zero assumed exit-day turnover and the current exchange floor; an entry-day turnover discount is not carried into a later day. The cash benchmark accrues over elapsed time using the entry capital and annual cash rate. Projected dividends are not credited to these marks.

The default paper close requires both:

- At least **90% of the opening gross basis** captured, using the saved `exit_capture_fraction`.
- The entry's minimum eligible tariff clears its saved net-profit and excess-profit hurdles after all modeled costs.

At the future's `LastTrade`, an unclosed position becomes `awaiting_settlement`. Expired quotes cannot close it. Settlement requires explicit verified terminal data; the scanner does not manufacture convergence or a zero-price exit. These remain paper positions: simultaneous fills, available account funds and future variation-margin capacity are not guaranteed.

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

## Costs and alerts

See [verified commission rules and sources](docs/commissions.md).

- **Investor, Trader and Premium** are compared. Bronze, Silver, Gold and Diamond have identical Premium trading rates; their monthly prices and eligibility do not enter these calculations.
- Futures commission is the greater of the percentage charge and the MOEX exchange fee floor, rounded to kopecks. The `BUYSELLFEE` source must be present and valid; exchange fees are not added twice. Perpetual, cash-settled, special-list and non-RUB contracts are excluded. `T` shares are excluded because their Premium allowance requires a daily transaction counter.
- `assumed_prior_futures_turnover_rub` defaults to zero. Changing it simulates a discount, not a verified account total. A threshold-crossing order retains its pre-order rate. Entry rates and fees are frozen in each saved position.
- Production candidates are fetched again and their actual-account entry commission previews must match a published tariff/rate/floor. Unknown tariffs, missing previews or unexplained differences suppress new entries and their alerts. Account-specific initial margin can raise required funded capital. Sandbox does not claim this validation.
- Entry eligibility requires both net profit and excess profit above the cash benchmark. Funding includes the stock purchase, initial margin, reserve and entry fees. Quotes must be fresh, synchronized, uncrossed and tradeable, with full depth for both legs. Rejected candidates and unavailable marks remain visible in reports and history.

Telegram identifies the fixed minimum eligible tariff and shows open or closed P&L for all three scenarios. It includes source and fee-validation status. Alert cooldown prevents repeated observations from flooding the chat; failed delivery can retry. No broker trades are executed by an alert or a paper close.

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

SQLite stores runs, `positions`, `position_observations` and notification state. Entries remain immutable; observations record marks and transitions. Existing `mock_records` remain audit-only and are not promoted into managed positions. Production, sandbox and fixtures use separate databases. Keep `var/` on persistent disk and back it up if this history matters.

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

The wrapper changes to the install directory, prevents overlap and limits a run to five minutes. Set `TBANK_INSTALL_DIR` for another path. Use the same wrapper/lock for manual production runs and rotate `var/hourly.log`. No cron entry is installed by building or testing. Hourly observations can track persistent gaps but miss opportunities between samples.
