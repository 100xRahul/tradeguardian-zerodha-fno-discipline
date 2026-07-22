# TradeGuardian

TradeGuardian is a localhost-only Zerodha Kite F&O order gateway with a durable ₹30,000 daily NFO/BFO MTM loss lock.

For a product-level explanation of the implemented behavior, workflows, safety rules, limitations, and VPS model, read [PRODUCT.md](PRODUCT.md).

## Safety behavior

- Polls NFO/BFO net positions, today's executed trades, and orders every second to reconcile the complete account state.
- Calculates daily MTM from today's fill cash flows, the previous-close value of overnight quantity, and paid Kite WebSocket LTP ticks. The same complete live value drives the dashboard and ₹30,000 loss lock; missing or inconsistent trade/position/tick data fails closed.
- At or below −₹30,000, persists `LOCKED`, cancels pending F&O orders, submits protected market exits, and reconciles until flat.
- Treats all nonterminal Kite OMS states as live and tracks forced-exit parent IDs so an uncertain/autosliced exit is not duplicated.
- Uses exact Kite instrument tokens, lot/tick metadata, and reported pending quantities. Missing or inconsistent broker safety data blocks the operation; no symbol, quantity, or tick fallback is used.
- Rejects standalone CE/PE BUY orders. The basket builder permits validated same-underlying/same-expiry hedges, confirms protective BUY IOC fills before submitting SELL legs, and rolls incomplete baskets back.
- Allows standalone option SELL orders, including naked shorts. Standalone CE/PE BUY remains restricted to the validated basket workflow.
- Fails closed when monitoring is unavailable. Cancellation and full position exits remain available.
- Has no manual unlock. Unlock occurs at 09:15 IST on the next configured trading day.

This is an application-level gate. Orders placed directly in Kite are not pre-checked, although their positions affect the monitored MTM.

## Configuration

Export credentials without placing them in a repository file:

```sh
export KITE_API_KEY='your_key'
export KITE_API_SECRET='your_secret'
```

Optional variables are shown in `.env.example`. The running application connects only to the production Kite API. Zerodha published sandbox documentation before launching user access, so no sandbox broker mode is exposed.

`TRADEGUARDIAN_PUBLIC_ORIGIN` is the exact browser origin used for host/origin checks and secure cookies. It defaults to the local URL built from `TRADEGUARDIAN_PORT`.

Register this redirect URL in the Kite developer console:

```text
http://127.0.0.1:8080/auth/callback
```

Start the app:

```sh
go run ./cmd/tradeguardian
```

Open <http://127.0.0.1:8080>, click **Connect Kite**, and complete the daily login.

## VPS deployment

The application remains bound to `127.0.0.1` on the VPS. Tailscale Serve provides private HTTPS at `https://tradeguardian.tail020b72.ts.net`; keep application port 8080 closed publicly and register `https://tradeguardian.tail020b72.ts.net/auth/callback` with Kite.

Do not expose the dashboard until the reverse proxy enforces authentication: this version intentionally has no application login. Run one process against one SQLite database. The planned 4-vCPU/8-GB VPS is more than sufficient for the workload.

## Trading calendar

`config/trading_holidays_2026.json` contains the 2026 equity F&O holidays from NSE circular `NSE/FAOP/71777`, plus the subsequently notified January 15 closure. Before a new calendar year, add and validate a new file and point `TRADING_CALENDAR` to it. Special sessions remain unsupported unless explicitly added to `special_trading_days`.

## Storage and recovery

The SQLite database defaults to `data/tradeguardian.db`, is created with owner-only permissions, and contains lock state, liquidation progress, durable exit intents, idempotency records, and redacted audit events. It contains no Kite credentials or access tokens.

The short-lived access token is stored separately in `data/kite-session.json` (configurable with `TRADEGUARDIAN_SESSION_CACHE`) with owner-only `0600` permissions. This permits same-day service restarts without another login. On startup the token is accepted only before its recorded next-06:00-IST expiry and only after fresh Kite instruments, positions, and orders calls succeed. Expired or broker-rejected tokens are deleted. Zerodha still requires an interactive login once each trading day; TradeGuardian does not automate or bypass it.

Back up the database only while the app is stopped. Restoring an older backup can restore an older lock state, so inspect `trading_state` before starting after a restore. If a lock is active, do not edit the database; let automatic unlock handle it.

Do not include `kite-session.json` in backups. It is a short-lived bearer credential, not recovery data.

## Verification

```sh
gofmt -w .
go test ./...
go test -race ./...
go vet ./...
```

Ordinary tests use deterministic fakes and never place live orders. There are no automatic live-account tests. Zerodha production execution must be checked only through an explicitly approved, minimal controlled test after all risk controls and static-IP configuration are verified.

## API references

- [Kite authentication](https://kite.trade/docs/connect/v3/user/)
- [Kite orders](https://kite.trade/docs/connect/v3/orders/)
- [Kite portfolio positions](https://kite.trade/docs/connect/v3/portfolio/)
- [Kite rate limits](https://kite.trade/docs/connect/v3/exceptions/)
- [Official Go client](https://pkg.go.dev/github.com/zerodha/gokiteconnect/v4)
