# TradeGuardian F&O Risk-Control Application

## Summary

Build a single-user Go web application bound to `127.0.0.1` that places, modifies, cancels, and deploys validated Zerodha Kite F&O baskets while enforcing a persistent ₹30,000 daily loss limit. Standalone CE/PE BUY orders remain rejected; option BUYs are permitted only inside a validated hedge basket.

## Architecture

- Use Go, `github.com/zerodha/gokiteconnect/v4`, embedded server-rendered templates, minimal JavaScript, SSE, and SQLite.
- Separate broker, risk, trading, monitoring, persistence, HTTP, and UI concerns.
- Read `KITE_API_KEY` and `KITE_API_SECRET` from the environment; never expose or persist them. Keep the daily access token in memory.
- Support Kite login/callback, graceful shutdown, explicit production/sandbox selection, and deterministic fake brokers in tests.

## Risk policy

- Support regular NFO/BFO MARKET, LIMIT, SL, and SL-M orders only.
- Allow futures BUY/SELL. Reject standalone CE/PE BUY with `OPTION_BUY_FORBIDDEN`; allow an individual option SELL only when the resulting same-type, same-underlying, same-expiry position remains protected by existing long quantity.
- Permit 2–4-leg same-underlying, same-expiry option baskets when they contain BUY and SELL legs, every BUY type is paired, and protective long quantity covers short quantity for each CE/PE type.
- Simulate a validated basket together with same-product filled positions and pending SELL exposure; reject it if an affected CE/PE group would retain uncovered short quantity. Never count a pending BUY as protection until its fill appears in positions, and never use MIS quantity to protect NRML quantity or vice versa. The displayed maximum loss remains basket-only, not whole-account risk.
- Require basket legs to be IOC limit orders so maximum planned expiry loss can be calculated before submission and fills terminate promptly.
- Deploy protective BUY legs first, confirm fills, then deploy SELL legs. On any incomplete phase, cancel outstanding legs, close filled shorts first, and unwind only quantities filled by that basket. No generic client bypass exists.
- Never unwind protective long fills while final short fills or short-closing rollback orders are uncertain; retain protection and fail closed for attention.
- Persist `ACTIVE` or `LOCKED`; expose `AUTH_REQUIRED`, `READY`, `MONITORING_DEGRADED`, `BASKET_DEPLOYING`, and `LIQUIDATING` runtime states.
- Serialize risk decisions and submission. Conservatively cache accepted orders as pending immediately so rapid requests cannot reuse stale hedge coverage; require a fresh portfolio poll after basket deployment or explicit exits. When locked, reject placements and modifications with `Trading is locked for today.` Cancellations remain available.
- Poll positions immediately after authentication and once per second thereafter. Sum NFO/BFO net-position `m2m`, converted to paise.
- At MTM `<= -₹30,000`, persist `LOCKED` first, publish `Daily Loss Limit Reached. Trading Locked Until Tomorrow.`, cancel pending F&O orders, exit all nonzero F&O positions, and reconcile until flat.
- Sum included `m2m` values before the single rupees-to-paise rounding boundary. Reserve idempotency keys durably before broker submission so an uncertain response cannot be retried as a duplicate.
- If monitoring fails, fail closed for placements/modifications while permitting cancellation and explicit risk-reducing exits.
- Direct Kite orders are outside the pre-trade gate but still affect monitored MTM.

## Unlock and persistence

- Persist lock date, trigger MTM/time, unlock time, liquidation progress, and append-only audit events in SQLite.
- Unlock automatically at 09:15 Asia/Kolkata on the next weekday not listed in the exchange-holiday file, including after restart. There is no manual unlock route or control.
- Withhold scheduled unlock while liquidation is not reconciled `COMPLETED`; unresolved broker risk remains locked and requires reauthentication/reconciliation.
- Validate the holiday file and document its annual maintenance.

## HTTP and UI

- Provide dashboard, Kite login/callback, instrument search, order ticket, hedge-basket builder, positions, orders, audit, settings, and health views.
- Provide typed status/positions/orders/instruments/place/modify/cancel endpoints and SSE state updates.
- Protect mutations with origin checks, CSRF, size limits, strict decoding, idempotency keys, and server-side validation.
- Let the user search and explicitly select broker instruments; show contract metadata and derive broker quantity from a whole number of lots. Keep only relevant price/trigger inputs enabled.
- Continuously show authentication, trading/runtime state, MTM, threshold, refresh time, positions, orders, and liquidation progress.

## Public contracts

- `TradingStatus`: `ACTIVE | LOCKED`.
- `RuntimeStatus`: `AUTH_REQUIRED | READY | MONITORING_DEGRADED | BASKET_DEPLOYING | LIQUIDATING`.
- `RiskDecision`: allowed flag, stable code, message, evaluated MTM, trading status, and timestamp.
- `Broker`: session, positions, orders, place, modify, cancel, instruments, and reconciliation operations.
- Stable rejection codes include `TRADING_LOCKED`, `AUTH_REQUIRED`, `MONITORING_DEGRADED`, `UNSUPPORTED_SEGMENT`, `UNSUPPORTED_VARIETY`, `OPTION_BUY_FORBIDDEN`, and `UNHEDGED_OPTION_EXPOSURE`. `HEDGE_POLICY_PENDING` remains reserved for backward compatibility but is no longer emitted by the implemented basket policy.

## Tests and acceptance

- Cover policy decisions, exact threshold behavior, segment filtering, rounding, degraded monitoring, concurrency, idempotency, partial failures, liquidation reconciliation, restart recovery, calendar unlock, auth/CSRF/input validation, and redaction.
- Run formatting, `go test ./...`, `go test -race ./...`, `go vet ./...`, and opt-in sandbox smoke tests. Never run live-account order tests automatically.
- No browser request may bypass a lock or option-BUY prohibition. Locks survive restart. Liquidation is never reported complete before reconciliation.

## Hedge basket policy

- Hedge approval is basket-scoped and single-use; it never authorizes a later standalone BUY or modification.
- Version one supports same-expiry vertical spreads, iron condors, and iron flies expressible with 2–4 option legs and fully covered CE/PE short quantities.
- Calendar spreads, ratio spreads, cross-underlying baskets, futures-option combinations, saved approvals, and reuse remain unsupported until separately specified.
- Kite has no atomic basket API. The coordinator therefore uses validated, tagged IOC phases and deterministic rollback rather than a temporary discretionary hedge window.

## Assumptions

- One user, deployed as a single process on a 4-vCPU/8-GB VPS. The application remains loopback-only behind an authenticated TLS reverse proxy.
- Fixed ₹30,000 limit, with fees and taxes excluded.
- All NFO/BFO positions and orders are in liquidation scope.
- Broker cancellation and exits are best effort; the lock remains durable after failures.
