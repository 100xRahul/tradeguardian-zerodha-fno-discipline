# TradeGuardian Agent Guide

## Safety invariants

- Treat this as financial risk-control software. Preserve fail-closed behavior and deterministic decisions.
- Never add a browser/client-controlled bypass for locks, option BUY rules, monitoring health, or forced liquidation.
- Standalone CE/PE BUY orders remain rejected. Basket BUYs may only pass through the validated internal basket coordinator; never expose its capability as a generic bypass.
- Preserve the basket invariant: same underlying/expiry/product, paired BUY/SELL option types inside the basket, BUY fills confirmed before SELL submission, and short closures confirmed before any protective long is unwound during rollback. Standalone option SELL and pre-existing naked shorts are allowed by product policy; never apply that permission to standalone option BUY.
- Persist `LOCKED` before cancellation or liquidation. Never expose a manual unlock endpoint, flag, or UI control.
- Cancellations are allowed while locked or degraded. Forced liquidation is an internal-only capability.
- Never report liquidation complete until broker reconciliation confirms no pending NFO/BFO orders and no nonzero NFO/BFO positions.
- Reserve idempotency durably before broker placement/modification. An uncertain broker response must never be retried automatically with the same key.
- Reserve a durable, position-specific liquidation intent before every forced/explicit exit broker call. Never clear or replace an uncertain same-day intent without broker reconciliation; recognise autoslice children by parent order ID.
- Never invent missing broker data. Use exact Kite instrument tokens, lot/tick metadata, and reported order quantities; do not parse symbols, substitute a default tick, derive pending quantity, or inherit omitted modify fields. Missing or inconsistent safety data must fail closed.

## Go practices

- Write idiomatic Go with small interfaces, dependency injection, explicit errors, contexts, timeouts, bounded retries, and graceful shutdown.
- Do not panic for runtime/configuration/broker/storage failures. Return or log redacted errors and retain safe state.
- Use integer paise for policy comparisons. Keep clocks injectable and state transitions race-safe.
- Wrap errors with context. Use structured logging and never log API secrets, access tokens, request tokens, or auth headers.
- Keep HTTP handlers thin. Domain rules belong in the risk/trading services; Kite-specific types belong behind the broker adapter.

## Sources and changes
- <<<< IMportant and MUST FOLLOW RULE : DONT DO GUESS WORKS try to refer docs,sdk code whenever in need/confused.  while fetching kite docs just append to url /index.md to get it in MARKDOWN format. else it will return the raw HTML even if we do content negotiation. >>>>
- Use the Markdown Kite Connect documentation under `https://kite.trade/docs/connect/v3/` and official Go package docs as sources of truth.
- Preserve the broker abstraction and deterministic fake-broker test path. The running adapter is production-only until Zerodha launches generally available sandbox users; do not reintroduce a documentation-only sandbox route.
- Never commit credentials or real account/order data. Live-order tests must always be opt-in and must never run in ordinary test commands.
- Add or update tests for every risk-rule, state-machine, persistence, calendar, or order-flow change.
- Update `plan.md` before widening the hedge policy to calendars, ratios, mixed futures/options, cross-expiry, or reusable approvals.

## Verification

- Run `gofmt` on changed Go files.
- Run `go test ./...`, `go test -race ./...`, and `go vet ./...` before handoff.
- Keep the application bound to `127.0.0.1` unless the security model and plan are explicitly revised.

User Requests
- if a user need any changes , try to think from a product mindset and not just coding perspective , if user has feature requests, you can try to ask him some questions so that you dont get fixated on just what he is saying but you understand the reletaed things he want for it. eg (i told you to implement a feature that while exiting position give option to put quantity but you didnt gave increase  decrease buttons, thats incomplete product understanding and mindset.