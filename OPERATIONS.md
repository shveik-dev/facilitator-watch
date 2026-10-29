# Operations

The public endpoint is <https://facilitators.shveik.dev>.

## Reference deployment

The reference deployment runs one instance as a systemd service with a loopback
listener (`127.0.0.1:8501`), a static Linux amd64 binary and a persistent SQLite
database in the service's private state directory (directory mode 0700, database
mode 0600). A reverse proxy terminates HTTPS and forwards to the loopback port; the
public site is <https://facilitators.shveik.dev>. Suggested limits for a small
read-only service: nginx 5 requests/s per client (burst 10), 20 requests/s globally,
8 active connections per client; systemd `MemoryHigh=384M`, `MemoryMax=512M`,
`CPUQuota=50%`, `TasksMax=64`, Go memory target 384 MiB, `GOMAXPROCS=1`, restart on
failure with a 60 s delay and at most three starts per 15 minutes.

Per release: build in rootless Podman from a clean checkout, verify the binary's
SHA-256 after transfer, stop the service, back up the previous binary, unit files
and SQLite state (with the `-wal`/`-shm` sidecars) to a root-only directory, install
the new binary, start, and verify the checks listed under "Deployment checks". The
schema is versioned and migrations never run silently on an incompatible database;
the scheme timeline keeps a process-local index, so it needs no migration.
Rollback is: stop the service, restore the previous binary, start, verify.

## Safety rules

- Collection is `GET /supported` against the nine fixed targets in README only. There is no
  fixture mode, endpoint override, manual refresh or request-triggered poll.
- Do not probe the providers by hand to "check" the service; every restart already
  polls immediately.
- Run project tooling (format, test, vet, build, scan, dependency fetch) through
  `dev.sh` in rootless Podman, never directly on the host.
- Keep the app bound to loopback and put TLS and rate limits in a reverse proxy.
- No proxy or payment credentials. The only optional secret is a CDP Ed25519 key
  for the exact fixed Coinbase CDP GET endpoint; never log/store its key or JWT.

## Local preview modes

The source-owned collection targets are:

| ID | Exact GET URL |
| --- | --- |
| payai | `https://facilitator.payai.network/supported` |
| mogami | `https://facilitator.mogami.tech/supported` |
| dexter | `https://x402.dexter.cash/supported` |
| solvador | `https://api.solvador.com/supported` |
| polygon | `https://x402.polygon.technology/supported` |
| near-x402 | `https://x402.mikedotexe.com/supported` |
| meridian | `https://api.mrdn.finance/v1/supported` |
| casper | `https://x402-facilitator.cspr.cloud/supported` |
| coinbase-cdp | `https://api.cdp.coinbase.com/platform/v2/x402/supported` |

No MPP collection. Polygon and Meridian returned valid anonymous declarations
during the deployment check; future HTTP errors or invalid JSON shapes must still
remain failures. No additional credentials or relaxed parsing are permitted.

| Command | Provider traffic | Database |
| --- | --- | --- |
| `sh ./dev.sh serve` | None: all nine hostnames are sinkholed to container loopback | Temporary, in RAM |
| `sh ./dev.sh serve-once` | One `GET /supported` per enabled target, no retries or periodic polling | Temporary, in RAM |

Both publish only `127.0.0.1:8501` and disable container-to-host-loopback access.
This is the local preview/internal backend, not the public URL.
`serve-once` is the only preview mode that contacts the providers; restarting it
repeats the pass. Do not use it merely to refresh the UI.

To browse previously collected evidence without any provider requests, run the
binary with `-collect=false -db <copy of the database>`. This is not a read-only
mode: startup migrations and WAL setup still apply, so work on a copy and keep the
database file and its `-wal`/`-shm` siblings together. Keep provider-hostname
sinkholes and a loopback-only port as defense in depth.

## Building a release binary

`dev.sh build` verifies that the project compiles but discards the artifact. To
produce a static binary, run the same hardened container settings with the source
mounted read-only, networking off, vendored modules and an output directory on
tmpfs or another location you control:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -mod=vendor -o /out/facilitator-watch .
```

Run that inside the Go image with `--network=none`. Before relying on a binary,
run all `dev.sh` gates (`format`/`check-format`, `test`, `vet`, `build`, `scan`)
and review the dependency diff and vendor license files. `dev.sh deps` reproduces
the vendor tree from the public module proxy; the other verification commands
run offline. Browser previews use the separate networking rules described above.

## Running as a service

Example systemd unit (adapt paths, then review before use). `DynamicUser` and
`StateDirectory` give the service its own writable storage; no environment file
is needed.
The `ExecStart` listen address below is the internal backend, not the public URL.

```ini
[Unit]
Description=Facilitator Watch passive declaration monitor
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
DynamicUser=yes
StateDirectory=facilitator-watch
StateDirectoryMode=0700
WorkingDirectory=/var/lib/facilitator-watch
ExecStart=/opt/facilitator-watch/facilitator-watch -listen 127.0.0.1:8501 -db /var/lib/facilitator-watch/facilitators.sqlite
Restart=on-failure
RestartSec=60
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

The process needs outbound DNS and HTTPS to the enabled fixed hosts, normal CA trust,
and a writable state directory that includes SQLite's WAL/SHM sidecars. Use a
file path, never an in-memory database or URI. Run one instance per database.
The app uses a 0077 umask, schema version 2, WAL and a five-second busy timeout.

For the planned public site, configure DNS for `facilitators.shveik.dev` to the
approved host, and a reverse-proxy vhost for that hostname (for example nginx or
Caddy). Terminate TLS with a certificate for `facilitators.shveik.dev` and forward
to `http://127.0.0.1:8501` (internal backend, not public URL). Keep the app's
listener on loopback; do not bind it to the public hostname or all interfaces.
Validate the proxy configuration before
reloading, choose request and rate limits suited to a small read-only service,
and obtain certificates in a way that does not disturb other sites on the host.

## Checking a running service

### Optional CDP credential and rollout preconditions

Without a CDP key file, the other eight targets collect anonymously; CDP has no
attempts and shows a configuration note. All collection shares two request slots,
with independent per-target backoff and eight-second timeouts after slot acquisition.
`-once` attempts each enabled target at most once and joins all schedulers.

To configure CDP, use a protected
Ed25519 CDP JSON key file (`id` and base64 `privateKey`, 32-byte seed or
64-byte seed/public pair) with systemd credentials. Example drop-in (generic paths;
this is documentation, not an activation):

```ini
[Service]
LoadCredential=cdp-key.json:/secure/operator-managed/cdp-readonly.json
Environment=FACILITATOR_WATCH_CDP_KEY_FILE=%d/cdp-key.json
```

The key grants only the application's exact read-only GET scope; never reuse it
for payment calls. An explicitly configured invalid key aborts startup without
printing path or secret. `-collect=false` never reads it, even if configured.
Keep file permissions restrictive; never pass key contents as environment values
or command arguments. Do not store secrets/JWTs in logs, SQLite or backups.

Before activation: run offline gates,
review the target/auth diff, stop and back up the existing service/SQLite sidecars,
arrange a reviewed rollback binary, and verify credential provisioning without
printing its contents. Adding targets keeps SQLite version 2 and upserts only
metadata; old evidence survives. An older binary ignores extra IDs without
deleting their history. A restart begins up to eight anonymous requests (nine
with CDP configured), capped at two in flight. Do not restart under prior
two-provider or one-pass permission. This implementation does not activate them.

Health only reports process liveness, not database, collector or provider state.
## Deployment checks

After a deployment, verify the public HTTPS path through DNS, TLS and
the reverse proxy (these are planned checks, not completed verification):

- [Health](https://facilitators.shveik.dev/health): process liveness only.
- [Overview](https://facilitators.shveik.dev/) and
  [PayAI detail](https://facilitators.shveik.dev/facilitators/payai): UI and evidence.
- [Facilitators API](https://facilitators.shveik.dev/api/v1/facilitators),
  [summary](https://facilitators.shveik.dev/api/v1/summary) and
  [PayAI history](https://facilitators.shveik.dev/api/v1/facilitators/payai/history).

For local process diagnostics only, use the same paths at
`http://127.0.0.1:8501` on the host. This internal backend bypasses DNS, TLS and
the proxy, so success there does not verify the public endpoint.

- `/api/v1/facilitators` lists nine targets, `pending` with empty evidence until
  their first check completes, then real results.
- A reachable HTTP error can coexist with an `unavailable` declaration. An absent
  or invalid `/supported` shows as a failure, never as synthetic success.
- Confirm the SQLite file is created, logs show no failed writes, and raw
  successful JSON and capture times appear in the detail and history views.

Stop and investigate on store errors, unexpected request URLs, synthetic-looking
data or HTTP 5xx.

## Backup, migration and rollback

A target removed from the roster is excluded from collection and public reads, but
its retained SQLite rows are not purged and the schema stays version 2. Removing
retained data requires a separate, explicitly scoped operator action with the
service stopped and a backup of SQLite and its sidecars. Startup never performs
that purge. Restoring an older binary can re-enable its old target roster.

Stop the service and confirm it has exited before any filesystem backup or
restore. Preserve the database and any `-wal`/`-shm` siblings together with
restrictive permissions; a copy of the main file alone, taken while the process
is writing, is not a backup. Test restores in isolated temporary storage with the
same binary and do not point a second live collector at a restored database.

Back up before each schema upgrade. Newer schema versions and unversioned
development tables are refused rather than altered; preserve them for inspection
and use a new explicit database path rather than deleting or relabeling them.

Rollback: stop the service, restore the previous reviewed binary and, if needed,
a compatible backup, and revert only proxy or DNS changes you introduced. Keep the
current SQLite files for inspection. If there is no earlier working version, leave
the service stopped until the problem is fixed.

## Ongoing limits

- No automatic retention, storage quota, inbound authentication or behavioral testing.
  The database grows with retained history; track its size and agree a retention
  policy before scaling. Never prune observations silently.
- Failure backoff is in memory and restarts poll immediately, so avoid restart
  loops against provider rate limits.
- Watch logs for failed database writes and repeated declaration failures; health
  cannot detect a collector persistence failure. The last successful capture time
  is visible in the UI and API.
- The History API is chronological and cursor-paginated within a seven-day range,
  at most 100 events and 2 MiB of raw bodies per page. Cursors expire on restart.
  UI timelines and the ledger show only the latest bounded slice of that range.
- Provider response bodies are untrusted public data: HTML output escapes them and
  JSON serves them as data. Do not put secrets in the database or publish backups.
- Provider availability, TLS/DNS, certificates, disk and backup ownership need
  validation in each deployment.
