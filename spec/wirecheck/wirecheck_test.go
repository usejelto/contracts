package wirecheck

import (
	"path/filepath"
	"strings"
	"testing"
)

func testValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := New(filepath.Join("..", "wire-v1.schema.json"))
	if err != nil {
		t.Fatalf("compile the executable schema: %v", err)
	}
	return v
}

// The tool's whole claim is that it uses the SAME validator W1 uses, so the
// promotion out of spec/conformance/runner must not have changed a verdict.
// These are the runner's own cases, kept here as the package's own floor.
func TestValidatorStillAgreesWithW1(t *testing.T) {
	v := testValidator(t)
	app := `{"n":"heartbeat","s":"app","iid":"0191c000-0000-7000-8000-000000000001","av":"1.0","os":"macos","osv":"15.1","arch":"arm64"}`
	accept := map[string]string{
		"an ordinary app heartbeat": `{"v":1,"p":"prd_conform001","e":[` + app + `]}`,
		"a pre-epoch t (C15b)":      `{"v":1,"p":"prd_conform001","e":[{"n":"x","s":"app","t":-14182940000,"iid":"0191c000-0000-7000-8000-000000000001","av":"1.0","os":"macos","osv":"15.1","arch":"arm64"}]}`,
	}
	reject := map[string]string{
		"a nil-UUID id":                    `{"v":1,"p":"prd_conform001","e":[{"id":"00000000-0000-0000-0000-000000000000","n":"x","s":"app","iid":"0191c000-0000-7000-8000-000000000001","av":"1.0","os":"macos","osv":"15.1","arch":"arm64"}]}`,
		"a nil-UUID iid":                   `{"v":1,"p":"prd_conform001","e":[{"n":"x","s":"app","iid":"00000000-0000-0000-0000-000000000000","av":"1.0","os":"macos","osv":"15.1","arch":"arm64"}]}`,
		"a bare-number client version":     `{"v":1,"p":"prd_conform001","e":[{"n":"x","s":"app","v":"1.2.0","iid":"0191c000-0000-7000-8000-000000000001","av":"1.0","os":"macos","osv":"15.1","arch":"arm64"}]}`,
		"a product key of the wrong shape": `{"v":1,"p":"k","e":[` + app + `]}`,
		"an empty event array":             `{"v":1,"p":"prd_conform001","e":[]}`,
	}
	for name, body := range accept {
		if err := v.ValidateBody([]byte(body)); err != nil {
			t.Errorf("%s: want accepted, got %v", name, err)
		}
	}
	for name, body := range reject {
		if err := v.ValidateBody([]byte(body)); err == nil {
			t.Errorf("%s: want rejected, got accepted", name)
		}
	}
}

// Every message sdk/swift can write, classified. The rows marked NOT "drop" are
// the reason this table exists: checking only `grep '^jelto: drop'` would
// miss these three client-side loss messages.
func TestClassifyCoversTheWholeSDKVocabulary(t *testing.T) {
	cases := []struct {
		raw  string
		want Kind
	}{
		// One case per client-side loss family (eight families, sixteen messages;
		// `drop event` gets two, since six different statements produce it).
		{`jelto: drop event "Signup": spec/wire-v1.md §3 ` + "`n`" + ` is ^[a-z0-9_:.-]{1,64}$`, KindClientLoss},
		{`jelto: drop event "signup": props has 21 keys, spec/wire-v1.md §3 caps them at 20`, KindClientLoss},
		{`jelto: drop event "signup": props value for "plan" has no wire representation; spec/wire-v1.md §3 admits a string, a number or a boolean`, KindClientLoss},
		{`jelto: drop onboarding step "Permissions": spec/wire-v1.md §4 ` + "`<step>`" + ` is ^[a-z0-9_-]{1,32}$`, KindClientLoss},
		{`jelto: drop install property "license": spec/wire-v1.md §4 install-property values are ^[a-z0-9_.-]{1,24}$, not "Paid"`, KindClientLoss},
		{`jelto: drop setprops: 21 install properties, spec/wire-v1.md §3 caps them at 20`, KindClientLoss},
		{`jelto: drop app slug "EQBase Helper": spec/wire-v1.md §5.2 ` + "`a`" + ` is ^[a-z0-9-]{1,32}$`, KindClientLoss},
		// NOT "drop" -- and this one loses the field the Ops card needs to name a bad release.
		{`jelto: client version "1.2.0" does not match spec/wire-v1.md §3's ^[a-z]+/[0-9A-Za-z.+-]{1,24}$; ` + "`v`" + ` is omitted`, KindClientLoss},
		// NOT "drop" -- and these two mean the SDK sends NOTHING, ever, on that build.
		{"jelto: spec/wire-v1.md §5.2 `os` is macos|windows|linux; this build is none of them, so nothing can be sent", KindClientLoss},
		{"jelto: spec/wire-v1.md §5.2 `arch` is arm64|x64|x86; this build is none of them, so nothing can be sent", KindClientLoss},

		// Not client-side losses.
		{`jelto: POST {"v":1,"p":"prd_conform001","e":[]}`, KindPost},
		{`jelto: event 2 rejected: prop_not_allowlisted (email) (spec/wire-v1.md §6)`, KindServerRejection},
		{`jelto: batch dropped: status=400 -- final, not retried (RFC-0001 §8.3 items 8-9, spec/wire-v1.md §2a)`, KindBatchDropped},
		{`jelto: termination flush gave up with 12 events queued (best effort, RFC-0001 §8.3 item 7)`, KindFlushGaveUp},
		{`jelto: retry in 1043 ms (backoff governs; step was 1000 ms, refusal 1) status=503`, KindOperational},
		{`jelto: kill switch: no request until 1785578400000 ms, scope all (spec/wire-v1.md §8)`, KindOperational},
		{`jelto: ignoring a stop scoped to web (spec/wire-v1.md §8; this client is s=app)`, KindOperational},
		{`jelto: 202 with a body that is not JSON`, KindOperational},

		// The embedding app's own logging is skipped, not misread.
		{`2026-09-01 12:00:00 EQBase: opening the library`, KindOther},
		{`jelto is a great product`, KindOther},
	}
	for _, c := range cases {
		got := Classify(1, c.raw)
		if got.Kind != c.want {
			t.Errorf("Classify(%q).Kind = %d, want %d", c.raw, got.Kind, c.want)
		}
	}
}

// A grep for "drop" is not a substitute for the table, stated as a test so the
// table cannot be quietly replaced by one later.
func TestTheDropGrepWouldMissThreeLosses(t *testing.T) {
	missed := []string{
		"jelto: client version \"1.2.0\" does not match spec/wire-v1.md §3's ^[a-z]+/[0-9A-Za-z.+-]{1,24}$; `v` is omitted",
		"jelto: spec/wire-v1.md §5.2 `os` is macos|windows|linux; this build is none of them, so nothing can be sent",
		"jelto: spec/wire-v1.md §5.2 `arch` is arm64|x64|x86; this build is none of them, so nothing can be sent",
	}
	for _, raw := range missed {
		if strings.HasPrefix(raw, "jelto: drop") {
			t.Fatalf("this line DOES start with `jelto: drop`; the premise of this test has changed: %q", raw)
		}
		if got := Classify(1, raw); got.Kind != KindClientLoss {
			t.Errorf("%q classified %d, want KindClientLoss", raw, got.Kind)
		}
	}
}

// A POST line is the body verbatim, so the classifier must not trim, unquote or
// otherwise touch it -- C17 compares stderr against the exact bytes sent.
func TestPostLineIsTheBodyVerbatim(t *testing.T) {
	body := `{"v":1,"p":"prd_conform001","e":[{"n":"x","s":"app","t":99999999999999999999}]}`
	got := Classify(7, "jelto: POST "+body)
	if got.Kind != KindPost {
		t.Fatalf("Kind = %d, want KindPost", got.Kind)
	}
	if got.Message != body {
		t.Fatalf("body was altered:\n got %q\nwant %q", got.Message, body)
	}
	if got.Number != 7 {
		t.Fatalf("Number = %d, want 7", got.Number)
	}
}

func TestCensusCountsFieldsPerSurfaceAndNamesWhatWasNotSeen(t *testing.T) {
	c := NewCensus()
	// Exactly what sdk/swift emits: no `l`, and no web fields at all.
	body := `{"v":1,"p":"prd_fixture001","e":[` +
		`{"id":"0191c000-0000-7000-8000-000000000001","n":"heartbeat","t":1785578400000,"s":"app","iid":"0191c000-0000-7000-8000-000000000002","av":"2.4.1","os":"macos","osv":"15.6.1","arch":"arm64","a":"mac","v":"swift/0.1.0","props":{"license":"paid"}}` +
		`]}`
	if err := c.Add([]byte(body)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.Bodies != 1 || c.Events != 1 {
		t.Fatalf("Bodies=%d Events=%d, want 1/1", c.Bodies, c.Events)
	}
	if c.FieldsBySurf["app"]["iid"] != 1 || c.FieldsBySurf["app"]["osv"] != 1 {
		t.Fatalf("app fields not counted: %v", c.FieldsBySurf["app"])
	}
	if c.PropKeys["license"] != 1 || c.ClientVers["swift/0.1.0"] != 1 || c.EventNames["heartbeat"] != 1 {
		t.Fatal("props / v / event name not counted")
	}
	// `l` is the field the Swift SDK has no code path for. The census must SAY
	// so rather than leave a blank -- after the freeze its grammar is fixed
	// whether or not any sender ever exercised it.
	unseen := strings.Join(c.Unseen("app"), ",")
	if !strings.Contains(unseen, "l") {
		t.Fatalf("Unseen(app) = %q, want it to name `l`", unseen)
	}
	for _, present := range []string{"iid", "av", "os", "osv", "arch", "a"} {
		if strings.Contains(unseen, ","+present) || strings.HasPrefix(unseen, present+",") {
			t.Fatalf("Unseen(app) = %q, but %q was sent", unseen, present)
		}
	}
}

// A body that is JSON but not schema-valid is still counted by the census: the
// census records what a real client EMITTED, which is the question, not what the
// server would have accepted.
func TestCensusCountsAnInvalidBodyToo(t *testing.T) {
	c := NewCensus()
	body := `{"v":1,"p":"k","e":[{"n":"x","s":"app","v":"1.2.0"}]}`
	if err := c.Add([]byte(body)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.Bodies != 1 || c.ClientVers["1.2.0"] != 1 {
		t.Fatal("an invalid body must still be censused")
	}
	if err := testValidator(t).ValidateBody([]byte(body)); err == nil {
		t.Fatal("this body is supposed to be schema-invalid; the fixture has drifted")
	}
}
