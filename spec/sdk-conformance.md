# spec/sdk-conformance — SDK behavioural contract as tests

Status: v0.18 · Gates: M1 (Swift), M3 (Electron, Tauri 2, .NET desktop) · Normative for: every SDK that calls itself
Jelto-compatible. RFC-0001 §8 lists the rules; this document makes each one a test.

## 1. Harness

`spec/conformance/` contains:

- **`mockd`** — a Go binary implementing `POST /v1/e` with scriptable behaviour (§2) and a
  recording of every request (headers, body, arrival time).
- **`runner`** — drives a *conformance host* (§3) through scenarios (§4) and asserts on the five
  surfaces below.
- **`scenarios/*.yaml`** — one file per C-number.

A scenario may assert on these five surfaces and on nothing else. Every claim in §4 is a claim
about one of them, and an SDK is free in every respect they do not reach:

1. **`mockd`'s recording** — every request it received (headers, body, arrival time in real
   monotonic microseconds) and every connection it accepted. A connection that carried no request
   is still a connection, which is what C5's "no socket opened" watches.
2. **The host process** — its stdout replies (§3.1 gives one JSON line per command), its exit
   code, and how long it took to exit (C1, C2, C5, C10, C18).
3. **The host's stderr** — the SDK's own debug output under `JELTO_DEBUG=1` (RFC-0001 §8.7
   item 17). Eleven scenarios assert a line here — C8c, C9b, C10, C16, C16b, C17, C20b, C22c,
   W2, W3, W4 — and they have to: a rule of the form *do not send this* leaves nothing in the
   recording by construction, so the debug line is the only thing separating an SDK that dropped
   the call for the stated reason from one that lost it.
4. **The state the host exports** — `dumpstate` (§3.2) prints the facts RFC-0001 §8 requires an
   SDK to survive a launch with, as canonical JSON. It is **not** the SDK's storage: C2, C4, C4b,
   C4c, C6, C8c, C9b and C18 assert on the export, and how the SDK arrived at the answer — a
   plist, `UserDefaults`, SQLite, a JSON file — is its own business.
5. **The emptiness of `JELTO_STATE_DIR`** — and nothing else about what is in it. This one stays a
   file-system assertion on purpose. RFC-0001 §8.2 item 5's "no file is created" (C5) and §8.7
   item 18's wipe (C18, C22d) are claims about the directory itself; they hold for every storage
   format because the runner owns the directory and can simply look; and an export could not
   carry them anyway, since a state an SDK has forgotten it wrote is exactly the state that
   reports itself empty while the bytes are still on disk.

An SDK passes when every scenario passes on a clean machine, twice in a row.

## 2. `mockd` modes

Set per request via the `X-Mock` header the host forwards from `JELTO_MOCK` env, or per scenario
via the runner's control socket:

| Mode | Behaviour |
|---|---|
| `ok` | `202 {}` — the bytes wire §2a names and the server sends. NOT `{"rejected":[]}`: `ingestResponse.Rejected` is `omitempty`, so an envelope accepted whole serialises to `{}`, and a mock answering otherwise would certify an SDK against a body production never produces |
| `reject:<reason>` | `202` with every event rejected for `<reason>` |
| `429` | `429`, `Retry-After: 2` |
| `503` | `503`, `Retry-After: 2` |
| `429:<v>` \| `503:<v>` | that status with the **literal** header value `<v>`, so a scenario can send `9999`, `soon`, or an HTTP-date. An empty `<v>` (`429:`) omits `Retry-After` entirely. Needed because wire §9 rev 0.17 requires a client honour the header, and the four edges of that rule — larger than the backoff, smaller than it, absent, unparseable — cannot be reached with a fixed `2` |
| `400` | `400 {"error":"malformed"}` |
| `402` | `402 {"error":"payment_required"}` |
| `stop:<seconds>` | `202` plus `"stop":{"until":now+<seconds>,"scope":"app"}` |
| `slow:<ms>` | delays the response by `<ms>` |
| `down` | connection refused |

## 3. Conformance host

Each SDK ships a tiny CLI host (`sdk/<platform>/conformance-host`) that links the SDK exactly as
a customer would and executes commands from stdin:

```
init <key> [app]
track <name> [json-props]
onboarding <step> <ok|fail|skip> [reason]
setprops <json>
installid            → prints it
dumpstate            → prints the exported state (§3.2)
legacyversion        → test-only: remove the stored version baseline, preserving other state
reset
disable
sleep <ms>
exit
```

**The host is one file the runner can execute with no arguments and kill by pid.** The runner
spawns `sdk/<platform>/conformance-host` as a bare executable — no interpreter, no arguments,
no shell (`runner/hostproc.go`, `exec.Command(binary)`) — and C4c's second arm ends it with a
signal to that pid. Both constraints bind an SDK whose runtime is interpreted: the committed
file MUST be executable, and if it is a wrapper it MUST `exec` its runtime rather than fork it,
so the pid the runner holds is the process that holds the SDK. A wrapper that forks would
survive the kill and leave the SDK running, and C4c would then measure an orderly exit it never
asked for. `sdk/electron/conformance-host` is the worked example; until v0.14 the constraint
was discoverable only from the runner's source.

**`setprops` takes its argument.** The command language writes `setprops <json>` with the
argument non-optional, and a bare `setprops` is a scenario error, not a call: the host MUST
answer `{"cmd":"setprops","ok":false,"error":…}` and MUST NOT forward it to the SDK as an empty
or nil property set, since "set no properties" is a write the SDK would honour and no scenario
means. No scenario sends one; the rule exists because two conforming hosts disagreed about it
until v0.14, one refusing and one calling the SDK with nothing.

### 3.1 Replies, environment, clock

**One reply per command.** The host MUST write exactly one line of JSON to **stdout** for every
command it reads, after that command has been carried out, and MUST write nothing else there —
the SDK's own output belongs on stderr (RFC-0001 §8.7 item 17). **The prohibition binds the SDK
too, and that is the half this section left unsaid until v0.13.** The SDK runs *inside* the host
process and shares its file descriptors, so an SDK that prints to stdout — `console.log`, `print`,
a structured logger left on its default sink — writes into the reply channel, and the host cannot
stop it without redirecting the descriptor, which would hide a real SDK defect rather than report
it. **Everything an SDK emits goes to stderr, on every platform, in every build.** The line is an
object:

```json
{"cmd":"track","ok":true}
{"cmd":"init","ok":true,"us":41}
{"cmd":"installid","ok":true,"value":"9f2c…"}
{"cmd":"dumpstate","ok":true,"state":{…}}
{"cmd":"track","ok":false,"error":"track <name> [json-props]"}
```

`cmd` echoes the command word, `ok` says whether it ran, `error` says why not, `value` carries
what a printing command printed, `state` carries §3.2's object, and `us` carries a duration in
microseconds where a row asks for one. A host MAY add keys of its own; the runner ignores what it
does not name. Two rows turn on this line and could not be written without it. C1's "`init`
returns in < 5 ms (host measures)" is `us` on the `init` reply — the wall time between the runner
writing a command and reading the answer includes the runner's own scheduling, so only the host
can measure it. And every `sleep <ms>` needs an *end*: without a reply a driver can only guess
when `sleep 3000` finished, which makes every timing assertion in §4 an assertion about the
runner's own timer rather than about the SDK.

**`cmd` is checked, and the reason is that the unchecked case fails silently.** Nothing verified
the echo until v0.13, and the two ways stdout gets polluted are not equally visible. A stray line
that is merely malformed already failed, loudly, on the JSON decode. A stray line that is **valid
JSON** did not: it was accepted as that command's reply, and every later reply was then read
against the wrong command, one behind, for the rest of the arm. That is not a hypothetical shape —
a structured logger writing JSON objects to stdout is the default for several Node logging
libraries, and Electron is a launch SDK. The arm would still pass or fail, on timings belonging to
a different command, which is worse than either an error or a hang. So the runner now requires
`cmd` to echo the command's first word and fails the arm by name when it does not
(`runner/hostproc.go`). A host that adds keys of its own is unaffected; the runner still ignores
what it does not name.

Proving it is `runner_test.go`'s job rather than a scenario's, for the reason W1's nil-UUID clause
is there: a host that prints noise to stdout is a broken host, a correct one has no way to produce
one on request, and adding a normative command to §3 so that a conforming host *could* misbehave
would be a worse contract bought for a better test. `TestSendRejectsAStrayJSONLineOnStdout` builds
that host by hand, and it fails against the pre-v0.13 runner — the stray line is accepted as the
reply — which is what makes it evidence rather than decoration.

**Environment.** The host MUST honour these variables:

| Variable | Meaning |
|---|---|
| `JELTO_ENDPOINT` | the `mockd` URL the SDK posts to. Required; a host without one has nothing to be conformant against |
| `JELTO_STATE_DIR` | where the SDK persists — root and all, overriding the platform default of §5. Required. The runner owns this directory: it hands each arm a fresh empty one and reads its emptiness back (§1, surface 5) |
| `JELTO_NOW` | pins the SDK's clock, in whole milliseconds. **Its presence is the pin**, so `JELTO_NOW=0` is a legal 1970-01-01 clock (C15) and not "unset" |
| `JELTO_DEBUG` | `1` turns on the payload printing RFC-0001 §8.7 item 17 requires. Twelve of §4's scenarios set it, and C10 asserts the other direction: nothing reaches stderr without it |
| `JELTO_MOCK` | a §2 mode string the host forwards verbatim as the `X-Mock` request header. §2's first line has always required this and this list omitted it. The suite drives modes through the runner's control socket, so no scenario sets it — but an SDK author reproducing one arm by hand has no socket, and the header is the only other channel |
| `JELTO_APP_VERSION` | application version observed at initialization; presence overrides platform metadata, including the empty string (unknown). Unset uses the host application version |
| `JELTO_CLIENT_VERSION` | the client version the SDK reports as `v` (wire §3). W4 needs a host "built with a version string it did not choose" — `1.2.0`, `Electron/1.0`, `""` — and there is no other way to give it one. Unset means the SDK's real version; set, the empty string included, replaces it |

`JELTO_PROV_FIXTURE` is retired along with the client-side attribution ladder it fed — see §4,
"Attribution ladder (removed)".

**`JELTO_NOW`'s syntax is the host's to check, before the first command.** A pin is a whole
number of milliseconds, negative and past-`int64` values included (C15b sets both), and a host
MUST validate it before it reads its first command and exit 2 with one stderr line when it does
not parse — so a malformed pin never runs a scenario against the real clock, which is the one
failure that would pass every timing row for the wrong reason. The SDK MAY parse the variable
again for its own clock; the host's check comes first and is the one the runner can observe. A
host that hands the whole environment to the SDK unread satisfies this by asking the SDK's own
parser, which is what `sdk/electron`'s host does, rather than by writing a second grammar.

The host has no seed knob. Install deadlines equal the draw instant and are persisted
once; retry jitter remains random. Scenarios that need an accepted install begin under
`ok` and settle the initial batch before asserting on later retries or stops. Application
versions are controlled by `JELTO_APP_VERSION` for §7's transition scenarios.

**The clock.** `JELTO_NOW` pins it and `sleep <ms>` advances it. A clock that were merely *fixed*
could not run a day-rollover scenario in seconds — it could not run one at all, nothing ever
moving it — so under `JELTO_NOW` the host adds `<ms>` to the SDK's clock, fires every timer that
falls due, and returns once the work that fell due has settled. That is the only reading under
which C3's `sleep 3000`, C4's `sleep 3000` and C4b's thirty days mean anything. **Without
`JELTO_NOW` the clock is the real one and `sleep <ms>` really sleeps**, which is what C7's
"2 s ± 0.5 s", C8's head-of-schedule and C22's "≤ 2 s" require: those are assertions on `mockd`'s
arrival times, and `mockd` cannot see a frame the host invented. The request timeout of RFC-0001
§8.3 item 8 is real in both frames — it is a property of the network, not of the host's calendar,
and a virtual one would never fire against `slow:10000`.

### 3.2 `dumpstate` — the state contract

§4 reads persisted state in ten scenarios: eight read a **fact** and three read the state
directory's emptiness, C18 doing both. The three are §1's fifth surface and stay where they are.
The eight assert on facts the host **exports** on demand — never on a file, a format or a key on
disk. An SDK stores its state however its platform prefers, and `dumpstate` translates whatever it
holds into the one shape below. Until this section existed the only written state contract was one
implementation's `state.json` and `queue.jsonl`, and a Swift SDK using a plist failed all eight of
those rows for a storage choice no rule forbids.

`dumpstate` MUST print, as its reply's `state`, one JSON object with these keys. A fact that is
not set is absent or empty; a host MAY add keys and the runner ignores them.

| Key | Type | The rule it serves |
|---|---|---|
| `install_id` | UUIDv4 string, `""` before `init` and after `disable()` | §8.2 item 2 — loaded on launch, created if absent (C2, C18) |
| `last_heartbeat_day` | UTC day index, `floor(ms / 86 400 000)`, decimal string | §8.2 item 3 — a `heartbeat` is enqueued only when this differs from today (C3, C22) |
| `install_claimed` | boolean | §8.2 item 4 — set on a `202`, or after 30 days of attempts (C4, C4b) |
| `install_due_at` | instant | §8.2 item 4 — drawn and persisted once when the SDK first sees `install_claimed = false`, equal to the draw instant with no random offset. Relaunch resumes this deadline rather than redrawing it, including after abrupt death (C4c) |
| `install_first_try` | instant | §8.2 item 4's "after 30 days of attempts" — a 30-day window has to be measured from something, and it is not the current launch or a machine that relaunches daily never reaches it (C4b) |
| `install_props` | object, string → string | §8.1 `setProps`, "persisted, sent with every heartbeat" (C22, C22d) |
| `backoff_step_ms` | integer; `0` or absent means *not in backoff* | §8.3 item 8, "persisted across launches" — the step is what a new process must not restart at 1 s (C8, C8c, C9b) |
| `backoff_next_at` | instant | §8.3 item 8 — a step does not say *when*, and C8c reads from here the two edges it cannot watch elapse |
| `stop_until` | instant | §8.6 item 16 and wire §8 — a switch a relaunch has forgotten is not a switch (C16) |
| `stop_probe_due` | boolean | wire §8's single-`heartbeat` probe is a MUST, and it is owed across a relaunch too. Without this fact a restart inside a pause drains the queue straight into a live switch, which is the one thing the probe exists to prevent |
| `last_app_version` | string or absent | last known observed version, §7; absent on legacy/unknown state |
| `queue` | object, below | §8.3 item 6 (C6, C4b, C18) |

That is the whole list, and every row is derived from a rule rather than transcribed from an
implementation: each names the RFC item that makes the fact survive a launch, and a fact no item
requires is not in the export. The reference host persists one more, a consecutive-refusal
counter — it is recomputable from `backoff_step_ms`, no rule asks an SDK to keep it, and so it
stays that host's own bookkeeping and the runner never reads it.

**Instants are decimal strings, not JSON numbers.** Every value typed *instant* above is
milliseconds on the SDK's clock written as a decimal string, and so is `last_heartbeat_day`. The
clock is arbitrary precision — C15b sends a `t` past `int64` and RFC-0001 §8.5 forbids the SDK
correcting it — so a reader that took these as JSON numbers would decode them into a float64 and
round them, the same trap the wire's own `t` sets for a `map[string]any`. Nothing else in the
export is an instant: `backoff_step_ms` is a *duration* bounded by §8.3 item 8's 1 h ceiling, and
`install_claimed` and `stop_probe_due` are booleans.

**`queue`** is what the SDK is holding, oldest first, and how much it counts that as:

```json
"queue": {
  "bytes": 62431,
  "events": [{"id": "…", "n": "x500", "t": "1788134400000"}, …]
}
```

Each entry carries the three fields the wire fixes when an event is *enqueued* and which the SDK
must not regenerate afterwards: `id` (wire §6 — a retry resends the same batch with the same
`id`s), `n`, and `t` (the instant the event happened, not the instant it is sent). Properties are
not exported; they are asserted where they are visible, in the recording, and the export exists
for what a recording cannot show — a queue that was capped, refused or wiped. There is no count
field beside `events` on purpose: a count and a list that disagreed would leave the runner to
choose between them on exactly the row (C6) that is testing the cap.

**`bytes` is what the SDK counts against its own cap, not what a file system reports.** RFC-0001
§8.3 item 6 caps the queue at "1 MB **or** 1 000 events" and C6 asserts both halves. A semantic
export cannot report bytes-on-disk, and it should not want to: an on-disk figure is not comparable
between implementations — a JSON-lines file is exactly its lines, a plist is padded, a SQLite
database has pages, a free list and possibly a write-ahead log — so a cap read off `stat` would
forbid on one platform a queue it permits on another while both hold the same events. Item 6 sits
beside item 11's 2 MB memory ceiling and bounds the same thing: what the SDK is **holding**. So
`bytes` is the serialised size of the queued events in whatever encoding the SDK would send or
store them in, and the cap binds on that. This is what the 1 MB has always meant.

The alternative — drop the byte half and cap on the event count alone, the one figure no export
has to strain for — is not available: the two halves cross at 1 048 576 ⁄ 1 000 ≈ 1 049 B per
event — below that the event count binds first, above it the byte count does — so on events
larger than about a kilobyte the byte cap is the only one doing any work, and a single 2 MB
`props` blob would become a conforming queue.

**The host itself has no logic.** It links the SDK and reads commands; it decides nothing, retries
nothing, and remembers nothing of its own. If a scenario needs something the host cannot do, the
scenario is wrong, not the host.

`dumpstate` is the one place a host *translates*, and the carve-out is narrow enough to say in a
sentence: **the host may re-encode what the SDK already holds, and may not compute, default or
infer anything it does not.** A fact the SDK does not have is absent from the export — which is a
failing row wherever a row asserts it, never a plausible value the host supplied. If `dumpstate`
has to work the answer out, the SDK did not survive the launch and the export is lying about it.
And the read is only a read: `dumpstate` MUST NOT create, load or write anything, so that calling
it before `init` prints an empty export and leaves the state directory as empty as it found it
(§8.2 item 5, C5). A host that fills its own gaps certifies nothing, and §6 certifies the SDK
behind the host.

## 4. Scenarios

Every scenario starts with an empty `JELTO_STATE_DIR` and `mockd` in `ok` mode unless stated.
"Recording" = mockd's request log. "State" = what the host exports for `dumpstate` (§3.2),
never its files; "state dir empty" alone is a claim about `JELTO_STATE_DIR` itself (§1).

**`k` is a placeholder** — the only one in these rows. It stands for a well-formed product key,
which `spec/wire-v1.md` §2 fixes at `^prd_[a-z0-9]{10}$` and `spec/wire-v1.schema.json` enforces.
W1 validates *every* request of *every* scenario against that schema, so a host handed the letter
`k` fails the first request of each of the 21 rows below that spell it — on the key, before the
row has tested the thing it exists to test. The scenarios use `prd_conform001`. Nothing else in
the rows is shorthand: the event names `x` and `y` are literal and already satisfy wire §3's `n`
grammar (`^[a-z0-9_:.-]{1,64}$`), which is why `k` alone is substituted and they are not.

### Lifecycle

| C | Scenario | Assert |
|---|---|---|
| **C1** | `init k` then immediately `track x` ×1000 then `exit` | `init` returns in < 5 ms (host measures); process exits within 1 s; no crash |
| **C2** | `init k`, `installid`, `exit`; new process `init k`, `installid` | same UUIDv4 both times; state contains it; nil UUID never printed |
| **C3** | `init k`, `sleep 3000`; then `JELTO_NOW += 1 day`, new process `init k`, `sleep 3000` | recording has exactly 2 `heartbeat` events, one per UTC day; a third `init` on the same day sends none |
| **C4** | `init k`; `sleep 3000` (with `JELTO_NOW`); then another launch | `install` is enqueued immediately on first init, with `install_due_at` equal to the draw instant; exactly one `install` event (no attribution payload — the wire's `install` carries only `iid av os osv arch`); state marks `install_claimed` on `202`; a further `init` sends no second `install` |
| C4b | as C4 but `mockd down` for 30 days of simulated time, then `ok` | `install` retried on each launch; after 30 simulated days state marks claimed without a `202` and **no further `install` event is enqueued**. That last clause is the whole of what "no further attempts" can mean here: RFC-0001 §8.2 item 4 says what marking claimed stops, and what it stops is the enqueueing of a second `install`. Nothing in the contract discards an event already in the queue because a timer expired, so the copy already queued keeps being offered with the rest of the batch — §8.3 item 8 doing its job — and the queue holds exactly **one** `install` however many launches have offered it |
| C4c | as C4 but the process ends before the initial flush; new process `init k`. Arm A ends with `exit`; arm B **SIGKILLs it** after `installid` confirms bootstrap completed | The immediate deadline is persisted when drawn, and the queued event survives abrupt death. Relaunch resumes that deadline and sends the install if still queued. Arm A permits a termination flush; arm B proves persistence without one. The restart advances 7 h: a lost deadline redrawn there is 25 200 s from the initial `jelto_now` anchor and fails `min: 0, max: 0` (C4c; `spec/conformance/TODO.md` §7) |
| **C5** | `track x` ×10, `sleep 5000`, `exit` — **no `init`** | recording empty; state dir empty; no socket opened (runner watches `mockd` connections) |

### Queue and network

| C | Scenario | Assert |
|---|---|---|
| **C6** | `mockd down`; `init k`; `track x` ×1500; `exit`; inspect state | the queue holds ≤ 1 MB **and** ≤ 1 000 events — the byte half read as §3.2's `queue.bytes`, what the SDK counts against its own cap; the oldest were dropped (events 1–500 absent, 501–1500 present) |
| **C7** | `init k`; `sleep 2500` | first batch arrives 2 s ± 0.5 s after init. Then `track x`, `sleep 5500` → second batch 5 s ± 0.5 s after the track. Then `track` ×250 at once, `sleep 6000` → 3 requests of ≤ 100 events |
| **C8** | `mockd 503`; `init k`; `track x`; observe the head of the schedule on the **real** clock | retries on a backoff of ~1, 2, 4, 8 … s with ±20 % jitter, capped at 3 600 s; state persists it (a new process continues, does not restart at 1 s). **An hour of this cannot be simulated**: §1's recording times every arrival in real monotonic microseconds, and a backoff is a claim about *differences* in that frame, so under a pinned `JELTO_NOW` every retry lands within milliseconds of the last and the schedule disappears rather than compressing. The head of each schedule is where the three arms differ from one another, so the head is what is observed, on the real clock; a claim about the ceiling or the tail is read from the backoff the SDK persisted instead (C8c). Same for `429` and `down` — but **the observed schedule is the backoff composed with the server's `Retry-After`, not the backoff alone**, and the arms differ because of it: `down` has no response and so no header, and shows the pure ~1, 2, 4, 8 …; `503` and `429` carry `Retry-After: 2`, so the first wait is ≥ 2 s where the step is ~1 s ±20 %, giving ~2, 2, 4, 8 … The rule is one line — **the next attempt is at the later of the two floors** (wire §9) — and C8b measures it. Through wire rev 0.16 this row and the wire contradicted each other and both were satisfiable: the wire advertised `Retry-After` and required nothing of the client, so a ~1 s first retry fired before the moment the server had just named. **Every retry resends the same batch with the same `id`s** — `spec/wire-v1.md` §6 says a client MUST NOT treat a lost `202` as a reason to alter the `id`s in the batch it resends, and until now nothing in this suite asserted it. An SDK that regenerates them defeats the server's 10-minute window, so a batch that was in fact stored is stored again on every retry and the customer reads the duplicates as real traffic. |
| C8b | `mockd 429:20`; `init k`; `track x`; observe the head of the schedule — then the same run with `mockd down` | with the header, **no request before 20 s**, and the schedule thereafter is the later of the two floors at every step, so the header governs until the backoff overtakes it. **Where it overtakes, exactly**: the waits are 20, 20, 20, 20, 20, then 32, because the backoff's 16 s step is still under the header at the top of its ±20 % jitter and its 32 s step is over it at the bottom. So at `Retry-After: 20` the crossover is the **sixth** retry and arrives ~132 s after the first attempt — two minutes of wall time to watch one number, and not observable any other way, since the clock this row is measured on is the real one (C8). The suite therefore asserts the identical rule at `429:3`, where the same arithmetic puts the crossover at the third retry — 3, 3, 4, 8, the arm over in ~18 s. One rule; only the number moves. Without the header, the same run retries at ~1 s. The pair is what makes C8 an assertion about composition rather than about one number: the two runs differ only in a header, and an SDK that ignores `Retry-After` produces the same schedule for both. |
| C8c | `429:9999`, then `429:` (no header), then `429:soon`, then `503:1` at the tenth consecutive refusal | `9999` is honoured but **clamped to 3 600 s**, the same ceiling C8 puts on the SDK's own backoff — an unbounded header is a second kill switch settable by any proxy on the path (wire §9). No header falls back to the backoff alone, identical to C8's `down` arm. `soon` is unparseable and is treated as **absent, never as zero**: a proxy that rewrites the header must not be able to turn a refusal into an immediate retry. And a `Retry-After: 1` at the tenth refusal does **not** shorten or reset the schedule — the step is already ~512 s and stays there. **Two of these four edges cannot be watched elapse, and are asserted from the backoff the SDK persisted** — §1 makes the exported state an assertion surface (§3.2), and both are claims about a wait the SDK has *scheduled* rather than one it has served. The clamp is a 3 600 s wait; and the tenth consecutive refusal is ~511 s into the schedule before its step is measured at all, that being the nine waits ahead of it (1 + 2 + 4 + … + 256 s). So the `9999` arm reads the deadline written after a single refusal and expects 3 600 s from that response rather than 9 999, and the tenth-refusal arm runs on a simulated clock — ten refusals cost ten `sleep`s and no real time — and reads the wait scheduled after the tenth: ~512 s, which is neither the 1 s the header named nor the 1 s a reset would put back. |
| **C9b** | `mockd 402`; `init k`; `track x`; `sleep 5000`; `track y`; `sleep 6000` | **no retry of either event and no backoff loop** — `402` is final, like `400` (`spec/wire-v1.md` §2a; RFC-0001 §8.3 item 9). The batch is dropped, one debug line names `payment_required`, and the SDK keeps accepting calls: the host app must not change behaviour because the customer's billing lapsed. A `402` arrives whenever a product is `paused` or its trial expired without a subscription, so an SDK that treated it as retryable would hammer the endpoint for the life of the install. |
| **C9** | `mockd 400`; `init k`; `track x`; `sleep 5000`; `mockd ok`; `track y`; `sleep 6000` | exactly one request with `x` (no retry); `y` is sent in a fresh batch |
| **C10** | `mockd slow:10000` and `mockd 500`-style garbage responses, invalid JSON, oversized bodies; `init k`; `track` ×100; `exit` | host exit code 0; nothing written to stderr except with `JELTO_DEBUG=1` |
| **C11** | `init k`; `track x` ×10 000 over 10 s | the SDK's own accounted state ≤ 2 MB (`dumpstate`, not host RSS — RFC-0001 §8.3 item 11, v0.24); per-`track` wall time ≤ 1 ms p99 (host measures) |

### Attribution ladder (removed)

C12–C14 tested the client-side attribution ladder (file provenance, own quarantine record, store
detection) and its 200 ms deadline. The ladder is off the roadmap for now.
The scenarios are retired with it. **C12, C13 and C14 are deliberately
left as numbering gaps**, not renumbered, so the surviving C-numbers keep the same meaning in
this document, in the server's design record, and in any conformance log already on file.

### Clock, kill switch, transparency

| C | Scenario | Assert |
|---|---|---|
| **C15** | `JELTO_NOW` = 1970-01-01 and then = now + 10 years; `init k` | `t` is sent as the host clock says, unmodified; no client-side correction |
| C15b | `JELTO_NOW` = 1969-07-20 (a host clock **before** the epoch), then a clock so far ahead that `t` exceeds `int64`; `init k` | the same: `t` is sent as the host clock says, and the request still validates against `spec/wire-v1.schema.json` (W1). This is the half C15 never reached — it set the clock **to** 1970-01-01, which is `t = 0`, so a negative `t` was never sent, and until wire rev 0.16 the schema's `minimum: 0` would have failed the body while the server stored the event. Wire §3 clamps every `t`, so neither case may be corrected, dropped or held back by the SDK; the server answers `202` and stores `ingest_ts`. **No platform clock reaches the second arm** and none is asked to: a Swift `Date` is a `Double` of seconds and a Go `time.Time` an `int64` of nanoseconds, so neither yields a millisecond value outside `int64` unless the SDK computes one. The value arrives through `JELTO_NOW`, the conformance host's clock (§3), which must carry it at the width it was given, and what the arm tests is therefore the SDK's **arithmetic**, not its clock: whatever the clock abstraction hands the SDK goes on the wire as it stands, because saturating it into `int64` is precisely the correction RFC-0001 §8.5 forbids. The saturation belongs to the server, which does it on arrival and then clamps like any other `t` (wire §3) |
| **C16** | `mockd stop:60`; `init k`; `track x` ×5; `sleep 30 s`; `mockd ok`; `track y`; `sleep 40 s` | after the `stop` response no request for 60 s; what is tracked during the pause queues (cap applies), and **the queue holds `y` alone**; at ~60 s one `heartbeat`, then the queue drains. The five `x`s are not in it: they were in the batch the switch answered, that answer was a `202`, and wire §2a makes a `202` an acceptance of the envelope however many events it rejected. Nothing in RFC-0001 §8 or wire §6/§8 re-queues an event rejected `stopped`, and §8's own rationale says the opposite in as many words — a client draining into a live switch "spends its whole backlog" — which is *why* the probe exists. The recorded order is `[heartbeat, x, x, x, x, x]`, then `[heartbeat]`, then `[y]`: `x` went out once, before the pause, and was never resent. The single-heartbeat probe is a wire **MUST** as of rev 0.17 (§8) and was a `MAY` before it, so an SDK that drained the queue straight into a switch that was still on broke nothing the wire said while failing this row. It is the probe that is required, not merely the pause: draining a thousand queued events into a live switch answers `stopped` **per event**, counted per event, to learn what one event would have told it |
| C16b | `stop` with `scope: web` | app SDK ignores it |
| **C17** | `JELTO_DEBUG=1`; `init k`; `track x` | stderr contains the exact JSON body before it is sent |
| **C18** | `init k`; `track x`; `disable`; `installid`; `track y`; `sleep 6000`; `exit` | the queue is deleted; `installid` prints empty; `y` never sent; state dir empty |
| **C19** | build twice from the same commit on the same toolchain | byte-identical artifacts; `CHECKSUMS` file matches |

### Onboarding helper

| C | Scenario | Assert |
|---|---|---|
| **C20** | `init k`; `onboarding permissions ok`; `onboarding driver fail no_kext`; `onboarding tour skip`; `sleep 6000` | three events named `onboarding:permissions`, `onboarding:driver`, `onboarding:tour` with `props.status` = `ok` / `fail` / `skip` and `props.reason = no_kext` only on the second; and, **but for `id` and `t`**, identical to what `track("onboarding:permissions", {"status":"ok"})` sends in the same batch — every other field, and the whole `props` object. It cannot be byte-identical: wire §3 gives every event its own `id` and its own `t`, so two calls a millisecond apart differ in exactly those two fields by construction, and a helper that reused either would be the defect this row is looking for rather than the thing it asserts. What is being asserted is that the helper **is** `track` and does nothing else (RFC-0001 §8.1) |
| C20b | `onboarding "Bad Step!" ok` and `onboarding x ok "Free text reason"` | both dropped client-side with a debug log naming the rule (step `^[a-z0-9_-]{1,32}$`, reason `^[a-z0-9_.-]+$`) |

### Install properties and app slug

| C | Scenario | Assert |
|---|---|---|
| **C21** | `init k mac`; `sleep 3000` | the heartbeat carries `a = "mac"`; without the argument no `a` field is sent |
| **C22** | `init k`; `setprops {"license":"trial"}`; `sleep 3000`; `setprops {"license":"paid","edition":"pro"}`; `sleep 3000`; then `JELTO_NOW += 1 day`, new process `init k`, `sleep 3000` | first heartbeat has `props.license = trial`; the second `setprops` triggers an **immediate** heartbeat with `license = paid, edition = pro` (≤ 2 s); the next-process heartbeat still carries both (persisted). **The day has to turn for that last clause to have a subject.** RFC-0001 §8.2 item 3 enqueues a `heartbeat` only when `last_heartbeat_day ≠ today (UTC)`, and C3 asserts in as many words that a third `init` on the same day sends none — so a new process on the same day sends no heartbeat at all, and a row that read the props off it would be reading a request that was never made |
| C22b | `setprops {"license":"paid"}` twice with the same value | no extra heartbeat on the second call (unchanged) |
| C22c | `setprops {"Email":"x@y.z"}` and a value of 30 chars | dropped client-side with a debug log (key `^[a-z0-9_]{1,32}$`, value `^[a-z0-9_.-]{1,24}$`); the server would reject them anyway |
| C22d | `disable()` after `setprops` | props wiped with the rest of the state |

### Wire validity

| C | Scenario | Assert |
|---|---|---|
| W1 | every request in every scenario | body validates against `spec/wire-v1.schema.json`; `Content-Type: application/json`; body ≤ 64 KB; ≤ 100 events. Since wire rev 0.14 that schema also rejects a **nil-UUID `id` or `iid`** and a non-absolute `r`, which is what makes this row cover the zero-initialised `id`: `id` is optional (§3 `id` is SHOULD), so an SDK that omits it passes, and an SDK that sends `00000000-0000-0000-0000-000000000000` fails **here**, in its own test run, instead of on a production server that answers `202 {}` while collapsing ten minutes of the product's traffic onto one row |
| W2 | `track "Bad Name!"` | dropped client-side, logged in debug; never sent |
| W3 | `track x {"k":"<201 chars>"}` and 21 keys | dropped client-side with a debug log naming the rule. 21 is §3's `props` cap of 20 plus one, and since wire rev 0.16 it is the cap for a `heartbeat` too — §4's unenforced "up to 5 install properties" is gone |
| W4 | every request in every scenario, plus an SDK built with a version string it did not choose (`init` after setting the client version to `1.2.0`, to `Electron/1.0` and to `""`) | `v` matches `^[a-z]+/[0-9A-Za-z.+-]{1,24}$` or is absent. Normative in wire §3 since rev 0.16, and it is the whole request that dies otherwise, not the field: the server answers `invalid_field` naming `v` for **every** event the SDK sends, for the life of that build. `1.2.0` and `Electron/1.0` are the two shapes a version string naturally takes and both are rejected — the platform segment is lower-case and the version follows a `/`. An **empty** `v` is an absent `v` and is accepted, so an SDK that has no version to report omits the field or sends `""`, and must not fall back to a bare number |

## 5. Platform-specific addenda

Each directory below is where the SDK persists when nothing overrides it. Under a conformance host
`JELTO_STATE_DIR` overrides it and the SDK MUST use that directory and no other, because §1's
fifth surface is the emptiness of *that* directory: an SDK keeping a byte anywhere else would pass
C5, C18 and C22d while failing what they assert. **What is inside is the SDK's own** — the format,
the file names, the number of files, whether there are files at all — and it is §3.2's export, not
the directory, that the other eight state scenarios read.

On POSIX hosts the SDK creates its state directory owner-only (`0700`) and every file it writes
there owner-only (`0600`), re-asserting the mode on a directory that already exists, because what
is inside is an install identifier and the customer's queued events (threat model A2) and another
local account on the same host is not the customer's end user. On Windows the per-user
`LocalApplicationData` ACL is the bound. The runner does not assert this — §1's fifth surface is the
directory's emptiness, not its mode — so each SDK's own unit tests pin it (v0.18).

**Swift.** State in `~/Library/Application Support/<bundle-id>/jelto/`. Runs on a dedicated
`DispatchQueue(qos: .utility)`; never touches the main thread; no `@MainActor`. Uses
`URLSession` with `waitsForConnectivity = false` (the SDK's own backoff governs).

**Electron.** Main process only. State in `app.getPath('userData')/jelto/`. Never uses `remote`,
never touches a renderer.

**.NET.** Dependency-free `net8.0` library for Windows, macOS and Linux desktop processes.
State is under `Environment.SpecialFolder.LocalApplicationData`, partitioned by entry assembly,
product key and app slug; `JELTO_STATE_DIR` overrides the entire path exactly. One background
worker owns lifecycle, persistence and delivery. Process exit requests a flush bounded to 600 ms;
framework-specific background hooks are outside v1. `spec/dotnet-sdk.md` fixes the public API,
packaging and additional verification gates.

**Tauri 2.** State in `app.path().app_local_data_dir()/jelto`; the test override
replaces the entire directory. Rust owns all app events through one Tokio worker.
The headless host uses the same engine with the Tauri adapter disabled. Command
permissions, bindings, C11 and C19 are separate gates in `spec/tauri-sdk.md` §3.

## 6. Certification

A community SDK is listed as compatible when its conformance run log (runner output, all C and
W scenarios) is attached to a PR against `spec/conformance/CERTIFIED.md` with the SDK version and
commit. Recertification on every minor wire-format addition.


## 7. Automatic application update detection

Updater activity is separate from automatic version detection. Applications use
the existing `track("app_update", props)` API for wire §4's explicit stages,
failures and postponements. No extra SDK method or state is required. The ordinary
queue, stable event IDs, retry, reset and disable rules apply. C24 verifies all
statuses through `track`, including unchanged version baselines and distinct IDs
for two observations of the same stage. SDKs do not infer updater outcomes from
the appcast URL or missing subsequent launches.

Gates: each SDK's own unit, build and budget checks, and two consecutive
full conformance runs for the reference host and each supported app SDK.
Swift, Electron, Tauri 2 and .NET MUST implement the same behavior.

At each fresh, consent-authorized initialization, observe the application's displayed
version from platform metadata. A known version is a nonblank string of at most 32
Unicode scalar values, preserved exactly (including whitespace inside a nonblank value,
case and build suffixes). Missing, empty, whitespace-only and overlong versions are
unknown; they MUST NOT erase a known baseline or produce a transition. Existing wire
metadata fallbacks may remain for ordinary events, but MUST NOT establish a baseline. Compare known
versions for exact scalar equality only. Do not parse SemVer, truncate for comparison,
strip suffixes or infer upgrade direction. Downgrades and returns to a previous version
are transitions too. Versions not launched with collection initialized cannot be observed.

Persist `last_app_version` with existing install state. A first known observation, including
an existing install whose legacy SDK state lacks this field, establishes a baseline with
no update. Later different known observations enqueue one `app_updated`, with the existing
`iid`, stable UUID event `id`, the observation time `t`, and exactly the fixed string props
`from_version` and `to_version` (wire §4). `av` equals `to_version`. Freeze transition
metadata at observation, including OS, architecture, app slug and client version, so a
later launch, offline drain or retry cannot rewrite history. Newly queued ordinary events
SHOULD also retain observed app metadata; legacy queue records remain readable.

Observation MUST run independently of the UTC daily heartbeat gate. It MUST NOT rotate
identity, reset install claim/deadline/first-attempt state, enqueue another install, or
change install metrics. A still-pending first install remains a single pending claim.
Repeated initialization while active is a no-op. Multiple transitions before delivery
are separate events, even A→B→A→B; a version-pair-derived ID would incorrectly merge them.

Baseline advancement and a recoverable transition MUST be one durable transaction, before
any send. An atomic combined state/queue checkpoint or durable state intent containing the
complete event is valid. With an intent, recover its same ID into the queue idempotently,
persist the queue, then durably retire the intent before dispatch. A crash before commit
leaves the old baseline; a crash after commit recovers the original event. Retry and
response-loss paths keep the same ID and immutable payload. Do not send a transition whose
required persistence failed, or advance the committed baseline without a recoverable event.
Existing queue caps/oldest-first eviction, final refusals, stop directives and opt-out still
apply; intentionally discarded events MUST NOT be resurrected by recovery. Unavailable or
corrupt storage cannot guarantee durability and MUST remain fail-soft. Multiple concurrent
process writers to one SDK state directory are outside this contract.

Reset discards queued transitions/intents belonging to the old identity and establishes
the current known version for the new identity without an update, retaining the SDK's
existing install-property behavior. Disable/deletion wipes the baseline and transition
state with identity and queue. No observation, disk write or socket occurs before init or
while disabled. A later consent-authorized init establishes a fresh baseline. No IP address,
device fingerprint or row-level web/app join key is introduced.

`legacyversion` is a conformance-only host command, never a public SDK API. After init and
only with no pending transition intent, it durably removes only `last_app_version` to
simulate a pre-feature state, preserving identity, install claim/deadline, properties and
queue. It MUST fail before init or while a transition intent is pending. Scenarios inspect
only §1's five surfaces, never SDK storage formats.

Shared C23 scenarios MUST cover: first known launch with no update; claimed legacy install
migration with no update; known changes and unchanged repeated launches; opaque version
strings, downgrades and A→B→A→B; same-day update despite a suppressed heartbeat; unknown
versions before/after a known baseline; offline multiple transitions, retries and abrupt
restart with stable IDs and metadata; reset and disable/re-init. Per-SDK storage tests MUST
exercise interrupted transition commits/recovery as well as write failure. Raw recordings
may contain retry requests, but each logical transition has one ID and one payload.

Existing dashboard analytics consumers select `surface` when discovering events through
GET `/api/v1/products/{product}/events`; app discovery includes `app_updated` and its fixed
properties. Unscoped settings reads remain the editable custom schema, so automatic telemetry
cannot consume custom schema slots or appear as a newly editable event. This requires only
existing data-loading plumbing, no new dashboard controls or visual design.
