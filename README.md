# Facilitator Watch

A small Go + SQLite monitor that records what nine fixed [x402](https://docs.x402.org)
payment facilitators **declare** at `GET /supported`, and how that declaration
changes over time.

**Website:** <https://facilitators.shveik.dev> ·
[API](https://facilitators.shveik.dev/api/v1/summary)

![Facilitator Watch overview](docs/overview.png)

*Screenshot of a local instance showing retained observations from a review run.
It is not a continuously running public deployment.*

## What it is, and is not

Facilitator Watch watches declarations. It does not verify payments, settlement,
extension behavior or uptime, and it will not monitor arbitrary URLs. Exactly nine
targets are compiled in, and `GET /supported` is the only request it makes:

| Facilitator | Requested URL |
| --- | --- |
| PayAI | `https://facilitator.payai.network/supported` |
| Mogami | `https://facilitator.mogami.tech/supported` |
| Dexter | `https://x402.dexter.cash/supported` |
| Solvador | `https://api.solvador.com/supported` |
| Polygon | `https://x402.polygon.technology/supported` |
| NEAR x402 | `https://x402.mikedotexe.com/supported` |
| Meridian | `https://api.mrdn.finance/v1/supported` |
| Casper | `https://x402-facilitator.cspr.cloud/supported` |
| Coinbase CDP (optional auth) | `https://api.cdp.coinbase.com/platform/v2/x402/supported` |

The targets are selected from the
[official x402 facilitator directory](https://docs.x402.org/dev-tools/facilitators)
and provider documentation linked in the SPEC.
There are no POSTs, `/verify` or `/settle` calls, wallets, redirects
or proxies.

Coinbase CDP alone accepts optional read-only Ed25519 JWT authentication. Without
a key it is skipped and shows a configuration note, not a failed or fabricated
check. The other eight targets are requested anonymously. Polygon and Meridian
both returned valid declarations without authentication during deployment;
future errors are retained honestly.
MPP is not monitored. See [provider sources](SPEC.md#provider-sources).

## Features

- **Declaration history.** Every check, its bounded raw response body, and each
  change in evidence is stored in SQLite (WAL, versioned migrations).
- **Exact tuples.** Capabilities are kept as complete
  `(x402Version, scheme, network)` tuples. Network IDs are never rewritten.
- **Honest states.** Reachability, declaration validity and staleness are separate
  facts. Failures never erase earlier evidence; they mark it stale.
- **Readable UI and read-only API.** An embedded, dependency-free page with
  elapsed-time lanes, plus a GET-only JSON API. No external fonts, scripts or
  analytics.
- **Polite collection.** At most two requests globally, one per target, TLS verification on,
  8 s timeout, 1 MiB body limit, a 30-minute success interval and bounded backoff.
- **Sandboxed workflow.** Format, test, vet, build and scan all run in rootless
  Podman with networking disabled.

## Quick start (local preview)

Requires rootless Podman and an already-pulled Go image (`docker.io/library/golang:1.26`
by default, or any Go 1.23+ image via `FW_GO_IMAGE`). Nothing is pulled for you.

```sh
sh ./dev.sh serve        # http://127.0.0.1:8501 (local preview, not public URL)
```

`serve` publishes only host loopback, uses a throwaway in-container database, and
maps all nine provider hostnames to the container's own loopback. Polling therefore
fails locally and the page shows failed checks, never real provider traffic.

### Optional live monitoring

To see real declarations you must opt in explicitly. `sh ./dev.sh serve-once`
is identical to `serve` but removes the hostname sinkholes and starts the app
with `-once`: one `GET /supported` per enabled target, no retries, no periodic polling,
then it keeps serving what it captured. Restarting repeats the pass.

The built binary polls continuously by default; `-once` limits collection to one
pass, and `-collect=false` disables it entirely.
See [OPERATIONS.md](OPERATIONS.md) for running it as a service.

Other `dev.sh` actions: `format`, `check-format`, `test`, `vet`, `build`, `scan`
and `deps`. Verification commands run offline against vendored modules; `deps`
fetches the pinned Go modules through Podman. The preview networking exceptions
are described above.

## Configuration

| Flag | Environment | Default |
| --- | --- | --- |
| `-listen` | `FACILITATOR_WATCH_ADDR` | `127.0.0.1:8501` |
| `-db` | `FACILITATOR_WATCH_DB` | `/var/lib/facilitator-watch/facilitators.sqlite` |
| `-once` | | `false`; set to collect each target once, without retries |
| `-collect` | | `true`; set `-collect=false` to serve retained evidence without provider requests |
| `-cdp-key-file` | `FACILITATOR_WATCH_CDP_KEY_FILE` | Empty; skips Coinbase CDP collection |

When collection is enabled, an explicitly configured invalid CDP key fails startup
with a generic error. `-collect=false` never reads the key file. Protect the key
file and use systemd `LoadCredential` as described in OPERATIONS.md; it authorizes
only the fixed CDP GET endpoint, never payments or another provider.

The database path is a literal filesystem path, not a SQLite DSN. Run one process
per database. The listen address is the local preview/internal backend, not the
public URL; keep it on loopback behind a TLS reverse proxy.

## API

All routes are GET-only; anything else returns 405.

| Endpoint | Payload |
| --- | --- |
| `/health` | Process liveness only |
| `/api/v1/facilitators` | All configured facilitators with their latest evidence |
| `/api/v1/facilitators/{id}` | Latest check, last successful raw declaration, projected tuple states |
| `/api/v1/facilitators/{id}/history` | Chronological, cursor-paginated events for a fixed target ID |
| `/api/v1/facilitators/{id}/scheme-timeline` | Per-scheme shares of checks per time bucket (`range=24h\|7d\|30d\|90d\|all` or `start`/`end`) |
| `/api/v1/standards` | Referenced protocol documents |
| `/api/v1/summary` | Counts and the median response time of valid latest responses |

```sh
curl -s https://facilitators.shveik.dev/api/v1/summary
curl -s 'https://facilitators.shveik.dev/api/v1/facilitators/payai/history?limit=20'
```

Every JSON response is wrapped in an envelope with `data_mode: "live"`,
`fabricated: false`, a response-time `as_of`, a scope disclaimer and `data` (or
`error`). `as_of` is when the response was built, not a poll time; capture times
live on the evidence itself.

History takes an optional paired RFC3339 `start`/`end` (at most 7 days, default the
last 7), a `limit` (max 100) and an opaque `cursor`. Cursors are bound to the
facilitator and range and are valid only until the process restarts. The UI shows
only the latest bounded slice of that window, not full history.

## How to read the evidence

- **operational** means the latest check returned a valid 2xx declaration (an empty
  `kinds` list is valid). **unavailable** means transport, HTTP or validation failed.
  **pending** means no check has completed. **reachable** only means an HTTP
  response arrived, even an error.
- A **failed check** keeps earlier positive evidence but marks it *stale*.
- A tuple missing from one or two successful responses is also *stale*. After three
  consecutive successful omissions it becomes *not advertised*, which is never
  reported as the provider withdrawing support.
- **First seen** is the first time this monitor observed a tuple, not a release or
  launch date. Filled timeline spans join observations and do not claim continuous
  support; hatched gaps mean unconfirmed.
- Lists of versions, schemes and networks are independent; they do not imply every
  combination is supported.

## Documentation

- [SPEC.md](SPEC.md): the exact runtime, storage, projection and HTTP contract.
- [OPERATIONS.md](OPERATIONS.md): running as a service, backups, migrations, rollback and limits.

## Status and limitations

Early-stage software. Only the nine fixed targets and `GET /supported` are
implemented. There is no inbound authentication, automatic retention or storage quota, so
the database grows with retained history. Failure backoff lives in memory, so a
restart polls immediately. Tests use mocked transports and temporary SQLite files;
they do not contact live providers. The hosted instance collects real declarations,
but does not verify payment or settlement behavior.

The project has no license file yet. The vendored dependencies under `vendor/`
carry their own licenses and are not a license for this project.
