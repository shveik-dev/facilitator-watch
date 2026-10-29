# Facilitator Watch contract

## Runtime scope

Go 1.23+, pure-Go modernc.org/sqlite v1.38.2, vendored dependencies, embedded
HTML/CSS/JS and a local icon. Only web/page.html, web/style.css, web/app.js and web/favicon.ico are embedded;
source-only design mockups are excluded. No production fixtures. Nine fixed source-owned target
bases (including API prefixes) in `targets()`, as listed in the README. Only GET `/supported`; no
arbitrary URLs, redirects, environment proxies, payments,
verification, settlement or discovery. TLS certificate verification stays on.
Public base is `https://facilitators.shveik.dev`; deployment checks are recorded in
OPERATIONS.md. TLS terminates at a reverse proxy; the default
listener `127.0.0.1:8501` is the local preview/internal backend, not the public URL.
Listen and DB flags override their
FACILITATOR_WATCH_ADDR/FACILITATOR_WATCH_DB environment variables.

One scheduler per enabled target with a cancellation-aware global two-request
semaphore, no duplicate in-flight target, immediate initial collection; after each
valid result wait 30 minutes. An eight-second request context and client
timeout bound each request, starting only after acquiring a slot; response headers are capped at 64 KiB and decoded
body reads at one MiB plus one sentinel byte. Stored bodies never exceed one
MiB; oversize is a failed check with a truncation note. Max 256 kind/extension
entries. No cookies. Only Coinbase CDP may receive an Authorization header,
signed using the optional Ed25519 CDP key file, scoped to its exact fixed HTTPS
GET URL without query. No credentials on the other eight targets. User-Agent identifies the app,
and passive scope (see `userAgent` in collector.go). No HTTP retry on reused connections.

Failure delays: 1, 2, 4, 8, 16, 30 minutes, capped thereafter. For 429/503,
positive Retry-After delta seconds or HTTP date extends the delay, capped at
six hours. Invalid/past values do not shorten backoff. Schedule is measured
after completion, with no overlapping poll or user-triggered refresh. Restart
polls immediately. Shutdown cancels and joins workers before closing DB.

Explicit `-once` mode performs only the initial attempt per enabled fixed target and
joins the workers, regardless of success, HTTP/transport failure or Retry-After.
It creates no retry timers and leaves the HTTP server running. Cancellation can
interrupt/skip attempts; shutdown still joins before DB close. The authorized
local-preview exception `dev.sh serve-once` enables this mode without provider
sinkholes; ordinary `serve` retains sinkholes. Both publish only host loopback
and disable container-to-host-loopback access. Tests/builds remain network-none.

A valid result requires a 2xx JSON object with non-null `kinds[]`. Every kind
has a positive integer `x402Version` and nonempty `scheme`/`network`. A kind at
version 2 additionally requires non-null `extensions[]` and `signers{}` at the
top level. `signers` must be an object whose values are arrays of nonempty
strings; null arrays, null elements and other element types invalidate the
whole snapshot. Optional field types are validated even for v1/empty snapshots.
Unknown fields/versions remain in raw JSON; legacy network identifiers remain
unchanged. Empty kinds is a valid declaration, never fabricated support.

## Persistence

Optional `-cdp-key-file` overrides `FACILITATOR_WATCH_CDP_KEY_FILE`. With no file,
CDP is skipped without requests, retry timers or stored checks. Fresh CDP state
is pending with a public `configuration_note`. Retained observations remain
available as historical evidence; a disabled CDP is excluded from operational,
reachable and latency summary samples and counted pending. Explicit invalid key
configuration fails startup with a generic error, without the file path or secret.
`-collect=false` never reads or requires the file. Tokens/keys are never stored,
logged or returned. Transport/read errors are generic, not arbitrary error text.
Target metadata upserts add new IDs without changing schema version 2
or existing checks/snapshots/events. The previous binary ignores extra IDs and
leaves their history intact; no destructive rollback migration is required.

A target removed from the roster is no longer configured. Its retained SQLite rows are outside
the fixed Store/HTTP boundary: detail and history routes return 404, list/summary
exclude it, and collection rejects it. Startup does not purge historical rows or
change schema version 2; any targeted database purge is a separate operator action.

Store is the public read boundary: context-aware List, Get, History, Standards;
ErrNotFound distinguishes unknown IDs. The collector writes separately. All
runtime records come from SQLite, except fixed target metadata and supplied
specification references. Startup opens a literal configured filesystem path,
uses a single connection, WAL, foreign_keys=ON and busy_timeout=5000. A mutex
serializes compound reads/transactions. No cursor is held across another query.

`PRAGMA user_version` controls transactional migrations (currently 2). Newer
versions are rejected. Existing unversioned development tables are rejected
without deleting data. Version 1 creates the tables below; version 2 adds the
facilitator/time/event index used to bound date-range history queries.

| Table | Contents |
| --- | --- |
| facilitators | Fixed ID/name/endpoint metadata |
| checks | Every completed attempt: UTC timestamp, status, duration, reachable, nullable HTTP status, error |
| snapshots | At most one bounded raw body per check, with validity flag |
| events | Check FK, UTC time, nullable facet ID/JSON, basis, change and source evidence metadata |
| facet_misses | Successful omission streak per facilitator/facet |

A check, raw body, all derived events and miss updates commit atomically. Any
error rolls back the entire attempt. Body-read failures retain any bounded
bytes available; transport failures may have no body. No automatic retention
or destructive migration is implemented. WAL sidecars belong with the DB.

Facets use SHA-256 IDs of the JSON `[version,scheme,network]` tuple, or of an
extension string with a distinct prefix. No independent lane combination can
invent a capability. Duplicate facets coalesce within one snapshot. Optional
extra and all unknown fields remain in raw snapshots.

Each valid check creates a snapshot event even when empty. Each present facet
creates first_seen or confirmed evidence; omitted known facets create unknown
on misses 1/2, then not_advertised from miss 3. Failed checks create probe_failed
without a facet and reset miss counters. Events link to snapshots by check_id;
the DB stores raw bodies once. History hydrates raw JSON only on check/snapshot
events; per-facet evidence references check_id. Non-JSON bodies stay in DB and
serialize as null raw evidence. Overview projection queries only earliest and
latest positives, latest facet/omission event and latest failure by durable event
sequence; it does not load all historical raw snapshots. Capture timestamps remain
wall-clock values for history/chart display and do not determine current state
after a clock rollback. History API reads are bounded pages; capability
projection remains independent and uses its compact sequence projection.

## Evidence projection

Events have id (string), at, nullable facet `{id,category,value,scope}`, basis,
change, evidence, data_mode=live and fabricated=false. Production categories
are kind and extension; basis is declared for facets, unknown for check events.
No observed behavior evidence is created. Evidence has optional check_id,
source (fixed HTTPS /supported), source_type=http_get, captured_at, nullable
raw JSON, note and fabricated=false.

| Change | Projection |
| --- | --- |
| first_seen / confirmed | present, earliest retained first_seen, updated last_confirmed, positive evidence |
| unknown | stale if previously positive; preserve positive dates/source; established not_advertised remains |
| not_advertised | Distinct recently omitted state, no withdrawal/date inference |
| probe_failed | All present facets stale; positive sources/dates retained; established not_advertised remains |
| snapshot | Check evidence only, no support claim |

The shared projector also understands explicit removed evidence for regression
tests, but the live collector never creates it. Reappearance restores present,
preserves earliest first_seen and starts a new chart window after uncertainty.
Declaration/behavior channels are independently keyed. No payment behavior,
settlement, production readiness or provider-announced withdrawal is inferred.
Timeline geometry uses elapsed UTC time through response-time as_of. Filled
spans connect positive observations; hatched intervals are unconfirmed. Unknown
dates never invent first seen or launch dates. Future events are excluded.

## HTTP surface

GET `/`, status-filtered overview (operational/unavailable/pending),
`/facilitators/{id}`, `/standards`, `/assets/style.css`, `/assets/app.js`,
`/favicon.ico` (image/x-icon, 16/32/48-pixel black-square/white-slash images).
Native links and disclosure work without JS. The Dispatch/Monochrome
presentation uses a light editorial masthead, bold system type, square boundaries,
thin rules and black/white contrast. Muted accents carry functional status only.
Horizontal lanes and tables scroll within their regions. No external assets or analytics.
The default profile shows deduplicated versions, schemes and networks from
the last successful snapshot, explicitly not their Cartesian product. Failed
latest checks label that summary stale; successful empty snapshots say no kinds
were advertised. On both overview and profile, networks default to a count in a
closed native disclosure. Its full list retains every distinct exact source ID,
without alias merging or guessed names. Long IDs wrap in a responsive grid with
a bounded, keyboard-scrollable height. The overview uses fixed column proportions
so network counts cannot squeeze the summary or grow rows with the network list.
At widths up to 760px, that same overview table reflows into stacked provider rows:
name and summary span the row, with check and capture time side by side. No
horizontal scroll is needed to see the summary. Column headers stay accessible,
cells retain explicit header associations, and visible mobile labels identify
each field without duplicating provider data. Advanced tables remain scrollable.
Missing values do not imply unsupported behavior. The default scheme timeline is an aggregate of every retained check, not a
bounded event slice. It groups only actual declarations by scheme name, never by
network/version, and shows shares of checks per UTC time bucket, never uptime,
continuous support, verified behavior or provider withdrawal. Ranges 24h, 7d, 30d
(default), 90d and all choose the bucket size (1 h, 3 h, 1 day, 1 day, then 1 day
up to about 130 buckets and 7 days beyond); a bucket can be zoomed to an explicit
window with prev/next day steps. Per scheme lane and bucket the UI draws one
stacked bar: advertised (solid), failed check (hatched) and reachable but absent
from the declaration (open box), as a share of checks at or after the scheme's
first observation. Earlier checks are "before first seen" and never count as
omissions; buckets without checks stay blank; a first-seen flag marks the
bucket of the earliest observation, which is a monitoring observation, not a
launch date. A check-health row shows the failed share. Shape, not color,
carries meaning; hover/keyboard focus show a column highlight and a tooltip with
counts. On narrow screens only the chart scrolls. The chart needs script;
without it a noscript fallback shows the legacy seven-day/100-event discrete
chart. Navigation is light with a ruled active state; cards, borders and
typography remain restrained and square.
Schemes come from valid snapshots' `kinds[]` scheme names. A process-local,
append-only index derives them once per check (the schema is unchanged, so older
binaries still open the database); it is rebuilt after a restart.

GET `/api/v1/facilitators/{id}/scheme-timeline` takes `range=24h|7d|30d|90d|all`
or paired RFC3339 `start`/`end` (end clamped to now, at most 3650 days), never
both; default `range=30d`. Other parameters, repeats and empty values return 400.
`data` has `range`, `from`, `to`, `step_seconds`, `first_check` (null without
checks), `buckets[{start,end,checks,failed}]` and `lanes[{scheme,first_seen,
buckets[{advertised,omitted,failed,before,first_seen}]}]`, aligned by index.
Lanes cover every scheme ever advertised by that facilitator.

The Advanced evidence timeline and ledger still consume the latest bounded
seven-day/100-event slice, excluding future events. Scheme mapping there uses exact tuple IDs derived from the
last valid declaration and available bounded snapshot JSON, never parsed display
labels.
Exact tuple states (including stale/not_advertised), detailed timeline, raw evidence and recent ledger
are inside a closed native Advanced evidence disclosure. Stored/API tuples and
evidence projection are unchanged; no JS is required to expand the details.

GET JSON routes: `/health`, `/api/v1/facilitators`,
`/api/v1/facilitators/{id}`, `/api/v1/facilitators/{id}/history`,
`/api/v1/facilitators/{id}/scheme-timeline`,
`/api/v1/standards`, `/api/v1/summary`.

Envelope: data_mode=live, fabricated=false, UTC response-time as_of, disclaimer,
data. Errors replace data with `{error:{code,message}}`. Every facilitator and
event also has mode/provenance. Check contains status, nullable checked_at,
response_ms, reachable, http_status, evidence. Pending has no evidence or
invented measurements; unavailable means no valid declaration, independently
of reachable. Operational includes successful empty snapshots. Summary has
total, operational, unavailable,
pending, reachable, median_response_ms and scope. Median covers valid latest
responses only, integer mean of center pair, null for no samples.

Facilitator fields: id, name, endpoint, data_mode, fabricated, check, supported,
supported_evidence, capabilities. `supported` represents the last valid
snapshot with exact kinds[] tuple fields and optional extensions/signers;
raw is authoritative for optional empty fields and unknown fields. Before the
first success: empty kinds, no raw/capture time, empty capabilities. A later
failure preserves the last successful raw declaration. Capability fields are
facet, basis, state, first_seen, last_confirmed, removed_at (always null for
live collection), evidence. Missing dates serialize null.

200 success; 400 API query parameters or invalid UI filter; 404 unknown
route/ID; 405 every non-GET including HEAD/OPTIONS, Allow: GET; 500 store error
without internal detail. No mutation/auth/admin/CORS opt-in. History accepts
paired RFC3339 `start`/`end` (half-open range; default last seven days), optional `limit` (default
100, maximum 100), and opaque `cursor`. Ranges cannot exceed seven days. A page
contains at most 100 events and 2 MiB raw-body bytes. The first event is never
dropped when its raw body exceeds remaining page budget; its body is omitted and
the evidence note says so. An HMAC-authenticated cursor binds facilitator/range
and the initial high-water `events.id`; it is valid only for the running process.
Malformed query encoding and invalid, mismatched, tampered or restart-expired
cursors return 400; genuine store failures remain 500.
Pages keyset by `(at,id)`, excluding later inserts even if backdated. Response
`data` includes `events`, `start`, `end`, `limit`, and
optional `next_cursor`. Detail timelines/ledger scan newest-first within the
latest seven-day window, stop at 100 events/2 MiB raw body, then reverse the
bounded slice for chronological display. Earlier events (even part of a check)
can be excluded; the UI identifies this as a partial slice, not continuous history.
Capability projection does not use this display history. Health means process liveness
only, not database/collector/target readiness. Response-time as_of is never a
capture timestamp. Responses set no-store, nosniff, no-referrer and CSP limiting
assets to self, prohibiting embedding/external connections. The embedded stylesheet
and numeric chart positions use inline styles, permitted by the CSP.
Server request/header/shutdown times are bounded.

## Verification boundary

Tests use mocked transports and temporary SQLite files only; no live target
requests or network sockets. Tests cover migration/reopen/rollback, WAL/pragmas, DB path,
concurrency/deadlock, tuple identity, source fields, failure/omission/reappearance,
empty/live UI/API, methods/errors/filters, JSON contracts, timeline geometry,
redirect/timeout/TLS/body bounds, immediate workers, backoff and Retry-After.
Format, test, vet, build and a source/history secret scan run in rootless Podman via `dev.sh`.
Vendor fetching is a separate container-only dependency operation (`dev.sh deps`).
See OPERATIONS.md for service, backup and rollback guidance and operational limits.

## Provider sources

Fixed requested URLs are in the README; these references are documentation, not
runtime discovery or extra requests. Deployment check results are recorded in
OPERATIONS.md; documentation claims alone never create live evidence.

| Provider | Official source / access caveat |
| --- | --- |
| PayAI / Mogami | [x402 directory](https://docs.x402.org/dev-tools/facilitators) |
| Dexter | [Facilitator and chains](https://docs.dexter.cash/docs/facilitator-and-chains), public |
| Solvador | [Supported API](https://docs.solvador.com/api/supported), anonymous GET |
| Polygon | [Seller quickstart](https://docs.polygon.technology/payment-services/agentic-payments/x402/guides/quickstart-sellers/), SDK `/supported` path; valid anonymous response observed during deployment |
| NEAR x402 | [Reference access](https://github.com/fastnear/x402-facilitator/blob/main/docs/reference-access.md), public mainnet |
| Meridian | [Supported payments](https://docs.mrdn.finance/api-reference/endpoint/supported-payments.md), exact `/v1` prefix; valid anonymous response observed despite conflicting OpenAPI security annotations |
| Casper | [CSPR.cloud docs](https://docs.cspr.cloud), public `/supported` and `/health`; `/verify` and `/settle` require an API key and are never requested |
| Coinbase CDP | [Supported API](https://docs.cdp.coinbase.com/api-reference/v2/rest-api/x402-facilitator/get-supported-payment-schemes-and-networks), optional read-only JWT configuration |

HTTP 401 and invalid response shapes remain failures. No relaxed v2 parsing,
new provider credentials, MPP or payment/settlement tests are implied.
