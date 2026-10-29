# Working on Facilitator Watch

This is a SQLite-backed app for passive monitoring of nine fixed x402
HTTPS targets in `targets()`. Commit, push or deploy only with explicit maintainer
authorization. No wallets or paid services.
The sole credential exception is an optional Coinbase CDP Ed25519 key file for
the exact fixed GET https://api.cdp.coinbase.com/platform/v2/x402/supported.
Never load it with -collect=false; never log or store keys or authorization tokens.
Unconfigured CDP is skipped without fake checks. No MPP or other auth mechanisms.

Public URL: `https://facilitators.shveik.dev` (deployment recorded in OPERATIONS.md);
`127.0.0.1:8501` is local preview/internal backend only, not the public URL.

Runtime collection is **GET /supported only**. No POST, /verify, /settle,
payments, discovery requests, arbitrary URLs, redirects or proxy credentials.
Preserve TLS verification, the eight-second timeout, one-MiB response bound,
descriptive User-Agent, two-worker limit, immediate initial checks, 30-minute
success interval and bounded failure/Retry-After delays. Never contact the
targets during implementation or tests. Production activation requires separate
maintainer authorization and the operational checks in OPERATIONS.md.

Run all project commands, formatting, tests, builds, scans and dependency
operations in rootless Podman. Use `dev.sh`: `--pull=never`, `--read-only`,
`/tmp` tmpfs, source read-only except explicit edits/format/deps. Checks use
`--network=none` and vendored modules. `deps` alone may fetch pinned Go modules
inside Podman using the public module proxy/checksum service; it never runs
the app. `serve` is the browser-preview exception: rootless slirp publishes only
host loopback port 8501, blocks container-to-host-loopback access, and maps the
all nine fixed provider hostnames to container loopback so preview polling cannot
reach them. Do not remove those restrictions from ordinary `serve`.
The explicit maintainer-authorized `serve-once` exception is operator-activated only:
omit provider sinkholes for one initial pass with `-once`, no retries or
periodic polling, then keep serving. Retain all other preview hardening. Do not
activate it during implementation/tests or restart it under one-pass permission.
All build/cache/download scratch storage stays in RAM. No host Go, Node or
browser tooling. If Podman is blocked,
finish source edits and report the blocked commands; do not bypass the sandbox.

Use the Store boundary. SQLite must retain transactional checks, raw snapshots,
events and omission counters with versioned migrations, WAL and busy timeout.
Honor the configured DB path; never silently reset an incompatible DB. Public
HTTP routes remain GET-only and loopback is the default bind.

Production must contain no fabricated fixtures or mock claims. Synthetic runtime
data belongs in `_test.go` only; source-only design mockups must stay outside the
production embed. Before a successful poll, show empty declaration
evidence. Keep reachability distinct from declaration validity. Failed checks
preserve prior positive evidence as stale. After three consecutive successful
omission snapshots use `not_advertised`, never provider withdrawal. Keep
version/scheme/network tuple associations, exact source fields and capture
times. First seen is a monitoring observation, never release/launch time.
Declarations do not verify payments, settlement or optional extension behavior.

Keep the small embedded frontend: the Dispatch/Monochrome blend
uses a light editorial masthead, sharp typography, thin rules, square edges and
muted functional status colors. Retain detail routes and horizontal elapsed-time lanes.
No external fonts, scripts, styles, images, analytics or new UI dependencies.

Verify migrations/reopen/rollback, concurrent reads and writes, API envelopes,
routes/methods/not-found, empty/live template rendering, source retention,
tuple identity, omission/failure/reappearance and transport/scheduler policy.
Use mocked transports and temporary SQLite files exclusively. Update SPEC and
operations docs when contracts change. Scan only project source/history,
without printing secret contents or reading environment/key files.
