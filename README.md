# TradeGuardian

TradeGuardian is a localhost-only Zerodha Kite F&O order gateway with a durable ₹30,000 daily NFO/BFO MTM loss lock.

For a product-level explanation of the implemented behavior, workflows, safety rules, limitations, and VPS model, read [PRODUCT.md](PRODUCT.md).

## Safety behavior

- Polls NFO/BFO net positions every second and sums their `m2m` values.
- At or below −₹30,000, persists `LOCKED`, cancels pending F&O orders, submits protected market exits, and reconciles until flat.
- Treats all nonterminal Kite OMS states as live and tracks forced-exit parent IDs so an uncertain/autosliced exit is not duplicated.
- Rejects standalone CE/PE BUY orders. The basket builder permits validated same-underlying/same-expiry hedges, confirms protective BUY IOC fills before submitting SELL legs, and rolls incomplete baskets back.
- Rejects an individual option SELL if it would leave uncovered short quantity after simulating the resulting position.
- Fails closed when monitoring is unavailable. Cancellation and full position exits remain available.
- Has no manual unlock. Unlock occurs at 09:15 IST on the next configured trading day.

This is an application-level gate. Orders placed directly in Kite are not pre-checked, although their positions affect the monitored MTM.

## Configuration

Export credentials without placing them in a repository file:

```sh
export KITE_API_KEY='your_key'
export KITE_API_SECRET='your_secret'
export KITE_MODE='production'
```

Optional variables are shown in `.env.example`. `KITE_MODE` must be `production` or `sandbox`. Sandbox uses `https://sandbox.kite.trade/oms` for authenticated routes and `https://sandbox.kite.trade` for instrument data.

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

The application remains bound to `127.0.0.1` on a VPS. Put an HTTPS reverse proxy with single-user authentication in front of it, keep the application port closed in the firewall, and set `TRADEGUARDIAN_PUBLIC_ORIGIN` to the exact external origin such as `https://trade.example.com`. Register the corresponding `https://trade.example.com/auth/callback` URL with Kite.

Do not expose the dashboard until the reverse proxy enforces authentication: this version intentionally has no application login. Run one process against one SQLite database. The planned 4-vCPU/8-GB VPS is more than sufficient for the workload.

## Trading calendar

`config/trading_holidays_2026.json` contains the 2026 equity F&O holidays from NSE circular `NSE/FAOP/71777`, plus the subsequently notified January 15 closure. Before a new calendar year, add and validate a new file and point `TRADING_CALENDAR` to it. Special sessions remain unsupported unless explicitly added to `special_trading_days`.

## Storage and recovery

The SQLite database defaults to `data/tradeguardian.db`, is created with owner-only permissions, and contains lock state, liquidation progress, durable exit intents, idempotency records, and redacted audit events. It contains no Kite credentials or access tokens.

Back up the database only while the app is stopped. Restoring an older backup can restore an older lock state, so inspect `trading_state` before starting after a restore. If a lock is active, do not edit the database; let automatic unlock handle it.

## Verification

```sh
gofmt -w .
go test ./...
go test -race ./...
go vet ./...
```

Ordinary tests use fakes and never place live orders. Any sandbox integration test must be explicitly enabled and must use sandbox credentials. There are no automatic live-account tests.

To run the simulated sandbox smoke test after completing the sandbox login flow and obtaining a fresh request token:

```bash
KITE_SANDBOX_SMOKE=1 \
KITE_API_KEY='<sandbox-key>' \
KITE_API_SECRET='<sandbox-secret>' \
KITE_SANDBOX_REQUEST_TOKEN='<one-time-request-token>' \
go test -run TestKiteSandboxSmoke -count=1 ./internal/broker
```

This test reads instruments, LTP, and account state, submits one NFO futures LIMIT order approximately 1% below LTP, and cancels it if it remains pending. It runs only against the explicitly selected Kite sandbox and never runs as part of ordinary tests.

## API references

- [Kite authentication](https://kite.trade/docs/connect/v3/user/)
- [Kite orders](https://kite.trade/docs/connect/v3/orders/)
- [Kite portfolio positions](https://kite.trade/docs/connect/v3/portfolio/)
- [Kite rate limits](https://kite.trade/docs/connect/v3/exceptions/)
- [Kite sandbox](https://kite.trade/docs/connect/v3/sandbox/)
- [Official Go client](https://pkg.go.dev/github.com/zerodha/gokiteconnect/v4)
