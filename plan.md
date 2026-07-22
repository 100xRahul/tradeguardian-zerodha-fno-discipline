# TradeGuardian F&O Risk-Control Application

## Summary

Build a single-user Go web application bound to `127.0.0.1` that places, modifies, cancels, and deploys validated Zerodha Kite F&O baskets while enforcing a persistent ₹30,000 daily loss limit. Standalone CE/PE BUY orders remain rejected; option BUYs are permitted only inside a validated hedge basket.

## Architecture

- Use Go, `github.com/zerodha/gokiteconnect/v4`, embedded server-rendered templates, minimal JavaScript, server-side Kite WebSocket streaming, browser SSE, and SQLite.
- Separate broker, risk, trading, monitoring, persistence, HTTP, and UI concerns.
- Read `KITE_API_KEY` and `KITE_API_SECRET` from the environment; never expose them to the browser, logs, SQLite, or repository. Persist the daily access token only in a dedicated owner-only (`0600`) cache outside SQLite, record its next 06:00 IST expiry, validate it through fresh broker reads after restart, and delete it when expired or rejected.
- Support production Kite login/callback, graceful shutdown, and deterministic fake brokers in tests. Do not expose a nonfunctional sandbox mode; Zerodha has confirmed sandbox user access is not launched.

## Risk policy

- Support regular NFO/BFO MARKET, LIMIT, SL, and SL-M orders only.
- Allow futures BUY/SELL and standalone CE/PE SELL, including naked short options. Reject standalone CE/PE BUY with `OPTION_BUY_FORBIDDEN`; option BUY remains available only through the validated hedge-basket coordinator.
- Permit 2–4-leg same-underlying, same-expiry option baskets when they contain BUY and SELL legs, every BUY type is paired, and protective long quantity covers short quantity for each CE/PE type.
- Validate the new basket itself as same-product paired option BUY/SELL legs. Existing naked short positions and ordinary standalone SELL orders do not require long-option coverage. Never turn this permission into a standalone option-BUY bypass.
- Support a basket-wide `LIMIT` or `MARKET` order type. Use IOC for every leg, automatic Kite market protection for MARKET legs, and the same BUY-first/fill-confirmed sequencing. Calculate maximum planned expiry loss before submission only for LIMIT baskets; report it as unavailable for MARKET baskets because execution prices are unknown until fill.
- Deploy protective BUY legs first, confirm fills, then deploy SELL legs. On any incomplete phase, cancel outstanding legs, close filled shorts first, and unwind only quantities filled by that basket. No generic client bypass exists.
- Never unwind protective long fills while final short fills or short-closing rollback orders are uncertain; retain protection and fail closed for attention.
- Persist `ACTIVE` or `LOCKED`; expose `AUTH_REQUIRED`, `READY`, `MONITORING_DEGRADED`, `BASKET_DEPLOYING`, and `LIQUIDATING` runtime states.
- Serialize risk decisions and submission. After any accepted placement or modification, enter a fail-closed reconciliation state until fresh Kite positions and orders are available; never fabricate a temporary pending quantity from the submitted request. Require the same fresh portfolio poll after basket deployment or explicit exits. When locked, reject placements and modifications with `Trading is locked for today.` Cancellations remain available.
- Treat Kite fields as authoritative without inferred fallbacks: require exact instrument-token identity, exact positive broker lot/tick metadata, exact integer quantities, and explicit modification fields. Never derive a missing pending quantity; use exact broker values for order display, cancellation, and reconciliation.
- Poll net positions, today's executed trades, and orders immediately after authentication and once per second thereafter. Reconcile every trade to the exact position token, product, exchange, symbol, and resulting quantity; never use delayed `buy_m2m`/`sell_m2m` as the live realised component. Keep REST `m2m` as a conservative threshold backstop, but never treat its delayed `last_price` as live.
- Require the paid server-side Kite WebSocket to be connected before allowing new exposure. Subscribe to exact tokens of every current nonzero position and calculate live daily MTM as `today's sell fills − today's buy fills − overnight quantity × previous close + net quantity × WebSocket LTP`, with Kite's position multiplier applied to every term. Every complete tick state drives both the dashboard and loss lock. A disconnected stream, missing trade/position identity, quantity mismatch, missing position tick, or invalid price degrades monitoring and blocks placements/modifications. Reconcile immediately after WebSocket order updates and coalesce browser frames without coalescing away a threshold breach.
- At MTM `<= -₹30,000`, persist `LOCKED` first, publish `Daily Loss Limit Reached. Trading Locked Until Tomorrow.`, cancel pending F&O orders, exit all nonzero F&O positions, and reconcile until flat. Treat every nonterminal Kite OMS state as live, retain forced-exit parent intents across retries/restart, recognise autoslice children, and cancel orphan exits that could reverse a flat position.
- Sum included `m2m` values before the single rupees-to-paise rounding boundary. Reserve idempotency keys durably before broker submission so an uncertain response cannot be retried as a duplicate.
- If monitoring fails, fail closed for placements/modifications while permitting cancellation and explicit risk-reducing exits.
- Direct Kite orders are outside the pre-trade gate but still affect monitored MTM.

## Unlock and persistence

- Persist lock date, trigger MTM/time, unlock time, liquidation progress, pre-submission liquidation intents, and append-only audit events in SQLite.
- Unlock automatically at 09:15 Asia/Kolkata on the next weekday not listed in the exchange-holiday file, including after restart. There is no manual unlock route or control.
- Withhold scheduled unlock while liquidation is not reconciled `COMPLETED`; unresolved broker risk remains locked and requires reauthentication/reconciliation.
- Validate the holiday file and document its annual maintenance.

## HTTP and UI

- Provide dashboard, Kite login/callback, instrument search, order ticket, hedge-basket builder, positions, orders, audit, settings, and health views.
- Provide typed status/positions/orders/instruments/place/modify/cancel endpoints and SSE state updates.
- Protect mutations with origin checks, CSRF, size limits, strict decoding, idempotency keys, and server-side validation.
- Let the user search and explicitly select broker instruments; show contract metadata and derive broker quantity from a whole number of lots. Keep only relevant price/trigger inputs enabled.
- Never substitute a hard-coded browser tick or infer an underlying from a trading symbol. If required catalogue metadata is absent, show it as unavailable and block the operation server-side.
- Continuously show authentication, trading/runtime state, MTM, threshold, refresh time, positions, orders, and liquidation progress.

## Public contracts

- `TradingStatus`: `ACTIVE | LOCKED`.
- `RuntimeStatus`: `AUTH_REQUIRED | READY | MONITORING_DEGRADED | BASKET_DEPLOYING | LIQUIDATING`.
- `RiskDecision`: allowed flag, stable code, message, evaluated MTM, trading status, and timestamp.
- `Broker`: session, positions, orders, place, modify, cancel, instruments, and reconciliation operations.
- Stable rejection codes include `TRADING_LOCKED`, `AUTH_REQUIRED`, `MONITORING_DEGRADED`, `UNSUPPORTED_SEGMENT`, `UNSUPPORTED_VARIETY`, and `OPTION_BUY_FORBIDDEN`. `UNHEDGED_OPTION_EXPOSURE` and `HEDGE_POLICY_PENDING` remain reserved for backward compatibility but are not emitted by the current policy.

## Tests and acceptance

- Cover policy decisions, exact threshold behavior, segment filtering, rounding, degraded monitoring, concurrency, idempotency, partial failures, liquidation reconciliation, restart recovery, calendar unlock, auth/CSRF/input validation, and redaction.
- Run formatting, `go test ./...`, `go test -race ./...`, and `go vet ./...`. Never run live-account order tests automatically.
- No browser request may bypass a lock or option-BUY prohibition. Locks survive restart. Liquidation is never reported complete before reconciliation.

## Hedge basket policy

- Hedge approval is basket-scoped and single-use; it never authorizes a later standalone BUY or modification. Standalone option SELL does not require hedge approval.
- Version one supports same-expiry vertical spreads, iron condors, and iron flies expressible with 2–4 option legs and fully covered CE/PE short quantities.
- Calendar spreads, ratio spreads, cross-underlying baskets, futures-option combinations, saved approvals, and reuse remain unsupported until separately specified.
- Kite has no atomic basket API. The coordinator therefore uses validated, tagged IOC phases and deterministic rollback rather than a temporary discretionary hedge window.

## Assumptions

- One user, deployed as a single process on a 4-vCPU/8-GB VPS. The application remains loopback-only behind an authenticated TLS reverse proxy.
- Fixed ₹30,000 limit, with fees and taxes excluded.
- All NFO/BFO positions and orders are in liquidation scope.
- Broker cancellation and exits are best effort; the lock remains durable after failures.
