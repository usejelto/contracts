package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jelto.io/jelto/contracts/spec/wirecheck"
)

func validator(t *testing.T) *wirecheck.Validator {
	t.Helper()
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	v, err := wirecheck.New(filepath.Join(root, "spec", "wire-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const appEvent = `{"n":"heartbeat","s":"app","iid":"3f1b6c3e-0f2a-4d55-9b21-2f5c0f6a1234","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`

func envelope(events ...string) string {
	return `{"v":1,"p":"prd_conform001","e":[` + strings.Join(events, ",") + `]}`
}

// W1's nil-UUID clause is the one thing no correct host can be asked to
// produce, so it is proved here instead of in a scenario. spec/wire-v1.md §3:
// "a present `id` MUST NOT be the nil UUID".
func TestValidatorRejectsANilUUIDId(t *testing.T) {
	body := envelope(`{"n":"x","s":"app","id":"00000000-0000-0000-0000-000000000000","iid":"3f1b6c3e-0f2a-4d55-9b21-2f5c0f6a1234","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`)
	if err := validator(t).ValidateBody([]byte(body)); err == nil {
		t.Fatal("a nil-UUID `id` validated; W1 would pass an SDK that collapses ten minutes of a product's traffic onto one row")
	}
}

func TestValidatorRejectsANilUUIDInstallID(t *testing.T) {
	body := envelope(`{"n":"x","s":"app","iid":"00000000-0000-0000-0000-000000000000","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`)
	if err := validator(t).ValidateBody([]byte(body)); err == nil {
		t.Fatal("a nil `iid` validated; spec/wire-v1.md §5.2 rejects it")
	}
}

func TestValidatorRejectsAClientVersionOutsideTheGrammar(t *testing.T) {
	for _, version := range []string{"1.2.0", "Electron/1.0"} {
		body := envelope(`{"n":"x","s":"app","v":"` + version + `","iid":"3f1b6c3e-0f2a-4d55-9b21-2f5c0f6a1234","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`)
		if err := validator(t).ValidateBody([]byte(body)); err == nil {
			t.Fatalf("v=%q validated; spec/wire-v1.md §3's grammar is normative (W4)", version)
		}
	}
}

func TestValidatorRejectsAnEnvelopeOutsideItsBounds(t *testing.T) {
	if err := validator(t).ValidateBody([]byte(`{"v":1,"p":"k","e":[` + appEvent + `]}`)); err == nil {
		t.Fatal("p=\"k\" validated; §4 writes `init k` in twenty rows and §2 requires ^prd_[a-z0-9]{10}$")
	}
	if err := validator(t).ValidateBody([]byte(`{"v":1,"p":"prd_conform001","e":[]}`)); err == nil {
		t.Fatal("an empty `e` validated; §2 requires 1-100 events")
	}
	events := make([]string, 101)
	for i := range events {
		events[i] = appEvent
	}
	if err := validator(t).ValidateBody([]byte(envelope(events...))); err == nil {
		t.Fatal("101 events validated; §2 caps `e` at 100")
	}
}

// C15b's two values, through the decode path the runner actually uses. Without
// UseNumber the exponent form reads back 1785578400000 and the past-int64 one
// reads back 100000000000000000000, and the schema would then be validating
// numbers the SDK never sent.
func TestValidatorAcceptsTheClocksC15bSends(t *testing.T) {
	for _, literal := range []string{"-14256000000", "0", "1.7855784e12", "99999999999999999999", "1785578400000.0"} {
		body := envelope(`{"n":"heartbeat","s":"app","t":` + literal + `,"iid":"3f1b6c3e-0f2a-4d55-9b21-2f5c0f6a1234","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`)
		if err := validator(t).ValidateBody([]byte(body)); err != nil {
			t.Fatalf("t=%s did not validate: %v", literal, err)
		}
	}
	body := envelope(`{"n":"heartbeat","s":"app","t":1.5,"iid":"3f1b6c3e-0f2a-4d55-9b21-2f5c0f6a1234","av":"1.0.0","os":"macos","osv":"15.1","arch":"arm64"}`)
	if err := validator(t).ValidateBody([]byte(body)); err == nil {
		t.Fatal("t=1.5 validated; spec/wire-v1.md §3 makes a genuine fraction invalid_field")
	}
}

// The trap mockd's doc.go records, asserted on the runner's own decoder rather
// than assumed.
func TestRecordDecodeKeepsTheTLiteral(t *testing.T) {
	line := `{"seq":1,"kind":"request","method":"POST","path":"/v1/e","envelope":{"p":"prd_conform001","event_count":2,"events":[{"n":"a","t":1.7855784e12},{"n":"b","t":99999999999999999999}]}}`
	var record Record
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatal(err)
	}
	if got := string(record.Envelope.Events[0].T); got != "1.7855784e12" {
		t.Fatalf("t[0] = %s, want the literal 1.7855784e12", got)
	}
	if got := string(record.Envelope.Events[1].T); got != "99999999999999999999" {
		t.Fatalf("t[1] = %s, want the literal 99999999999999999999", got)
	}

	// What the careless decode would have done, so the difference is on the
	// record and not merely in a comment.
	var loose map[string]any
	if err := json.Unmarshal([]byte(line), &loose); err != nil {
		t.Fatal(err)
	}
	events := loose["envelope"].(map[string]any)["events"].([]any)
	if got := events[0].(map[string]any)["t"].(float64); got != 1785578400000 {
		t.Fatalf("the float64 path produced %v; the point of this test is that it produces 1785578400000", got)
	}
}

func TestScenariosLoadAndAreOrdered(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := LoadScenarios(filepath.Join(root, "spec", "conformance", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no scenarios loaded")
	}
	seen := map[string]string{}
	for _, scenario := range scenarios {
		if previous, ok := seen[scenario.C]; ok {
			t.Fatalf("%s is claimed by both %s and %s; §1 is one file per C-number", scenario.C, previous, scenario.Path)
		}
		seen[scenario.C] = scenario.Path
		if scenario.Implements == "" {
			t.Fatalf("%s names no `implements:`; CLAUDE.md requires every task name its spec section", scenario.Path)
		}
		if scenario.Skip == "" && len(scenario.Arms) == 0 {
			t.Fatalf("%s has no arms and no `skip:`; a scenario that runs nothing must say so", scenario.Path)
		}
		for _, arm := range scenario.Arms {
			for _, check := range arm.Assert {
				if !knownChecks[check.Check] {
					t.Fatalf("%s uses check %q, which the catalogue in checks.go does not have", scenario.Path, check.Check)
				}
			}
		}
	}
	if order("C8") >= order("C10") {
		t.Fatal("C8 must sort before C10")
	}
}

// knownChecks mirrors runCheck's switch. A scenario naming a check that does
// not exist fails at assert time otherwise, which reads as a host bug.
var knownChecks = map[string]bool{
	"request_count": true, "connection_count": true, "event_count": true,
	"max_events_per_request": true, "event_names": true, "request_shape": true,
	"schedule": true, "no_request_before_s": true, "stable_ids": true, "app_updates": true,
	"statuses": true, "stderr": true, "host_reply": true, "state_dir_empty": true,
	"state": true, "queue": true, "t_literal": true, "event_field": true,
	"event_prop": true, "exit_code": true, "exit_within_ms": true,
	"host_reply_stable": true, "request_count_after_step": true,
	"request_after_step": true, "state_deadline": true,
	"stderr_contains_body": true, "events_congruent": true,
}

func TestAppUpdatesRejectsChangedRetryPayloadAndRepeatedPairWithSameID(t *testing.T) {
	update := func(id, to string) Record {
		return Record{Body: envelope(`{"id":"` + id + `","n":"app_updated","iid":"install","av":"` + to + `","t":1788134400000,"props":{"from_version":"A","to_version":"` + to + `"}}`)}
	}
	check := Check{Check: "app_updates", Values: []string{"A=>B"}}
	if result := runCheck(Evidence{Requests: []Record{update("one", "B"), update("one", "B")}}, check); !result.Passed {
		t.Fatal(result.Detail)
	}
	if result := runCheck(Evidence{Requests: []Record{update("one", "B"), update("one", "C")}}, check); result.Passed {
		t.Fatal("changed retry payload accepted")
	}
	if result := runCheck(Evidence{Requests: []Record{update("one", "B"), update("two", "B")}}, check); result.Passed {
		t.Fatal("extra logical transition accepted")
	}
}

func TestStateStringComparisonAcceptsEquivalentJSONEscaping(t *testing.T) {
	check := Check{Check: "state", Path: "last_app_version", Value: `"build-A+2"`}
	for _, literal := range []string{`"build-A+2"`, `"build-A\u002B2"`} {
		result := runCheck(Evidence{State: json.RawMessage(`{"last_app_version":` + literal + `}`)}, check)
		if !result.Passed {
			t.Fatal(result.Detail)
		}
	}
	if result := runCheck(Evidence{State: json.RawMessage(`{"last_app_version":"build-A-2"}`)}, check); result.Passed {
		t.Fatal("different versions compared equal")
	}
}

// Use constructed recordings to exercise retry-prefix growth deterministically;
// random install timing cannot provide reliable coverage of this case.

func requestWithIDs(ids ...string) Record {
	events := make([]Event, len(ids))
	for i, id := range ids {
		events[i] = Event{N: "x", ID: id}
	}
	return Record{
		Kind: "request", Method: "POST", Path: "/v1/e",
		Envelope: &Envelope{P: "prd_conform001", EventCount: len(ids), Events: events},
	}
}

func stableIDs(t *testing.T, check Check, requests ...Record) Result {
	t.Helper()
	return runCheck(Evidence{Requests: requests}, check)
}

func intp(v int) *int { return &v }

// The rule: spec/wire-v1.md §6 forbids ALTERING the ids in the batch it
// resends. A resend that carries the same ids at the same indices and appends
// a never-before-sent event alters nothing, and cannot defeat `id`'s 10-minute
// dedup window, which is the harm §6 names.
func TestStableIDsAllowsAnAppendedEvent(t *testing.T) {
	result := stableIDs(t, Check{Check: "stable_ids"},
		requestWithIDs("a", "b"),
		requestWithIDs("a", "b"),
		requestWithIDs("a", "b", "c"),
		requestWithIDs("a", "b", "c"),
	)
	if !result.Passed {
		t.Fatalf("an appended event was reported as an altered id: %s", result.Detail)
	}
	if !strings.Contains(result.Detail, "appended") {
		t.Fatalf("the pass detail hides the growth, which is the thing a reader needs to see: %s", result.Detail)
	}
}

func TestStableIDsRejectsAnAlteredID(t *testing.T) {
	result := stableIDs(t, Check{Check: "stable_ids"},
		requestWithIDs("a", "b"),
		requestWithIDs("a", "z"),
	)
	if result.Passed {
		t.Fatal("a regenerated id passed; this is exactly what spec/wire-v1.md §6 forbids and C8 exists to catch")
	}
	if !strings.Contains(result.Detail, "ALTERED the id at position 1") {
		t.Fatalf("the failure does not say which id moved: %s", result.Detail)
	}
}

func TestStableIDsRejectsAReorder(t *testing.T) {
	result := stableIDs(t, Check{Check: "stable_ids"},
		requestWithIDs("a", "b"),
		requestWithIDs("b", "a"),
	)
	if result.Passed {
		t.Fatal("a reordered batch passed; the ids must keep their indices, not merely their set")
	}
}

func TestStableIDsRejectsAShrunkBatch(t *testing.T) {
	result := stableIDs(t, Check{Check: "stable_ids"},
		requestWithIDs("a", "b", "c"),
		requestWithIDs("a", "b"),
	)
	if result.Passed {
		t.Fatal("a batch that lost an event it had already offered passed; that is data loss before a 202")
	}
	if !strings.Contains(result.Detail, "shrank") {
		t.Fatalf("the failure does not name the shrink: %s", result.Detail)
	}
}

// `equal` pins the batch size for an arm that wants no growth at all, and
// `request` scopes the comparison past requests that were accepted and are
// therefore a different batch.
func TestStableIDsHonoursEqualAndRequest(t *testing.T) {
	grown := stableIDs(t, Check{Check: "stable_ids", Equal: intp(2)},
		requestWithIDs("a", "b"),
		requestWithIDs("a", "b", "c"),
	)
	if grown.Passed {
		t.Fatal("`equal: 2` did not pin the batch size")
	}

	scoped := stableIDs(t, Check{Check: "stable_ids", Request: intp(1), Equal: intp(1)},
		requestWithIDs("first", "batch"), // accepted; a different batch entirely
		requestWithIDs("x"),
		requestWithIDs("x"),
	)
	if !scoped.Passed {
		t.Fatalf("`request: 1` did not scope past the accepted batch: %s", scoped.Detail)
	}
}

func TestStableIDsRejectsAMissingID(t *testing.T) {
	result := stableIDs(t, Check{Check: "stable_ids"}, requestWithIDs("a", ""), requestWithIDs("a", ""))
	if result.Passed {
		t.Fatal("an event with no `id` passed; §6's rule cannot be checked against one")
	}
}

// --- spec/sdk-conformance.md §3.2, the state export ---------------------------

// export builds the evidence a state check reads: §3.2's object, and a
// StateDir that DOES NOT EXIST. The missing directory is the assertion: §3.2
// makes the state contract semantic, so a check that still opened
// `state.json` or `queue.jsonl` fails here, and an SDK storing its state in a
// plist, in UserDefaults or in SQLite passes.
func export(t *testing.T, body string) Evidence {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Fatalf("test export is not JSON: %s", body)
	}
	return Evidence{
		State:    json.RawMessage(body),
		StateDir: filepath.Join(t.TempDir(), "no-such-directory"),
	}
}

func mustPass(t *testing.T, result Result) {
	t.Helper()
	if !result.Passed {
		t.Fatalf("%s failed: %s", result.Name, result.Detail)
	}
}

func mustFail(t *testing.T, result Result) {
	t.Helper()
	if result.Passed {
		t.Fatalf("%s passed and must not: %s", result.Name, result.Detail)
	}
}

// C2, C4, C4b, C8c and C9b read a fact out of the export. Their `path:`
// arguments are unchanged from when they read a file, which is what makes this
// rewrite behaviour-preserving.
func TestStateChecksReadTheExportAndNotTheDirectory(t *testing.T) {
	const body = `{
	  "install_id": "9f2c8f3e-1a2b-4c3d-8e4f-5a6b7c8d9e0f",
	  "install_claimed": true,
	  "backoff_step_ms": 2000,
	  "install_props": {"license": "trial"},
	  "queue": {"bytes": 0, "events": []}
	}`
	evidence := export(t, body)

	mustPass(t, checkStateJSON("install_id", evidence, Check{
		Path:  "install_id",
		Regex: `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	}))
	mustPass(t, checkStateJSON("install_claimed", evidence, Check{Path: "install_claimed", Value: "true"}))
	mustPass(t, checkStateJSON("backoff_step_ms", evidence, Check{Path: "backoff_step_ms", Value: "2000"}))
	// C9b's form: §3.2's "a fact that is not set is absent or empty", and for
	// backoff_step_ms specifically "0 or absent means NOT IN BACKOFF".
	mustPass(t, checkStateJSON("stop_until", evidence, Check{Path: "stop_until", Empty: true}))
	mustPass(t, checkStateJSON("backoff_step_ms", export(t, `{"backoff_step_ms": 0}`), Check{Path: "backoff_step_ms", Empty: true}))
	mustFail(t, checkStateJSON("backoff_step_ms", evidence, Check{Path: "backoff_step_ms", Empty: true}))

	mustFail(t, checkStateJSON("install_claimed", evidence, Check{Path: "install_claimed", Value: "false"}))
	mustFail(t, checkStateJSON("missing", evidence, Check{Path: "last_heartbeat_day", Value: "20696"}))
}

// C4, C4c and C8c measure an instant in the export from an anchor. §3.2 makes
// every instant a decimal string precisely so a clock past int64 survives the
// read, so the arithmetic here is big.Int on both sides.
func TestStateDeadlineReadsTheExportAtArbitraryPrecision(t *testing.T) {
	evidence := export(t, `{"install_due_at": "1788141600000", "queue": {"bytes": 0, "events": []}}`)
	evidence.Arm = Arm{Env: map[string]string{"JELTO_NOW": "1788134400000"}}

	// 7 200 000 ms after the pin: inside §8.2 item 4's 0-6 h.
	mustPass(t, checkStateDeadline("install_due_at", evidence, Check{
		Path: "install_due_at", Anchor: "jelto_now", Min: intp(0), Max: intp(21600),
	}))
	mustFail(t, checkStateDeadline("install_due_at", evidence, Check{
		Path: "install_due_at", Anchor: "jelto_now", Min: intp(0), Max: intp(3600),
	}))

	// C15b's width. A reader that took these as JSON numbers would round both
	// sides into a float64 and report a gap of 0.
	huge := export(t, `{"backoff_next_at": "99999999999999999999999", "queue": {"bytes": 0, "events": []}}`)
	huge.Arm = Arm{Env: map[string]string{"JELTO_NOW": "99999999999999996399999"}}
	mustPass(t, checkStateDeadline("backoff_next_at", huge, Check{
		Path: "backoff_next_at", Anchor: "jelto_now", Seconds: 3600, TolerancePct: 1,
	}))
}

// C6 and C4b and C18 read §3.2's `queue`. `bytes` is "what the SDK counts
// against its own cap, not what a file system reports", and the events carry
// `n`, so "the oldest were dropped" stays a fact about identity.
func TestQueueCheckReadsTheExportedQueue(t *testing.T) {
	evidence := export(t, `{
	  "install_id": "",
	  "queue": {"bytes": 1048000, "events": [
	    {"id": "a", "n": "x500", "t": "1788134400000"},
	    {"id": "b", "n": "x1499", "t": "1788134400001"}
	  ]}
	}`)

	// C6's row, argument for argument.
	mustPass(t, checkQueueFile("queue", evidence, Check{Max: intp(1000), Value: "1048576"}))
	mustPass(t, checkQueueFile("queue", evidence, Check{
		Contains: []string{"x1499", "x500"}, NotContains: []string{"x0", "x499", "heartbeat"},
	}))
	// C4b's row.
	mustPass(t, checkQueueFile("queue", evidence, Check{Counts: map[string]int{"x500": 1}}))

	mustFail(t, checkQueueFile("queue", evidence, Check{Max: intp(1)}))
	mustFail(t, checkQueueFile("queue", evidence, Check{Value: "1000"}))
	mustFail(t, checkQueueFile("queue", evidence, Check{Contains: []string{"install"}}))
	mustFail(t, checkQueueFile("queue", evidence, Check{NotContains: []string{"x500"}}))
	// C18: disable() leaves nothing queued.
	mustFail(t, checkQueueFile("queue", evidence, Check{Absent: true}))
	mustPass(t, checkQueueFile("queue", export(t, `{"queue":{"bytes":0,"events":[]}}`), Check{Absent: true}))
}

// A host that answered nothing is a failing row, never a passing one: §3.2's
// "a fact the SDK does not have is absent from the export -- which is a failing
// row wherever a row asserts it".
func TestStateChecksFailWhenTheHostExportedNothing(t *testing.T) {
	empty := Evidence{StateDir: filepath.Join(t.TempDir(), "no-such-directory")}
	mustFail(t, checkStateJSON("state", empty, Check{Path: "install_id", Regex: "."}))
	mustFail(t, checkQueueFile("queue", empty, Check{Max: intp(1000)}))
	mustFail(t, checkStateDeadline("state_deadline", empty, Check{Path: "backoff_next_at", Seconds: 1}))
	// Even `absent: true` and `empty: true` fail: an export that was never
	// taken is not evidence that the SDK is holding nothing.
	mustFail(t, checkQueueFile("queue", empty, Check{Absent: true}))
	mustFail(t, checkStateJSON("state", empty, Check{Path: "backoff_step_ms", Empty: true}))
}

// Spawn deliberately malformed hosts to test stdout framing. Correct hosts cannot
// be asked to emit protocol violations through the conformance command language.

// writeFakeHost puts an executable sh host in a temp dir and returns its path.
func writeFakeHost(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conformance-host")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A structured logger pointed at stdout is the dangerous case: the line is
// valid JSON, so the decode succeeds and the runner would take it as the
// reply. Every later reply is then read one behind.
func TestSendRejectsAStrayJSONLineOnStdout(t *testing.T) {
	host := writeFakeHost(t, `read line
printf '{"level":"info","msg":"jelto: posting 1 event"}\n'
printf '{"cmd":"init","ok":true}\n'
`)
	started, err := StartHost(host, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer started.Kill()
	_, err = started.Send("init prd_conform001", 10*time.Second)
	if err == nil {
		t.Fatal("a stray JSON line on stdout was accepted as the reply to `init`; §3.1 makes stdout the reply channel and nothing else")
	}
	for _, want := range []string{"§3.1", "desynchronises"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// The merely-malformed case. It already failed before the `cmd` check existed,
// but the message blamed JSON rather than the rule, so a reader chased a parser
// bug instead of a print statement.
func TestSendRejectsAStrayNonJSONLineOnStdout(t *testing.T) {
	host := writeFakeHost(t, `read line
printf 'jelto: sending 1 event\n'
printf '{"cmd":"init","ok":true}\n'
`)
	started, err := StartHost(host, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer started.Kill()
	if _, err = started.Send("init prd_conform001", 10*time.Second); err == nil {
		t.Fatal("a stray non-JSON line on stdout was accepted")
	} else if !strings.Contains(err.Error(), "§3.1") {
		t.Errorf("error %q does not name the rule it broke", err)
	}
}

// The control. Without it the two rows above would pass against a Send that
// rejected everything.
func TestSendAcceptsAReplyThatEchoesTheCommandWord(t *testing.T) {
	host := writeFakeHost(t, `read line
printf '{"cmd":"init","ok":true,"us":41}\n'
`)
	started, err := StartHost(host, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	defer started.Kill()
	reply, err := started.Send("init prd_conform001 mac", 10*time.Second)
	if err != nil {
		t.Fatalf("a well-formed reply was rejected: %v", err)
	}
	if reply.Cmd != "init" || !reply.OK || reply.Micros != 41 {
		t.Errorf("reply = %+v, want cmd=init ok=true us=41", reply)
	}
}

func TestCommandWordIsTheBareVerb(t *testing.T) {
	for command, want := range map[string]string{
		`init prd_conform001 mac`:      "init",
		`track x`:                      "track",
		`setprops {"license":"trial"}`: "setprops",
		`track "Bad Name!"`:            "track",
		`onboarding x ok "Free text"`:  "onboarding",
		`dumpstate`:                    "dumpstate",
		``:                             "",
	} {
		if got := commandWord(command); got != want {
			t.Errorf("commandWord(%q) = %q, want %q", command, got, want)
		}
	}
}
