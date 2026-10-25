# Commission assumptions

Verified against T-Bank's official PDFs on **2026-10-25**. These are versioned estimates for scenario comparison, not a claim that a hypothetical tariff is active on the account. A qualified-investor designation does not select a brokerage tariff or a Premium service level.

## Brokerage tariffs and Premium levels

Bronze, Silver, Gold and Diamond are real **Premium service levels**. All four connect the same brokerage tariff, **Премиум**: they do not have four different stock/futures commission schedules. This follows from the Premium service terms, p. 8, and the [official investment benefits page](https://www.tbank.ru/bank/help/general/premium/services/investment/), which explicitly makes the reduced commission available at every Premium level.

Percentages below apply to ordinary exchange orders. A buy and a sell each incur commission. Free Investor account service does not mean commission-free trading.

| Scenario | Shares, each transaction | Ordinary MOEX futures, base rate |
| --- | ---: | ---: |
| Investor | 0.30% | 0.10% |
| Trader | 0.05% | 0.04% |
| Premium, all service levels | 0.04% | 0.025% |

The runtime uses exactly these three trading profiles. Monthly subscriptions and service-level eligibility are intentionally excluded from every calculation and alert.

### Futures turnover discounts

Only MOEX futures trades count toward this daily futures turnover. Do not substitute securities turnover, account assets or the Premium level qualification turnover.

| Brokerage tariff | Daily futures turnover band, RUB | Rate |
| --- | --- | ---: |
| Trader | Up to 5,000,000 inclusive | 0.04% |
| Trader | Above 5,000,000 through 10,000,000 inclusive | 0.03% |
| Trader | Above the second threshold | 0.025% |
| Premium, any service level | Up to 12,000,000 | 0.025% |
| Premium, any service level | 12,000,000.01 through 17,000,000 | 0.02% |
| Premium, any service level | Above the second threshold | 0.015% |

The PDFs literally describe the last bands as above **10,000,000.01** and **17,000,000.01**, respectively. Keep the preceding, higher rate at that exact one-kopeck boundary. The documents do not unambiguously explain a fill crossing a threshold or retroactive repricing of earlier fills. The scanner defaults to **zero assumed pre-order turnover**, and should retain the pre-order rate for a crossing order. A supplied turnover is a hypothetical scenario input, not an automatically verified account total.

Futures with special underlying codes `IBIT`, `ETHA`, `SBERF`, `GAZPF`, `BTC`, `ETH` have separate rates: **0.08% Trader / 0.06% Premium**, regardless of daily turnover. Investor charges 0.10%. These are excluded from the initial physical-delivery universe. `SBERF` perpetual futures must not be confused with dated Sber futures merely because both refer to Sber shares.

## Entry, executable exit and physical delivery

Entry buys fully funded shares and sells matching dated physical-delivery MOEX futures. Entry eligibility estimates the carry through delivery, charging:

1. Share purchase commission.
2. Futures sale commission, with the exchange fee floor.
3. Share sale commission on the delivery transaction.

Saved positions can close before expiry: sell the shares against executable bids and buy back the futures against executable asks. This path incurs both exit commissions in addition to both saved entry commissions. The delivery estimate is replaced by these executable exit costs, not charged again.

At physical delivery the securities transaction is charged; do not also invent a futures expiry commission. Cash-settled futures instead incur a futures commission at settlement and require a separate strategy model. After `LastTrade`, this scanner waits for explicitly verified terminal prices and cumulative cash dividends rather than closing against expired books.

Each entry freezes its entry rates/fees, profit thresholds, cash rate and one execution reserve. It also saves the base exit schedules. Exit estimates use that saved schedule for the same named tariff with **zero assumed exit-day turnover**, plus the exchange fee floor refreshed for the observation. Opening-day volume discounts do not carry into a later day; for example, Trader's discounted 0.03% entry rate does not replace its 0.04% base exit rate. Valuation scenario rates/sources describe the exit schedule; original entry fees remain itemized separately. The reserve is deducted once per P&L calculation, not accumulated on every hourly observation. The cash benchmark uses elapsed time and entry capital. The first qualifying profile in Investor → Trader → Premium order remains the minimum eligible tariff for the life of the position; a later, cheaper profile cannot replace its close hurdle. The default close target is 90% of opening gross basis captured, plus that fixed profile clearing both saved profit hurdles.

- **Exchange fee floor:** futures broker commission cannot be less than the exchange organiser's commission. Calculate the greater of the percentage charge and the applicable exchange charge. MOEX exchange fees are included in the broker charge; adding the exchange charge again double-counts it. SPB futures have separate exceptions and are outside this model.
- **Rounding:** tariff rule 1 requires ordinary mathematical rounding to two decimals and a minimum of **0.01 unit of the transaction currency** for a calculated commission. It does not establish whether every possible partial-fill pattern is aggregated before rounding. An order-book level is not necessarily an actual fill; estimates need a small execution/rounding reserve and reconciliation against actual broker previews/reports.
- **Contract value:** use the exchange price-step value and price-step size to convert a quoted futures price into money, then multiply by contracts. Do not assume every future is quoted in RUB per underlying share.
- **Delivery value:** the eventual delivery transaction price is unknown today. Applying today's futures notional to the delivery share commission is an estimate, not an exact locked fee. A stress allowance for the terminal price makes this limitation visible.
- **Funding:** a fully funded stock purchase does not fund future variation margin automatically. Reserve RUB for initial margin, variation margin and fees. The tariffs can charge overnight uncovered balances when futures initial margin exceeds the planned RUB balance. Margin financing is account-level and is not captured by a flat stock borrow APR.
- **Scope:** iceberg orders add 0.01 percentage point to the ordinary share rates (0.31% / 0.06% / 0.05%); they are not simulated. OTC, foreign exchanges and special products require different fee paths. Ordinary two-leg early exit is modeled as described above.
- **Premium issuer privilege:** the service terms waive commission on the first 15 daily T-Technologies share trades and first 15 daily trades per T-Bank bond issue. Later trades use the normal tariff. The initial stock universe excludes `T`; do not assume the waiver without an eligibility check and an execution counter.

`GetOrderPrice` previews estimated commission for the actual account, instrument, direction, quantity and price. It has no alternative-tariff input. Production entries reconcile the active tariff and simulate other profiles locally. Sandbox observations use shared market data and unverified hypothetical fees; no actual-account fee preview is claimed. `GetUserTariff` describes API limits, not brokerage commission rates. [Official orders API](https://developer.tbank.ru/invest/services/orders/methods), [users API](https://developer.tbank.ru/invest/services/accounts/users).

## Curve strategies

Both the OFZ/RGBI pair and the three futures calendars estimate four trade commissions: opening and closing each leg. OFZ purchases reserve the dirty price, including accrued coupon interest (ACI), while the percentage commission uses clean value. This distinction is documented in §28.7, p. 282 of the [brokerage regulations, edition 94, approved 2024-11-28](https://acdn.t-static.ru/static/documents/31ac1430-2157-46dd-af02-42d217b025d3.pdf). That inspected edition is historical; it has not been verified as the applicable 2026 regulation. Production mock entries therefore also require current account-specific `GetOrderPrice` fees to reconcile with the modeled clean-value basis.

The same three trading profiles, futures exchange-fee floors and frozen entry/base-exit schedules apply. Monthly charges remain excluded. Curve entry is blocked if account-specific preview margin exceeds the margin already funded in the candidate model; the stock scanner can instead recalculate capital with the higher preview margin.

Curve positions are intended to close through executable books before the earliest expiry. After `LastTrade` they enter `awaiting_settlement`: v1 has no verified curve terminal-settlement workflow and does not book terminal P&L or assume a zero cash-settled expiry commission. Such a workflow must verify the settlement values and applicable terminal charges before recognizing a closed result.

For USD-quoted relative-value futures such as GOLD/GOLDM, the paper lifecycle accumulates quote-price changes between observations using each observation's verified RUB conversion factor. Previously accrued RUB P&L remains frozen when FX changes. This is an **observation-based approximation**, not a reconstruction of the exchange's clearing variation-margin cash ledger; hourly observations can differ from actual clearing prices and conversion rates. Entry projections use the current conversion factor as a scenario assumption. Fees still use the applicable trade notional and tariff independently.

## Cash shorts and overnight uncovered balances

The current Investor and Trader PDFs, §11.2–11.4, and Premium PDF, §14.2–14.4, charge the following **RUB per calendar day**. A position fully closed intraday has no overnight charge. These are account-level charges, not an annual stock-borrow percentage and not separate charges for every short leg.

| Aggregate uncovered balance, RUB | Investor | Trader | Premium, all levels |
| --- | ---: | ---: | ---: |
| Up to 5,000 inclusive | 0* | 0* | 0* |
| Above 5,000 through 50,000 | 40 | 40 | 35 |
| Through 100,000 | 80 | 75 | 70 |
| Through 250,000 | 190 | 180 | 175 |
| Through 500,000 | 375 | 350 | 340 |
| Through 1,000,000 | 750 | 700 | 680 |
| Through 2,500,000 | 1,850 | 1,750 | 1,700 |
| Through 5,000,000 | 3,700 | 3,500 | 3,400 |
| Through 10,000,000 | 7,200 | 6,900 | 6,800 |
| Through 25,000,000 | 0.070% of balance | 0.068% of balance | 0.066% of balance |
| Through 50,000,000 | 0.067% of balance | 0.065% of balance | 0.063% of balance |
| Above 50,000,000 | 0.059% of balance | 0.057% of balance | 0.055% of balance |

Every upper bound is inclusive. Percentage bands apply to the **whole** uncovered balance, not just the portion above a threshold. In configuration, 0.070% is the decimal daily rate `0.00070`, not `0.070`.

The broker assesses the sum of uncovered RUB, foreign-currency and securities positions within one agreement daily between 00:00 and 04:00 Moscow time, using the preceding trading day's closing valuations. Futures initial margin reduces the planned RUB balance for this purpose. Weekends and holidays therefore remain chargeable when the uncovered balance persists.

\* The free allowance applies to no more than three simultaneously uncovered trading accounts. From the fourth account, a balance of up to 5,000 RUB costs 5 RUB per day. The broker selects the smallest uncovered balances first; equal balances are ordered by the newer account opening date. Do not assume the exemption when the client's other accounts are unknown.

A new trade can change the tariff band for the entire account. Given verified pre-existing exposure, its incremental daily cost is `fee(existing + added) - fee(existing)`; a standalone zero-balance estimate is a separately labeled scenario. Unknown existing exposure must not silently become zero. Saved mock positions need dated fee observations or an explicitly labeled estimate for every chargeable calendar day, including days without a scan. Multiple mock positions sharing one account must not each claim the same free allowance or independently ignore the combined tier.

## TGLD commissions, fund expenses and reference values

[T-Bank's current fund-fee help](https://www.tbank.ru/invest/help/services/capital/about/how-to-invest/) explicitly lists `TMOS`, `TRUR` and `TGLD` as free for the first **15 orders per day**, with the normal brokerage tariff from the 16th order. This fund allowance is distinct from Premium issuer benefits. An order is not an order-book level or a partial fill. Until the applicable allowance and consumed order count are verified, estimate the ordinary ETF transaction fee on both entry and exit; do not presume a new allowance at every scan or reserve a future day's exemption. The general tariff PDFs defer the specific fee for T-Capital fund units to the order preview, so reconcile the actual account preview/report as well.

The [TGLD key information document dated 2026-09-30](https://cdn.t-capital-funds.ru/kids/TGLD/2026/09/61184907-5a5f-4168-95be-90d83d24deac/kid_TGLD_2026_09.pdf), §6, published 2026-10-06, caps annual fund remuneration and expenses at **2.369% of NAV** (`0.02369` as a decimal annual rate). These expenses are embedded in its NAV and market value, not a second cash debit on each ETF trade. A forward fair-value projection may use that dated cap; a realized mark based on observed ETF prices must not deduct the same embedded expenses again. The KID's absence of a subscription/redemption markup does not waive brokerage trading fees.

MOEX publishes a RUB-denominated indicative NAV for the fund as [TGLDB](https://www.moex.com/ru/index/TGLDB?index=inav), available through the [official ISS index endpoint](https://iss.moex.com/iss/engines/stock/markets/index/securities/TGLDB.json?iss.meta=off). Validate `SECID=TGLDB`, `BOARDID=INAV` and `CURRENCYID=RUB`; use `marketdata.CURRENTVALUE`, with the observation timestamp constructed from `TRADEDATE` and `TIME` in Moscow time. `LASTVALUE` is not the current iNAV, and HTTP retrieval time or `SYSTIME` must not replace the valuation time. A RUB iNAV needs no additional FX multiplier. Entry eligibility requires its timestamp to agree with both current traded-leg quotes within the configured `max_quote_skew`. Delayed or stale iNAV remains a visible monitor and cannot authorize an entry merely because the endpoint was fetched recently.

The manager's [daily statistics](https://t-capital-funds.ru/statistics/) separately publishes TGLD's closed-day NAV **in USD**, despite the broker's RUB trading instrument `TGLD@` (same ISIN `RU000A101X50`). This is a fallback reference, not intraday iNAV: require a matching-date verified USD/RUB conversion, and apply the same quote-time eligibility gate. Do not interpret an overnight gold-price move as a fund discount by comparing current quotes with an unchanged prior-day NAV. Keep the NAV date separate from the holdings date.

The [2026-09-30 holdings report](https://cdn.t-capital-funds.ru/static/documents/otchet-scha-tgld-30-09-2026.PDF), published 2026-10-19, supports a dated gold-exposure reference: 340,137.2 grams of physical gold plus claims equivalent to 1,002,242 grams, divided by 1,100,753,262.11268 units, approximately `0.0012195096269109086` grams per unit. This calculation is a historical exposure estimate, not a claim that current holdings or a current redemption entitlement are unchanged.

## Perpetual funding and dividends

The [October 2026 MOEX perpetual-futures guide](https://www.moex.com/media/4218-gid-vechnye-fyuchersy-birzha.pdf), §§7, 13–15, distinguishes indicative intraday funding from the final clearing payment. Historical final values are available from the official ISS history endpoints for [GLDRUBF](https://iss.moex.com/iss/history/engines/futures/markets/forts/securities/GLDRUBF.json) and [CNYRUBF](https://iss.moex.com/iss/history/engines/futures/markets/forts/securities/CNYRUBF.json): `history.TRADEDATE` identifies the trading day and `SWAPRATE` is the funding amount per underlying unit. For these RUB-quoted contracts, multiply by the contract's underlying lot: 1 gram for GLDRUBF and 1,000 CNY for CNYRUBF. A positive value is paid by the long to the short; signed cash flow is `-contracts_long_positive × SWAPRATE × underlying_lot`. The date-only historical row does not provide an exact payment timestamp. Position entitlement must follow the applicable clearing cutoff; absence of a row is not evidence of zero funding.

Funding must be recorded once per instrument and eligible trading date. Historical averages are forecast inputs, not guaranteed future receipts. Closing the position before the clearing cutoff changes entitlement. Neither historical funding nor favorable funding forecasts replace the entry and exit broker commissions.

Cash-share dividends and any compensation owed on a cash short require their own dated inputs. Do not assume a 13% personal withholding rate from the account's qualification, tariff or country of residence. Tax treatment and the short-dividend debit are separate configurable amounts/rates; unknown inputs must remain visible. MOEX's guide §21 separately states that perpetual dividend adjustments use gross dividends and form part of variation margin, so they must not be substituted for net cash-share dividends.

## Auditable sources

PDF footer identifiers below are document versions, not independently established legal effective dates. Files were fetched on 2026-10-25 from the official alternate CDN `acdn.t-static.ru/static/documents/` using the same filenames. Canonical T-Bank URLs are linked; no source PDFs are committed.

| Document | PDF footer version | SHA-256 of inspected file |
| --- | --- | --- |
| [Investor tariff](https://cdn.tbank.ru/static/documents/invest-tariff-fees.pdf) | `Т-ИНВЕСТ-260424` | `1c37257d54646605fc8e59f616a3623284f058c902fb0a3b5b6559ddf8cc4a2a` |
| [Trader tariff](https://cdn.tbank.ru/static/documents/invest-tariff-trader.pdf) | `Т-ТРЕЙД-260522` | `cfce48db8c31657b8eed6833a7ad3866b114175ac9a4bb2f737733ae31575164` |
| [Premium brokerage tariff](https://cdn.tbank.ru/static/documents/invest-tariff-premium.pdf) | `Т-ПРЕМ-260603` | `7cb9391b289153c17a646a0d23b31d9df252cfba9499e76059a06549debc82fd` |
| [Premium service terms](https://cdn.tbank.ru/static/documents/docs-terms-of-service-premium.pdf), p. 8 trading benefits | `УСЛ-ПС-PREMIUM-260930` | `c9cee855fc71b79afea1dd4e70a84af9c97600692d73483bd98fb3e8586bca0f` |
| [Special futures underlying list](https://cdn.tbank.ru/static/documents/basic-list-of-securities-for-trading-futures-contracts.pdf) | `БАЗ-СП-ЦБ-ФК-260203` | `f0b2b878d553962866f323cc1c05c5ae912406f6fb256bbda48d6425cb8bfcbc` |

Cross-checks: [brokerage tariffs](https://www.tbank.ru/invest/help/brokerage/account/get-bs/tariff/), [futures fees and settlement](https://www.tbank.ru/invest/help/brokerage/account/forts/trade-futures/), [Premium level terms](https://www.tbank.ru/bank/help/general/premium/access/terms/). Detailed current PDFs take precedence over marketing summaries and archived documents. The guessed filename `invest-tariff-investor.pdf` is an older schedule and is not the current Investor source above.
