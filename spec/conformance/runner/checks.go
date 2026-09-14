package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"jelto.io/jelto/contracts/spec/wirecheck"
)

// Result is one assertion's verdict.
type Result struct {
	Name   string
	Passed bool
	Detail string
}

// Evidence contains only the five conformance assertion surfaces: recordings,
// host replies/exits, stderr, exported state, and directory emptiness.
type Evidence struct {
	Scenario  Scenario
	Arm       Arm
	Records   []Record
	Requests  []Record
	Stderr    string
	Replies   []HostReply
	ExitCodes []int
	ExitMS    []int64

	// State is the last dumpstate export, retained raw for field-specific decoding.
	State json.RawMessage

	// Inspect StateDir only for emptiness; SDK storage contents are private.
	StateDir string

	// StepWallMS is the wall clock at the end of each step and
	// RequestsAfterStep the ingest requests recorded by then.
	StepWallMS        []int64
	RequestsAfterStep []int
	ArmStartWallMS    int64

	VirtualNowMS int64
	VirtualClock bool
	Stats        mockReply
	Validator    *wirecheck.Validator
}

// RunChecks applies the three that every arm gets and then the arm's own.
func RunChecks(evidence Evidence) []Result {
	results := []Result{
		checkWireValidity(evidence),  // W1
		checkClientVersion(evidence), // W4
		checkMockErrors(evidence),
	}
	for _, check := range evidence.Arm.Assert {
		results = append(results, runCheck(evidence, check))
	}
	return results
}

func pass(name, detail string) Result { return Result{Name: name, Passed: true, Detail: detail} }
func fail(name, format string, args ...any) Result {
	return Result{Name: name, Passed: false, Detail: fmt.Sprintf(format, args...)}
}

// --- the three that are never written down ----------------------------------

// checkWireValidity is W1, on every request of every scenario.
func checkWireValidity(evidence Evidence) Result {
	for i, record := range evidence.Requests {
		body, err := bodyBytes(record)
		if err != nil {
			return fail("W1", "request %d: %v", i, err)
		}
		if record.BodyTruncated {
			return fail("W1", "request %d: mockd truncated the stored body at its -max-body cap; raise it to validate", i)
		}
		if record.BodyBytes > wirecheck.MaxBodyBytes {
			return fail("W1", "request %d: body is %d bytes, spec/wire-v1.md §2 caps it at %d", i, record.BodyBytes, wirecheck.MaxBodyBytes)
		}
		contentType, ok := header(record.Headers, "Content-Type")
		if !ok || !strings.HasPrefix(contentType, "application/json") {
			return fail("W1", "request %d: Content-Type is %q, W1 requires application/json", i, contentType)
		}
		if record.Envelope == nil {
			return fail("W1", "request %d: mockd could not read the envelope: %s", i, record.EnvelopeError)
		}
		if record.Envelope.EventCount < 1 || record.Envelope.EventCount > wirecheck.MaxEvents {
			return fail("W1", "request %d: %d events, W1 allows 1-%d", i, record.Envelope.EventCount, wirecheck.MaxEvents)
		}
		if err := evidence.Validator.ValidateBody(body); err != nil {
			return fail("W1", "request %d does not validate against %s:\n%v", i, evidence.Validator.Path(), err)
		}
	}
	return pass("W1", fmt.Sprintf("%d request(s) validate against the wire schema", len(evidence.Requests)))
}

// reClientVersion is spec/wire-v1.md §3's normative `v` grammar (W4).
var reClientVersion = regexp.MustCompile(`^[a-z]+/[0-9A-Za-z.+-]{1,24}$`)

func checkClientVersion(evidence Evidence) Result {
	seen := map[string]bool{}
	for i, record := range evidence.Requests {
		if record.Envelope == nil {
			continue
		}
		for j, event := range record.Envelope.Events {
			if event.V == "" {
				seen["(absent)"] = true
				continue
			}
			if !reClientVersion.MatchString(event.V) {
				return fail("W4", "request %d event %d has v=%q; spec/wire-v1.md §3 requires ^[a-z]+/[0-9A-Za-z.+-]{1,24}$ or absent", i, j, event.V)
			}
			seen[event.V] = true
		}
	}
	return pass("W4", "v ∈ "+joinKeys(seen))
}

func checkMockErrors(evidence Evidence) Result {
	for i, record := range evidence.Records {
		if record.MockError != "" {
			return fail("mock_errors", "record %d: mockd could not honour mode %q: %s", i, record.Mode, record.MockError)
		}
	}
	return pass("mock_errors", "no mode went unhonoured")
}

// --- the catalogue -----------------------------------------------------------

func runCheck(evidence Evidence, check Check) Result {
	name := check.Check
	if check.Why != "" {
		name = check.Check + " (" + check.Why + ")"
	}
	switch check.Check {

	// C5, C7, C9, C9b, C16: how many ingest requests were made.
	case "request_count":
		return compareInt(name, len(evidence.Requests), check, "POST /v1/e requests")

	// C5: "no socket opened (runner watches mockd connections)".
	case "connection_count":
		return compareInt(name, len(Connections(evidence.Records)), check, "accepted connections")

	// C6, C7: events across every request.
	case "event_count":
		total := 0
		for _, record := range evidence.Requests {
			if record.Envelope != nil {
				total += record.Envelope.EventCount
			}
		}
		return compareInt(name, total, check, "events sent")

	// C7: "3 requests of <= 100 events".
	case "max_events_per_request":
		worst := 0
		for _, record := range evidence.Requests {
			if record.Envelope != nil && record.Envelope.EventCount > worst {
				worst = record.Envelope.EventCount
			}
		}
		return compareInt(name, worst, check, "largest batch")

	// C3, C4, C20: which events were sent. `counts` is a multiset assertion;
	// `values` is the ordered list.
	case "event_names":
		return checkEventNames(name, evidence, check)

	// C16: "one heartbeat, ALONE in its batch". request: which one.
	case "request_shape":
		return checkRequestShape(name, evidence, check)

	// C8, C8b, C7, C22: the schedule, measured on mockd's monotonic clock.
	case "schedule":
		return checkSchedule(name, evidence, check)

	// C8b: "no request before 20 s".
	case "no_request_before_s":
		return checkNoRequestBefore(name, evidence, check)

	// C8: "Every retry resends the same batch with the same `id`s".
	case "stable_ids":
		return checkStableIDs(name, evidence, check)

	case "app_updates", "update_activity":
		return checkAppUpdates(name, evidence, check)

	// C8b, C9, C9b: what the client was told, without re-deriving it.
	case "statuses":
		return checkStatuses(name, evidence, check)

	// C9b, C10, C17, C20b, C22c, W2, W3.
	case "stderr":
		return checkText(name, evidence.Stderr, check)

	// C1, C2, C11: what the host printed.
	case "host_reply":
		return checkHostReply(name, evidence, check)

	// C5/C18 require a filesystem check: exported state cannot prove no files remain.
	case "state_dir_empty":
		return checkStateDirEmpty(name, evidence)

	case "state":
		return checkStateJSON(name, evidence, check)

	// C6, C4b, C18: §3.2's `queue`.
	case "queue":
		return checkQueueFile(name, evidence, check)

	// C15, C15b: the exact decimal an SDK put in `t`.
	case "t_literal":
		return checkTLiteral(name, evidence, check)

	// C21: a surface field, present or absent.
	case "event_field":
		return checkEventField(name, evidence, check)

	// C20, C22: an event's props.
	case "event_prop":
		return checkEventProp(name, evidence, check)

	// C10: "host exit code 0".
	case "exit_code":
		return checkExitCode(name, evidence, check)

	// C1: "process exits within 1 s", measured from the `exit` command.
	case "exit_within_ms":
		worst := int64(0)
		for _, elapsed := range evidence.ExitMS {
			if elapsed > worst {
				worst = elapsed
			}
		}
		return compareInt(name, int(worst), check, "slowest exit, ms")

	// C2: "same UUIDv4 both times". Every reply to this command must carry the
	// same non-empty value.
	case "host_reply_stable":
		return checkHostReplyStable(name, evidence, check)

	// C16: "after the stop response no request for 60 s" -- 60 s of the HOST's
	// clock, which under JELTO_NOW is not mockd's. The observable form is
	// "how many requests had been made by the end of step j".
	case "request_count_after_step":
		return checkRequestCountAfterStep(name, evidence, check)

	// C7, C22: "the first batch arrives 2 s +- 0.5 s after init". The anchor is
	// the runner's own wall clock at the end of a step; mockd's at_unix_ms is
	// the same clock.
	case "request_after_step":
		return checkRequestAfterStep(name, evidence, check)

	// C4/C4c/C8c inspect scheduled deadlines without waiting for long delays to elapse.
	case "state_deadline":
		return checkStateDeadline(name, evidence, check)

	// C17: "stderr contains the exact JSON body before it is sent". The body is
	// mockd's recording of it, so this compares the two channels rather than
	// trusting either.
	case "stderr_contains_body":
		return checkStderrContainsBody(name, evidence, check)

	// C20: "byte-identical to what track(...) would send". `id` and `t` cannot
	// be identical between two calls, so this compares every other field and
	// the whole props object -- see the report on what C20 can actually mean.
	case "events_congruent":
		return checkEventsCongruent(name, evidence, check)
	}
	return fail(name, "unknown check %q; the catalogue is in spec/conformance/runner/checks.go", check.Check)
}

// Count logical transitions while checking immutable payloads on every retry.
func checkAppUpdates(name string, evidence Evidence, check Check) Result {
	activity := check.Check == "update_activity"
	eventName := "app_updated"
	if activity {
		eventName = "app_update"
	}
	seen := map[string]string{}
	var transitions []string
	installID := ""
	for i, record := range evidence.Requests {
		body, err := bodyBytes(record)
		if err != nil {
			return fail(name, "request %d: %v", i, err)
		}
		var envelope struct {
			Events []map[string]json.RawMessage `json:"e"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fail(name, "request %d: %v", i, err)
		}
		for _, event := range envelope.Events {
			str := func(key string) string { var value string; _ = json.Unmarshal(event[key], &value); return value }
			if installID == "" {
				installID = str("iid")
			}
			if str("n") != eventName {
				continue
			}
			var props map[string]string
			if err := json.Unmarshal(event["props"], &props); err != nil || (!activity && len(props) != 2) || props["from_version"] == "" || props["to_version"] == "" || props["from_version"] == props["to_version"] {
				return fail(name, "update telemetry must have distinct from_version/to_version string properties")
			}
			if !activity && str("av") != props["to_version"] {
				return fail(name, "av=%q differs from to_version=%q", str("av"), props["to_version"])
			}
			if str("iid") != installID {
				return fail(name, "transition rotated iid from %q to %q", installID, str("iid"))
			}
			id := str("id")
			if id == "" {
				return fail(name, "transition has no stable id")
			}
			canonical, _ := json.Marshal(event)
			if before, exists := seen[id]; exists {
				if before != string(canonical) {
					return fail(name, "retry changed metadata for id %s", id)
				}
				continue
			}
			seen[id] = string(canonical)
			if activity {
				transitions = append(transitions, props["status"])
			} else {
				transitions = append(transitions, props["from_version"]+"=>"+props["to_version"])
			}
		}
	}
	if len(transitions) != len(check.Values) {
		return fail(name, "transitions %q, want %q", transitions, check.Values)
	}
	for i := range transitions {
		if transitions[i] != check.Values[i] {
			return fail(name, "transitions %q, want %q", transitions, check.Values)
		}
	}
	return pass(name, fmt.Sprintf("%d transitions with stable IDs, metadata and install identity", len(transitions)))
}

func compareInt(name string, got int, check Check, what string) Result {
	switch {
	case check.Equal != nil && got != *check.Equal:
		return fail(name, "%s: got %d, want %d", what, got, *check.Equal)
	case check.Min != nil && got < *check.Min:
		return fail(name, "%s: got %d, want at least %d", what, got, *check.Min)
	case check.Max != nil && got > *check.Max:
		return fail(name, "%s: got %d, want at most %d", what, got, *check.Max)
	}
	return pass(name, fmt.Sprintf("%s = %d", what, got))
}

func eventNames(records []Record) []string {
	var names []string
	for _, record := range records {
		if record.Envelope == nil {
			continue
		}
		for _, event := range record.Envelope.Events {
			names = append(names, event.N)
		}
	}
	return names
}

func checkEventNames(name string, evidence Evidence, check Check) Result {
	names := eventNames(evidence.Requests)
	if len(check.Values) > 0 {
		if strings.Join(names, ",") != strings.Join(check.Values, ",") {
			return fail(name, "events sent were [%s], want [%s]", strings.Join(names, ", "), strings.Join(check.Values, ", "))
		}
	}
	if check.Counts != nil {
		counts := map[string]int{}
		for _, value := range names {
			counts[value]++
		}
		for want, n := range check.Counts {
			if counts[want] != n {
				return fail(name, "%q appears %d time(s), want %d (all events: [%s])", want, counts[want], n, strings.Join(names, ", "))
			}
		}
	}
	if len(check.NotContains) > 0 {
		for _, banned := range check.NotContains {
			for _, value := range names {
				if value == banned {
					return fail(name, "%q was sent and must not be (all events: [%s])", banned, strings.Join(names, ", "))
				}
			}
		}
	}
	return pass(name, "["+strings.Join(names, ", ")+"]")
}

func checkRequestShape(name string, evidence Evidence, check Check) Result {
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index < 0 {
		index += len(evidence.Requests)
	}
	if index < 0 || index >= len(evidence.Requests) {
		return fail(name, "request %d does not exist (%d requests were made)", index, len(evidence.Requests))
	}
	record := evidence.Requests[index]
	if record.Envelope == nil {
		return fail(name, "request %d has no readable envelope: %s", index, record.EnvelopeError)
	}
	if check.Equal != nil && record.Envelope.EventCount != *check.Equal {
		return fail(name, "request %d carries %d event(s), want %d", index, record.Envelope.EventCount, *check.Equal)
	}
	if check.Value != "" {
		if len(record.Envelope.Events) == 0 || record.Envelope.Events[0].N != check.Value {
			got := "(none)"
			if len(record.Envelope.Events) > 0 {
				got = record.Envelope.Events[0].N
			}
			return fail(name, "request %d's first event is %q, want %q", index, got, check.Value)
		}
	}
	return pass(name, fmt.Sprintf("request %d: %d event(s), first %q", index, record.Envelope.EventCount, firstName(record)))
}

func firstName(record Record) string {
	if record.Envelope == nil || len(record.Envelope.Events) == 0 {
		return ""
	}
	return record.Envelope.Events[0].N
}

// Measure response-completion to next-arrival using monotonic times so slow
// responses and wall-clock corrections do not alter the observed retry delay.
func checkSchedule(name string, evidence Evidence, check Check) Result {
	requests := evidence.Requests
	if len(requests) < len(check.ExpectS)+1 {
		return fail(name, "want %d gaps, so %d requests; got %d", len(check.ExpectS), len(check.ExpectS)+1, len(requests))
	}
	var detail []string
	for i, wantSeconds := range check.ExpectS {
		previous, current := requests[i], requests[i+1]
		anchor := previous.RespondedUS
		if check.Anchor == "arrival" || anchor == 0 {
			anchor = previous.SinceStartUS
		}
		gotMS := float64(current.SinceStartUS-anchor) / 1000
		wantMS := wantSeconds * 1000
		tolerance := wantMS*check.TolerancePct/100 + float64(check.SlackMS)
		detail = append(detail, fmt.Sprintf("%d->%d %.0f ms (want %.0f ±%.0f)", i, i+1, gotMS, wantMS, tolerance))
		if gotMS < wantMS-tolerance || gotMS > wantMS+tolerance {
			return fail(name, "gap %d->%d was %.0f ms, want %.0f ms ±%.0f ms\n      all gaps: %s", i, i+1, gotMS, wantMS, tolerance, strings.Join(detail, "; "))
		}
	}
	return pass(name, strings.Join(detail, "; "))
}

func checkNoRequestBefore(name string, evidence Evidence, check Check) Result {
	requests := evidence.Requests
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index >= len(requests) {
		return fail(name, "request %d does not exist (%d requests were made)", index, len(requests))
	}
	if index == 0 {
		return fail(name, "no_request_before_s needs an anchor request; `request` must be >= 1")
	}
	anchor := requests[index-1].RespondedUS
	if anchor == 0 {
		anchor = requests[index-1].SinceStartUS
	}
	gotMS := float64(requests[index].SinceStartUS-anchor) / 1000
	wantMS := check.Seconds * 1000
	if gotMS < wantMS {
		return fail(name, "request %d arrived %.0f ms after request %d was answered; the floor is %.0f ms", index, gotMS, index-1, wantMS)
	}
	return pass(name, fmt.Sprintf("request %d came %.0f ms later, floor %.0f ms", index, gotMS, wantMS))
}

// Retries preserve the previously sent ID prefix (wire §6). New events may be
// appended: the contract persists the queue and backoff, not a frozen batch boundary.
// request selects the first compared batch; equal additionally forbids batch growth.
func checkStableIDs(name string, evidence Evidence, check Check) Result {
	first := 0
	if check.Request != nil {
		first = *check.Request
	}
	if first < 0 {
		first += len(evidence.Requests)
	}
	if first < 0 || first >= len(evidence.Requests) {
		return fail(name, "request %d does not exist (%d requests were made)", first, len(evidence.Requests))
	}
	scoped := evidence.Requests[first:]

	lists := make([][]string, 0, len(scoped))
	for offset, record := range scoped {
		i := first + offset
		if record.Envelope == nil {
			return fail(name, "request %d has no readable envelope", i)
		}
		var ids []string
		for _, event := range record.Envelope.Events {
			if event.ID == "" {
				return fail(name, "request %d carries an event with no `id`; spec/wire-v1.md §6's rule about resending the same ids cannot be checked", i)
			}
			ids = append(ids, event.ID)
		}
		if check.Equal != nil && len(ids) != *check.Equal {
			return fail(name, "request %d carries %d event(s), want %d", i, len(ids), *check.Equal)
		}
		lists = append(lists, ids)
	}

	for offset := 1; offset < len(lists); offset++ {
		previous, current := lists[offset-1], lists[offset]
		i := first + offset
		if len(current) < len(previous) {
			return fail(name, "request %d dropped %d event(s) it had already offered -- the batch shrank from %d to %d\n      before: %s\n      after:  %s",
				i, len(previous)-len(current), len(previous), len(current), strings.Join(previous, ", "), strings.Join(current, ", "))
		}
		for at := range previous {
			if previous[at] != current[at] {
				return fail(name, "request %d ALTERED the id at position %d -- spec/wire-v1.md §6 forbids it\n      before: %s\n      after:  %s",
					i, at, strings.Join(previous, ", "), strings.Join(current, ", "))
			}
		}
	}

	grew := len(lists[len(lists)-1]) - len(lists[0])
	detail := fmt.Sprintf("%d request(s) carried the same %d id(s), unaltered and in order", len(lists), len(lists[0]))
	if grew > 0 {
		detail += fmt.Sprintf("; %d newly queued event(s) were appended behind them, which §6 permits", grew)
	}
	return pass(name, detail)
}

func checkStatuses(name string, evidence Evidence, check Check) Result {
	var got []string
	for _, record := range evidence.Requests {
		if record.Response == nil {
			got = append(got, "(none)")
			continue
		}
		if record.Response.Hangup {
			got = append(got, "hangup")
			continue
		}
		got = append(got, strconv.Itoa(record.Response.Status))
	}
	if len(check.Values) > 0 && strings.Join(got, ",") != strings.Join(check.Values, ",") {
		return fail(name, "statuses were [%s], want [%s]", strings.Join(got, ", "), strings.Join(check.Values, ", "))
	}
	if check.Value != "" {
		for i, status := range got {
			if status != check.Value {
				return fail(name, "request %d was answered %s, want every request answered %s (all: [%s])", i, status, check.Value, strings.Join(got, ", "))
			}
		}
	}
	return pass(name, "["+strings.Join(got, ", ")+"]")
}

func checkText(name, text string, check Check) Result {
	if check.Empty {
		if strings.TrimSpace(text) != "" {
			return fail(name, "want empty, got %d bytes:\n      %s", len(text), truncate(text, 400))
		}
		return pass(name, "empty")
	}
	for _, want := range check.Contains {
		if !strings.Contains(text, want) {
			return fail(name, "does not contain %q\n      got: %s", want, truncate(text, 800))
		}
	}
	for _, banned := range check.NotContains {
		if strings.Contains(text, banned) {
			return fail(name, "contains %q and must not", banned)
		}
	}
	if check.Regex != "" {
		expression, err := regexp.Compile(check.Regex)
		if err != nil {
			return fail(name, "bad regex %q: %v", check.Regex, err)
		}
		if !expression.MatchString(text) {
			return fail(name, "does not match %q\n      got: %s", check.Regex, truncate(text, 800))
		}
	}
	return pass(name, fmt.Sprintf("%d bytes matched", len(text)))
}

func checkHostReply(name string, evidence Evidence, check Check) Result {
	var matching []HostReply
	for _, reply := range evidence.Replies {
		if check.Key == "" || reply.Cmd == check.Key {
			matching = append(matching, reply)
		}
	}
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index < 0 {
		index += len(matching)
	}
	if index < 0 || index >= len(matching) {
		return fail(name, "no reply %d for command %q (%d seen)", index, check.Key, len(matching))
	}
	reply := matching[index]
	switch check.Field {
	case "", "value":
		if check.Empty {
			if reply.Value != "" {
				return fail(name, "%s[%d].value is %q, want empty", check.Key, index, reply.Value)
			}
			return pass(name, "empty")
		}
		if check.Value != "" && reply.Value != check.Value {
			return fail(name, "%s[%d].value is %q, want %q", check.Key, index, reply.Value, check.Value)
		}
		if check.Regex != "" {
			expression, err := regexp.Compile(check.Regex)
			if err != nil {
				return fail(name, "bad regex %q: %v", check.Regex, err)
			}
			if !expression.MatchString(reply.Value) {
				return fail(name, "%s[%d].value is %q, want to match %q", check.Key, index, reply.Value, check.Regex)
			}
		}
		return pass(name, fmt.Sprintf("%s[%d].value = %q", check.Key, index, reply.Value))
	case "us":
		return compareInt(name, int(reply.Micros), check, fmt.Sprintf("%s[%d] microseconds", check.Key, index))
	case "track_p99_us":
		return compareInt(name, int(reply.TrackP99US), check, "track p99 microseconds")
	case "ok":
		if !reply.OK {
			return fail(name, "%s[%d] failed: %s", check.Key, index, reply.Error)
		}
		return pass(name, "ok")
	}
	return fail(name, "host_reply has no field %q", check.Field)
}

func checkHostReplyStable(name string, evidence Evidence, check Check) Result {
	var values []string
	for _, reply := range evidence.Replies {
		if reply.Cmd == check.Key {
			values = append(values, reply.Value)
		}
	}
	if len(values) < 2 {
		return fail(name, "%q was asked %d time(s); stability needs at least two", check.Key, len(values))
	}
	for i, value := range values {
		if value == "" {
			return fail(name, "%s[%d] printed an empty value", check.Key, i)
		}
		if value != values[0] {
			return fail(name, "%s[%d] printed %q, the first printed %q", check.Key, i, value, values[0])
		}
	}
	return pass(name, fmt.Sprintf("%d identical values: %q", len(values), values[0]))
}

func checkRequestCountAfterStep(name string, evidence Evidence, check Check) Result {
	if check.Step == nil {
		return fail(name, "request_count_after_step needs `step`")
	}
	index := *check.Step
	if index < 0 {
		index += len(evidence.RequestsAfterStep)
	}
	if index < 0 || index >= len(evidence.RequestsAfterStep) {
		return fail(name, "step %d does not exist (%d steps ran)", index, len(evidence.RequestsAfterStep))
	}
	return compareInt(name, evidence.RequestsAfterStep[index], check, fmt.Sprintf("requests by the end of step %d", index))
}

func checkRequestAfterStep(name string, evidence Evidence, check Check) Result {
	if check.Step == nil {
		return fail(name, "request_after_step needs `step`")
	}
	step := *check.Step
	if step < 0 {
		step += len(evidence.StepWallMS)
	}
	if step < 0 || step >= len(evidence.StepWallMS) {
		return fail(name, "step %d does not exist (%d steps ran)", step, len(evidence.StepWallMS))
	}
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index < 0 {
		index += len(evidence.Requests)
	}
	if index < 0 || index >= len(evidence.Requests) {
		return fail(name, "request %d does not exist (%d requests were made)", index, len(evidence.Requests))
	}
	gotMS := float64(evidence.Requests[index].AtUnixMS - evidence.StepWallMS[step])
	wantMS := check.Seconds * 1000
	tolerance := wantMS*check.TolerancePct/100 + float64(check.SlackMS)
	if gotMS < wantMS-tolerance || gotMS > wantMS+tolerance {
		return fail(name, "request %d arrived %.0f ms after step %d, want %.0f ms ±%.0f ms", index, gotMS, step, wantMS, tolerance)
	}
	return pass(name, fmt.Sprintf("request %d arrived %.0f ms after step %d (want %.0f ±%.0f)", index, gotMS, step, wantMS, tolerance))
}

// checkStateDeadline compares exact exported instants with one of three anchors:
//
//	last_request  wall clock of the last recorded request
//	virtual_now   JELTO_NOW plus elapsed virtual sleeps
//	jelto_now     the arm's initial JELTO_NOW, before restarts
//
// Use big.Int to preserve C15b values beyond int64.
func checkStateDeadline(name string, evidence Evidence, check Check) Result {
	state, err := exportedState(evidence)
	if err != nil {
		return fail(name, "%v", err)
	}
	value, present := state[check.Path]
	if !present {
		return fail(name, "the export has no %q (keys: %s)", check.Path, strings.Join(sortedKeys(state), ", "))
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		text = strings.TrimSpace(string(value))
	}
	deadline, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return fail(name, "the export's %q = %s is not a whole decimal of milliseconds; §3.2 makes every instant a decimal STRING", check.Path, value)
	}

	var anchor *big.Int
	switch check.Anchor {
	case "", "last_request":
		if len(evidence.Requests) == 0 {
			return fail(name, "anchor last_request, but no request was made")
		}
		anchor = big.NewInt(evidence.Requests[len(evidence.Requests)-1].AtUnixMS)
	case "virtual_now":
		if !evidence.VirtualClock {
			return fail(name, "anchor virtual_now needs a JELTO_NOW that fits int64")
		}
		anchor = big.NewInt(evidence.VirtualNowMS)
	case "jelto_now":
		pin, ok := new(big.Int).SetString(evidence.Arm.Env["JELTO_NOW"], 10)
		if !ok {
			return fail(name, "anchor jelto_now, but this arm has no JELTO_NOW")
		}
		anchor = pin
	default:
		return fail(name, "unknown anchor %q; use last_request, virtual_now or jelto_now", check.Anchor)
	}

	gapMS := new(big.Int).Sub(deadline, anchor)
	if !gapMS.IsInt64() {
		return fail(name, "the export's %q is %s ms from the anchor, which does not fit int64", check.Path, gapMS)
	}
	gotS := float64(gapMS.Int64()) / 1000
	switch {
	case check.Min != nil || check.Max != nil:
		if check.Min != nil && gotS < float64(*check.Min) {
			return fail(name, "%s is %.1f s from the anchor, want at least %d s", check.Path, gotS, *check.Min)
		}
		if check.Max != nil && gotS > float64(*check.Max) {
			return fail(name, "%s is %.1f s from the anchor, want at most %d s", check.Path, gotS, *check.Max)
		}
	default:
		tolerance := check.Seconds*check.TolerancePct/100 + float64(check.SlackMS)/1000
		if gotS < check.Seconds-tolerance || gotS > check.Seconds+tolerance {
			return fail(name, "%s is %.1f s from the anchor, want %.1f s ±%.1f s", check.Path, gotS, check.Seconds, tolerance)
		}
	}
	return pass(name, fmt.Sprintf("%s is %.1f s from %s", check.Path, gotS, orDefault(check.Anchor, "last_request")))
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func checkStderrContainsBody(name string, evidence Evidence, check Check) Result {
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index < 0 {
		index += len(evidence.Requests)
	}
	if index < 0 || index >= len(evidence.Requests) {
		return fail(name, "request %d does not exist (%d requests were made)", index, len(evidence.Requests))
	}
	body, err := bodyBytes(evidence.Requests[index])
	if err != nil {
		return fail(name, "%v", err)
	}
	if !strings.Contains(evidence.Stderr, string(body)) {
		return fail(name, "stderr does not carry the exact body mockd received\n      body:   %s\n      stderr: %s", truncate(string(body), 400), truncate(evidence.Stderr, 400))
	}
	return pass(name, fmt.Sprintf("stderr carries request %d's %d-byte body verbatim", index, len(body)))
}

// checkEventsCongruent compares two events of one request field by field,
// ignoring `id` and `t`.
func checkEventsCongruent(name string, evidence Evidence, check Check) Result {
	record, _, err := pickRequestEvent(evidence, check)
	if err != nil {
		return fail(name, "%v", err)
	}
	if len(check.Values) != 2 {
		return fail(name, "events_congruent needs `values: [i, j]`")
	}
	body, err := bodyBytes(record)
	if err != nil {
		return fail(name, "%v", err)
	}
	var envelope struct {
		E []map[string]json.RawMessage `json:"e"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fail(name, "body is not an envelope: %v", err)
	}
	left, err := strconv.Atoi(check.Values[0])
	if err != nil {
		return fail(name, "values[0]: %v", err)
	}
	right, err := strconv.Atoi(check.Values[1])
	if err != nil {
		return fail(name, "values[1]: %v", err)
	}
	if left >= len(envelope.E) || right >= len(envelope.E) {
		return fail(name, "the request carries %d events; %d and %d were asked for", len(envelope.E), left, right)
	}
	ignore := map[string]bool{"id": true, "t": true}
	for _, key := range union(envelope.E[left], envelope.E[right]) {
		if ignore[key] {
			continue
		}
		a, b := string(envelope.E[left][key]), string(envelope.E[right][key])
		if a != b {
			return fail(name, "events %d and %d differ on %q: %s vs %s", left, right, key, a, b)
		}
	}
	return pass(name, fmt.Sprintf("events %d and %d are identical but for id and t", left, right))
}

func union(a, b map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	for key := range a {
		seen[key] = true
	}
	for key := range b {
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func checkStateDirEmpty(name string, evidence Evidence) Result {
	entries, err := os.ReadDir(evidence.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return pass(name, "the state directory was never created")
		}
		return fail(name, "read %s: %v", evidence.StateDir, err)
	}
	if len(entries) > 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return fail(name, "state directory holds %s", strings.Join(names, ", "))
	}
	return pass(name, "empty")
}

// exportedState reads the semantic dumpstate reply, independent of SDK storage.
// A missing export fails even an absent/empty assertion; silence does not prove empty state.
func exportedState(evidence Evidence) (map[string]json.RawMessage, error) {
	if len(evidence.State) == 0 {
		return nil, fmt.Errorf("the host exported no state: `dumpstate` (spec/sdk-conformance.md §3.2) answered nothing this arm")
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(evidence.State, &state); err != nil {
		return nil, fmt.Errorf("`dumpstate` did not print a JSON object: %v", err)
	}
	return state, nil
}

// checkStateJSON reads one fact out of §3.2's export: C2's install_id, C4's and
// C4b's install_claimed, C8c's and C9b's backoff_step_ms.
func checkStateJSON(name string, evidence Evidence, check Check) Result {
	state, err := exportedState(evidence)
	if err != nil {
		return fail(name, "%v", err)
	}
	value, present := state[check.Path]
	if check.Absent {
		if present && strings.TrimSpace(string(value)) != "null" {
			return fail(name, "the export carries %q = %s and must not", check.Path, value)
		}
		return pass(name, fmt.Sprintf("the export carries no %q", check.Path))
	}
	if check.Empty {
		// §3.2: "a fact that is not set is absent or empty", and for
		// backoff_step_ms specifically "0 or absent means NOT IN BACKOFF".
		if present && !isEmptyExported(value) {
			return fail(name, "the export's %q is %s, want absent or empty", check.Path, value)
		}
		return pass(name, fmt.Sprintf("the export's %q is absent or empty", check.Path))
	}
	if !present {
		return fail(name, "the export has no %q (keys: %s)", check.Path, strings.Join(sortedKeys(state), ", "))
	}
	if check.Value != "" && strings.TrimSpace(string(value)) != check.Value {
		// Strings are semantic facts, not a choice of JSON escaping. Leave numeric
		// literals exact: arbitrary-precision instant checks must never use float64.
		var actual, expected string
		if json.Unmarshal(value, &actual) != nil || json.Unmarshal([]byte(check.Value), &expected) != nil || actual != expected {
			return fail(name, "the export's %q is %s, want %s", check.Path, value, check.Value)
		}
	}
	if check.Regex != "" {
		expression, err := regexp.Compile(check.Regex)
		if err != nil {
			return fail(name, "bad regex %q: %v", check.Regex, err)
		}
		if !expression.MatchString(strings.Trim(string(value), `"`)) {
			return fail(name, "the export's %q is %s, want to match %q", check.Path, value, check.Regex)
		}
	}
	return pass(name, fmt.Sprintf("state[%q] = %s", check.Path, value))
}

// isEmptyExported is §3.2's "absent or empty" for a value that IS present.
// `0` counts because §3.2 says so for backoff_step_ms, which is the only row
// that uses `empty:` today (C9b).
func isEmptyExported(value json.RawMessage) bool {
	switch strings.TrimSpace(string(value)) {
	case `""`, "null", "0", "false", "{}", "[]":
		return true
	}
	return false
}

// exportedQueue reports held events in order and their live encoded byte count.
// Instants remain decimal strings; derive event count from the list.
type exportedQueue struct {
	Bytes  int `json:"bytes"`
	Events []struct {
		ID string `json:"id"`
		N  string `json:"n"`
		T  string `json:"t"`
	} `json:"events"`
}

// checkQueueFile applies C6 to exported queue contents and live encoded bytes.
// Filesystem size is not comparable across JSON, plist, and SQLite storage.
func checkQueueFile(name string, evidence Evidence, check Check) Result {
	state, err := exportedState(evidence)
	if err != nil {
		return fail(name, "%v", err)
	}
	var queue exportedQueue
	if raw, present := state["queue"]; present {
		if err := json.Unmarshal(raw, &queue); err != nil {
			return fail(name, "the export's `queue` is not {bytes, events}: %v", err)
		}
	}
	names := map[string]bool{}
	counts := map[string]int{}
	var held []string
	for _, event := range queue.Events {
		names[event.N] = true
		counts[event.N]++
		held = append(held, event.N)
	}
	if check.Absent {
		if len(queue.Events) > 0 {
			return fail(name, "the SDK is still holding %d event(s) (%s) and must not", len(queue.Events), strings.Join(held, ", "))
		}
		return pass(name, "the SDK is holding nothing")
	}
	if check.Max != nil && len(queue.Events) > *check.Max {
		return fail(name, "the queue holds %d events, cap is %d", len(queue.Events), *check.Max)
	}
	if check.Value != "" {
		limit, err := strconv.ParseInt(check.Value, 10, 64)
		if err != nil {
			return fail(name, "`value` must be the byte cap: %v", err)
		}
		if int64(queue.Bytes) > limit {
			return fail(name, "the SDK counts its queue at %d bytes, cap is %d", queue.Bytes, limit)
		}
	}
	for want, n := range check.Counts {
		if counts[want] != n {
			return fail(name, "the queue holds %d %q, want %d", counts[want], want, n)
		}
	}
	for _, want := range check.Contains {
		if !names[want] {
			return fail(name, "the queue does not hold a %q (it holds %s)", want, joinKeys(names))
		}
	}
	for _, banned := range check.NotContains {
		if names[banned] {
			return fail(name, "the queue still holds a %q and must not", banned)
		}
	}
	return pass(name, fmt.Sprintf("%d events, %d bytes as the SDK counts them, names %s", len(queue.Events), queue.Bytes, joinKeys(names)))
}

// checkTLiteral asserts on the exact decimal `t` carried. The comparison is
// arbitrary precision, because C15b sends a value int64 cannot hold and a
// float64 comparison would silently round both sides into agreement.
func checkTLiteral(name string, evidence Evidence, check Check) Result {
	event, err := pickEvent(evidence, check)
	if err != nil {
		return fail(name, "%v", err)
	}
	literal := strings.TrimSpace(string(event.T))
	if literal == "" {
		return fail(name, "the event carries no `t`")
	}
	if check.Value != "" && literal != check.Value {
		return fail(name, "t is %s, want the literal %s", literal, check.Value)
	}
	if check.Regex != "" {
		expression, err := regexp.Compile(check.Regex)
		if err != nil {
			return fail(name, "bad regex %q: %v", check.Regex, err)
		}
		if !expression.MatchString(literal) {
			return fail(name, "t is %s, want to match %q", literal, check.Regex)
		}
	}
	if len(check.Values) == 2 {
		value, ok := new(big.Int).SetString(literal, 10)
		if !ok {
			return fail(name, "t is %s, which is not a whole decimal; the range check cannot be made", literal)
		}
		low, lowOK := new(big.Int).SetString(check.Values[0], 10)
		high, highOK := new(big.Int).SetString(check.Values[1], 10)
		if !lowOK || !highOK {
			return fail(name, "`values` must be two whole decimals, got %v", check.Values)
		}
		if value.Cmp(low) < 0 || value.Cmp(high) > 0 {
			return fail(name, "t is %s, want it in [%s, %s]", literal, check.Values[0], check.Values[1])
		}
	}
	return pass(name, "t = "+literal)
}

func checkEventField(name string, evidence Evidence, check Check) Result {
	record, index, err := pickRequestEvent(evidence, check)
	if err != nil {
		return fail(name, "%v", err)
	}
	body, err := bodyBytes(record)
	if err != nil {
		return fail(name, "%v", err)
	}
	var envelope struct {
		E []map[string]json.RawMessage `json:"e"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fail(name, "body is not an envelope: %v", err)
	}
	if index >= len(envelope.E) {
		return fail(name, "event %d does not exist in that request", index)
	}
	value, present := envelope.E[index][check.Field]
	if check.Absent {
		if present {
			return fail(name, "field %q is present (%s) and must be absent", check.Field, value)
		}
		return pass(name, fmt.Sprintf("%q absent", check.Field))
	}
	if !present {
		return fail(name, "field %q is absent", check.Field)
	}
	if check.Value != "" {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			text = string(value)
		}
		if text != check.Value {
			return fail(name, "field %q is %q, want %q", check.Field, text, check.Value)
		}
	}
	return pass(name, fmt.Sprintf("%s = %s", check.Field, value))
}

func checkEventProp(name string, evidence Evidence, check Check) Result {
	record, index, err := pickRequestEvent(evidence, check)
	if err != nil {
		return fail(name, "%v", err)
	}
	body, err := bodyBytes(record)
	if err != nil {
		return fail(name, "%v", err)
	}
	var envelope struct {
		E []struct {
			N     string                     `json:"n"`
			Props map[string]json.RawMessage `json:"props"`
		} `json:"e"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fail(name, "body is not an envelope: %v", err)
	}
	if index >= len(envelope.E) {
		return fail(name, "event %d does not exist in that request", index)
	}
	value, present := envelope.E[index].Props[check.Key]
	if check.Absent {
		if present {
			return fail(name, "event %d (%s) has props.%s = %s and must not", index, envelope.E[index].N, check.Key, value)
		}
		return pass(name, fmt.Sprintf("props.%s absent", check.Key))
	}
	if !present {
		return fail(name, "event %d (%s) has no props.%s (props: %v)", index, envelope.E[index].N, check.Key, envelope.E[index].Props)
	}
	if check.Value != "" {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			text = string(value)
		}
		if text != check.Value {
			return fail(name, "event %d props.%s is %q, want %q", index, check.Key, text, check.Value)
		}
	}
	return pass(name, fmt.Sprintf("event %d props.%s = %s", index, check.Key, value))
}

func checkExitCode(name string, evidence Evidence, check Check) Result {
	for i, code := range evidence.ExitCodes {
		want := 0
		if check.Equal != nil {
			want = *check.Equal
		}
		if code != want {
			return fail(name, "host process %d exited %d, want %d", i, code, want)
		}
	}
	return pass(name, fmt.Sprintf("%d process(es) exited as expected", len(evidence.ExitCodes)))
}

// --- helpers ------------------------------------------------------------------

func pickRequestEvent(evidence Evidence, check Check) (Record, int, error) {
	index := 0
	if check.Request != nil {
		index = *check.Request
	}
	if index < 0 {
		index += len(evidence.Requests)
	}
	if index < 0 || index >= len(evidence.Requests) {
		return Record{}, 0, fmt.Errorf("request %d does not exist (%d requests were made)", index, len(evidence.Requests))
	}
	event := 0
	if check.Event != nil {
		event = *check.Event
	}
	return evidence.Requests[index], event, nil
}

func pickEvent(evidence Evidence, check Check) (Event, error) {
	record, index, err := pickRequestEvent(evidence, check)
	if err != nil {
		return Event{}, err
	}
	if record.Envelope == nil {
		return Event{}, fmt.Errorf("the request has no readable envelope: %s", record.EnvelopeError)
	}
	if index < 0 {
		index += len(record.Envelope.Events)
	}
	if index < 0 || index >= len(record.Envelope.Events) {
		return Event{}, fmt.Errorf("event %d does not exist (the request carried %d)", index, len(record.Envelope.Events))
	}
	return record.Envelope.Events[index], nil
}

func bodyBytes(record Record) ([]byte, error) {
	if record.BodyBase64 != "" {
		return base64.StdEncoding.DecodeString(record.BodyBase64)
	}
	return []byte(record.Body), nil
}

func joinKeys(values map[string]bool) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, ", ") + "}"
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
