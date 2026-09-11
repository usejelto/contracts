package main

import (
	"strings"
	"testing"
	"time"
)

// The mode grammar is a contract: spec/sdk-conformance.md §2's table is what an
// SDK author reads, and a mock that answers something else teaches the wrong
// thing. These are the parse-level assertions; mockd_test.go asserts the same
// rows again over real HTTP, because a status line and a header are exactly
// where an in-process call and a socket differ.

func TestParseModeAcceptsEverySpecRow(t *testing.T) {
	stopAt := time.Unix(1_700_000_000, 0)
	for _, testCase := range []struct {
		mode       string
		status     int
		body       string
		retryAfter string
		hasRetry   bool
	}{
		// spec/sdk-conformance.md §2, in the order the table gives them. The
		// bodies are spec/wire-v1.md §2a's, which is the authority where the
		// two disagree -- see doc.go on `ok`.
		{mode: "ok", status: 202, body: `{}`},
		{mode: "reject:stopped", status: 202, body: `{"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"},{"i":2,"reason":"stopped"}]}`},
		{mode: "429", status: 429, body: `{}`, retryAfter: "2", hasRetry: true},
		{mode: "503", status: 503, body: `{}`, retryAfter: "2", hasRetry: true},
		{mode: "429:9999", status: 429, body: `{}`, retryAfter: "9999", hasRetry: true},
		{mode: "503:1", status: 503, body: `{}`, retryAfter: "1", hasRetry: true},
		// The four edges of wire §9 rev 0.17 that a fixed `2` cannot reach.
		{mode: "429:", status: 429, body: `{}`, hasRetry: false},
		{mode: "503:", status: 503, body: `{}`, hasRetry: false},
		{mode: "429:soon", status: 429, body: `{}`, retryAfter: "soon", hasRetry: true},
		{mode: "429:Wed, 21 Oct 2015 07:28:00 GMT", status: 429, body: `{}`, retryAfter: "Wed, 21 Oct 2015 07:28:00 GMT", hasRetry: true},
		{mode: "400", status: 400, body: `{"error":"malformed"}`},
		{mode: "402", status: 402, body: `{"error":"payment_required"}`},
		{mode: "stop:60", status: 202, body: `{"stop":{"until":1700000060,"scope":"app"}}`},
		{mode: "slow:250", status: 202, body: `{}`},
		// Extensions; doc.go names the scenario each one exists for.
		{mode: "stop:60:web", status: 202, body: `{"stop":{"until":1700000060,"scope":"web"}}`},
		{mode: "reject:invalid_field:v", status: 202, body: `{"rejected":[{"i":0,"reason":"invalid_field","field":"v"},{"i":1,"reason":"invalid_field","field":"v"},{"i":2,"reason":"invalid_field","field":"v"}]}`},
		{mode: "500", status: 500, body: `{"error":"internal"}`},
		{mode: "413", status: 413, body: `{"error":"too_large"}`},
		{mode: "405", status: 405, body: ``},
		{mode: "204", status: 204, body: ``},
		{mode: "418", status: 418, body: `{}`},
		{mode: "400:too_many_events", status: 400, body: `{"error":"too_many_events"}`},
		// wire §8's executed answer under a live kill switch, which is what
		// C16 needs and what `stop:` alone cannot produce.
		{mode: "stop:60+reject:stopped", status: 202, body: `{"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"},{"i":2,"reason":"stopped"}],"stop":{"until":1700000060,"scope":"app"}}`},
		{mode: "slow:100+429:20", status: 429, body: `{}`, retryAfter: "20", hasRetry: true},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			mode, err := ParseMode(testCase.mode)
			if err != nil {
				t.Fatalf("ParseMode(%q): %v", testCase.mode, err)
			}
			if mode.Status != testCase.status {
				t.Errorf("status = %d, want %d", mode.Status, testCase.status)
			}
			if got := mode.Body(3, stopAt); got != testCase.body {
				t.Errorf("body\n got %s\nwant %s", got, testCase.body)
			}
			if mode.HasRetryAfter != testCase.hasRetry {
				t.Errorf("HasRetryAfter = %v, want %v", mode.HasRetryAfter, testCase.hasRetry)
			}
			if mode.HasRetryAfter && mode.RetryAfter != testCase.retryAfter {
				t.Errorf("Retry-After = %q, want %q", mode.RetryAfter, testCase.retryAfter)
			}
		})
	}
}

// An empty <v> omits the header; it does not send an empty one. The distinction
// is the whole of C8c's "no header falls back to the backoff alone" arm, and it
// is not visible in the parsed status.
func TestEmptyRetryAfterValueIsNotAnEmptyHeader(t *testing.T) {
	absent, err := ParseMode("429:")
	if err != nil {
		t.Fatal(err)
	}
	if absent.HasRetryAfter {
		t.Fatalf("`429:` must omit Retry-After entirely, got %q", absent.RetryAfter)
	}
	present, err := ParseMode("429:0")
	if err != nil {
		t.Fatal(err)
	}
	if !present.HasRetryAfter || present.RetryAfter != "0" {
		t.Fatalf("`429:0` must send the literal 0, got has=%v value=%q", present.HasRetryAfter, present.RetryAfter)
	}
}

// mockd never clamps <v>. The 3 600 s ceiling of wire §9 is the CLIENT's, and
// C8c asserts the client applies it; a mock that clamped would be answering the
// question the scenario asks.
func TestLargeRetryAfterIsNotClamped(t *testing.T) {
	mode, err := ParseMode("429:9999")
	if err != nil {
		t.Fatal(err)
	}
	if mode.RetryAfter != "9999" {
		t.Fatalf("Retry-After = %q, want the literal 9999", mode.RetryAfter)
	}
}

func TestSlowAloneDelaysAnOK(t *testing.T) {
	mode, err := ParseMode("slow:10000")
	if err != nil {
		t.Fatal(err)
	}
	if mode.Delay != 10*time.Second {
		t.Errorf("delay = %v, want 10s", mode.Delay)
	}
	if mode.Status != 202 || mode.Body(1, time.Now()) != "{}" {
		t.Errorf("slow: alone must answer an ok, got %d %s", mode.Status, mode.Body(1, time.Now()))
	}
}

// A reject on a body mockd could not read still answers one entry, and the
// record carries envelope_error to say why it was one rather than n.
func TestRejectOnAnUnreadableEnvelopeAnswersOneEntry(t *testing.T) {
	mode, err := ParseMode("reject:malformed")
	if err != nil {
		t.Fatal(err)
	}
	if got := mode.Body(0, time.Now()); got != `{"rejected":[{"i":0,"reason":"malformed"}]}` {
		t.Fatalf("body = %s", got)
	}
}

func TestParseModeRejectsWhatItCannotHonour(t *testing.T) {
	for _, testCase := range []struct{ mode, contains string }{
		{"nope", "unknown mode clause"},
		{"ok:1", "takes no value"},
		{"reject", "needs a reason"},
		{"reject:", "needs a reason"},
		{"stop", "needs a duration"},
		{"stop:soon", "whole number"},
		{"stop:60:desktop", "app` and `web"},
		{"slow", "needs a delay"},
		{"slow:-1", "whole number"},
		{"402:1", "only meaningful on 429 and 503"},
		{"400:nonsense", "envelope errors are"},
		{"429+503", "conflicts"},
		{"ok+402", "conflicts"},
		{"garbage+huge", "conflicts"},
		{"down+ok", "composes with nothing"},
		{"down+slow:5", "composes with nothing"},
		{"ok+", "empty clause"},
		{"huge:0", "whole number > 0"},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			_, err := ParseMode(testCase.mode)
			if err == nil {
				t.Fatalf("ParseMode(%q) must fail", testCase.mode)
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("error %q does not explain the problem (want %q in it)", err, testCase.contains)
			}
		})
	}
}

// A reason mockd does not recognise is passed through. Closing the set to §6's
// enum would make a scenario about an unknown reason unwritable, and §6 grows.
func TestRejectReasonIsNotCheckedAgainstTheEnum(t *testing.T) {
	mode, err := ParseMode("reject:some_future_reason")
	if err != nil {
		t.Fatalf("a reason outside spec/wire-v1.md §6 must still parse: %v", err)
	}
	if !strings.Contains(mode.Body(1, time.Now()), "some_future_reason") {
		t.Fatal("the reason must reach the body verbatim")
	}
}

func TestHugeBodyIsValidJSONPastItsSize(t *testing.T) {
	mode, err := ParseMode("huge:4096")
	if err != nil {
		t.Fatal(err)
	}
	body := mode.Body(1, time.Now())
	if len(body) < 4096 {
		t.Fatalf("body is %d bytes, want at least 4096", len(body))
	}
	if !strings.HasPrefix(body, `{"rejected":[],"pad":"`) || !strings.HasSuffix(body, `"}`) {
		t.Fatalf("huge: must stay syntactically valid JSON; got %.40s...", body)
	}
}

func TestGarbageIsNotJSON(t *testing.T) {
	mode, err := ParseMode("garbage")
	if err != nil {
		t.Fatal(err)
	}
	if mode.Status != 202 {
		t.Errorf("status = %d, want 202: C10's garbage is a bad BODY, not a bad status", mode.Status)
	}
	if strings.HasSuffix(mode.Body(1, time.Now()), "}") {
		t.Fatal("the garbage body must not parse as JSON")
	}
}
