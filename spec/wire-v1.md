# spec/wire-v1 — ingest wire format

Status: v1 draft, rev 0.24 (frozen at end of M1) · Gates: M1 · Normative for: server validation,
`spec/wire-v1.schema.json`, every SDK, the snippet, `spec/sdk-conformance.md`.

Key words MUST, SHOULD, MAY are RFC 2119. This document is the contract; RFC-0001 §2 is the
rationale. Where they differ, this document wins.

## 1. Endpoint

```
POST https://in.jelto.io/v1/e
Content-Type: application/json
```

**`in.jelto.io` is the production ingest host, and every client MUST default to it.** Through rev
0.18 this block read `https://<ingest-host>/v1/e` and no document anywhere named the host, which
read as a documentation gap and was not one: it is what a client does *in the absence of a name*
that makes it a defect. The snippet already defaulted correctly — `spec/snippet.md` §1 has carried
`data-endpoint` defaulting to `https://in.jelto.io/v1/e` since v0.1, and §7's CSP names
`connect-src in.jelto.io` — so the host was decided and merely written in a surface-specific spec
rather than in the contract the SDKs are built against. **Both app SDKs therefore had no default at
all**, and a shipped application, which cannot set an environment variable for itself, sent every
event nowhere and retried forever with nothing surfaced to the host (`sdk/swift` fell back to a
`/dev/null` URL; `sdk/electron` to the empty string). Naming the host here is the fix; a client
that has no endpoint configured sends **here**, not nowhere.

A client MAY be pointed elsewhere and the order is fixed: an **explicit endpoint** passed by the
application, else an **environment override** where the client offers one, else this default. The
environment override MUST beat the default rather than the other way round — a client whose
default won would ignore the endpoint its own test harness handed it and send real traffic here
while its suite reported green.

`<ingest-host>` survives as a placeholder in exactly one case, and it is the reason the explicit
endpoint exists at all: a customer on a **first-party subdomain** (RFC-0001 §10) serves `/v1/e` on
their own hostname, and their snippet and SDK are configured with it.

An endpoint, explicit or from the environment, MUST be an absolute `http` or `https` URL carrying
no userinfo, and an empty value is an absent value — the next level of the precedence applies. A
client handed a **non-empty value that is not one MUST become inactive for the process** and say
so under debug, rather than fall through to another endpoint: a mistyped first-party host must not
quietly route a customer's events to the default, and an unparseable one must not be retried
forever as a network error. Through rev 0.22 this document was silent on both and the four app
SDKs shipped four answers (rev 0.23).

- The server MUST accept `text/plain` as well (`sendBeacon` cannot set headers).
- The server MUST respond to `OPTIONS` with permissive CORS for any origin; the Origin check
  in §6 is done per event, not per request.
- The path is versioned. `/v2/e` will exist only for a breaking change; none is planned.

## 2. Envelope

```json
{ "v": 1, "p": "prd_8f3kq2m9x1", "e": [ … ] }
```

| Field | Req | Type | Rule |
|---|---|---|---|
| `v` | MUST | integer | `1`. Anything else → `400 {"error":"unsupported_version"}`. |
| `p` | MUST | string | `^prd_[a-z0-9]{10}$`. Unknown key → `202`, nothing stored. |
| `e` | MUST | array | 1–100 events. `0` → `400 empty`; `>100` → `400 too_many_events`. |

Body size MUST be ≤ 65 536 bytes → `413`. Malformed JSON → `400 malformed`. §2a gives every
status this endpoint answers and the exact body each one carries.

The key's shape is checked on both ends. An SDK MUST refuse `init` with a key outside
`^prd_[a-z0-9]{10}$`, logging the rule under debug and sending nothing, so a mistyped key fails
at the call site rather than as one `400 malformed` per batch for the life of the install
(rev 0.23; the shape W2 asserts for event names, applied to the key, and pinned by each SDK's own
unit tests).

## 2a. Response statuses

An SDK decides retry-or-drop from the **status alone**, so a status this endpoint can answer and
no document names is a branch no client has. Every one of them, with the exact bytes of its body:

| Status | Body | When |
|---|---|---|
| `202` | `{}`, or `{"rejected":[…]}`, or `{"stop":{…}}` — §6, §8 | The envelope was accepted for processing (§6). Every event in it may still have been rejected. An **unknown `p`** answers this too, with `{}` |
| `204` | *(empty)* | `OPTIONS` preflight (§1) |
| `400` | `{"error":"malformed"}` · `{"error":"unsupported_version"}` · `{"error":"empty"}` · `{"error":"too_many_events"}` | The envelope is not one this version can read (§2). `malformed` also covers a `p` outside `^prd_[a-z0-9]{10}$` — the key's *shape*, where an unrecognised but well-formed key is the `202` above |
| `402` | `{"error":"payment_required"}` | The product's account may not collect: a trial that expired without a subscription, or a subscription paused after its grace. Nothing is stored. **Specified ahead of its implementation** — see below |
| `405` | *(empty)* | Any method other than `POST` or `OPTIONS` |
| `413` | `{"error":"too_large"}` | Body over 65 536 bytes (§2). The refusal is on the byte count and happens before the body is parsed, so it names no field |
| `429` | `{}` + `Retry-After` | Rate limited, per product or per source (§9) |
| `500` | `{"error":"internal"}` | The server failed before it could answer. The cause is logged, never returned |
| `503` | `{}` + `Retry-After` | Backpressure — buffer plus spool over their ceiling (RFC-0001 §7.3, §9) |

Two `400` bodies exist only under the fixture harness and are listed for completeness rather than
because a production sender can provoke them: `{"error":"fixture_product_required"}` and
`{"error":"missing_field"}` answer a request that reaches a build running with fixture directives
under a key other than the fixture product, or without the timestamps that mode requires. They are
named here for the same reason the stats API names `fixture_mode_disabled` in its own
error vocabulary — the wrapper fronts the route, so its refusals are part of the route's contract
even though nothing in production sends the header.

**What a client does with each is already settled and is not restated per row.** RFC-0001 §8.3
items 8–9 close the set: an SDK retries a network error, a `429` and a `503`, and treats every
other answer as final — including every `400` (`spec/sdk-conformance.md` C9) and `402` (C9b).
§9 adds the one constraint this revision makes: a retry MUST NOT fire before the `Retry-After`
the refusal carried.

**`402` is the one row here that no code path can send yet, and it is written down deliberately.**
This document makes it normative for `trial_expired` and `paused`, and
`spec/sdk-conformance.md` C9b makes an SDK's handling of it mandatory — *"an SDK that treated it
as retryable would hammer the endpoint for the life of the install"* — while this document named
it nowhere. The SDK is written before the billing gate exists, so naming it now is what stops C9b
from testing an SDK against a status no document defines. The server has no billing package,
its account records carry no billing state, and its product lookup returns configuration rather
than entitlement; the ingest handler records exactly that, and the contract
test **fails if the entry outlives its reason** — that is, if `402` ever becomes emittable while
still listed. It is an append and the freeze would not have forbidden it later; it lands now
because C9b already depends on it.

**Where each row's body comes from.** Every one but three was **sent to the running server on the
compose stack and read off the response**: `202` (both the empty and the `stop` form), `204`,
`400` (`malformed`, `unsupported_version`, `empty`, `too_many_events`), `405`, `413`, `429` (both
buckets) and `500`. The `503` was driven through the real handler in-process instead, because
provoking it over the wire means filling the 2 GiB buffer-plus-spool ceiling; it shares
`writeRetry` with the `429`, whose `{}` body and `Retry-After` were measured. The two fixture-mode
`400`s are read from the source and are not reachable on a production build. `402` is specified,
not measured — see above. `internal/ingest.TestIngestAnswersTheBodiesTheWireDocuments` asserts the
exact bytes of each so this table cannot drift from the handler.

## 3. Event — common fields

| Field | Req | Type | Rule |
|---|---|---|---|
| `id` | SHOULD | UUID (v7 preferred), **never the nil UUID** | Dedup key, 10-minute window server-side. That window is **process-local memory in the HTTP handler**: it defends against a client resending the same event within 10 minutes, and against nothing else. It is not the defence against server-side replay — see §6's delivery semantics. Missing → server generates; duplicates then cannot be detected. A present `id` MUST NOT be the nil UUID `00000000-0000-0000-0000-000000000000`; one that is rejects the event `invalid_field` naming `id`, exactly as §5.2 rejects a nil `iid`. The asymmetry is not survivable: `id` **is** the dedup key, so an accepted nil collapses every event carrying it onto one row, and a client that zero-initialises the field discards ten minutes of a product's traffic per key behind a `202` whose body is `{}` — a `duplicate` is counted, never returned, unless `?debug=1` (§6). **Omitting `id` is always safe; sending a zero one never is.** |
| `n` | MUST | string | `^[a-z0-9_:.-]{1,64}$`. Reserved names in §4. |
| `t` | SHOULD | **whole number**, ms epoch | Clamped to `[ingest_ts − 30 d, ingest_ts + 5 min]`; outside → `ts = ingest_ts`. Missing → `ingest_ts`. **Every `t` is clamped and none is ever rejected for its magnitude** — not one before the epoch, and not one outside `int64`, which saturates at the `int64` bound and is then clamped like any other. Rejecting a whole event because a device has a wrong clock is data loss, and `spec/sdk-conformance.md` C15 forbids an SDK correcting its own clock, so the sender cannot fix it either. **JSON has no integer type** (RFC 8259 §6), so a *whole number* field is one whose **value** is whole, not one whose **literal** was written without a fraction or an exponent: `1785578400000`, `1785578400000.0` and `1.7855784e12` are the same instant and the server reads all three. A genuine fraction such as `1.5` is `invalid_field`. This rule governs every whole-number field in this document — `t`, `w`, `h`, `e`, `sd` — and the conversion is exact rather than routed through a float64, so a value `int64` can hold is read as written and only a value it cannot hold moves, by saturating rather than rounding. |
| `s` | MUST | `"web"` \| `"app"` | Selects the field set below. |
| `props` | MAY | object | ≤ 20 keys; key `^[a-z0-9_]{1,32}$`; values string ≤ 200 chars, number, or boolean. Stored as the value's **JSON text**, unquoted and unrounded: `29.90` is stored `29.90` and never a float, `"29,90"` is stored `29,90`, `true` is stored `true`. The server keeps the text it was sent and nothing derived from it, so a typed column can be added and backfilled from stored rows whenever a value-reading metric is specified (`spec/metrics.md` §4.7 — none exists in v1). Allowlisted per `(product, n)` — see §7. |
| `i` | MAY | boolean | Interactive. `false` for events fired without user intent (an auto-fired event). Default `true`. A visit with one pageview and an interactive non-pageview event is not a bounce. The server **forces `i = false` on `engagement`** whatever the client sends, so an engagement can never un-bounce a visit. |
| `v` | SHOULD | string ≤ 32 matching `^[a-z]+/[0-9A-Za-z.+-]{1,24}$` | Client version: `web/1.2.0`, `swift/1.0.3`, `electron/1.0.3`. Counted per version in the Ops card so a bad release is visible. The pattern is **normative** and the server enforces it: a value outside it rejects the event `invalid_field` naming `v`. It is not decoration. `client_ver` is `LowCardinality(String)` in both `events` and `ingest_counters` and nothing folds it the way §4 folds an install-property value, so this grammar is that column's only cardinality guard. It is deliberately narrow — a bare `1.2.0` does not say **which** client shipped it, which is the one thing the Ops card needs in order to name a bad release. **An empty `v` is an absent `v`**, said here rather than left to fall out of the pattern's `{1,24}` tail, exactly as §5.1 `f` and §5.2 `a` say it for themselves. |
| `l` | MAY | string ≤ 16 | Language: web `navigator.language`; app the OS locale (`en-US`). Stored `LowCardinality`. |

## 4. Reserved event names

| `n` | Surface | Required extra fields | Fixed props |
|---|---|---|---|
| `pageview` | web | `u` | — |
| `click:download` | web | `u` | `file` (string, the basename) |
| `click:outbound` | web | `u` | `url` (hostname only) |
| `engagement` | web | `u pv e sd` | — |
| `heartbeat` | app | `iid av os osv arch` | **install properties**: `license` (reserved, e.g. `trial` `paid` `expired` `free`) plus the keys the product allowlists for `heartbeat` in `event_schema`. **Every install-property value, `license` included, matches `^[a-z0-9_.-]{1,24}$`** — one grammar, the one the server has always applied to all of them. The property count is §3's `props` cap of **20**, which is also what `event_schema.allowed_props` and the account API bound; a heartbeat carrying more is `invalid_field` naming `props`, and a key the product has not allowlisted is `prop_not_allowlisted` whether or not the count is under the cap, so what bounds a heartbeat in practice is the allowlist and 20 is its ceiling. ≤ 50 distinct values per key per product, beyond that `other`. The heartbeat carries the app's **current** values every time; the server keeps the latest per install. `products.paid_license_value` — the Settings value the server's `license_conversion` metric compares a stored `license` against — takes this same grammar, because a value outside it can never match one an app sent. |
| `install` | app | `iid av os osv arch` | optional `install_origin` enum in §5.2 |
| `app_updated` | app | `id iid av os osv arch` | `from_version`, `to_version` (required nonblank strings ≤ 32 Unicode scalars); `av` equals `to_version`, and the two versions differ by exact scalar equality |
| `app_update` | app | `id iid av os osv arch` | `from_version`, `to_version` (required distinct nonblank strings ≤ 32 Unicode scalars), `status` (required: `download_started`, `downloaded`, `install_started`, `download_failed`, `install_failed`, `failed`, or `postponed`), `reason` (optional string matching `^[a-z0-9_.-]{1,64}$`) |
| `onboarding:<step>` | app | `iid av os osv arch` | `status` (required: `ok` \| `fail` \| `skip`), `reason` (optional, ≤ 64 chars, `^[a-z0-9_.-]+$`; free text is rejected) |
| `purchase` | web, app | — never sent by a client | `amount` (a decimal as a **string**, stored as sent and never rounded, negative for a refund), `currency` (ISO 4217, uppercase) |

`purchase` is the one reserved name with no client-facing required fields, because it has no
client-facing writer at all: it is **server-written**, produced only by `POST
/api/v1/payments` and never by a sender on this endpoint. `/v1/e`
is authenticated by a public product key and an `Origin` check — the right trust level for a
pageview and the wrong one for a number that ends up on an invoice, since anyone holding the
public key could inflate a competitor's revenue or their own. A client that sends `purchase` here
is rejected `reserved_event` (§6), not stored and not treated as an ordinary unknown name. The row
this endpoint does write carries the same five UTM columns a `click:download` from that channel
carries when attributed to a web `cohort`, or the app dimensions the `installs` family already
joins on when attributed to `install_id` — `cohort` and `install_id` are mutually
exclusive on that endpoint, and accepting both would build the row-level web ↔ app join the
product forbids.

`engagement` reports how far and how long one **pageview** was actually read. It is
auto-fired, never counts as a pageview, and carries values that are **totals for that pageview
since it started**, never a delta since the last engagement. The two totals are not the same
shape: `e` is a monotone accumulation and never decreases, while `sd` is a ratio whose divisor is
re-measured on every send and so may decrease (§5.1). A pageview therefore produces zero, one or
several engagement rows, and `spec/metrics.md` §4.1b reduces them per `pv` — `max()` for `e`, the
last row for `sd` — so a duplicate, a retry and a lost intermediate row all give the same answer.
A client MUST NOT send an engagement whose `e` and `sd` both equal the last one it sent for that
`pv`. Engagement rows are stored, never merged into the pageview row, and never rewrite the
session's entry, exit, pageview count or bounce flag (RFC §5.3).

`onboarding:<step>` is a reserved **namespace**: `<step>` matches `^[a-z0-9_-]{1,32}$` and needs
no `event_schema` row — every product may send onboarding steps from day one. The server keeps at
most 50 distinct steps and 100 distinct `reason` values per product; beyond that, new values are
stored as `other` and counted in the Ops card, so a bug that emits a unique reason per install
cannot blow up cardinality. A step is counted once per `install_id` (first occurrence wins) in
funnels; later repeats are stored but do not move the funnel.

`app_updated` is automatic SDK lifecycle telemetry (SDK conformance §7), available without
an `event_schema` row. Its fixed props cannot be extended. It records both upgrades and
downgrades; versions are opaque strings. Updates preserve `iid` and never count as installs.
`id` is required for this event. The server stores it in the app-only `event_id` column so
analytics can deduplicate repeated transitions beyond the common ten-minute memory window
and across server restarts. Physical duplicate rows remain possible under at-least-once
delivery; update completion metrics reduce them by `(install_id, event_id)` (metrics §4.2).
An older server needs this reserved-event support before updated SDKs are deployed; otherwise
it can reject the event as `unknown_event`. This is an additive wire-v1 event, not a new
envelope or identity format. Existing SDK state and queued events require no rewrite.

`app_update` records explicit updater activity through the existing SDK `track` API,
without an `event_schema` row. Its fixed props cannot be extended. Send a new event
once per observed stage or deliberate postponement; never once per progress tick.
`from_version` is the version being replaced and `to_version` is the known target.
`av` remains the SDK's app metadata; it need not equal either property (for example,
when reporting a saved installer outcome after restart). Use `download_failed` or
`install_failed` only when that stage is known, and `failed` otherwise. A cancelled
download is not evidence of a deliberate postponement. `postponed` requires an
explicit user choice or the host declining installation. `reason` is an application
defined category, never an exception message, path, URL or other free text.

These events describe observations, not a mandatory sequence or a conversion funnel.
A download can be cached, installation may run outside the app, and a process can
exit without reporting its outcome. Never infer failure or postponement from a
missing event. The later automatic `app_updated` event remains the evidence of a
changed version actually launching. Each `track` call gets its own stable event ID;
transport retries preserve that ID. The server retains `app_update` IDs and reduces
completion counts by `(install_id, event_id)` just as for `app_updated`. Neither
event affects install claims or heartbeats. A feed URL alone cannot emit activity.

Any other `n` is a custom event and MUST be present in the product's `event_schema` (§7) or the
event is rejected with `unknown_event`.

## 5. Surface fields

### 5.1 `s = "web"`

| Field | Req | Type | Rule |
|---|---|---|---|
| `u` | MUST | **absolute** `http(s)` URL ≤ 2 048 | Page URL. A root-relative path is rejected `invalid_field`: when the request carries no `Origin` header, `u`'s host is what §6's `origin_not_allowed` check reads, and it is the only thing keeping a product's rows on its own domains. A sender that must mask or normalise a path sends an absolute URL whose *path* is masked (`https://site.example/:masked/alfa`) — its own origin is never the secret. Server stores `host`, `path`, and exactly `utm_source`, `utm_medium`, `utm_campaign`, `utm_content`, `utm_term`; the rest of the query string is discarded, including every ad click-id **value** (only the parameter *name* is kept, as `click_id`). Fragment handling is `h` below. |
| `r` | MAY | **absolute** `http(s)` URL ≤ 2 048, or `""` | Referrer. A present `r` MUST be absolute with a non-empty host; the empty string is the only other legal value and means *no referrer*, which is what `document.referrer` reads on a direct visit. A path, a bare host, or any other scheme is rejected `invalid_field` naming `r` — and that rejects **the whole event**, not just the referrer, so a sender holding a referrer it cannot express this way omits the field rather than sending a fragment of one. The reason it cannot be relaxed instead: the server derives `ref_host` from the parsed hostname, which is empty for a path, so a host-less `r` skips the same-product blanking below and stores a bare `ref_path` that `spec/metrics.md` §3's `referrer_url` (`concat(ref_host, ref_path)`) then renders as a referring page belonging to no site. Server stores the hostname (`www.` stripped) **and the path**, trailing slash trimmed, capped at 255 bytes. Query and fragment are discarded, so no click-id value and no referring-site query parameter is ever stored. Same-product hostnames → empty, and then no path either — so a **same-origin referrer sent in full stores nothing at all**, which is why `spec/snippet.md` B1 sends it in full instead of trimming it to a path. |
| `w` | MAY | whole number (§3) | Screen width in CSS px; `0`–`10000`, outside → `invalid_field`. Stays **MAY**: a sender that has no viewport (a server-side integration) must not have its pageview rejected over one. An absent `w` is `screen_w = 0`, which `spec/metrics.md` §3 renders as its own `unknown` bucket — never as the smallest band. |
| `h` | MAY | whole number (§3) | `1` when the sender is in hash-routing mode; absent otherwise. When `h = 1` the server appends `#<fragment>` of `u` to the stored `path`. When `h` is **present it decides**, and the product's `hash_routing` setting is ignored for that event; when `h` is absent the server falls back to that setting, which is what a legacy snippet and a server-side sender get. The asymmetry this closes is one-directional and silent: a snippet with `data-hash` against a product whose setting is off had every hash route flattened to one stored `path`, at write time, unrecoverably. |
| `f` | MAY | string ≤ 200 | Attribution memory (PRD §6.6): the remembered first-touch label in `jl` syntax (`producthunt~social~launch` or `ref:news.ycombinator.com`). Sent only when the snippet runs with `data-memory="on"`. The server ignores it unless the product has memory enabled — and **as of rev 0.21 an ignored `f` is COUNTED**, `ingest_counters.reason = 'memory_ignored'`, on the accepted event, for the reason `vid`'s ignore is counted: the attribute is baked into the tag at paste time (`spec/snippet.md` §1), so a product whose setting is off while `f` keeps arriving is one still writing `jelto_first` into every visitor's `localStorage` behind a published paragraph that denies it. Dropping the label does not reach into the browser. An **empty `f` is an absent `f`** — no label, and no `fd` required — exactly as an empty `r` means no referrer; this is what the parser has always done and rev 0.15 writes it down rather than changing it. |
| `fd` | MUST if `f` | `YYYY-MM-DD` | Day the label was first stored. Outside `[today − 30 d, today]` → both fields dropped. |
| `pv` | MUST if `n = engagement` | UUID | The `id` of the `pageview` event this engagement belongs to. Tying engagement to the pageview id rather than to the path keeps two visits to the same path inside one visit apart. An engagement whose `pv` is unknown to the server **is still stored when its visitor has a live session** — the pageview may be in a later batch — and contributes to the metrics of the page in its own `u`. The qualifier is the rule, not a detail: an unknown `pv` is never *by itself* a reason to drop the row, but §6 rejects an engagement whose visitor has no live session as `no_session_for_engagement` **whatever its `pv`**, and the case where the two meet is the one that matters — a long read whose beacon arrives after the 30-minute idle window is dropped, not stored. Executed: with a live session and a `pv` the server has never seen, the row is stored with the `e` and `sd` it carried; with no live session and that same `pv`, the answer is `202 {"rejected":[{"i":0,"reason":"no_session_for_engagement"}]}` and nothing is stored. A matching `pv` also does **not** re-open a closed session — a proposal considered and declined, recorded in the rev 0.17 amendment so it is not re-proposed: re-opening would let a client extend the idle window, which makes the session boundary something the sender controls. |
| `e` | MUST if `n = engagement` | whole number (§3), ms | Time the page was **visible and focused**, accumulated since the pageview started. Cumulative, not a delta. Clamped to `[0, 1 800 000]` (30 min); **a value outside that range, larger or negative, is clamped and not rejected**. `e` is the only range in this document that clamps instead of rejecting, and deliberately: an over-counted engagement is a browser that lost a visibility event, not a malformed sender, and throwing the pageview's engagement row away to punish it would lose the reading entirely. |
| `sd` | MUST if `n = engagement` | whole number (§3), 0–100 | The deepest scroll position reached on this pageview, as a percentage of the document height **measured at the moment that beacon was sent**. The numerator is cumulative; the divisor is not. `sd` for one `pv` therefore **MAY decrease** between engagements — a document that grew after a beacon went out makes the next, smaller percentage the more honest one, and `spec/metrics.md` §4.1b keeps the last one rather than the largest. A client MUST NOT hold back a lower `sd` to keep the series non-decreasing. Outside the range → `invalid_field`. |
| `vid` | MAY | string `^[A-Za-z0-9_-]{22}$` | **Cookie mode only** (`spec/snippet.md` §7). The opaque identifier `jelto.cookie.js` keeps in its first-party cookie: 16 random bytes in base64url, generated in the browser and never derived from anything about the visitor. **The grammar is deliberately one a UUID cannot satisfy**, and that is boundary 2 made checkable rather than trusted: `install_id` is a UUID (§5.2), so an `install_id` cannot be *placed* in this field at all — no length, no alphabet and no dash position of a UUID matches — and the one identifier the decision forbids meeting this one is excluded by reading the grammar rather than by auditing a derivation. The server **never stores the value**. It derives `visitor_id = sipHash-2-4("jelto-cookie-v1" ‖ product_id(le64) ‖ vid)`, domain-separated by construction from the cookieless `visitor_id` (which is salted daily and rotates, `RFC:123`) and from the app surface's `cityHash64(install_id)` (§5.2, a different hash function over a different domain). It carries **no daily salt**, which is the entire point: a cookie that survives midnight is what makes a multi-day visitor one visitor, and it is why cookie mode's `visitors` can be strictly less than the sum of its own per-day `visitors` where the cookieless mode's is equal to it by construction. **Ignored unless the product has cookie mode enabled**, exactly as `f` is ignored unless memory is enabled — an ignored `vid` leaves the row's `visitor_id` at the daily salted hash, and the event is stored, never rejected. Being ignored is **counted and not silent**: a `vid` on a product whose mode is off means a cookie is being set in a real visitor's browser on a site whose published privacy paragraph says none is, so it increments `ingest_counters` under the reason `cookie_id_ignored` — an open `LowCardinality(String)` that costs no migration. This sentence read *"Unlike `f`, being ignored is counted"* through rev 0.20; **`f` is counted too as of rev 0.21** (`memory_ignored`), and the asymmetry it named is retired rather than merely unmentioned. That is the under-disclosure direction and it is the one this field's handling is designed around. **The cookie build sends `vid` on every web event and `jelto.js` cannot emit one under any configuration**, which is the property that makes *"is this product running the cookie build"* a fact **derivable from the rows themselves**. Nothing stores that answer: it is a read over `events`, so a customer who reverts to `jelto.js` stops being in cookie mode as soon as their next web event lands, with no job to run and no column to go stale. |

### 5.2 `s = "app"`

| Field | Req | Type | Rule |
|---|---|---|---|
| `iid` | MUST | UUID | `install_id`. Nil UUID → `400 invalid_install_id`. |
| `av` | MUST | string ≤ 32 | App version as displayed to users. |
| `os` | MUST | `"macos"` \| `"windows"` \| `"linux"` | |
| `osv` | MUST | string ≤ 32 | OS version, e.g. `15.1`, `10.0.22631`. |
| `arch` | MUST | `"arm64"` \| `"x64"` \| `"x86"` | |
| `a` | MAY | string `^[a-z0-9-]{1,32}$` | App slug as registered under Settings › Apps (`mac`, `win`, `helper`). Unknown slug → `other`; absent → the `os` value, and an **empty `a` is an absent `a`** (§5.1 `f`). Lets a product with two apps on one OS tell them apart. |

#### Installs that predate SDK adoption

This optional signal adds one reserved property to the fixed `install` schema, using the existing `props`
object and key/value grammar; it adds no timestamp, identifier or new top-level wire type.

| Field | Req | Type | Rule |
|---|---|---|---|
| `props.install_origin` | MAY, on `install` only | `"new"` \| `"existing"` \| `"unknown"` | Host-supplied classification at the first SDK initialization for this claim. `new`: the host knows this is the app installation's first launch. `existing`: the app installation already existed before Jelto initialization. Omitted means `unknown`, never `new`. Other values are `invalid_field`; this reserved property needs no customer allowlist. |

SDK initialization exposes an optional `installOrigin` enum (idiomatic naming per SDK),
defaulting to `unknown`. The host must inspect its **pre-existing** first-launch/onboarding
state before overwriting it: a saved first-launch date or completed onboarding can establish
`existing`; absence of a completed-onboarding flag alone cannot establish `new`. The SDK must
not infer origin from its own newly created identity file. This deliberately coarse bucket
does not transmit the date, elapsed days, onboarding history, or any host identifier.

Capture and persist the classification when the claim is first created, alongside its durable
claim state; queued events and retries retain it unchanged. Later initialization calls cannot
reclassify an existing claim. Legacy state without this classification stays `unknown`,
even if the host now supplies a hint. `reset()` rotates the SDK identity with origin
`unknown`: it cannot establish a new app installation. Disable followed by a new init
can capture that init's hint for its new claim. Do not backdate `t`, change claim timing, replay onboarding, or
attach this property to heartbeats. `existing` does not age into `new` or a numeric age bucket.
RFC §5.2/§8.2 and metrics §4.2d define storage and the resulting reads.

Deploy server/schema/storage/read support before SDK initialization options and host
adoption. Older servers need not accept this reserved property. Historical omissions stay
`unknown`; neither an adoption date nor a heartbeat can recover an installation's birth.

### 5.3 Reinstall detection

An `install` event carries no attribution payload of any kind — only the common §5.2 fields
and its reserved origin property. The
server treats an `install` for an `install_id` that already has an `installs` row, within 30 days,
as a **reinstall**: the row is stored in `events` with `is_reinstall = true` and is excluded from
the **`installs` table** (`installs_mv` filters `is_reinstall = false`, RFC §5.2) and from
`new_installs_by_day` (`spec/metrics.md` §4.2 filters `is_reinstall = 0`). It is **not** excluded
from the `installs` **metric** — a reinstall is an install (`spec/metrics.md` §4.2, §9), which is
why the fixture's `installs` is `ok 5` while `new_installs_by_day` sums to 4. This behaviour is
independent of attribution and unaffected by its removal.

## 6. Acceptance and rejection

Processing is per event. The envelope is accepted (`202`) even if every event is rejected.

```json
202 { "rejected": [ { "i": 2, "reason": "prop_not_allowlisted", "field": "email" } ] }
```

| `reason` | When |
|---|---|
| `unknown_event` | an event unsupported by an older server; current servers discover custom names automatically |
| `event_schema_limit` | discovery would exceed 100 custom names or 20 property keys for one event |
| `reserved_event` | a client-sent server-only payment/subscription name, including `purchase` (§4, §7) |
| `prop_not_allowlisted` | a property outside a fixed built-in schema (§4); custom keys are discovered |
| `origin_not_allowed` | web event whose request `Origin` (or `u` host when Origin absent) is not one of the product's domains |
| `missing_field` / `invalid_field` | §3–§5 violations; `field` names it |
| `duplicate` | `id` seen within 10 minutes (silently counted; not returned unless `?debug=1`) |
| `spam_referrer` | web event whose referrer host is on `spec/signatures/referrer-spam.txt` |
| `page_blocked` | web event whose `path` matches the product's page blocklist (Settings › Shields) |
| `attack_throttled` | eligible web event exceeding additional active attack-mode limits; counted once, with no stop instruction (spec/product-expansion.md §5) |
| `country_blocked` | event whose resolved country is on the product's country blocklist |
| `no_session_for_engagement` | an `engagement` whose visitor has no live session (RFC §5.3) — the page was open longer than the 30-minute idle window, or the session was lost to a crash. Counted in Ops; never stored, so it can never create a session of its own. |
| `stopped` | product or platform kill switch active; the response also carries `stop` |

Rejections are counted per `(product, reason)` and surfaced in the Ops card. They are never
stored as events. **Everything the server drops from the moment it knows whose it is, is
counted** — including the whole-request drops that never reach per-event processing: a
product-scoped `429` and a backpressure `503` each add `len(e)` to the reasons `throttled` and
`backpressure` (§9). A drop that no counter records is a traffic dip the customer reads as a real
decline, and `spec/snippet.md` B11 names the Ops card as the snippet's only diagnostic channel.

**Before that moment, nothing can be, and that is the whole of the exemption.** It is stated as a
class rather than as a list of cases because every member has one cause: `ingest_counters` is
keyed on `product_id`, and a request refused before `p` has been resolved to a product row has no
product to attribute it to. The class is the pre-parse per-source `429` (§9 names it), every
envelope `400` of §2a, the `413` — and the one that is *silent* rather than merely uncounted, an
**unknown `p`, which answers `202 {}` and stores nothing and counts nothing**. Measured on the
running server: seven events under an unrecognised key added no `ingest_counters` row of any
reason, and neither did `malformed`, `unsupported_version`, `empty`, `too_many_events` or
`too_large`. The difference that matters to a customer is the status, not the counter: every
other member of the class refuses out loud, while a mistyped key is acknowledged. Both leave the
Ops card empty, so an empty Ops card is not evidence that a product is receiving nothing — it is
also what a product receiving traffic under the wrong key looks like. §2a's `202` row names the
unknown key, so a reader arriving at the status meets it there too.

**`reserved_event` exists because `unknown_event` would have been a lie.** `purchase` is not
unknown to the server — §4 knows the name and forbids it — and answering as though it were merely
unrecognised would understate what happened by exactly the distance that matters. Worse,
Before automatic discovery, `unknown_event` was the rejection an `event_schema` entry cured: a customer who saw a stream of
`unknown_event` rows named `purchase` on their Ops card could add `purchase` to their own allowlist
to make the rejections stop, and have the event **accepted** — client-sent money landing on an
endpoint authenticated for a pageview. `reserved_event` is what stops that path from opening at
all: it is checked ahead of the `event_schema` lookup, not through it, so no allowlist entry can
turn it off.

`spam_referrer` is the one rejection that deletes a row on the strength of a signature match, so
its list stays **small and hand-reviewed**. It is not the place for a mechanically imported
third-party blocklist: a real site wrongly listed there loses data permanently, and this project's
rule — stored rows cannot be reclassified, classification stays on read — applies to a referrer
host exactly as it does to a bot needle. A large generated list belongs in the server's query layer as an
on-read `ref_host` classification beside `sources.yaml`, where growing it reclassifies history and
a mistake costs a label rather than a row. The reason value itself stays in this enum.

**Delivery semantics.** A `202` means the envelope was accepted for processing, not that its rows
are durable, and the pipeline is **at-least-once**: `internal/ingest` retries a failed insert,
spools on failure, and re-drains a spool file whose delete did not complete. Duplicates from those
paths are suppressed in ClickHouse by `insert_deduplication_token` = the batch id, stable across
every retry and carried across the spool as the file name (RFC §7.2). `id`'s 10-minute window
(§3) is a separate, process-local defence against a *client* resending; it does not cover replay
and must not be relied on for it. A client MUST NOT treat a lost `202` as a reason to alter the
`id`s in the batch it resends.

## 7. Custom event discovery

`event_schema(product_id, event, allowed_props[])` is a discovered catalog in Postgres.
A custom event requires no preregistration: its first structurally valid, eligible
receipt adds the name and property keys. New keys on an existing event are merged.
Only names and keys are stored in this catalog, never property values. The limits
are 100 custom names per product and 20 distinct keys per event; a discovery that
would exceed either is rejected `event_schema_limit` with no partial schema write.
Concurrent discovery and optional manual catalog edits serialize on the product.
A removed event can be discovered again when sent; removal is not a collection block.

Discovery follows origin, blocking, admission and reserved-name checks, before
claiming deduplication IDs or writing event/session data. A catalog storage failure
returns retryable HTTP 503 and MUST NOT consume the failing event's ID.

`heartbeat` install-property keys are also discovered. Its `license` key remains
built in, and install-property value rules remain unchanged. Other built-ins retain
fixed schemas: pageview and engagement take no props; install accepts only the optional
`install_origin` enum in §5.2; app_updated takes
from_version/to_version; app_update takes from_version/to_version/status and an
optional reason; click events take their fixed property; onboarding takes
status/reason. Their unsupported keys still return `prop_not_allowlisted`.

The following server-only names MUST be rejected `reserved_event` by public ingest
and by optional event settings writes, regardless of a catalog entry:
`purchase`, `payment`, `free_trial`, `trial_started`, `trial_converted`,
`subscription_started`, `subscription_upgraded`, `subscription_downgraded`,
`subscription_renewed`, `subscription_cancel_scheduled`, `subscription_reactivated`,
`subscription_ended`. Automatic payment goals do not consume custom catalog slots.

`GET /api/v1/products/{product}/events` returns the configured `schema`, plus
optional observed channels. `unknown_seen` lists recently rejected unknown names;
`observed_app_goals` lists received app goal names when `surface=app`.
`observed_props: [{"event":"paywall_shown","keys":["feature"]}]` lists stored
property keys absent from each configured event's current `allowed_props`.
This repairs historical or manually edited catalog gaps; normal receipt discovery
above already merges new custom keys. Allowlisting a key makes its previously
collected values queryable immediately; no resend or data backfill is needed.

Observed properties use rows received in the last seven days (`ingest_ts`, UTC),
scoped to this product and optional `surface=app|web`; omitting surface combines
both. Only editable custom events and heartbeat are eligible; fixed built-ins,
automatic payment events and heartbeat's built-in `license` key are excluded.
Select at most the first 100 configured eligible event names, and at most 20
distinct missing keys per event, both sorted bytewise. Only keys matching the
allowlist grammar `^[a-z0-9_]{1,32}$` are returned. Events without missing keys
are absent. Values are never returned. These bounds describe recent suggestions,
not an exhaustive lifetime inventory.

Each observed channel is optional independently: a successful empty property
read returns `observed_props: []`; a ClickHouse failure omits `observed_props`
while still returning the configured schema. Clients must treat event names and
keys as customer text, escape them when displayed and preserve their exact values
when requesting the existing allowlist edit operation. Discovery never grants
query access by itself: the per-event read-time allowlist still applies.

To add observed keys safely, use `PUT /api/v1/products/{product}/events?mode=merge`
with the existing array body, for example
`[{"event":"paywall_shown","allowed_props":["feature"]}]`. Under the same
product transaction lock as automatic discovery, merge unions each named event's
keys with its current keys and preserves every unmentioned event. The 100-event
and 20-key limits apply to the resulting union; overflow returns
`400 too_many_entries` (`field: schema`) and rolls back the whole request.
Success returns the complete sorted stored schema, in the ordinary PUT response
shape. An empty merge is a no-op. Omitting `mode` retains the original whole-list
replacement semantics; an empty, repeated or unsupported mode is
`400 invalid_field` (`field: mode`), never an implicit replacement.

## 8. Kill switch

Any response MAY include:

```json
{ "stop": { "until": 1756300000, "scope": "app" } }
```

`scope` is `app` or `web`. Clients matching the scope MUST stop sending until `until` (Unix
seconds). A client whose surface has a `heartbeat` — `s = "app"`, §4 — MUST make its first
request after `until` **one `heartbeat`, alone in its batch**, before anything the queue is
holding, and MUST NOT drain the queue until that request has been answered. There is no other
remote instruction.

The re-check was a `MAY` through rev 0.16 and nothing else in the contract treated it as optional.
`spec/sdk-conformance.md` C16 **asserts** it — "at ~60 s one `heartbeat`, then the queue drains" —
and RFC-0001 §8.6 item 16 states it flat: "The SDK re-checks by sending one heartbeat after
`until`". A wire-conformant SDK that simply drained its queue therefore failed C16 while breaking
no rule this document had, and the wire moved onto the test rather than the other way round. The
narrowing had to land before the freeze; after it, the only repair would have been a second field.

What the probe is *for*, which is why one event and not the batch: a switch still on at `until` —
or re-armed while the client was quiet — answers `202` with a `stopped` rejection **per event**,
counted per event, so a client that drains a thousand queued events into a live switch spends its
whole backlog and puts a thousand `stopped` rows on the customer's Ops card to learn what one
event would have told it. Executed: two events sent under an active switch answered
`{"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"}],"stop":{"until":1788188001,"scope":"web"}}`
and added `stopped = 2` to `ingest_counters`; an `install` sent to the same product on the app
surface, against a `web`-scoped switch, was `202 {}` and stored (C16b).

A web client has no `heartbeat` to send and no queue to retry from — `spec/snippet.md` B7 halts on
`stop.until` and has no retry at all — so for it the switch is simply a period of not sending, and
it resumes with the next pageview. A `stop` never carries `Retry-After` and a `429`/`503` never
carries `stop`: they are separate channels, and §9 governs the other one.

## 9. Rate limits

Token bucket per product: 2 000 events/s sustained, 10 000 burst → `429` with `Retry-After`.

**`Retry-After` is a floor, and a client that retries MUST honour it.** Rev 0.16 advertised the
header without requiring anything of the client, so C8's backoff was free to fire before the
moment the server had just named. Every `429` and every `503` this server sends carries it as
whole delay-seconds (RFC 9110 §10.2.3's `delay-seconds` form; the HTTP-date form is never sent —
executed, both `429`s answer `Retry-After: 1`). A client MUST NOT send again before it elapses.

**How it composes with the SDK's own backoff**, stated here because two rules that can disagree
are one rule missing. `spec/sdk-conformance.md` C8 gives an SDK a `~1, 2, 4, 8 …` s backoff with
±20 % jitter, capped at 3 600 s and persisted across launches. That schedule and this header are
**both floors, and the next attempt is at the later of the two**:

- **The header binds while it is the larger.** With `Retry-After: 1` it binds only the first
  retry, whose step is ~1 s ±20 % and may be 0.8 s. A server that names a longer wait gets it.
- **The backoff binds once it is the larger, and it advances regardless.** A `Retry-After` smaller
  than the current step never shortens the schedule and never resets it. A server answering
  `Retry-After: 1` for the tenth consecutive time is a server in trouble, and a fleet that dropped
  back to 1 s each time it was told to is the retry storm the backoff exists to prevent.
- **Absent, unparseable, or not `delay-seconds` → treated as absent, never as zero**, and the
  backoff alone governs. That is C8's `down` arm unchanged, and it is also what stops a proxy that
  strips or rewrites the header from turning a refusal into an immediate retry.
- **Clamped to the same 3 600 s ceiling C8 puts on its own backoff.** The header is the server's
  request, not a remote instruction: §8 is the only mechanism in this protocol that may silence a
  client for long, and it is bounded, scoped, and set by the customer. An unbounded `Retry-After`
  — settable by any intermediary on the path, not only by Jelto — would be a second kill switch
  with none of those properties.

Which of the two governed a given attempt is not observable to the server and needs no signalling.
C8 asserts the composed schedule, not the header.

The per-source bucket exists to bound **one abusive client**, not to cap a building. It is keyed
on the hashed `(IP, User-Agent)` pair, never on the address alone — several hundred people behind
one office NAT or a mobile CGNAT range share an address but not a UA, and keying on the address
alone silences the largest genuine reader population a site has. Sustained rate: 240 requests/min
per key. The bucket is process-wide and pre-parse, ahead of the body read, because it is a denial
-of-service guard rather than a per-product quota; the per-product bucket above is the quota.
Neither key is ever stored: both are SipHash values under a random per-process key, and the
User-Agent enters the hash only.

Both limits and the backpressure `503` are **counted** as `ingest_counters` reasons — `throttled`
for the product bucket, `backpressure` for the `503` — so a customer whose launch crossed the cap
sees it on the Ops card instead of reading a plateau as the launch fizzling. The web snippet
cannot act on a `429` (`spec/snippet.md` B7 has no retry, B11 swallows the error), so for web the
status code is not the diagnostic; the counter is. The status code stays `429`/`503` rather than a
counted `202`, because an app SDK **can** retry (RFC §7.3) and turning the refusal into an
acknowledgement would make it discard events it could have resent. The pre-parse per-source drop
has no product to attribute and is left to process metrics.

## 10. Versioning policy

- Fields are only ever **added**. A field name is never reused with a different meaning.
- Unknown fields MUST be ignored by the server, never rejected.
- A change that would break an existing client is a new path (`/v2/e`), not a new `v` value.
- `spec/wire-v1.schema.json` is the executable form of this document; when they disagree the
  schema is wrong and is fixed to match this text.
- **`vid` was reserved at rev 0.13 and became a field at rev 0.20.** The name was taken three
  revisions before anything could use it, for the one reason a later append cannot work around:
  the second line of this section. It is now §5.1's cookie-mode identifier and MUST NOT be given
  any other meaning. **This is the append the freeze was designed to permit, not an exception to
  it** — the first two lines of this section have permitted it the whole time, at the same cost,
  and rev 0.13's entry says so in advance. Nothing was narrowed and no sender that predates it is
  affected: a client that has never heard of `vid` sends none, and the server answers exactly as
  it did.
- **The freeze's real trigger is a build in the field, not a date.** End-of-M1 is only this
  document's estimate of when a client compiled against this contract is out there and cannot be
  recalled the way a server deployment can; that is what the freeze is bought against, not the
  milestone itself. The date holds or moves on evidence of when such a build actually ships, never
  on preference for a shorter or longer window to add a field.

## Amendments to RFC-0001

- rev 0.23: **the client side of §1 and §2 — an invalid endpoint or key is refused at `init`,
  never sent and never retried** (§1, §2) — *every app SDK; no server or schema change, and the
  freeze is not in play.* §1 now requires an endpoint to be an absolute `http(s)` URL without
  userinfo, treats an empty value as absent, and makes a non-empty invalid one leave the client
  inactive with a debug line instead of falling through to another host or retrying a builder
  error forever; §2 now requires the SDK to refuse a key outside `^prd_[a-z0-9]{10}$` at `init`.
  Written down because the four app SDKs had shipped four answers: .NET refused, Electron and
  Swift fell through to the next host, Tauri retried an empty endpoint indefinitely.
  `spec/sdk-conformance.md` v0.18 §5 adds the matching owner-only storage rule.
- rev 0.21: **an ignored `f` is counted too — the `f`/`vid` asymmetry is retired** (§5.1) —
  *the account API's `memory_active` fact, and the ingest handler.* **No field changes and the
  freeze is not in play**: this is server behaviour and one new value in `ingest_counters.reason`, an
  open `LowCardinality(String)`. No sender is affected, no validation moves, nothing is narrowed.

  What it reverses is a sentence rev 0.20 wrote deliberately — *"Unlike `f`, being ignored is counted
  and not silent"* — and the reversal is not an oversight, so the reasoning is recorded rather than
  left to be inferred. **The difference between a cookie and a `localStorage` label is severity, and
  severity was never the question.** That sentence was written when an ignored `f` was one thing: a
  LOST LABEL, a piece of attribution the customer had chosen not to keep, with nothing to report
  about it. What changed is not how bad the value is but **what its absence is evidence of**. Since
  the account API's request/fact split (`memory_active`, `cookie_mode_active`), an ignored `f` is the
  only signal anywhere that a tag pasted with `data-memory="on"` is still writing `jelto_first` into
  every visitor's `localStorage` while the setting is off and the paragraph the customer publishes
  denies it. The server dropping the label does not reach into the browser, exactly as its refusal to
  use a `vid` does not un-set a cookie.

  So both axes now have the same shape, for the same reason: **under-disclosure is the harmful
  direction, and severity is an argument about how bad a thing is, never about whether it can be
  seen.** A discarded value leaves no row — that is what made both facts invisible — so both leave a
  counter instead. `memory_ignored` rides an ACCEPTED event alongside `cookie_id_ignored` and is
  excluded from the Ops card's rejections for the same reason (`spec/metrics.md` §4.6).

- rev 0.20: **`vid` becomes a field — the reserved name is spent** (§5.1, §10) —
  *the snippet's cookie mode, and the product decision behind it.* **This spends the insurance
  rev 0.13 bought, on the thing it was bought for.** §10 called a reserved name "the only insurance
  the freeze actually calls for", and a reader who finds `vid` in §5.1 should be able to see that
  the insurance was *used* rather than wonder whether the freeze was broken: rev 0.13 took the name
  and nothing else, precisely so that this revision would be an ordinary append. It is one. §10's
  first line has permitted it the whole time, at the same cost it would have charged at rev 0.13,
  and nothing here is narrowed — no sender that predates this revision is affected, because a client
  that has never heard of `vid` sends none and the server answers exactly as it did.
  **§5.1** adds the row. Three of its clauses are decisions rather than description.
  **The grammar is one a UUID cannot satisfy** (`^[A-Za-z0-9_-]{22}$`, 16 random bytes in base64url),
  and that is boundary 2 — *the web cookie id and `install_id` never appear in the same row or the
  same table* — made **checkable by reading the grammar** instead of trusted to a derivation. The
  weaker alternative was a UUID plus a domain-separated hash, which is correct and which no reviewer
  can verify without reading `identity.go`; this way an `install_id` cannot be placed in the field at
  all. **The value is never stored** — `visitor_id` is `sipHash-2-4("jelto-cookie-v1" ‖ product_id ‖ vid)`,
  domain-separated from the salted cookieless hash and from the app surface's `cityHash64(install_id)`
  by using a different construction over a different domain, which `internal/ingest/identity` pins with
  a test that feeds the cookie constructor the base64url of the very UUID bytes the app constructor
  hashes and asserts the two do not meet. **And it carries no daily salt**, which is the whole feature:
  a cookie that survives midnight is what lets cookie mode's range `visitors` be strictly less than the
  sum of its per-day `visitors`, where the cookieless mode's is equal to it by construction.
  **Ignored, never rejected, and counted.** A `vid` from a product whose cookie mode is off leaves the
  row's `visitor_id` at the daily salted hash and the event is **stored** — the `f` precedent in §5.1,
  and for a stronger reason: the sender that ships `vid` early is a customer who pasted the wrong file,
  and dropping their pageviews would cure a text mismatch with a silent analytics blackout. Unlike `f`
  it is **not silent**, because it is evidence that a real cookie is in a real browser on a site whose
  published paragraph denies it — the under-disclosure direction, aimed at the readers least able to
  check. `cookie_id_ignored` is what makes that state visible on the Ops card.
  **Executed, unlike rev 0.18.** This revision is *not* specified ahead of its implementation: the
  parser reads the field, the schema constrains it, `spec/wirecheck`'s census knows it, and the
  Go/schema agreement corpus carries mutants for it. What is **not** here is the sender — `jelto.cookie.js`
  is a separate build on its own budget (spec/snippet.md §7 boundary 1) and is not this document's to ship.

- rev 0.19: **the production ingest host is named** (§1) — *the snippet's install block and cookie mode,
  and the M3 exit criterion.* This is an
  **append in the only sense that matters** — it fixes a value that was already normative on the
  web surface and gives the app SDKs something to default to — and it lands before the freeze
  because after it the two launch SDKs would be shipping against a contract that names no host.
  What it repairs is not a missing sentence but a **silent failure in two of the three clients**:
  `spec/sdk-conformance.md` §3 makes `JELTO_ENDPOINT` required and the runner injects it into
  every host it spawns, so the conformance suite certified both SDKs — twice in a row, repeatedly —
  against a configuration **no shipped application can reproduce**. A green gate on a path that
  does not exist in production is the same class of defect as an assertion nothing runs, and this
  is the largest instance of it found so far. The suite is not at fault and needs no change: §3's
  requirement is correct for a conformance host, and the default is a thing only an SDK's own unit
  tests can assert, which is where it now is.

- rev 0.18: **`purchase` reserved as a name a client may not send** (§4, §6, §7) — *the revenue product
  decisions.* The mechanism ships in
  **M2** and the metrics and the card in **M3**, but reserving the name lands now,
  before the end-of-M1 freeze, for exactly the reason `engagement` landed in rev 0.9 for metrics
  that did not ship until M2: reserving a name is a **narrowing** — a new rejection, not an
  append — and a narrowing is the one class of change §10's freeze forecloses. Everything else
  revenue touches is either an append this document already permits forever, or is not this wire
  at all: a payment reaches Jelto over `POST /api/v1/payments`,
  an endpoint this document neither governs nor freezes.
  **§4** adds `purchase` — surface **both**, no client-facing required fields because it has no
  client-facing writer, fixed props `amount` (a decimal as a string, stored as sent and never
  rounded, negative for a refund) and `currency` (ISO 4217, uppercase) — and states plainly that
  these rows are written only by `POST /api/v1/payments`, never by a sender on `/v1/e`: that
  endpoint is authenticated by a public product key and an `Origin` check, the right trust level
  for a pageview and the wrong one for a number that reaches an invoice.
  **§6** adds `reserved_event` for a client-sent `purchase`. `unknown_event` would have been a
  lie — the name is known to the server, it is forbidden, not merely unrecognised — and worse, it
  is the rejection `event_schema` (§7) exists to cure: a customer could add `purchase` to their
  own allowlist and have it **accepted**, which is exactly the failure `reserved_event` exists to
  prevent by running ahead of the `event_schema` lookup rather than through it.
  **§7** states that `purchase` may not be added to a product's `event_schema` under any prop
  set, and why an entry there would buy a client nothing even if saved: §4 reserves the name at
  the wire, not by the absence of an allowlist row, so the settings write path refuses to save
  one naming `purchase` rather than saving an entry that would do nothing.
  **§10** gains one sentence naming the freeze's real trigger: it holds because a build this
  project does not control will be in the field, not because a milestone ends. End-of-M1 is that
  build's expected date, not its cause, so the freeze is re-openable on evidence of when such a
  build actually ships — never on preference for a longer or shorter window to add a field. No
  existing rule in §10 is loosened by this; it only names why the date is the one it is.
  **Nothing here is executed.** This revision is **specified ahead of its implementation** — the
  phrase this document already uses for `402` (§2a) and for `license`'s Postgres grammar
  (rev 0.16) — because `internal/ingest` has no code path for `purchase`, `POST
  /api/v1/payments` does not exist, and no test asserts a byte of the above against a running
  server. Nothing was sent, measured, or read off ClickHouse for this entry.
  **The schema needs no edit.** `spec/wire-v1.schema.json` validates a request body's *shape*,
  and `event_schema` membership, `unknown_event` and now `reserved_event` are business-rule
  lookups the schema has never performed — a client-sent `purchase` is shape-valid JSON exactly
  as any other unbranched custom event is, and no `pattern` or `const` this side of a full
  allowlist reproduces "this name is forbidden, not merely unallowlisted." The precedent is the
  rev 0.15 amendment: the schema is deliberately left behind a rule the parser does not yet
  enforce, because a constraint no sender's request can exercise would pass the agreement test
  while diverging silently from what this revision means.

- rev 0.17: **the five prose contradictions the freeze would have locked in** (§2a new, §5.1
  `pv`, §6, §7, §8, §9) — *fix-queue Group W: W6, W7, W11, W12, W13, the last five rows in the
  table.* Two are **narrowings** and had to land now: §8's kill-switch re-check becomes a MUST,
  and §9 requires a client honour `Retry-After`. §2a is an **append**, which the freeze would not
  have forbidden later, and it lands now anyway because `spec/sdk-conformance.md` C9b already
  depends on it. §5.1, §6 and §7 change no behaviour at all — they were sentences this document
  stated more absolutely than the server keeps.
  **Every behaviour below was sent to the running server on the compose stack and read back out
  of ClickHouse or off the response**, except the one row that is marked as specified ahead of
  its implementation.

  **W6, first half — §8's `MAY` was the outlier, so the wire moved onto the test.**
  `spec/sdk-conformance.md` C16 **asserts** the post-`until` heartbeat ("at ~60 s one `heartbeat`,
  then the queue drains") and RFC-0001 §8.6 item 16 states it flat, while §8 said a client "MAY
  retry with one `heartbeat` afterwards". A wire-conformant SDK that simply drained its queue
  failed C16 while breaking no rule this document had — three artifacts, one of them permissive,
  and it was the permissive one that was wrong. §8 now requires the first request after `until` to
  be one `heartbeat` alone in its batch, before the queue. The cost of the alternative is
  measured, not asserted: a switch still on answers `202` with a `stopped` rejection **per event**,
  counted per event. Executed — two events under an active `web`-scoped switch returned
  `{"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"}],"stop":{"until":1788188001,"scope":"web"}}`
  and added `stopped = 2` to `ingest_counters`, while an `install` on the app surface against the
  same switch was `202 {}` and stored (C16b). A client draining a thousand queued events spends
  the backlog and writes a thousand Ops-card rows to learn what one event tells it. A web client
  has no `heartbeat` and no retry (`spec/snippet.md` B7), so for it the switch is only a pause.

  **W6, second half — `Retry-After` and C8's backoff are one rule now, not two that can
  disagree.** §9 advertised the header and required nothing of the client; C8 gives a
  `~1, 2, 4, 8 …` s backoff with ±20 % jitter. Both documents were satisfiable by an SDK whose
  first retry fired at ~0.8 s against a server that had just said 2 s. §9 now makes honouring the
  header a **MUST** and states the composition rather than leaving it to be inferred: **the header
  and the backoff are both floors, and the next attempt is at the later of the two.** Four
  consequences, each stated because each is a place two readers would have guessed differently —
  the header binds while it is the larger; the backoff advances regardless and is never shortened
  or reset by a smaller header (a fleet dropping back to 1 s each time it is told to is the retry
  storm the backoff exists to prevent); an absent, unparseable or non-`delay-seconds` header is
  treated as **absent, never as zero**, so a proxy rewriting it cannot manufacture an immediate
  retry; and the honoured value is **clamped to the same 3 600 s ceiling C8 puts on the backoff**,
  because §8 is the only mechanism in this protocol that may silence a client for long and it is
  bounded, scoped and customer-set, while `Retry-After` is settable by any intermediary on the
  path. Executed: both of this server's `429`s answer `Retry-After: 1` with a body of `{}` — the
  per-product bucket at request 103 of 100 events each (10 300 > the 10 000 burst) and the
  per-source bucket at request 241 (240/min). At `Retry-After: 1` the header binds only the first
  retry. C8's assertion is corrected to the composed schedule, and C8b and C8c are added for the
  four edges; `mockd` gains `429:<v>` / `503:<v>`, since none of the edges is reachable with a
  fixed `2`.

  **W7 — the `402` no document defined, and the `413` body no document named.** The product
  has required `402 Payment Required` on `/v1/e` for `trial_expired` and `paused` since its
  first billing revision, and C9b makes an SDK's handling of it mandatory — *"an SDK that treated it as retryable
  would hammer the endpoint for the life of the install"* — while this document named no `402`
  anywhere and the account API's `/v1/e` listed `202/400/413/429/503` and nothing else. Rather
  than adding one status to two files, **§2a now states every status this endpoint answers and
  the exact bytes of each body**, because the gap was never `402` alone: executed, the server also
  answers `500 {"error":"internal"}`, `204` to an `OPTIONS` preflight and `405` to any other
  method, and none of those three was documented on this path either. The `413` body is
  `{"error":"too_large"}` — executed, a 70 101-byte envelope answers exactly that — and it had
  been named nowhere while three of its `400` siblings were named in §2. The account API gains
  `402`, `405` and `500` on the POST and an `options` operation for the `204`.
  **The cross-reference that did not resolve.** The billing contract's last bullet sent the reader
  to "wire §8" for the SDK's handling of `402`. §8 is the kill switch — a different mechanism, and
  the only thing the two share is that both stop a client from sending. It now points at §2a.
  **`402` is documented ahead of its implementation, and the exception is guarded rather than
  trusted.** The server has no billing package, its account records carry no billing state, and the
  handler's product lookup returns configuration rather than entitlement, so nothing in the
  server can emit it; a census of the server sources for `402`, `StatusPaymentRequired`
  and the four billing states finds no code path at all — only the guard this
  revision adds and one ClickHouse migration comment whose upstream filename happens to contain
  the digits. The ingest handler records the one
  exemption with its reason, and the contract test **fails if `402` ever becomes emittable while
  still listed** — the same shape `pendingWireDecisions` had, so the entry cannot outlive the
  reason for it.
  **What binds the three artifacts from now on.**
  `internal/ingest.TestIngestResponseStatusesMatchTheWireSpecAndTheAPISchema` reads every status
  this package can write **out of the source with go/ast** — resolving `WriteHeader`'s forwarders
  to a fixpoint, so `writeJSON`, `writeEnvelopeError` and `writeRetry` are discovered rather than
  listed — and compares it against §2a's table and against the account API, in both directions.
  `TestEnvelopeErrorBodiesAreNamedInTheWireSpec` does the same for the `{"error":…}` vocabulary,
  which is disjoint from §6's per-event one: `too_large` and `too_many_events` are not §6 reasons
  and never appear in a `rejected` array. It mirrors `internal/ingest/api_contract_test.go` one
  layer up, and it is what makes the `413` body a checked claim rather than a sentence.

  **W12 — §5.1's `pv` promised unconditionally what §6 grants conditionally.** §5.1 said an
  engagement whose `pv` is unknown "is still stored"; §6 says one whose visitor has no live session
  is never stored. Both are true of different conditions, and the case where they meet is the one
  that matters — a long read whose beacon arrives after the 30-minute idle window, which is
  precisely where §5.1 read as a promise the server does not keep. §5.1 now carries the qualifier:
  an unknown `pv` is never *by itself* a reason to drop the row, and the live session is what
  decides. Executed both ways against the running server, same unknown `pv` both times: with a
  live session, `202 {}` and one stored row carrying its `e` and `sd`; with no live session,
  `202 {"rejected":[{"i":0,"reason":"no_session_for_engagement"}]}`, one `no_session_for_engagement`
  counter, and nothing stored.
  **F35's proposal is declined, and recorded here so it is not re-proposed.** F35 asked that an
  engagement whose `pv` matches a stored pageview be allowed to **re-open** the session, so a long
  read is not lost. It is refused for one reason that does not depend on the implementation cost:
  re-opening would let a client extend the idle window at will, which makes the session boundary
  **something the sender controls** rather than something the server measures. Every session-scoped
  number — visits, bounce rate, visit duration, entry and exit page — would then be a figure a
  well-meaning SDK bug or a hostile sender could move, and `spec/metrics.md` §5 has no state that
  can flag a number that is merely *stretched*. The reading is lost; the boundary is kept.

  **W13 — "everything the server drops is counted" had more than the one exemption §9 names, and
  they are one class.** §9 exempts the pre-parse per-source bucket. `handler.go` also answers
  `202 {}` with no counter for an **unknown product key**, which the filing named — and, measured
  alongside it, so does every envelope `400` and the `413`. §6 now states the exemption as a class
  rather than as a list, because every member has the same cause: `ingest_counters` is keyed on
  `product_id`, and a request refused **before `p` has been resolved to a product row** has no
  product to attribute. Executed: seven events under an unrecognised key, then one each of
  `malformed`, `unsupported_version`, `empty`, `too_many_events` and `too_large`, added **no
  `ingest_counters` row of any reason** — the table was unchanged at `accepted = 1`,
  `geo_no_database = 1` before and after — while a product-scoped `429` on the same product added
  `throttled = 100`, one per event of the refused envelope, exactly as §6 requires. The unknown key
  is the member worth naming separately: the others refuse out loud, and it is acknowledged. Both
  leave the Ops card empty, so an **empty Ops card is not evidence a product is receiving
  nothing** — it is also what a product receiving traffic under a mistyped key looks like.

  **W11 — §7 forbade in general what §4 does in one row.** §7 said reserved events "cannot be
  extended in v1" while §4's `heartbeat` row extends `heartbeat` with the keys a product
  allowlists in `event_schema`, which `internal/ingest/gates.go`'s `allowedProps` implements as
  `license` unioned with the product's row and C22 exercises. Rev 0.16 rewrote that very §4 row
  without touching the contradiction, which is why it was filed as sharpened-not-answered. §7 now
  carries the one exempting clause. Executed with `edition` allowlisted for `heartbeat`:
  `{"license":"paid","edition":"pro"}` is `202 {}` and both values are in `install_state`, while
  `{"license":"paid","seats":"5"}` is `prop_not_allowlisted` naming `seats`. The clause names the
  boundary as well as the exception — no other reserved event takes an allowlist, so this is one
  row of §4 and not a licence to widen the other six.

  **The schema needs no edit and rev 0.16 of it still matches this text**, for the reason rev 0.12
  and rev 0.13 needed none: nothing a sender may put in an event changed. `spec/wire-v1.schema.json`
  validates a request body; every rule in this revision is about a *response*, a *rejection* the
  body cannot express, or a sentence that was already true of the parser.

  **What Group W has left after this.** Nothing. All 15 rows are closed, `pendingWireDecisions` is
  empty and stays as the instrument for the next question, and the one item the group filed but
  deliberately did not fix — `products.kpi`'s `license:[A-Za-z0-9._-]{1,64}` arm, the W10 defect
  one column over — is still open and is not a wire field.

- rev 0.16: **the seven open wire questions are answered** (§3 `t` and `v`, §4 `heartbeat`,
  §5.1 `e`) — *fix-queue Group W: W3, W4, W5, W9, W10, plus W14 and W15, the two the W8
  comparator derived.* Every one is a **change to an existing field's rule**, so all seven land
  before the end-of-M1 freeze or they can never be made. `pendingWireDecisions` in
  `internal/ingest/wire/wire_test.go` is now **empty**: all 13 of its rows moved into
  `schemaMutants`, where the two validators are required to agree rather than merely to stay
  where they were. The corpus goes from 163 cases to **189** and its floor from 120 to 145, so the
  rows cannot be quietly deleted; **15 of the widened set fail against the rev 0.15 pair**, which
  is the mutation proof for this revision.
  **Every behaviour stated below was sent to the running server and read back out of ClickHouse**,
  not inferred from the code.
  **W3, W4 and W15 — clamp everywhere, and the outlier is not always the same side.** §5.1 says an
  `e` outside `[0, 1 800 000]` "is clamped, not rejected" and `enrich.go` clamps it; §3 says a `t`
  outside its window becomes `ingest_ts` whatever it was. On both, `spec/wire-v1.schema.json` was
  the outlier — `minimum: 0` on `t`, `minimum`/`maximum` on `e` — and its bounds are gone. On a `t`
  outside `int64` the **parser** was the outlier instead: `strconv.ParseInt` failed and the whole
  event was rejected, against a schema that states no maximum and against §3's own sentence. It
  saturates now, and `eventTime` then substitutes `ingest_ts` exactly as it does for any other
  out-of-window value. Executed before: `t = 99999999999999999999` →
  `202 {"rejected":[{"i":0,"reason":"invalid_field","field":"t"}]}`; after: `202 {}` with a row
  whose `ts` equals its `ingest_ts`. `e = 1800001` and `e = -1` store `1800000` and `0`.
  The reason this is a clamp and not a rejection: **rejecting a whole event because a device has a
  wrong clock is data loss**, and C15 forbids an SDK correcting its own clock, so the sender
  cannot fix it either — the customer would read the gap as a real decline.
  **W14 — JSON has no integer type, and only the parser could close it.** `type: integer` in JSON
  Schema accepts `1785578400000.0` and `1.7855784e12`; `optionalInteger` ran `strconv.ParseInt`
  over the raw bytes and accepted neither. A sender computing a timestamp in floating point —
  `time.time() * 1000` in Python, anything that round-trips through a JavaScript number — lost
  **every event** to `invalid_field` while a conformance run (`spec/sdk-conformance.md` W1)
  validated the same body and passed. No `pattern` can express a lexical rule about a number, so
  agreement could only come from the parser, and it is the parser that moved. It affects **five**
  fields, not the three the filing named: `optionalInteger` is called for `t`, `w`, `h`, `e` and
  `sd`. A genuine fraction is still `invalid_field` — `t = 1.5` was rejected before and is
  rejected now. Executed: the integer, zero-fraction and exponent spellings of one instant all
  store `ts_ms = 1788182242000`.
  **The precision edge, decided rather than left to be found.** A float64 cannot represent every
  int64, so a `strconv.ParseFloat` route would answer `9007199254740992` for `9007199254740993.0`
  — one millisecond from the value the sender wrote, silently, on a product whose one promise is
  that no number is shown unless it is true. The conversion walks the digits instead: **exact for
  every value `int64` holds, saturating and never rounding outside it**, and it refuses forms JSON
  does not have (`0x1p-2`, `1_000`, `1/2`, `Inf`) rather than delegating to a parser that accepts
  them. An absurd exponent (`1e400`) saturates without being materialised.
  **W5 — `v` adopts the pattern that existed only in the schema.** `^[a-z]+/[0-9A-Za-z.+-]{1,24}$`
  is normative §3 prose now and the parser enforces it. This **rejects values the server accepted
  and stored** — executed before the change, `1.2.0`, `Web/1.2.0` and `Electron/1.0` were all
  `202` and all three are in `events.client_ver` — so it is a narrowing, and a narrowing is
  precisely what the freeze forecloses. It is worth making because `client_ver` is
  `LowCardinality(String)` in `events` and in `ingest_counters` and **nothing folds it**: unlike a
  heartbeat install-property value, which `foldValue` caps at 50 distinct values per key, this
  column has no ceiling but the grammar. Nothing is deployed, so there is no stored data to
  migrate; the whole cost is in what SDKs may send, and no SDK exists yet.
  **`v: ""` means absent, stated and not implied.** Rev 0.15 already made an empty optional `f`
  and `a` mean *absent*; `v` is the same, and §3 now says so in as many words rather than leaving
  a sender to deduce it from the pattern's `{1,24}` tail, which reads as forbidding the empty
  string. The schema carries it the same way `f` and `a` carry it, as `anyOf: ["" , pattern]`.
  **W9 — the heartbeat cap is 20, not 5.** §4's "up to 5 keys" was enforced by nothing, and every
  other artifact already said 20: the parser's `MaxProps`, `event_schema`'s Postgres CHECK on
  `allowed_props`, the account API, and `$defs/props`'s `maxProperties`. §4 moved to 20. Executed:
  seven keys are `202 {}`, twenty-one are `invalid_field` naming `props`. §4 also now says what
  actually bounds a heartbeat in practice — the product's allowlist, whose own ceiling is 20 —
  because the cap and the allowlist are different refusals with different reasons.
  **W10 — `license` takes the general install-property grammar, on both sides.** §4 gave `license`
  its own `^[a-z0-9_-]{1,24}$` with no dot, while `validateReserved` has always applied
  `^[a-z0-9_.-]{1,24}$` to every heartbeat prop including `license`. §4 moved onto the general
  grammar; executed, `license: "paid.tier"` is accepted and stored in `install_state`.
  **The half that was not on the wire at all.** `products.paid_license_value` — the Settings value
  `license_conversion` compares a stored `license` against with `s.license = {paid:String}` — was
  a free string in the account API and `^[A-Za-z0-9._-]{1,64}$` in Postgres, **wider than any
  conformant app can send**. Executed on the compose stack: Postgres accepted `Paid` and a
  52-character capitalised value, and against `install_state` rows holding `paid`, the numerator
  for `Paid` was **0 out of a denominator of 4**. That is a true `0 %` the customer cannot
  distinguish from a real one, and because the query *succeeds* it is below the level any
  metric state can reach. The account API gains the pattern **and** the CHECK
  narrows (migration `20260831140000`, Postgres, goose): api.yaml alone would have been a rule
  nothing executes, since no PATCH handler exists yet, and this repo reserves "CHECK as backstop
  only" for *array* columns whose NULL and empty entries a per-row regex structurally cannot see —
  every scalar column here states its real grammar. The Postgres store now reads the grammar off
  `wire.InstallPropertyValueOK` rather than keeping a second copy of it.
  **What this does NOT close.** Group W keeps W6 (the kill-switch `MAY` against C16's assertion,
  and `Retry-After` against C8's backoff), W7 (`402` and the `413` body, unnamed in §9), W11, W12
  and W13. **W11 is sharpened by W9 and not answered by it**: §7 still says reserved events
  "cannot be extended in v1" while §4's `heartbeat` row still extends `heartbeat`, and this
  revision rewrote that very row without touching the contradiction. `products.kpi`'s
  `license:[A-Za-z0-9._-]{1,64}` arm is the W10 defect one column over — a KPI naming a license
  value no app can send renders a true `0` forever — and is left filed rather than fixed here,
  because `kpi` is not a wire field and S16 settled that contract three commits ago.

- rev 0.15: **the executable schema is brought level with the parser** (§5.1 `f`, §5.2 `a`, and
  `spec/wire-v1.schema.json` throughout) — *fix-queue Group W, W8.* Nothing the server accepts or
  rejects changed. This is §10's fourth line applied at scale — "when they disagree the schema is
  wrong and is fixed to match this text" — plus two sentences of this document that were true of
  the parser and written nowhere.
  **How it was measured, because the count in the filing was not reproducible.** A comparator ran
  220 candidate events through the compiled schema and through `wire.ParseEvent` and reported
  every case where the two disagreed on accept/reject: **64**, not the 31 the filing claimed. 53
  were schema defects, fixed here. 11 are open questions and are listed at the bottom of this
  entry. The comparator is not a throwaway: it is `validateBothWays` in
  `internal/ingest/wire/wire_test.go`, and the candidate set is that file's `schemaMutants`, which
  goes from 73 cases to 163. **52 of the widened set fail against the rev 0.14 schema** — that is
  the mutation proof, and it is also the answer to why W1–W5 had to be found by a human reading
  the two files side by side while this test was green.
  **The class that mattered: a field on the wrong surface.** The schema stated every field
  constraint globally; the parser applies §5.1 only to `s = "web"` events and §5.2 only to
  `s = "app"` ones. Under §10 a field the surface does not define is an unknown field, which the
  server MUST ignore rather than reject — so 16 of the 64 were the schema rejecting an event the
  server stores. The sharpest was `iid` on a web event, which the schema forbade outright with
  `not: {required: ["iid"]}`: no sentence of this document says that, `parseWeb` never looks at
  `iid`, and the rule would have failed a conformant SDK for a field the server ignores. Every
  §5.1 constraint now lives under `if s = "web"` and every §5.2 constraint under `if s = "app"`.
  **§4's Surface column is normative and the schema bound one row of it.** `engagement` had a
  branch; `pageview`, `click:download`, `click:outbound`, `heartbeat`, `install` and
  `onboarding:<step>` did not, so the schema accepted a `heartbeat` on the web surface and a
  `pageview` on the app surface that `validateReserved` answers `invalid_field` naming `s`. All
  seven rows are now branches.
  **`click:outbound` had no branch at all.** §4 gives it a fixed `url` prop and the server answers
  `missing_field` naming `url`; the schema validated an outbound click with no `props` at all.
  Neither fixed prop was typed either, so `{"file": 4}` and `{"file": ""}` both validated while
  the server rejected them; `file` and `url` are now `string, minLength 1`.
  **`heartbeat` had no branch, so no install-property value was checked.** §4 gives those values
  `^[a-z0-9_.-]{1,24}$` and `validateReserved` enforces exactly that on every heartbeat prop; the
  schema accepted `{"license": 5}`, `{"license": true}` and `{"license": "NOT OK"}`. The branch
  added here asserts **only** what the parser asserts: not the narrower `license` grammar in §4's
  own row (W10) and not the five-key cap (W9), because both are open.
  **`fd` is a day, not a shape.** The pattern was `^\d{4}-\d{2}-\d{2}$` while the server parses
  the value with `time.Parse`, so `2026-02-30`, `2026-02-29` and `2026-13-01` validated and were
  then rejected by the server. The pattern is now leap-year accurate. It also applied when `f` was
  absent, where the parser only length-checks `fd` — it is now scoped to the branch that requires
  `fd`, which is the one §5.1 states.
  **`u` and `r` needed a host.** §5.1 has required "a non-empty host" in as many words since rev
  0.14, and the pattern was `^https?://`, so `https://` and `https://user@` validated while
  `validHTTPURL` rejected them. The scheme is now matched case-insensitively, because RFC 3986
  §3.1 makes schemes case-insensitive, `net/url` lower-cases them and the server therefore accepts
  `HTTPS://`. The pattern approximates RFC 3986 reg-name syntax, and the approximation is a
  measured trade, not a claim of exactness: it closes six divergences (a host-less URL, a
  non-numeric port, a space, a backslash, a control character anywhere in the URL, and
  %-encoding in a host, all of which `net/url` rejects and `^https?://` accepted) and opens three
  in the other direction, because `net/url` tolerates a raw `<`, `"` and `]` inside a reg-name
  and no `pattern` this side of a full URI grammar reproduces that. The three are asserted by
  `TestSchemaApproximatesTheURLHostGrammar` rather than left to be rediscovered; a schema
  stricter than the server there rejects only hosts no sender emits.
  **`pv`, and a correction to the rev 0.14 entry below.** That entry left `pv` on the permissive
  `#/$defs/uuid` reasoning that constraining it "would make the schema stricter than the parser
  rather than equal to it". On an `engagement` the opposite is true: `pv` is the `id` of a
  pageview, §3 forbids a nil `id`, and `validateReserved` answers `missing_field` naming `pv` for
  a nil one — so the schema was the **looser** of the two and accepted an engagement the server
  drops. `pv` is now `#/$defs/nonNilUuid` inside the `engagement` branch and stays permissive on
  every other web event, which is what rev 0.14 intended.
  **The two prose sentences.** §5.1 `f` and §5.2 `a` now say an empty value is an absent one.
  `optionalString` has always returned `""` for both an absent field and an empty one, and the
  parser has always skipped the pattern check on the empty value — `r` already said so in as many
  words. Writing it down is what lets the schema accept `""` there without being wrong; nothing a
  sender may send changed.
  **What is deliberately NOT resolved.** The other 11 are questions this revision has no standing
  to answer. They are recorded in `pendingWireDecisions` in the same test file — 13 rows, the 11
  live disagreements plus W9 and W10, where the two validators agree with each other and with
  neither §4's own row — and each row asserts what **each** side does today and fails if either
  moves, so none of them can be answered silently and the suite cannot go green by having
  forgotten them. They are W3 (`e` over the cap and negative: §5.1 clamps, the schema rejects,
  `enrich.go` clamps), W4 (`t` before the epoch, against C15's ban on an SDK correcting its
  clock), W5 (`v`'s pattern exists only in the schema), W9 (the five-key heartbeat cap nothing
  enforces — measured on the running server, seven keys clear the wire and are then stopped by
  the product's `event_schema` allowlist, whose own ceiling is 20) and W10 (`license`'s grammar
  in three documents), plus two the comparator derived that the Group W table does not name.
  **JSON has no integer type**, so `type: integer` accepts `1785578400000.0` and `1.7855784e12`
  while `strconv.ParseInt` over the raw bytes does not — a sender computing `t` in floating point loses
  the whole event to `invalid_field` while a conformance run calls the body valid, and no
  `pattern` can express the lexical rule, so agreement can only come from the parser — and a `t`
  outside `int64` is rejected by the parser and accepted by a schema that states no maximum,
  which is W4 one bound further out.

- rev 0.14: **`r` is pinned absolute and `id` may not be the nil UUID** (§3, §5.1) — *fix-queue
  Group W, W1 and W2.* Both are **changes to an existing field's rule**, not appends, so both land
  before the end-of-M1 freeze or they can never be made: after it, the only repair for either
  would be a second field carrying the corrected meaning beside the broken one.
  **W1.** `internal/ingest/wire/wire.go` has always required `r` to be an absolute `http(s)` URL
  with a non-empty host, and rejects the **whole pageview** `invalid_field` when it is not. §5.1
  said only "URL string ≤ 2 048" and `spec/wire-v1.schema.json` enforced only `maxLength`, so
  `spec/snippet.md` B1 and B18 were free to specify — and did specify — that a same-origin
  referrer is "sent as path-only so the server never stores the host". Nothing had ever executed
  the three together, because `web/snippet/` does not exist; the first internal navigation on a
  customer's site would have lost its pageview outright, and the customer would have read the
  gap as a real decline. The wire is right and the snippet is wrong, for two measured reasons.
  *The path-only rule buys no privacy:* on a same-origin referrer the host is the customer's own
  site, which the server already has — `internal/ingest/gates.go` reads it out of `u` as the
  `origin_not_allowed` check (§6) and refuses the event unless it is one of the product's own
  domains — so sending it discloses nothing the request did not already carry. *And it costs the
  privacy it claims to buy:* enrich.go blanks a same-product referrer **host and path together**,
  so the full form stores nothing at all, while the path-only form has no host to match, keeps
  the blanking branch from firing, and stores the bare path. Executed: `r = "/pricing"` yields
  `ref_host = "" ref_path = "/pricing"`; `r = "https://eqbase.app/pricing"` yields both empty.
  The path-only rule stores strictly more about a customer's visitors than the rule replacing it.
  **W2.** `ParseUUID` accepts the nil UUID, so a present `id` of
  `00000000-0000-0000-0000-000000000000` set `IDProvided` and became a real dedup key. §5.2 has
  always forbidden a nil `iid` and the parser has always enforced it; `id` had no such rule, and
  `id` is the field where it matters more, because the dedup window acts on it. Executed against
  the handler before the fix: **three distinct pageviews sharing a nil `id` stored one row**, the
  two suppressions invisible because a `duplicate` is counted and not returned without
  `?debug=1`, so the response body was `{}` and the status `202`. An SDK that zero-initialises the
  field would have thrown away a product's traffic while being told it had been received.
  **No new rejection reason.** Both take `invalid_field` with `field` naming `r` or `id`, which is
  what §6 already means by "§3–§5 violations; `field` names it" and what the parser already
  emits. A new reason would be a wire addition, permanent, and would need its own row in the
  frozen enum and its own arm in `internal/ingest/api_contract_test.go`, to say something
  `invalid_field` already says precisely.
  **The schema needed an edit this time, and rev 0.12 and 0.13 did not.** Those two changed
  nothing executable; these two change what is accepted, and `spec/wire-v1.schema.json` is loaded
  by the server's agreement test and by every conformance run (`spec/sdk-conformance.md` W1,
  `spec/snippet.md` T22), so a rule left out of it is a rule nothing checks. It is brought into
  agreement at rev 0.14, from the rev 0.11 the rev 0.13 entry recorded: `r` takes
  `#/$defs/referrer` (`""` or an absolute `http(s)` URL — the empty string is legal and
  the fixture corpus sends it), and `id` and `iid` take a new `#/$defs/nonNilUuid`. `pv` is
  deliberately **left** on the permissive `#/$defs/uuid`: a nil `pv` on an `engagement` is already
  `missing_field` in §5.1, and on any other web event it is ignored, so constraining it would make
  the schema stricter than the parser rather than equal to it. `iid` is included because its rule
  existed in prose and in the parser but never in the schema — §10's "when they disagree the
  schema is wrong" applies, and leaving it out would have moved W2's asymmetry rather than closed
  it.
  **What pinning `r` to `http(s)` does not foreclose.** `android-app://com.reddit.frontpage` is a
  referrer Chrome on Android really produces, and Plausible accepts the scheme and maps the package
  back to a source. Jelto does not, and a visit from a native app is counted as direct
  (`spec/snippet.md` §2 records the limit). Accepting a third scheme later **widens** what the
  server takes; it breaks no sender that was conformant under this revision, which is the only
  thing §10's "a change that would break an existing client is a new path" protects against. So the
  freeze locks the narrowing in, not the widening out — and the narrowing is the half that had to
  land now, because it is what stops the snippet being written to a rule the server rejects.

- rev 0.13: **`vid` reserved for the opt-in cookie mode** (§10) — *`spec/snippet.md` §7.*
  The original sequencing decision said the one thing to do before the freeze was to keep the wire
  from foreclosing cookie mode. Checked against this document, **it does not foreclose it**, and
  recording that is the point of this entry: §10's first two lines already permit an append, §10's
  third line already promises that unknown fields are ignored rather than rejected, and **nothing
  in this document asserts that a web identifier is never client-supplied** — that sentence lives
  in RFC-0001 §2, which is amendable and not frozen. So a `vid` field can be appended at M4 with
  no more cost than any other append, and adding an inert one now would be specifying a field for
  a feature whose shape is three milestones from being designed.
  **(2026-09-02: that append is now M3, not M4 — PRD v0.12 moved the mode. This entry keeps its
  original wording because it is the record of what rev 0.13 decided, and the correction carries
  no revision of its own: the contract did not change. §10 permitted the append before the freeze
  and permits it now, on the same terms and at the same cost; only the date moved, and a date is
  not a wire rule. What rev 0.13 bought — the reserved NAME, the one thing a later append cannot
  work around — is exactly what makes moving the date free.)**
  What the freeze *does* make irreversible is §10's second line: a name, once used, never means
  something else. That is the one thing a later append cannot work around, so the name — and only
  the name — is taken now. **The schema needs no edit and rev 0.11 of it still matches this
  text**, for the same reason rev 0.12 needed none: nothing executable changed.

- rev 0.12: **`sd` stops claiming to be monotone** (§4, §5.1) — *fix-queue S19d.* This is a change
  to an existing field's semantics, not an append, and it lands **before** the end-of-M1 freeze
  deliberately: after the freeze the same correction could only be made by appending a second
  field, which is strictly more expensive for a claim no sender and no reader ever relied on.
  Nothing consumes the monotonic reading today — `sdk/` and `spec/conformance/` do not exist,
  `internal/ingest/wire/wire.go` enforces only the `0–100` range, and
  `spec/wire-v1.schema.json` constrains `sd` to `integer, 0..100` and nothing more, so **the
  schema needs no edit and rev 0.11 of it still matches this text**. What was wrong: `sd` is a
  monotone numerator (deepest pixel reached) over a divisor that can grow (document height), so
  it is not itself monotone. Requiring it never to decrease made the *stale* percentage the
  contractual one — a document that grew after a beacon went out kept the older, higher number
  and no honest later value could displace it, on a product whose one promise is that no number
  is shown unless it is true (`spec/metrics.md` §5). `spec/metrics.md` §4.1b changes its
  reduction to match, and `spec/snippet.md` B17 drops the clamp that was upholding the old rule.
  `e` is unaffected and keeps `max()`: engaged time has no divisor.

- rev 0.11: **numeric prop values keep the string column** (§3 `props`) — *reference audit F61,
  fix-queue S3.* No field changes; what changes is the normative sentence about what the server
  keeps. "Coerced to string on store" did not say *which* string, and the whole decision rests on
  the answer: `internal/ingest/enrich.go` `stringifyProps` writes `json.Number.String()`, which is
  the verbatim JSON literal — `29.90` stays `29.90` and `1e20` stays `1e20`, neither one rounded
  through a float. So `events.props` is lossless with respect to the wire, a typed column can be
  derived from stored rows at any later date, and this is **not** one of the columns that had to
  land while the tables were empty. `RFC-0001` §11 records why neither reference's route was
  taken; `spec/metrics.md` §4.7 closes the catalog against value-reading metrics and fixes the
  read shape for the day one is added. The trap the finding names is real and was reproduced on
  24.8.14.39 — `toFloat64OrZero` returns `0` for `29,90`, `$29.90` and `''` — but it is a *read*
  expression, and a server guard test now fails the build if it is written.

- rev 0.10: **wire-freeze triage** of the eighteen audit findings marked *URGENT before wire freeze*. Those were unverified auditor leads; every
  one below was checked against this repo first, and the verdict names what was confirmed. They
  reduce to sixteen decisions: seven changed this document, one changed the account API, and eight
  are recorded as **no**, so the freeze closes over a decision rather than an omission.

  **Accepted.**

  1. **`h`, hash routing (§5.1)** — *tracker-snippet-1.* Confirmed: `spec/snippet.md` B1 sends the
     fragment only under `data-hash`, and `internal/ingest/enrich.go:142` appends it only under the
     product's `hash_routing` column. The mismatch is one-directional — snippet on, product off —
     and it flattens every hash route to one stored `path` at write time, silently, with nothing
     rejected and nothing counted; the fragment is gone, so flipping the setting later repairs no
     row. A per-event signal is the only fix that cannot drift, it is what Plausible does
     (`tracker/src/track.js:144`, `request.ex:409`), and it is a pure append under §10. Present
     `h` decides; absent `h` keeps the product setting, so legacy and server-side senders are
     unaffected. **This spends the field name `h`** — §10 forbids reusing it, so a future viewport
     *height* field needs a different name.
  2. **`r` keeps the path (§5.1)** — *referrer-source-classification-5.* Confirmed:
     `enrich.go:171` takes `referrer.Hostname()` and nothing else; there is no path column in
     `events` or `sessions`. The wire already carries the whole referrer URL, so no field changes —
     what changes is the normative sentence about what the server keeps, and that sentence freezes.
     Without the path the Referrer tab can say `reddit.com 400` and can never say which subreddit,
     which HN item or which review drove it; all three references keep it (Plausible
     `source.ex:129`, GoatCounter `ref.go:232`, Umami `referrerPath`). The loss is one-directional:
     dropped at ingest, unrecoverable by any re-read. Query and fragment stay discarded, which is
     where click-id values and referring-site PII actually live, so the privacy invariants hold.
  3. **`u` is pinned absolute, and the stored `utm_*` set is named (§5.1)** — *tracker-snippet-4,
     feature-surface-1, referrer-source-classification-6, schema-storage-4.* `utm_*` was ambiguous
     in a document about to freeze; it now reads as the explicit five. Confirmed:
     `utm_content` and `utm_term` have **zero hits** anywhere in the repo, and `enrich.go:146-148`
     reads three. No wire field is needed — `u` already carries them — so this is the freeze
     pinning what is kept, and the storage work is tracked below.
  4. **`w` stays MAY, with the unknown bucket named (§5.1)** — *ua-device-parsing-3.* Confirmed
     (and already on record as V10): `spec/metrics.md` §3 buckets `<576 mobile`, so an absent `w`
     is `screen_w = 0` and reads as a phone. That is a read-side defect with a read-side fix, and
     the row now says so rather than leaving the repair unstated at the freeze.
  5. **What a `202` promises (§3 `id`, §6)** — *ingest-robustness-1.* The duplication itself was
     confirmed and fixed before this triage (V2, `insert_deduplication_token`), but the contract
     never stated its own delivery semantics: `grep -rn 'at-least-once|at-most-once|idempot'` over
     `spec/`, the RFC and the PRD returned nothing, and §3's `id` row read as though the 10-minute
     window were the replay defence. It is process-local memory in the HTTP handler. SDK authors
     build retry logic against this document; it now says at-least-once and names the real defence.
  6. **Every drop is counted; the per-source bucket is keyed and scoped (§6, §9)** —
     *ingest-robustness-2, ingest-robustness-3.* Confirmed by reading `handler.go`: the per-IP
     `429` (`:75`), the per-product `429` (`:115`) and the backpressure `503` (`:119`) all return
     before `h.countAt` is ever reached, while §6 claimed rejections are counted and §9 said
     "Neither is stored as data" — the two sentences contradicted each other and the code matched
     the wrong one. `ingest_counters.reason` is `LowCardinality(String)` with no closed enum, so
     `throttled` and `backpressure` cost no migration. Confirmed for the key: `ratelimit.go:70`
     hashes the bare address into **one process-wide map**, so a NAT's traffic to one customer
     spends another customer's budget, at a sustained 3.33 req/s (burst 200). GoatCounter hit this
     and fixed it by adding the UA to the key (`handlers/mw.go:432`). The status code stays
     `429`/`503` rather than Plausible's counted `202`, because an app SDK can retry and an
     acknowledgement would make it discard what it could have resent.
  7. **`spam_referrer` stays in the enum, and its list stays small (§6)** — *bot-spam-filtering-3.*
     Confirmed: `spec/signatures/referrer-spam.txt` holds 19 hosts against the 2 348 in
     GoatCounter's `refspam.go` (counted in the tree, not taken from the finding), and
     `gates.go:95` deletes the event. Removing the reason would break every client built against
     v1, and importing 2 300 third-party judgements into a permanent delete is precisely what
     this project forbids. Keeping the value while capping its scope, and putting the large list
     on-read, is the only combination that satisfies both.
  8. **Account API drift** — *api-contract-1.* Confirmed: the published 202 enum omitted
     `no_session_for_engagement`, which the ingest handler emits and four test files assert, and
     `?debug=1` was implemented but absent from the contract. A strict generated
     client fails validation on a `202` it must accept. Fixed in the account API (v0.8.0); after
     the freeze, adding a value to a published closed enum is a breaking change.

  **Declined — no wire change.**

  9. **A root-relative `u`** — *tracker-snippet-4.* The masking case it was raised for does not
     need it: an absolute URL whose *path* is masked (`https://site.example/:masked/alfa`) is
     accepted today and solves it, and a customer's own origin is not the secret. Against that,
     `gates.go:84` falls back to `u`'s host whenever the `Origin` header is absent, and that
     fallback is the only thing keeping a product's rows on its own domains. Relaxing the type
     would trade a real guard for a capability that already exists. The client-side half of that
     finding — `data-auto-pageview="off"` and a first-class `jelto('pageview', {u})` — is a
     `spec/snippet.md` M2 decision with no wire impact and is not blocked by the freeze.
  10. **Narrowing web `t` to a short past window** — *identity-sessionization-2.* The defect behind
      it is real and confirmed: `session.go:249` `Sweep` compares **server** wall time against
      `Last.End`, which comes from the client's `t` (`enrich.go:192`), while `Feed` (`:154`)
      correctly compares event time to event time — so a client with a slow clock has live sessions
      evicted, and eight pageviews in one visit become eight bounced sessions. But the proposed
      wire half does not fix it: with `t` clamped to `ingest_ts − 30 min` the same `Sweep` still
      evicts up to 30 minutes early. It only bounds the damage, and it pays for that by destroying
      *correct* timestamps — a beacon flushed after a laptop sleep carries a genuinely old `t`, and
      clamping rewrites it to the wake time. The real fix is a `LastSeen` field on the session
      state, taken from the server clock, which is code and needs no wire change. Separately, the
      backdating vector this raises is not web-specific — the threat model already has anyone
      POSTing to `/v1/e` with a public product key, and app `t` is equally forgeable — so a web-only
      clamp would be a half-measure against it. It records **no row for a forged
      `t`** at all; that gap is the right place to settle the window, for both surfaces.
  11. **`w` as MUST for web pageviews** — *ua-device-parsing-3.* A server-side sender has no
      viewport; making `w` required rejects its pageviews outright, which is strictly worse than
      an honest `unknown` bucket. The bucket is the fix (accepted above, item 4).
  12. **A viewport height field** — *ua-device-parsing-3.* The motivation given was telling a
      rotated tablet from a laptop, but `events.device` already carries the form factor from the
      UA, and once `screen`'s labels describe width bands instead of device names — the V10 repair —
      nothing is left inferring a device from a width. Adding a field for a dimension no spec
      defines is the "materialized view because it will be needed" pattern CLAUDE.md rejects, and
      the snippet's 1 800 B budget is real. Recorded with its cost stated plainly: after the freeze
      this cannot be added, and `h` is now taken, so the name would have to differ.
  13. **The trusted-IP-header startup check** — *geo-location-1.* Already fixed in `dd91844`
      (with `-allow-untrusted-remote-addr` as the explicit dev escape).
      Config and startup, never wire.
  14. **A geo-unavailable marker** — *geo-location-2.* Confirmed and real: `geo/geo.go:121` returns
      the same `("", "", 0)` for a `Lookup` **error** as for a genuine miss, and `geo.Empty` returns
      it for "no database configured", so three different facts store one value and the Locations
      card can print `(not set) 100 %` as a measured distribution. The finding's own fix needs no
      events column — `ingest_counters.reason` is an open `LowCardinality(String)` and
      `spec/metrics.md` §5 is where the state belongs — so none of it is gated on the freeze.
  15. **`(gclid)` medium keyed on the resolved source** — *referrer-source-classification-2.*
      Confirmed: `enrich.go:159` requires `row.UTMSource == "google"`, so Google auto-tagging (no
      UTM params, referrer `www.google.com`, `?gclid=…`) never takes the branch, and RFC §4.1:157
      says "the source", which includes the referrer-derived one. Write-time enrichment on a stored
      column, no wire field involved. Worth noting the finding's preferred fix — derive the medium
      on read — is the one that matches RFC §4.1's own on-read rule.
  16. **A `hostname` dimension, and `host` on `sessions`** — *feature-surface-2, schema-storage-3.*
      Confirmed: `20260828090400_sessions.sql` has no host column of any kind, while `events` has
      one; a product may own 20 domains, so `visits`, `bounce_rate`,
      `entry_page` and `exit_page` merge every domain into one. The finding says it itself — `u`
      already carries the full URL and the server already derives `host`, so **no wire change**.
      Time-critical for a different reason, below.

  **Confirmed, not wire, and not to be lost.** Nothing here is blocked by the freeze, but three
  items are time-critical because the ClickHouse tables are still empty and CLAUDE.md's add-only
  rule makes a late column indistinguishable from a genuine blank forever — the argument
  `20260828090400_sessions.sql:3-10` already makes for `ua` and `screen_w`, applied to: `host` (and
  `exit_host`) on `sessions`; `utm_content` / `utm_term` on `events` and `sessions`; `ref_path` on
  both, for item 2 above. The remainder — the `Sweep` clock split, the geo counters, the on-read
  `(gclid)` medium, the on-read spam list and its `make signatures` target, the `screen` unknown
  bucket and width-band labels, `throttled` / `backpressure` counting, and the `(IP, UA)` rate-limit
  key — are code and read-side spec, safe to land after the freeze. **None of it is implemented:**
  this revision decided the contract, and §5.1's `h`, `r` and `u` rules are ahead of the code.
  `spec/wire-v1.schema.json` is deliberately left at rev 0.9 rather than given an `h` constraint the
  parser does not enforce — `internal/ingest/wire/wire_test.go:199` asserts the two **agree**, and a
  constraint on a field no mutant carries would pass that test while diverging silently. §10's rule
  stands: the text wins, and the schema is fixed alongside the parser when `h` lands.

- rev 0.9: **`engagement` event** with `pv`, `e`, `sd` (§4, §5.1) and the rejection reason
  `no_session_for_engagement` (§6). Serves `engagement_time` and `scroll_depth`
  (`spec/metrics.md` §4.1b), which replace the retired `visit_duration`. Landed **before** the
  end-of-M1 freeze on purpose: the metrics ship in M2, but appending a reserved event name and
  three fields after the freeze would cost a wire revision for nothing. Cumulative-not-delta and
  server-forced `i = false` are the two rules that make it safe.
- rev 0.8: §5.3 wording fix — a reinstall is excluded from the `installs` *table* / `installs_mv`
  and from `new_installs_by_day`, never from the `installs` *metric*. No wire change; no schema
  change.
- v0.7: removed `prov` (`m`/`u`/`r`/`st`) and the app-side attribution pipeline it fed (RFC §4.2,
  §8.4); reinstall detection now uses `is_reinstall` on the install row instead of
  `attr_method = 'reinstall'` (§5.3). OS-level download → install
  attribution is off the roadmap for now.
- §4.2 cohort universe: also includes `ref:<host>` for every `ref_host` observed in the product's
  web pageviews in the last 90 days, so a `jl=ref:news.ycombinator.com` claim is accepted when the
  site has actually received Hacker News traffic.
- v0.6: `a` (app slug) on app events; heartbeat may carry install properties (`license` reserved).
- v0.5: reserved `onboarding:<step>` namespace with fixed `status` / `reason` props (PRD §7.4).
- v0.4: `f`, `fd` web fields; download links may carry `jt=first` beside `jl` when the label came
  from attribution memory. RFC §2.3 holds the `jl` grammar.
- v0.3: the server also reads `ref`, `source`, `via` from `u` into
  `utm_source` when `utm_source` is absent. No wire change.
- v0.2: `i`, `v`, `l` fields; rejection reasons
  `spam_referrer`, `page_blocked`, `country_blocked`.

## Platform account gates

[admin.md](admin.md) §4 requires an uncached account-state check before accepting `/v1/e` data. Expired effective billing rights return the existing `402 {"error":"payment_required"}` response. Administrative suspension rejects every event with existing `stopped` under the `202` envelope; no event is stored. It does not mutate client kill-switch configuration. Unknown products retain their existing response. SDK wire shapes and retry rules do not change.
