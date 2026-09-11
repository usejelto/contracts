# Conformance limitations and resolved constraints

Current limitations are the random install timing in §3 and §7, and the Electron
memory budget in §7. Resolved sections retain the constraints that callers reference.

## 1. Scenario semantics — resolved

The conformance specification defines `k` as a product-key placeholder; executable
scenarios use `prd_conform001`. The following distinctions remain relevant when
editing scenarios:

- A 202 accepts the batch even when events are rejected as `stopped`. In C16,
  previously sent `x` events are gone; the queue retains only `y`.
- A next-process heartbeat requires a new UTC day.
- Mock arrival schedules use real time. Long retry ceilings are checked through
  exported backoff state rather than waiting an hour.
- C15b injects an arbitrary-precision clock value beyond real platform clocks.
- C20 compares equivalent event fields while excluding independently generated IDs
  and timestamps.

## 2. Host protocol and state — resolved

`spec/sdk-conformance.md` §3 defines completion replies, environment inputs,
`dumpstate`, and virtual clock advancement. State checks read semantic exports;
SDK file formats remain private. Directory emptiness is checked separately on disk,
since an export cannot prove that forgotten files do not exist. Stderr is an
explicit assertion surface.

## 3. Random install timing — remaining limitation

A random 0–6 hour install delay can put an install inside an arm that asserts an
exact event list. C16 avoids this by spending seven simulated hours under `ok`
before its assertions. Its stop duration also absorbs that preamble, and a stderr
guard verifies that the stop is actually active despite the two clock frames.

The corresponding changes to these eight arms were declined because changing their
reviewed assertions and request indexes carried risk. Approximate exposure follows
from the arm span divided by six hours:

| Arms | Span | Exposure per arm |
|---|---|---|
| C9, C9b | 11 s | 0.05% |
| C16b | 9 s | 0.04% |
| C20, C20b, W2, W3 | 6 s | 0.03% |
| C21 | 3 s | 0.01% |

Reconsider these arms using measured failures after the synchronization fix in §5.
Use the contract's seven-hour preamble instead of disabling randomness with a seed.
C3 has the same exposure; see §7.

## 4. Documentation corrections — resolved

Mock `ok` responses are `202 {}`. Runner examples must use valid product keys.

## 5. Virtual-sleep synchronization — resolved

The host must wait for an explicit idle acknowledgement before and after advancing
its pinned clock. Advancing before bootstrap samples time can move the first flush
past the requested sleep. Polling activity counters for a quiet interval can also
return before the scheduler runs the pump.

The pump reads the barrier before reading the clock and acknowledges it only after
one full evaluation finds nothing due, including completion of any request in flight.
Clear wake-coalescing state before that observation so a concurrent notification
schedules a fresh evaluation. This is required for request counts: a flat event list
can pass even when a race combines events into the wrong batches.

## 6. Harness maintenance — resolved

Compiled host binaries are build outputs. State checks are named `state`, `queue`,
and `state_deadline`; only `state_dir_empty` inspects the filesystem.

## 7. Cross-SDK coverage and remaining budget gap

C4c includes both graceful restart and abrupt `kill_host` coverage. The abrupt arm
requires persisting the install deadline when drawn, rather than during shutdown.
`killCurrent` captures neither `dumpstate` nor a synthetic exit code: either would
misrepresent an abrupt death or shift later scenario result indexes. The
abrupt-restart contract is exercised by [C4c](scenarios/C4c.yaml).

C3 advances 3,000 ms on three launches while the install remains unclaimed, giving
approximately 1-in-2,400 exposure to the same install timing described in §3. Its
scenario notes do not currently explain that limitation.

The host contract now also requires:

- A directly executable, argument-free host. Wrappers must `exec` so SIGKILL reaches
  the runtime PID held by the runner.
- An error reply for bare `setprops`, without calling the SDK.
- Host validation of `JELTO_NOW` before the first command, exiting 2 on invalid syntax.
- Stdout reserved for replies from both host and SDK, with a checked `cmd` echo.
  Stray valid JSON would otherwise be mistaken for a reply and shift later commands.

**Electron's C11 memory budget remains unmet.** The recorded RSS delta is 47,968 KiB
across 10,000 `track` calls against a 2,048 KiB ceiling, with the baseline after
`init`; CPU passed at p99 188 µs. `C11.yaml` skips this row for every host and delegates
budget verification to Swift tests. Electron has no equivalent passing gate. The
memory ceiling needs an RFC decision; the harness cannot certify it as satisfied.
