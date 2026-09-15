# Conformance limitations and resolved constraints

The remaining limitation is the Electron memory budget in §7. Resolved sections retain the constraints that callers reference.

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

## 3. Immediate install timing — resolved

RFC-0001 §8.2 item 4 now queues the install immediately on first init, with a
persisted deadline equal to the draw instant. There is no install lottery or
seed knob: the first batch carries the install with the initial heartbeat.

C3 counts heartbeats across launches and needs no preamble. C8c and C16 retain
their seven-hour simulated preambles under `ok` to preserve reviewed request
indexes and clock arithmetic, although seven hours is no longer needed to make
the install due. C16's stop duration still absorbs its retained preamble, and
its stderr guard verifies the switch is active despite the two clock frames.

Exact event-list assertions must account for the immediate install in the first
batch. Change existing arm structure only where a run demonstrates a failure.

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
