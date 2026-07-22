# TradeGuardian VPS Deployment

This is the operational source of truth for deploying TradeGuardian to the
single production VPS. It contains no passwords, API secrets, access tokens, or
Tailscale authentication material.

## Target and security model

- Host: `147.93.169.168`
- OS observed on 2026-07-22: Ubuntu 24.04 LTS, x86-64
- Capacity: 4 vCPU, 8 GB RAM
- Public IPv4 used for Kite order egress: `147.93.169.168`
- The host also has public IPv6, but the production Kite REST and WebSocket adapters
  deliberately dial IPv4 so all broker traffic has deterministic egress. Register
  `147.93.169.168` in Kite's IP whitelist.
- TradeGuardian listens only on `127.0.0.1:8080`.
- Tailscale Serve is the only dashboard ingress and provides tailnet-private
  HTTPS. Port 8080 must never be opened publicly.
- Both authorised people may have identical dashboard access. Access control is
  enforced by Tailscale identity and tailnet grants; neither person's home
  network needs a static IP.
- Zerodha sees requests made by the VPS, not dashboard users. Register the
  VPS's actual static outbound address in the Kite developer IP whitelist.

Traffic flow:

```text
authorised device -> Tailscale -> private HTTPS hostname -> Tailscale Serve
                  -> 127.0.0.1:8080 TradeGuardian -> Kite API
                                                     (VPS static egress IP)
```

## Current VPS state

As of 2026-07-22:

- Tailscale 1.98.9 was installed from Tailscale's official Ubuntu `noble`
  repository.
- `tailscaled` was enabled by the package installation.
- Tailnet authentication completed successfully. `tailscaled` is enabled and
  active.
- Tailnet IPv4: `100.95.112.55`
- Private DNS name: `tradeguardian.tail020b72.ts.net`
- UFW was inactive and only SSH port 22 was listening publicly before the
  Tailscale installation.
- Ubuntu reported that a reboot is required to load kernel
  `6.8.0-136-generic`; the host was not rebooted automatically.
- The unavailable Kite sandbox service, environment file, and isolated database
  were removed on 2026-07-22 after Zerodha confirmed that sandbox user access
  had not launched. Port 13173 is no longer used.
- The verified production-only binary is installed at
  `/opt/tradeguardian/tradeguardian` with SHA-256
  `e2ad71b911266a4144451d93eedf87c3b30623d1041c2910d371a85eab23ed04`.
- `tradeguardian.service`, the unprivileged service user, production data
  directory, calendar, and root-only environment are installed. The service is
  enabled and active with production credentials supplied directly for this
  deployment; credentials are not recorded in this document or repository.
- Tailscale Serve is active at
  `https://tradeguardian.tail020b72.ts.net`, proxying privately to
  `127.0.0.1:8080`. Funnel is not enabled. The HTTPS endpoint returned 200 and
  local health reported `AUTH_REQUIRED`/`ACTIVE` before the daily Kite login.
- The dashboard requires Kite WebSocket connectivity before enabling new
  exposure. The deployed build calculates daily live MTM from today's exact
  trade fills, the overnight previous-close anchor, the current net quantity and
  WebSocket LTP. It reconciles every fill to its position and fails closed on a
  mismatch; delayed REST `m2m` remains a conservative backstop and diagnostic.
- Zerodha payment and a fresh Connect-app login were completed on 2026-07-22.
  After the 10:20 CEST deployment, the cached session restored successfully,
  runtime reported `READY`, the paid WebSocket reported `LIVE`, and private
  Tailscale HTTPS returned 200. The build includes tradebook-based realised P&L,
  standalone naked option SELL permission, MARKET/LIMIT hedge baskets, blank-row
  handling, leg deletion, and an authenticated `Kite connected` button state.

## Tailnet enrolment and two-user access

1. The tailnet owner completes the one-time URL printed by `tailscale up`.
2. In the Tailscale admin console, give the VPS a stable descriptive name such
   as `tradeguardian` and disable key expiry for this server only if that is an
   accepted operational choice.
3. Keep the owner in the main tailnet. Share only the `tradeguardian` machine
   with the second authorised person instead of adding them as a tailnet Admin;
   this prevents visibility of the owner's Mac and other machines.
4. Add a narrow grant allowing the two authorised identities to reach the
   shared VPS on TCP 443. Administrative SSH remains owner-only.
5. Verify both devices can reach only the intended resources before changing
   SSH or firewall rules.

The private HTTPS name is:

```text
https://tradeguardian.tail020b72.ts.net
```

After the application is listening locally, publish only that service:

```sh
tailscale serve --bg http://127.0.0.1:8080
tailscale serve status
```

Do not use Tailscale Funnel; Funnel would make the dashboard publicly
reachable.

## Kite configuration

In the Kite developer dashboard:

1. Register the VPS's verified static outbound IP in **IP Whitelist**.
2. Set the production redirect URL to the exact private HTTPS callback:

   ```text
   https://tradeguardian.tail020b72.ts.net/auth/callback
   ```

3. Use the same origin in the TradeGuardian environment:

   ```text
   TRADEGUARDIAN_PUBLIC_ORIGIN=https://tradeguardian.tail020b72.ts.net
   ```

The browser completing Kite login must be connected to Tailscale so it can
follow the callback. The Kite API key and secret stay on the VPS. The daily
access token is stored separately at `/var/lib/tradeguardian/kite-session.json`
with owner-only permissions, allowing a same-day service restart to restore and
validate it. Kite invalidates access tokens at about 06:00 the following day,
so the user still completes one interactive Kite login per trading day.

Before live orders, verify the address Kite actually sees. The adapter forces
IPv4 and the expected address is `147.93.169.168`. Do not configure a Tailscale
exit node for this VPS, because that could change Kite's observed source
address.

## Filesystem layout

Recommended layout:

```text
/opt/tradeguardian/tradeguardian        root-owned executable
/opt/tradeguardian/config/              versioned holiday configuration
/var/lib/tradeguardian/                 SQLite database, token cache, and backups
/etc/tradeguardian/tradeguardian.env    root-readable environment file
```

The environment file must be mode `0600` and must never enter Git, backups
without encryption, shell history, logs, or support messages. Required values:

```text
KITE_API_KEY=<production-api-key>
KITE_API_SECRET=<production-api-secret>
TRADEGUARDIAN_PORT=8080
TRADEGUARDIAN_PUBLIC_ORIGIN=https://tradeguardian.tail020b72.ts.net
TRADEGUARDIAN_DB=/var/lib/tradeguardian/tradeguardian.db
TRADEGUARDIAN_SESSION_CACHE=/var/lib/tradeguardian/kite-session.json
TRADING_CALENDAR=/opt/tradeguardian/config/trading_holidays_2026.json
```

## Service definition

Install a dedicated unprivileged system user and run exactly one process against
the SQLite database. The eventual `/etc/systemd/system/tradeguardian.service`
should use these controls:

```ini
[Unit]
Description=TradeGuardian F&O risk gateway
After=network-online.target tailscaled.service
Wants=network-online.target
Requires=tailscaled.service

[Service]
Type=simple
User=tradeguardian
Group=tradeguardian
EnvironmentFile=/etc/tradeguardian/tradeguardian.env
WorkingDirectory=/var/lib/tradeguardian
ExecStart=/opt/tradeguardian/tradeguardian
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/tradeguardian
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
```

Validate locally on the server before publishing it:

```sh
systemctl daemon-reload
systemctl enable --now tradeguardian
systemctl status tradeguardian
curl --fail --show-error --silent http://127.0.0.1:8080/ >/dev/null
```

Do not enable the production service until the production API credentials,
holiday file, public origin, static-IP whitelist, and tailnet restrictions have
all been independently checked.

## Firewall and SSH sequence

Avoid lockout by applying controls in this order:

1. Confirm Tailscale enrolment and note the server's Tailscale IP/name.
2. Confirm both authorised devices can access the server through Tailscale.
3. Configure and verify Tailscale SSH or ordinary SSH over the Tailscale IP.
4. Enable UFW with outbound traffic allowed, Tailscale traffic allowed, and
   port 8080 denied publicly.
5. Only after a second verified private SSH session, decide whether public port
   22 should be restricted or closed.

Never close the existing SSH session while changing firewall or SSH policy.

## Verification before real trading

- Confirm the dashboard is unreachable through the VPS public IP.
- Confirm both authorised users can open the private HTTPS dashboard.
- Confirm an unauthorised tailnet identity cannot reach it.
- Confirm HTTPS, host/origin validation, secure cookies, and Kite callback.
- Confirm `api.kite.trade` sees the whitelisted static egress address.
- Run `go test ./...`, `go test -race ./...`, and `go vet ./...` on the exact
  revision being deployed.
- Back up SQLite while the service is stopped, or use SQLite's online backup
  mechanism; never copy only the main file while WAL writes may be active.
- Exclude `/var/lib/tradeguardian/kite-session.json` from backups. It is a
  short-lived bearer credential, not durable application state.
- Restore a backup into a separate path and verify lock/audit records before
  relying on the backup procedure.
- Confirm system time synchronisation and `Asia/Kolkata` policy behavior.
- Confirm monitoring becomes `READY` only after fresh positions and orders are
  fetched.
- Confirm standalone option BUY rejection and basket validation.
- Confirm the loss lock persists across a service restart.
- Do not intentionally trigger a live loss-limit liquidation as a deployment
  test. Validate that path with the fake broker and perform only explicitly
  approved minimal live checks.

## Routine operations

```sh
systemctl status tradeguardian
journalctl -u tradeguardian --since today
tailscale status
tailscale serve status
```

Logs must remain redacted. Never paste the environment file, Kite request token,
access token, authentication headers, or SQLite production data into tickets or
chat.

Update the versioned exchange-holiday file before each calendar year and deploy
it only after validation. A missing supported trading session intentionally
fails closed.

## Recovery

- If Kite authentication expires, the dashboard shows `AUTH_REQUIRED`; reconnect
  through the normal Kite login flow.
- If monitoring is degraded, placements/modifications remain blocked until a
  completely successful positions/orders refresh restores `READY`.
- If liquidation is unresolved, retain `LOCKED` and restore broker connectivity;
  never edit SQLite to unlock it.
- If Tailscale is unavailable, use the retained VPS console or carefully
  controlled public SSH access. Do not expose port 8080 as a workaround.
- After restoring the database, start only one TradeGuardian process and verify
  reconciliation before allowing new orders.
