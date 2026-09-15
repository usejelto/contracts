package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// tokenize is the only parser in the host, and three §4 rows depend on it:
// W2's quoted `track "Bad Name!"`, C20b's quoted reason, and W3's JSON
// argument that contains spaces and braces.
func TestTokenize(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`init prd_conform001 mac`, []string{"init", "prd_conform001", "mac"}},
		{`track "Bad Name!"`, []string{"track", "Bad Name!"}},
		{`onboarding x ok "Free text reason"`, []string{"onboarding", "x", "ok", "Free text reason"}},
		{`track x {"k":"a b c","n":1}`, []string{"track", "x", `{"k":"a b c","n":1}`}},
		{`setprops {"license":"paid","edition":"pro"}`, []string{"setprops", `{"license":"paid","edition":"pro"}`}},
	}
	for _, test := range cases {
		got := tokenize(test.line)
		if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Fatalf("tokenize(%q) = %q, want %q", test.line, got, test.want)
		}
	}
}

// spec/wire-v1.md §9: "Absent, unparseable, or not delay-seconds -> treated as
// absent, never as zero."
func TestParseRetryAfter(t *testing.T) {
	if _, ok := parseRetryAfter("", false); ok {
		t.Fatal("an absent header parsed")
	}
	if _, ok := parseRetryAfter("soon", true); ok {
		t.Fatal("`soon` parsed; it must be treated as absent")
	}
	if _, ok := parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT", true); ok {
		t.Fatal("the HTTP-date form parsed; §9 says it is never sent and is treated as absent")
	}
	if _, ok := parseRetryAfter("-1", true); ok {
		t.Fatal("a negative delay parsed")
	}
	millis, ok := parseRetryAfter(" 20 ", true)
	if !ok || millis != 20_000 {
		t.Fatalf("delay-seconds 20 parsed as %d ms, ok=%v", millis, ok)
	}
	if millis, ok := parseRetryAfter("0", true); !ok || millis != 0 {
		t.Fatalf("a zero delay is parseable and is a floor of 0: got %d, ok=%v", millis, ok)
	}
}

// RFC-0001 §8.3 item 6, and C6.
func TestQueueCapDropsTheOldestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.jsonl")
	queue := NewQueue(path)
	for i := 0; i < queueMaxEvents+500; i++ {
		queue.Append(QueuedEvent{ID: "id", N: "x", T: []byte("1")})
	}
	if got := queue.Len(); got != queueMaxEvents {
		t.Fatalf("queue holds %d events, cap is %d", got, queueMaxEvents)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > queueMaxBytes {
		t.Fatalf("queue file is %d bytes, cap is %d", len(raw), queueMaxBytes)
	}

	// The byte cap has to bite on its own, not only through the count.
	big := strings.Repeat("v", 2000)
	fat := NewQueue(filepath.Join(t.TempDir(), "queue.jsonl"))
	for i := 0; i < 900; i++ {
		fat.Append(QueuedEvent{ID: "id", N: "x", T: []byte("1"), Props: map[string]any{"p": big}})
	}
	if fat.Len() >= 900 {
		t.Fatalf("the 1 MB cap never bit: %d events of ~2 kB are held", fat.Len())
	}
}

// RFC-0001 §8.5 and C15b: a day index is floor(ms / 86 400 000) for every
// value, negative ones included, so a pre-epoch clock still has exactly one
// heartbeat day.
func TestDayIndexFloorsBelowTheEpoch(t *testing.T) {
	clock := NewClock()
	clock.Pin(big.NewInt(-14_256_000_000))
	first := clock.DayIndex()
	clock.Advance(86_399_999)
	if second := clock.DayIndex(); second != first {
		t.Fatalf("a day index changed inside one day: %s then %s", first, second)
	}
	clock.Advance(1)
	if third := clock.DayIndex(); third == first {
		t.Fatalf("a day index did not change across a day boundary: still %s", third)
	}
}

func TestClockKeepsAValuePastInt64(t *testing.T) {
	clock := NewClock()
	pin, _ := new(big.Int).SetString("99999999999999999999", 10)
	clock.Pin(pin)
	if got := string(literal(clock.Now())); got != "99999999999999999999" {
		t.Fatalf("t = %s; saturating into int64 is the correction RFC-0001 §8.5 forbids", got)
	}
	if id := newUUIDv7(clock.Now()); id == NilUUID || len(id) != 36 {
		t.Fatalf("newUUIDv7 produced %q for a clock past int64", id)
	}
}

// spec/wire-v1.md §2: 100 events and 65 536 bytes, whichever binds first.
func TestEnvelopeRespectsBothCaps(t *testing.T) {
	events := make([][]byte, 250)
	for i := range events {
		events[i] = []byte(`{"n":"x","s":"app"}`)
	}
	body, used := buildEnvelope("prd_conform001", events)
	if used != maxEventsPerReq {
		t.Fatalf("packed %d events, want %d", used, maxEventsPerReq)
	}
	if len(body) > maxBodyBytes {
		t.Fatalf("body is %d bytes, cap is %d", len(body), maxBodyBytes)
	}

	fat := make([][]byte, 100)
	for i := range fat {
		fat[i] = []byte(`{"n":"x","s":"app","props":{"p":"` + strings.Repeat("v", 1000) + `"}}`)
	}
	body, used = buildEnvelope("prd_conform001", fat)
	if len(body) > maxBodyBytes {
		t.Fatalf("body is %d bytes, cap is %d", len(body), maxBodyBytes)
	}
	if used == 0 || used == 100 {
		t.Fatalf("the byte cap did not bind: %d of 100 events packed into %d bytes", used, len(body))
	}
}

// --- spec/sdk-conformance.md §3.2, the state export ---------------------------

// hostReplies drives the §3 command loop the way the runner does: one command
// per line on stdin, one JSON line per command on stdout (§3.1). The replies
// are decoded with UseNumber, because §3.2's instants are decimal strings and
// nothing in a test may be the thing that rounds them.
func hostReplies(t *testing.T, env map[string]string, commands ...string) []map[string]json.RawMessage {
	t.Helper()
	for key, value := range env {
		t.Setenv(key, value)
	}
	var stdout, stderr bytes.Buffer
	if code := run(strings.NewReader(strings.Join(commands, "\n")+"\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("refhost exited %d: %s", code, stderr.String())
	}
	var replies []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var reply map[string]json.RawMessage
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&reply); err != nil {
			t.Fatalf("host reply %q is not JSON: %v", line, err)
		}
		replies = append(replies, reply)
	}
	return replies
}

// dumpstateExport returns the `state` of the one `dumpstate` reply, as §3.1
// carries it: on `state`, never on `value`.
func dumpstateExport(t *testing.T, replies []map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	for _, reply := range replies {
		var cmd string
		if err := json.Unmarshal(reply["cmd"], &cmd); err != nil || cmd != "dumpstate" {
			continue
		}
		var ok bool
		if err := json.Unmarshal(reply["ok"], &ok); err != nil || !ok {
			t.Fatalf("`dumpstate` answered %v", reply)
		}
		raw, present := reply["state"]
		if !present {
			t.Fatalf("the `dumpstate` reply carries no `state`; §3.1 puts §3.2's object there: %v", reply)
		}
		var export map[string]json.RawMessage
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&export); err != nil {
			t.Fatalf("`state` is not a JSON object: %v", err)
		}
		return export
	}
	t.Fatalf("no `dumpstate` reply in %v", replies)
	return nil
}

// exportedQueue decodes §3.2's `queue`. `t` is a string here because §3.2 makes
// every instant a decimal string; decoding it as a number is the float64 trap.
type exportedQueue struct {
	Bytes  int `json:"bytes"`
	Events []struct {
		ID string `json:"id"`
		N  string `json:"n"`
		T  string `json:"t"`
	} `json:"events"`
}

func exportedQueueOf(t *testing.T, export map[string]json.RawMessage) exportedQueue {
	t.Helper()
	var queue exportedQueue
	raw, present := export["queue"]
	if !present {
		t.Fatalf("the export has no `queue`; §3.2 requires it")
	}
	if err := json.Unmarshal(raw, &queue); err != nil {
		t.Fatalf("`queue` is not {bytes, events}: %v", err)
	}
	return queue
}

// §3.2: "the read is only a read: `dumpstate` MUST NOT create, load or write
// anything, so that calling it before `init` prints an empty export and leaves
// the state directory as empty as it found it (§8.2 item 5, C5)". Both halves
// are asserted here, and the second one with a directory that does not exist at
// all -- a `dumpstate` that called Load or MkdirAll would bring it into being.
func TestDumpstateBeforeInitCreatesNothingAndReportsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	replies := hostReplies(t, map[string]string{
		"JELTO_ENDPOINT":  "http://127.0.0.1:1/v1/e",
		"JELTO_STATE_DIR": dir,
	}, "dumpstate")
	export := dumpstateExport(t, replies)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("`dumpstate` brought %s into being (stat err = %v); §3.2 forbids it creating anything", dir, err)
	}

	var installID string
	if err := json.Unmarshal(export["install_id"], &installID); err != nil || installID != "" {
		t.Fatalf(`install_id is %s, want "" before init`, export["install_id"])
	}
	for _, key := range []string{"last_heartbeat_day", "install_due_at", "install_first_try", "backoff_next_at", "stop_until"} {
		if raw, present := export[key]; present && strings.Trim(string(raw), `"`) != "" {
			t.Fatalf("%s is %s in an export taken before init; §3.2: a fact that is not set is absent or empty", key, raw)
		}
	}
	if queue := exportedQueueOf(t, export); queue.Bytes != 0 || len(queue.Events) != 0 {
		t.Fatalf("queue is %d bytes and %d events before init, want empty", queue.Bytes, len(queue.Events))
	}

	// §3.2 names eleven keys and rules one refhost field OUT of the export:
	// "the reference host persists one more, a consecutive-refusal counter ...
	// it stays that host's own bookkeeping and the runner never reads it".
	if _, present := export["backoff_failures"]; present {
		t.Fatal("the export carries backoff_failures; §3.2 keeps it out of the contract")
	}
	named := map[string]bool{
		"install_id": true, "last_heartbeat_day": true, "install_claimed": true,
		"install_due_at": true, "install_first_try": true, "install_props": true,
		"backoff_step_ms": true, "backoff_next_at": true, "stop_until": true,
		"stop_probe_due": true, "queue": true,
	}
	for key := range export {
		if !named[key] {
			t.Fatalf("the export carries %q, which is not one of §3.2's eleven keys", key)
		}
	}
}

// §3.2: "Instants are decimal strings, not JSON numbers ... a reader that took
// these as JSON numbers would decode them into a float64 and round them". C15b
// pins a clock past int64 and RFC-0001 §8.5 forbids correcting it, so the
// export has to carry the digits it was given.
func TestDumpstateExportsInstantsPastInt64AsDecimalStrings(t *testing.T) {
	const pin = "99999999999999999999" // C15b's clock: past int64 by two digits
	replies := hostReplies(t, map[string]string{
		"JELTO_ENDPOINT":  "http://127.0.0.1:1/v1/e",
		"JELTO_STATE_DIR": t.TempDir(),
		"JELTO_NOW":       pin,
	}, "init prd_conform001", "track x", "dumpstate")
	export := dumpstateExport(t, replies)

	queue := exportedQueueOf(t, export)
	if len(queue.Events) < 2 {
		t.Fatalf("the export holds %d events, want the heartbeat and x", len(queue.Events))
	}
	for i, event := range queue.Events {
		if event.T != pin {
			t.Fatalf("queue.events[%d].t is %q, want %q -- a value that went through float64 rounds to 100000000000000000000", i, event.T, pin)
		}
		if event.ID == "" || event.N == "" {
			t.Fatalf("queue.events[%d] is %+v; §3.2 fixes id, n and t at enqueue time", i, event)
		}
	}

	// install_due_at is the other instant this arm sets: §8.2 item 4's immediate
	// deadline, equal to a draw instant int64 cannot hold.
	var dueAt string
	if err := json.Unmarshal(export["install_due_at"], &dueAt); err != nil {
		t.Fatalf("install_due_at is %s, want a decimal string: %v", export["install_due_at"], err)
	}
	due, ok := new(big.Int).SetString(dueAt, 10)
	if !ok {
		t.Fatalf("install_due_at %q is not a whole decimal of milliseconds", dueAt)
	}
	now, _ := new(big.Int).SetString(pin, 10)
	gap := new(big.Int).Sub(due, now)
	if gap.Sign() != 0 {
		t.Fatalf("install_due_at is %s ms from the pin, want 0; the draw instant did not survive the width", gap)
	}
}

// C4/C4c: the deadline is immediate at every supported clock width, and both
// it and the already queued event resume on a later launch without a 202.
func TestImmediateInstallDeadlineAndQueuedEventSurviveRelaunch(t *testing.T) {
	for _, pin := range []string{"0", "-1", "99999999999999999999"} {
		t.Run(pin, func(t *testing.T) {
			env := map[string]string{
				"JELTO_ENDPOINT":  "http://127.0.0.1:1/v1/e",
				"JELTO_STATE_DIR": t.TempDir(),
				"JELTO_NOW":       pin,
			}
			first := dumpstateExport(t, hostReplies(t, env, "init prd_conform001", "sleep 0", "dumpstate"))
			if string(first["install_due_at"]) != strconv.Quote(pin) {
				t.Fatalf("deadline %s, want draw instant %s", first["install_due_at"], pin)
			}
			var installID string
			for _, event := range exportedQueueOf(t, first).Events {
				if event.N == "install" {
					if installID != "" || event.T != pin {
						t.Fatalf("install is duplicated or not queued at init: %+v", event)
					}
					installID = event.ID
				}
			}
			if installID == "" {
				t.Fatal("install was not queued immediately")
			}
			now, _ := new(big.Int).SetString(pin, 10)
			env["JELTO_NOW"] = formatBig(after(now, 25_200_000))
			second := dumpstateExport(t, hostReplies(t, env, "init prd_conform001", "sleep 0", "dumpstate"))
			if string(second["install_due_at"]) != string(first["install_due_at"]) {
				t.Fatalf("relaunch redrew the deadline: %s -> %s", first["install_due_at"], second["install_due_at"])
			}
			installs := 0
			for _, event := range exportedQueueOf(t, second).Events {
				if event.N == "install" {
					installs++
					if event.ID != installID || event.T != pin {
						t.Fatalf("relaunch replaced the queued event: %+v", event)
					}
				}
			}
			if installs != 1 {
				t.Fatalf("relaunch holds %d installs, want 1", installs)
			}
		})
	}
}

// §3.2: "`bytes` is what the SDK counts against its own cap, not what a file
// system reports". RFC-0001 §8.3 item 6 caps the queue at 1 MB OR 1 000 events
// and C6 reads the byte half off this figure, so it has to be the running sum
// the cap actually binds on -- and it has to bind before the event count does
// on events of a few kilobytes, which is the whole reason §3.2 keeps the byte
// half at all.
func TestDumpstateQueueBytesIsWhatTheCapBindsOn(t *testing.T) {
	fat := map[string]string{}
	for i := 0; i < maxProps; i++ {
		fat["k"+strconv.Itoa(i)] = strings.Repeat("v", maxPropString)
	}
	props, err := json.Marshal(fat)
	if err != nil {
		t.Fatal(err)
	}
	commands := []string{"init prd_conform001"}
	const tracks = 400 // ~4.3 kB each: past 1 MB, far short of 1 000 events
	for i := 0; i < tracks; i++ {
		commands = append(commands, "track x"+strconv.Itoa(i)+" "+string(props))
	}
	commands = append(commands, "dumpstate")

	replies := hostReplies(t, map[string]string{
		"JELTO_ENDPOINT":  "http://127.0.0.1:1/v1/e",
		"JELTO_STATE_DIR": t.TempDir(),
		"JELTO_NOW":       "1788134400000",
	}, commands...)
	queue := exportedQueueOf(t, dumpstateExport(t, replies))

	if queue.Bytes > queueMaxBytes {
		t.Fatalf("queue.bytes is %d, over §8.3 item 6's cap of %d", queue.Bytes, queueMaxBytes)
	}
	if queue.Bytes <= queueMaxBytes-8192 {
		t.Fatalf("queue.bytes is %d after %d fat tracks; it is not tracking the running total the cap binds on", queue.Bytes, tracks)
	}
	if len(queue.Events) >= tracks {
		t.Fatalf("the byte cap never bit: %d events of ~4 kB are still held", len(queue.Events))
	}
	if len(queue.Events) >= queueMaxEvents {
		t.Fatalf("%d events held, so the COUNT cap bound; this arm exists to make the BYTE half do the work", len(queue.Events))
	}
	// Oldest dropped first (§8.3 item 6, C6's other half).
	if first := queue.Events[0].N; first == "x0" {
		t.Fatal("x0 is still queued, so nothing was dropped from the front")
	}
	if last := queue.Events[len(queue.Events)-1].N; last != "x"+strconv.Itoa(tracks-1) {
		t.Fatalf("the newest queued event is %q, want x%d", last, tracks-1)
	}
}

// --- what a virtual `sleep` promises (spec/conformance/TODO.md §5) ------------

// wireBatch is one recorded request: the §2 envelope reduced to the two fields
// these two tests assert on.
type wireBatch []struct {
	ID string          `json:"id"`
	N  string          `json:"n"`
	T  json.RawMessage `json:"t"`
}

func (b wireBatch) names() []string {
	names := make([]string, 0, len(b))
	for _, event := range b {
		names = append(names, event.N)
	}
	return names
}

// sleepHarness is refhost driven in-process against a recording endpoint, on a
// pinned clock, with the install already claimed so these synchronization tests
// isolate heartbeat and track batches from the immediate first-init install.
type sleepHarness struct {
	host  *host
	sdk   *SDK
	store *Store
	mu    sync.Mutex
	seen  []wireBatch
}

// pinnedMS is the clock both tests pin, and the `t` the first heartbeat has to
// carry.
const pinnedMS = "1788134400000"

func newSleepHarness(t *testing.T, statuses ...int) *sleepHarness {
	t.Helper()
	status := http.StatusAccepted
	if len(statuses) > 0 {
		status = statuses[0]
	}
	harness := &sleepHarness{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		var envelope struct {
			E wireBatch `json:"e"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("request body is not a spec/wire-v1.md §2 envelope: %v", err)
		}
		harness.mu.Lock()
		harness.seen = append(harness.seen, envelope.E)
		harness.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	seeded := `{"install_id":"11111111-1111-4111-8111-111111111111","install_claimed":true}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	platform, err := detectPlatform("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := new(big.Int).SetString(pinnedMS, 10)
	clock := NewClock()
	clock.Pin(pin)
	harness.store = NewStore(dir)
	harness.sdk = NewSDK(NewDebug(io.Discard, false), clock, harness.store, server.URL, "", "refhost/0.1.0", platform, 1)
	harness.host = &host{sdk: harness.sdk, clock: clock, out: io.Discard, err: io.Discard}
	t.Cleanup(harness.sdk.Stop)
	return harness
}

func (h *sleepHarness) sent() []wireBatch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wireBatch(nil), h.seen...)
}

// await blocks for up to two seconds for `count` requests to have arrived. It
// is the test waiting for something it EXPECTS, which is not what settle does;
// nothing here is asserted from a poll going quiet.
func (h *sleepHarness) await(count int) []wireBatch {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if batches := h.sent(); len(batches) >= count {
			return batches
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h.sent()
}

// held is how long each test pins the pump at a lock it must take before it can
// do the work that is due. It is nearly twenty times the 8 ms window the
// quiet-poll settle returned in, so neither test is a timing coin flip. The
// lock stands in for the only thing the host ever really faced: the pump has
// been WOKEN and the Go scheduler has not run it yet.
const held = 150 * time.Millisecond

func stall(lock *sync.Mutex) {
	lock.Lock()
	// Released from a timer, never with a defer: the point is that the pump is
	// still held at the moment a settle that guesses would already have
	// returned.
	time.AfterFunc(held, lock.Unlock)
}

// Hold the state-store lock to keep bootstrap from sampling the clock. Virtual
// sleep must wait for bootstrap before advancing, or it can move the first flush
// past the sleep and combine the heartbeat with the next event (C16b).
func TestVirtualSleepDoesNotAdvanceThePinnedClockPastInitsOwnWork(t *testing.T) {
	harness := newSleepHarness(t)

	stall(&harness.store.mu)
	harness.host.dispatch("init prd_conform001")
	harness.host.dispatch("sleep 3000")

	batches := harness.await(1)
	if len(batches) != 1 {
		t.Fatalf("%d request(s) arrived, want 1: the clock advanced past `init`'s own work, so the 2 s flush was anchored at T+5000 and never came due", len(batches))
	}
	if names := batches[0].names(); len(names) != 1 || names[0] != "heartbeat" {
		t.Fatalf("the first batch is %v, want [heartbeat]", names)
	}
	if got := string(batches[0][0].T); got != pinnedMS {
		t.Fatalf("the heartbeat carries t=%s, want %s: bootstrap read the clock AFTER `sleep` moved it", got, pinnedMS)
	}
}

// Hold the queue lock to model a woken pump that has not run yet. Virtual sleep
// must wait for its work; assert request counts because flat event lists cannot
// detect two batches incorrectly combined into one (TODO.md §5).
func TestVirtualSleepDoesNotReplyBeforeTheDueFlushIsDispatched(t *testing.T) {
	harness := newSleepHarness(t)

	harness.host.dispatch("init prd_conform001")
	harness.host.dispatch("sleep 3000")
	if batches := harness.await(1); len(batches) != 1 {
		t.Fatalf("%d request(s) arrived for the init flush, want 1", len(batches))
	}

	// The 5 s track debounce (§8.3 item 7) is now due at T+9000.
	harness.host.dispatch("track y")
	stall(&harness.sdk.queue.mu)
	harness.host.dispatch("sleep 6000")

	batches := harness.sent()
	if len(batches) != 2 {
		t.Fatalf("%d request(s) on the wire when `sleep 6000` replied, want 2: %v -- an early reply lets the next track ride the batch the pump was still holding", len(batches), batches)
	}
	if names := batches[1].names(); len(names) != 1 || names[0] != "y" {
		t.Fatalf("the second batch is %v, want [y]", names)
	}
}

// §3 (v0.14): "a bare `setprops` is a scenario error, not a call". This host
// used to call SetProps(nil) and reply ok, which is a write the SDK honours
// -- it replaces the property set with nothing -- while the Swift host
// refused, and no scenario sends one, so the disagreement was invisible to
// the suite. The check is on the SDK's exported state: `install_props` must
// still carry what the previous, well-formed `setprops` wrote.
func TestBareSetpropsIsRefusedAndNeverReachesTheSDK(t *testing.T) {
	replies := hostReplies(t, map[string]string{
		"JELTO_ENDPOINT":  "http://127.0.0.1:1/v1/e",
		"JELTO_STATE_DIR": t.TempDir(),
		"JELTO_NOW":       "1788134400000",
	}, "init prd_conform001", `setprops {"license":"paid"}`, "setprops", "dumpstate")
	// Four commands, four replies, plus the `eof` reply the host writes when
	// stdin closes without an `exit`.
	if len(replies) < 4 {
		t.Fatalf("got %d replies, want at least 4", len(replies))
	}
	bare := replies[2]
	if string(bare["cmd"]) != `"setprops"` || string(bare["ok"]) == "true" {
		t.Fatalf("bare setprops reply = %s, want cmd setprops and not ok", jsonLine(bare))
	}
	var usage string
	if err := json.Unmarshal(bare["error"], &usage); err != nil || usage != "setprops <json>" {
		t.Fatalf("bare setprops error = %s, want the usage line", bare["error"])
	}
	export := dumpstateExport(t, replies)
	var props map[string]any
	if err := json.Unmarshal(export["install_props"], &props); err != nil {
		t.Fatalf("install_props: %v", err)
	}
	if props["license"] != "paid" {
		t.Fatalf("install_props = %v after a bare setprops; the well-formed write was replaced, so the bare one reached the SDK", props)
	}
}

func jsonLine(reply map[string]json.RawMessage) string {
	raw, _ := json.Marshal(reply)
	return string(raw)
}
