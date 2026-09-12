package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Drive the real binary over sockets to verify wire headers, including the
// difference between an omitted and an empty Retry-After value.

var mockdBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mockd-build")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockd test: temp dir:", err)
		os.Exit(1)
	}
	name := "mockd"
	if runtime.GOOS == "windows" {
		// exec resolves a path without an extension through PATHEXT, so a
		// bare "mockd" is never found; the runner names its host the same way.
		name += ".exe"
	}
	mockdBinary = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", mockdBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockd test: build:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// batch is a three-event envelope. Its `t` values are deliberately awkward: a
// plain integer, an exponent literal and a value past int64. wire §3 makes all
// three legal whole numbers and C15b sends the last one, so a recording that
// re-serialised them would fail the scenario that matters most here.
const batch = `{"v":1,"p":"prd_8f3kq2m9x1","e":[` +
	`{"n":"heartbeat","s":"app","t":1785578400000,"id":"018f0000-0000-7000-8000-000000000001","v":"swift/1.0.3","iid":"018f0000-0000-7000-8000-0000000000aa"},` +
	`{"n":"install","s":"app","t":1.7855784e12},` +
	`{"n":"x","s":"app","t":99999999999999999999}]}`

type instance struct {
	t          *testing.T
	addr       string
	control    string
	recordPath string
	stderrPath string
	cmd        *exec.Cmd
	client     *http.Client
}

// start runs the binary and blocks until its ready line names the addresses.
// The socket path is kept short on purpose: a unix path is capped near 104
// bytes and t.TempDir() plus a long test name goes past it.
func start(t *testing.T, args ...string) *instance {
	t.Helper()
	dir, err := os.MkdirTemp("", "mockd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	in := &instance{
		t:          t,
		control:    filepath.Join(dir, "c.sock"),
		recordPath: filepath.Join(dir, "record.jsonl"),
		stderrPath: filepath.Join(dir, "stderr.log"),
		// Keep-alive off by default so a request and a connection count the
		// same and a scenario's arithmetic is legible; one test turns it back
		// on to check conn_id groups requests.
		client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second},
	}

	full := append([]string{"-addr", "127.0.0.1:0", "-control", in.control, "-record", in.recordPath}, args...)
	cmd := exec.Command(mockdBinary, full...)
	stderr, err := os.Create(in.stderrPath)
	if err != nil {
		t.Fatalf("stderr file: %v", err)
	}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mockd: %v", err)
	}
	in.cmd = cmd
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = stderr.Close()
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("mockd wrote no ready line: %v (stderr: %s)", err, in.stderr())
	}
	var ready Ready
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		t.Fatalf("ready line %q is not JSON: %v", line, err)
	}
	if ready.Addr == "" || ready.Control != in.control || ready.PID != cmd.Process.Pid {
		t.Fatalf("ready line %q does not name this process's listeners", line)
	}
	in.addr = ready.Addr
	return in
}

func (in *instance) stderr() string {
	data, _ := os.ReadFile(in.stderrPath)
	return string(data)
}

func (in *instance) url() string { return "http://" + in.addr + IngestPath }

// post sends one envelope, with X-Mock when mode is non-empty.
func (in *instance) post(mode, body string) (*http.Response, string) {
	in.t.Helper()
	request, err := http.NewRequest(http.MethodPost, in.url(), strings.NewReader(body))
	if err != nil {
		in.t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if mode != "" {
		request.Header.Set(MockHeader, mode)
	}
	response, err := in.client.Do(request)
	if err != nil {
		in.t.Fatalf("POST %s (X-Mock: %s): %v", in.url(), mode, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		in.t.Fatalf("read body: %v", err)
	}
	return response, string(payload)
}

// call sends one control command and returns the decoded reply.
func (in *instance) call(command string) map[string]any {
	in.t.Helper()
	conn, err := net.Dial("unix", in.control)
	if err != nil {
		in.t.Fatalf("dial control socket: %v (stderr: %s)", err, in.stderr())
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		in.t.Fatalf("write command: %v", err)
	}
	var reply map[string]any
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		in.t.Fatalf("read reply to %s: %v", command, err)
	}
	return reply
}

// mustCall fails when the reply is not ok.
func (in *instance) mustCall(command string) map[string]any {
	in.t.Helper()
	reply := in.call(command)
	if ok, _ := reply["ok"].(bool); !ok {
		in.t.Fatalf("%s -> %v", command, reply)
	}
	return reply
}

func (in *instance) records() []map[string]any {
	in.t.Helper()
	reply := in.mustCall(`{"cmd":"recording"}`)
	raw, _ := reply["records"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		record, _ := item.(map[string]any)
		out = append(out, record)
	}
	return out
}

func (in *instance) requests() []map[string]any {
	in.t.Helper()
	var out []map[string]any
	for _, record := range in.records() {
		if record["kind"] == "request" {
			out = append(out, record)
		}
	}
	return out
}

func number(t *testing.T, record map[string]any, key string) float64 {
	t.Helper()
	value, ok := record[key].(float64)
	if !ok {
		t.Fatalf("record has no numeric %q: %v", key, record)
	}
	return value
}

// --- §2's mode table, over the wire ------------------------------------------

func TestEveryModeAnswersWhatTheSpecSays(t *testing.T) {
	in := start(t)
	for _, testCase := range []struct {
		mode       string
		status     int
		body       string
		retryAfter string // "" means the header must be ABSENT
	}{
		{mode: "ok", status: 202, body: `{}`},
		{mode: "reject:stopped", status: 202, body: `{"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"},{"i":2,"reason":"stopped"}]}`},
		{mode: "429", status: 429, body: `{}`, retryAfter: "2"},
		{mode: "503", status: 503, body: `{}`, retryAfter: "2"},
		{mode: "429:9999", status: 429, body: `{}`, retryAfter: "9999"},
		{mode: "429:soon", status: 429, body: `{}`, retryAfter: "soon"},
		{mode: "503:1", status: 503, body: `{}`, retryAfter: "1"},
		{mode: "429:", status: 429, body: `{}`},
		{mode: "503:", status: 503, body: `{}`},
		{mode: "400", status: 400, body: `{"error":"malformed"}`},
		{mode: "402", status: 402, body: `{"error":"payment_required"}`},
		{mode: "500", status: 500, body: `{"error":"internal"}`},
		{mode: "413", status: 413, body: `{"error":"too_large"}`},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			response, body := in.post(testCase.mode, batch)
			if response.StatusCode != testCase.status {
				t.Errorf("status = %d, want %d", response.StatusCode, testCase.status)
			}
			if body != testCase.body {
				t.Errorf("body\n got %s\nwant %s", body, testCase.body)
			}
			values := response.Header.Values("Retry-After")
			switch {
			case testCase.retryAfter == "" && len(values) != 0:
				// The distinction wire §9 turns on: an absent header falls back
				// to the backoff alone, an empty one is a value a client has to
				// parse. `429:` must send NO header.
				t.Errorf("Retry-After must be absent, got %q", values)
			case testCase.retryAfter != "" && (len(values) != 1 || values[0] != testCase.retryAfter):
				t.Errorf("Retry-After = %q, want [%q]", values, testCase.retryAfter)
			}
		})
	}
}

func TestStopCarriesTheDeadlineAndTheScope(t *testing.T) {
	in := start(t)
	before := time.Now().Unix()
	_, body := in.post("stop:60", batch)
	after := time.Now().Unix()

	var payload struct {
		Stop struct {
			Until int64  `json:"until"`
			Scope string `json:"scope"`
		} `json:"stop"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("stop body %s: %v", body, err)
	}
	if payload.Stop.Until < before+60 || payload.Stop.Until > after+60 {
		t.Errorf("until = %d, want now+60 in [%d, %d]", payload.Stop.Until, before+60, after+60)
	}
	if payload.Stop.Scope != "app" {
		t.Errorf("scope = %q, want app", payload.Stop.Scope)
	}

	// C16b needs the web scope and §2's table has no way to ask for it.
	_, webBody := in.post("stop:30:web", batch)
	if !strings.Contains(webBody, `"scope":"web"`) {
		t.Errorf("stop:30:web = %s, want the web scope", webBody)
	}
}

// C16 asserts "queue holds x...y" across a stop, which only holds if the events
// sent under the switch were REJECTED. wire §8's executed answer is exactly
// this shape, and `stop:` alone cannot produce it.
func TestStopComposesWithTheStoppedRejection(t *testing.T) {
	in := start(t)
	_, body := in.post("stop:60+reject:stopped", batch)
	if !strings.Contains(body, `"rejected":[{"i":0,"reason":"stopped"},{"i":1,"reason":"stopped"},{"i":2,"reason":"stopped"}]`) {
		t.Errorf("body %s carries no per-event stopped rejection", body)
	}
	if !strings.Contains(body, `"stop":{"until":`) {
		t.Errorf("body %s carries no stop", body)
	}
}

func TestSlowDelaysTheResponse(t *testing.T) {
	in := start(t)
	started := time.Now()
	response, _ := in.post("slow:400", batch)
	elapsed := time.Since(started)
	if response.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202: slow: alone delays an ok", response.StatusCode)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("answered after %v, want at least 400ms", elapsed)
	}
}

func TestGarbageAndHugeBodies(t *testing.T) {
	in := start(t)
	response, body := in.post("garbage", batch)
	if response.StatusCode != http.StatusAccepted {
		t.Errorf("garbage status = %d, want 202", response.StatusCode)
	}
	if json.Valid([]byte(body)) {
		t.Errorf("garbage body %q parses as JSON", body)
	}
	response, body = in.post("huge:200000", batch)
	if response.StatusCode != http.StatusAccepted || len(body) < 200000 {
		t.Errorf("huge: gave %d bytes at status %d, want >= 200000 at 202", len(body), response.StatusCode)
	}
	if !json.Valid([]byte(body)) {
		t.Error("huge: must stay valid JSON; C10 tests an oversized body, not a broken one")
	}
}

// --- routing -----------------------------------------------------------------

func TestRouteContract(t *testing.T) {
	in := start(t)
	preflight, err := in.client.Do(mustRequest(t, http.MethodOptions, in.url(), ""))
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	_ = preflight.Body.Close()
	if preflight.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS = %d, want 204 (spec/wire-v1.md §2a)", preflight.StatusCode)
	}
	if preflight.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("OPTIONS must answer permissive CORS (wire §1)")
	}

	get, err := in.client.Do(mustRequest(t, http.MethodGet, in.url(), ""))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/e = %d, want 405", get.StatusCode)
	}

	wrong, err := in.client.Do(mustRequest(t, http.MethodPost, "http://"+in.addr+"/v2/e", batch))
	if err != nil {
		t.Fatalf("POST /v2/e: %v", err)
	}
	_ = wrong.Body.Close()
	if wrong.StatusCode != http.StatusNotFound {
		t.Errorf("POST /v2/e = %d, want 404", wrong.StatusCode)
	}

	// All three are recorded: an SDK calling the wrong path or method must be
	// visible, not silently absent from the log.
	if got := len(in.requests()); got != 3 {
		t.Errorf("recorded %d requests, want 3 (the preflight, the GET and the wrong path)", got)
	}
}

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	return request
}

// --- the control socket ------------------------------------------------------

func TestControlSocketSetsTheScenarioMode(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"mode","mode":"429:20"}`)
	response, _ := in.post("", batch)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "20" {
		t.Fatalf("status %d Retry-After %q, want 429 / 20", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if source := in.requests()[0]["mode_source"]; source != "default" {
		t.Errorf("mode_source = %v, want default", source)
	}
}

// C8c is a positional program -- "429:9999, then 429:, then 429:soon, then
// 503:1 at the tenth consecutive refusal" -- and a runner that raced to flip
// modes between an SDK's retries would be asserting on its own timing.
func TestScriptDrivesOneModePerRequest(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"script","steps":[{"mode":"429:9999"},{"mode":"429:"},{"mode":"429:soon"},{"mode":"429:2","times":6},{"mode":"503:1"}],"default":"ok"}`)

	want := []struct {
		status int
		retry  string
	}{
		{429, "9999"}, {429, ""}, {429, "soon"},
		{429, "2"}, {429, "2"}, {429, "2"}, {429, "2"}, {429, "2"}, {429, "2"},
		{503, "1"},
		{202, ""}, // the steps are spent; the default governs
	}
	for i, expected := range want {
		response, _ := in.post("", batch)
		if response.StatusCode != expected.status {
			t.Fatalf("request %d: status %d, want %d", i+1, response.StatusCode, expected.status)
		}
		if got := response.Header.Get("Retry-After"); got != expected.retry {
			t.Fatalf("request %d: Retry-After %q, want %q", i+1, got, expected.retry)
		}
	}
}

// The two mode channels are independent. A header-driven request in the middle
// of a scripted run must not shift the script under it.
func TestHeaderWinsAndDoesNotConsumeAScriptStep(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"script","steps":[{"mode":"503:7"}]}`)

	response, _ := in.post("402", batch)
	if response.StatusCode != 402 {
		t.Fatalf("the header must win: status %d, want 402", response.StatusCode)
	}
	if remaining := number(t, in.mustCall(`{"cmd":"stats"}`), "script_remaining"); remaining != 1 {
		t.Fatalf("script_remaining = %v, want 1: a header-driven request must not consume a step", remaining)
	}
	response, _ = in.post("", batch)
	if response.StatusCode != 503 || response.Header.Get("Retry-After") != "7" {
		t.Fatalf("the step survived unconsumed: status %d Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}

	sources := []any{}
	for _, record := range in.requests() {
		sources = append(sources, record["mode_source"])
	}
	if len(sources) != 2 || sources[0] != "header" || sources[1] != "script" {
		t.Errorf("mode_source = %v, want [header script]", sources)
	}
}

// A preflight or a wrong path must not spend a step: C8c's assertions are
// positional and would shift by one.
func TestOnlyIngestPostsConsumeAScriptStep(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"script","steps":[{"mode":"402"}]}`)
	for _, request := range []*http.Request{
		mustRequest(t, http.MethodOptions, in.url(), ""),
		mustRequest(t, http.MethodGet, in.url(), ""),
		mustRequest(t, http.MethodPost, "http://"+in.addr+"/nope", batch),
	} {
		response, err := in.client.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", request.Method, request.URL, err)
		}
		_ = response.Body.Close()
	}
	if remaining := number(t, in.mustCall(`{"cmd":"stats"}`), "script_remaining"); remaining != 1 {
		t.Fatalf("script_remaining = %v, want 1", remaining)
	}
}

func TestControlSocketRefusesAModeItCannotHonour(t *testing.T) {
	in := start(t)
	// A bad scenario must fail at setup, where it reads as a scenario bug, and
	// not at assert time, where it reads as an SDK bug.
	reply := in.call(`{"cmd":"mode","mode":"nonsense"}`)
	if ok, _ := reply["ok"].(bool); ok {
		t.Fatalf("an unparseable mode was accepted: %v", reply)
	}
	if reply := in.call(`{"cmd":"nope"}`); reply["ok"] == true {
		t.Fatalf("an unknown command was accepted: %v", reply)
	}
	// The connection survives a bad command.
	if in.call(`{"cmd":"ping"}`)["ok"] != true {
		t.Fatal("ping failed after a refused command")
	}
}

func TestClockPinMovesTheStopDeadline(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"clock","unix_ms":1000000000000}`)
	_, body := in.post("stop:60", batch)
	if !strings.Contains(body, `"until":1000000060`) {
		t.Fatalf("stop body %s does not use the pinned clock; a scenario running the host under JELTO_NOW would get an `until` in its own past", body)
	}
	in.mustCall(`{"cmd":"clock","unix_ms":0}`)
	_, body = in.post("stop:60", batch)
	if strings.Contains(body, `"until":1000000060`) {
		t.Fatalf("unpinning the clock did not restore the real one: %s", body)
	}
}

func TestAwaitBlocksUntilTheRequestArrives(t *testing.T) {
	in := start(t)
	done := make(chan map[string]any, 1)
	go func() { done <- in.call(`{"cmd":"await","count":1,"since":1,"timeout_ms":8000}`) }()

	select {
	case reply := <-done:
		t.Fatalf("await returned before any request arrived: %v", reply)
	case <-time.After(250 * time.Millisecond):
	}

	in.post("ok", batch)
	select {
	case reply := <-done:
		if ok, _ := reply["ok"].(bool); !ok {
			t.Fatalf("await = %v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await did not return after the request arrived")
	}

	// And it reports a timeout rather than blocking forever.
	reply := in.call(`{"cmd":"await","count":9,"since":1,"timeout_ms":300}`)
	if reply["error"] != "timeout" {
		t.Fatalf("await past the deadline = %v, want a timeout", reply)
	}
}

func TestResetIsTheScenarioBoundary(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"mode","mode":"402"}`)
	in.mustCall(`{"cmd":"script","steps":[{"mode":"503"}]}`)
	in.mustCall(`{"cmd":"clock","unix_ms":1000000000000}`)
	in.post("", batch)

	in.mustCall(`{"cmd":"reset"}`)

	stats := in.mustCall(`{"cmd":"stats"}`)
	if number(t, stats, "requests") != 0 || number(t, stats, "connections") != 0 {
		t.Fatalf("reset left counters behind: %v", stats)
	}
	if stats["mode"] != "ok" {
		t.Fatalf("mode = %v, want ok", stats["mode"])
	}
	if number(t, stats, "script_remaining") != 0 {
		t.Fatalf("reset left a script behind: %v", stats)
	}
	response, body := in.post("stop:60", batch)
	if response.StatusCode != 202 || strings.Contains(body, `"until":1000000060`) {
		t.Fatalf("reset did not unpin the clock or restore the mode: %d %s", response.StatusCode, body)
	}
	if records := in.records(); records[0]["seq"].(float64) != 1 {
		t.Fatalf("seq did not restart at 1 after reset: %v", records[0])
	}
	data, err := os.ReadFile(in.recordPath)
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}
	if bytes.Count(data, []byte("\n")) > 4 {
		t.Fatalf("reset did not truncate the record file: %d lines", bytes.Count(data, []byte("\n")))
	}
}

func TestShutdownCommandStopsTheProcess(t *testing.T) {
	in := start(t)
	in.mustCall(`{"cmd":"shutdown"}`)
	done := make(chan error, 1)
	go func() { done <- in.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("mockd exited with %v (stderr: %s)", err, in.stderr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mockd did not exit after a shutdown command")
	}
	if _, err := os.Stat(in.control); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket outlived the process: %v", err)
	}
}

// --- down --------------------------------------------------------------------

func TestDownRefusesConnectionsAndComesBackOnTheSamePort(t *testing.T) {
	in := start(t)
	addr := in.addr

	in.mustCall(`{"cmd":"mode","mode":"down"}`)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a connection succeeded while mockd was down")
	}
	if listening, _ := in.mustCall(`{"cmd":"stats"}`)["listening"].(bool); listening {
		t.Error("stats says mockd is listening while it is down")
	}

	in.mustCall(`{"cmd":"mode","mode":"ok"}`)
	if in.addr != addr {
		t.Fatalf("address moved from %s to %s; a host configured for the first would never find the second", addr, in.addr)
	}
	response, _ := in.post("", batch)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d after coming back up", response.StatusCode)
	}
}

func TestDownCannotBeSetPerRequest(t *testing.T) {
	in := start(t)
	// Refusing a connection is decided before any header exists. Answering `ok`
	// would let a scenario believe it had taken the endpoint away.
	response, _ := in.post("down", batch)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	if !strings.Contains(response.Header.Get("X-Mock-Error"), "control socket") {
		t.Errorf("X-Mock-Error = %q, want it to name the control socket", response.Header.Get("X-Mock-Error"))
	}
	if listening, _ := in.mustCall(`{"cmd":"stats"}`)["listening"].(bool); !listening {
		t.Error("a per-request `down` took the listener away")
	}
}

// `hangup` is what C4b and C6 have instead of `down`: an attempt made against a
// closed listener is invisible to mockd, so a scenario that must SEE the retry
// accepts the connection and drops it.
func TestHangupRecordsTheAttemptItRefusesToAnswer(t *testing.T) {
	in := start(t)
	request, err := http.NewRequest(http.MethodPost, in.url(), strings.NewReader(batch))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(MockHeader, "hangup")
	if response, err := in.client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("hangup answered the request")
	}
	requests := in.requests()
	if len(requests) != 1 {
		t.Fatalf("recorded %d requests, want 1: the point of hangup is that the attempt is recorded", len(requests))
	}
	response, _ := requests[0]["response"].(map[string]any)
	if hangup, _ := response["hangup"].(bool); !hangup {
		t.Fatalf("the record does not say the connection was dropped: %v", requests[0])
	}
}

// --- the recording -----------------------------------------------------------

func TestRecordingCarriesWhatTheScenariosAssertOn(t *testing.T) {
	in := start(t)
	in.post("429:20", batch)

	records := in.records()
	if len(records) != 2 || records[0]["kind"] != "connection" || records[1]["kind"] != "request" {
		t.Fatalf("want a connection record then a request record, got %v", records)
	}
	connection, request := records[0], records[1]
	if connection["conn_id"] != request["conn_id"] {
		t.Errorf("the request is not attributed to the connection that carried it: %v vs %v", connection["conn_id"], request["conn_id"])
	}
	if number(t, connection, "seq") >= number(t, request, "seq") {
		t.Error("seq is not arrival order")
	}

	// Headers, verbatim. W1 reads Content-Type here.
	headers, _ := request["headers"].(map[string]any)
	if values, _ := headers["Content-Type"].([]any); len(values) != 1 || values[0] != "application/json" {
		t.Errorf("Content-Type not recorded: %v", headers)
	}
	if values, _ := headers["X-Mock"].([]any); len(values) != 1 || values[0] != "429:20" {
		t.Errorf("X-Mock not recorded: %v", headers)
	}

	// The RAW body. C20's "byte-identical to what track() would send" and W1's
	// schema validation are both claims about these exact bytes.
	if request["body"] != batch {
		t.Errorf("body was not recorded verbatim:\n got %v\nwant %v", request["body"], batch)
	}
	if number(t, request, "body_bytes") != float64(len(batch)) {
		t.Errorf("body_bytes = %v, want %d", request["body_bytes"], len(batch))
	}

	// What was answered, so a scenario need not re-derive it from the mode.
	response, _ := request["response"].(map[string]any)
	if number(t, response, "status") != 429 {
		t.Errorf("response.status = %v, want 429", response["status"])
	}
	responseHeaders, _ := response["headers"].(map[string]any)
	if values, _ := responseHeaders["Retry-After"].([]any); len(values) != 1 || values[0] != "20" {
		t.Errorf("the recorded response does not carry Retry-After: %v", responseHeaders)
	}
	if request["mode"] != "429:20" || request["mode_source"] != "header" {
		t.Errorf("mode/%v source/%v not recorded", request["mode"], request["mode_source"])
	}
	if request["mock_error"] != nil {
		t.Errorf("mock_error = %v, want none", request["mock_error"])
	}
	if number(t, request, "responded_us") < number(t, request, "since_start_us") {
		t.Error("responded_us precedes arrival")
	}
}

// C16's assertion is "one heartbeat, ALONE in its batch" -- which is
// event_count == 1 and events[0].n == heartbeat, and is exactly the pair the
// summary has to make separable from a heartbeat at the front of a batch.
func TestEnvelopeSummarySeparatesALoneHeartbeatFromABatch(t *testing.T) {
	in := start(t)
	in.post("ok", `{"v":1,"p":"prd_8f3kq2m9x1","e":[{"n":"heartbeat","s":"app"}]}`)
	in.post("ok", batch)

	requests := in.requests()
	if len(requests) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(requests))
	}
	alone, _ := requests[0]["envelope"].(map[string]any)
	if number(t, alone, "event_count") != 1 {
		t.Fatalf("event_count = %v, want 1", alone["event_count"])
	}
	events, _ := alone["events"].([]any)
	first, _ := events[0].(map[string]any)
	if first["n"] != "heartbeat" {
		t.Fatalf("events[0].n = %v, want heartbeat", first["n"])
	}

	batched, _ := requests[1]["envelope"].(map[string]any)
	if number(t, batched, "event_count") != 3 {
		t.Fatalf("event_count = %v, want 3: a heartbeat at the FRONT of a batch is not a lone heartbeat", batched["event_count"])
	}
	batchedEvents, _ := batched["events"].([]any)
	head, _ := batchedEvents[0].(map[string]any)
	if head["n"] != "heartbeat" {
		t.Fatalf("the batch does not start with a heartbeat, so this test proves nothing: %v", head)
	}
}

// Assert recorded bytes for exponent literals and large whole numbers. Decoding
// through float64 would change them before the assertion (C15b).
func TestEnvelopeSummaryKeepsTimestampLiterals(t *testing.T) {
	in := start(t)
	in.post("ok", batch)

	data, err := os.ReadFile(in.recordPath)
	if err != nil {
		t.Fatalf("read %s: %v", in.recordPath, err)
	}
	recorded := string(data)
	for _, want := range []string{`"t":1785578400000`, `"t":1.7855784e12`, `"t":99999999999999999999`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the recording lost %s; it reads\n%s", want, recorded)
		}
	}

	envelope, _ := in.requests()[0]["envelope"].(map[string]any)
	events, _ := envelope["events"].([]any)
	first, _ := events[0].(map[string]any)
	if first["id"] != "018f0000-0000-7000-8000-000000000001" || first["v"] != "swift/1.0.3" {
		t.Errorf("id and v are what C8 and W4 assert on, and are missing: %v", first)
	}
	if first["iid"] != "018f0000-0000-7000-8000-0000000000aa" {
		t.Errorf("iid is missing: %v", first)
	}
}

// C5 asserts "no socket opened", which no per-request log can answer.
func TestABareConnectionIsRecordedWithNoRequest(t *testing.T) {
	in := start(t)
	conn, err := net.Dial("tcp", in.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := in.mustCall(`{"cmd":"stats"}`)
		if number(t, stats, "connections") == 1 {
			if number(t, stats, "requests") != 0 {
				t.Fatalf("a bare connection was counted as a request: %v", stats)
			}
			records := in.records()
			if len(records) != 1 || records[0]["kind"] != "connection" {
				t.Fatalf("want one connection record, got %v", records)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a connection that sent nothing was never recorded; C5 cannot be written")
}

// C7's "3 requests of <= 100 events" has to tell three requests on one
// keep-alive connection from three connections.
func TestConnIDGroupsRequestsOnOneConnection(t *testing.T) {
	in := start(t)
	in.client = &http.Client{Timeout: 30 * time.Second} // keep-alive on
	for i := 0; i < 3; i++ {
		in.post("ok", batch)
	}
	requests := in.requests()
	if len(requests) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(requests))
	}
	if requests[0]["conn_id"] != requests[1]["conn_id"] || requests[1]["conn_id"] != requests[2]["conn_id"] {
		t.Fatalf("keep-alive requests were not attributed to one connection: %v %v %v",
			requests[0]["conn_id"], requests[1]["conn_id"], requests[2]["conn_id"])
	}
	if connections := number(t, in.mustCall(`{"cmd":"stats"}`), "connections"); connections != 1 {
		t.Errorf("connections = %v, want 1", connections)
	}
}

// A schedule is a claim about DIFFERENCES, so the field a schedule is measured
// in must be monotonic and fine-grained. C8b has to separate "no request before
// 20 s" from 19.98 s.
func TestArrivalTimesAreMonotonicAndFineGrained(t *testing.T) {
	in := start(t)
	for i := 0; i < 3; i++ {
		in.post("ok", batch)
		time.Sleep(60 * time.Millisecond)
	}
	requests := in.requests()
	if len(requests) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(requests))
	}
	previous := float64(-1)
	for i, request := range requests {
		at := number(t, request, "since_start_us")
		if at <= previous {
			t.Fatalf("request %d arrived at %v, not after %v", i, at, previous)
		}
		previous = at
	}
	gap := number(t, requests[1], "since_start_us") - number(t, requests[0], "since_start_us")
	if gap < 40_000 || gap > 2_000_000 {
		t.Errorf("gap between two requests 60ms apart = %v us, which is not a usable measurement", gap)
	}
}

func TestBodyLengthSurvivesTheStorageCap(t *testing.T) {
	in := start(t, "-max-body", "32")
	in.post("ok", batch)
	request := in.requests()[0]
	if number(t, request, "body_bytes") != float64(len(batch)) {
		t.Errorf("body_bytes = %v, want the TRUE length %d: W1 bounds the body at 64 KB and C6 at 1 MB, and both are assertions about the length", request["body_bytes"], len(batch))
	}
	if truncated, _ := request["body_truncated"].(bool); !truncated {
		t.Error("body_truncated is not set on a capped body")
	}
	if body, _ := request["body"].(string); len(body) != 32 {
		t.Errorf("kept %d bytes, want the 32 the cap allows", len(body))
	}
}

func TestAnUnparseableModeIsLoudRatherThanAnOK(t *testing.T) {
	in := start(t)
	response, _ := in.post("teapot", batch)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: an X-Mock nobody can parse must not behave like `ok`", response.StatusCode)
	}
	if !strings.Contains(response.Header.Get("X-Mock-Error"), "unknown mode clause") {
		t.Errorf("X-Mock-Error = %q", response.Header.Get("X-Mock-Error"))
	}
	request := in.requests()[0]
	if mockError, _ := request["mock_error"].(string); !strings.Contains(mockError, "unknown mode clause") {
		t.Errorf("the record does not carry the mock error: %v", request["mock_error"])
	}
	if errors := number(t, in.mustCall(`{"cmd":"stats"}`), "mock_errors"); errors != 1 {
		t.Errorf("mock_errors = %v, want 1: a runner should be able to assert this is zero", errors)
	}
}

// The -record file is the durable artefact §6's certification log is built
// from, so it must agree with the socket.
func TestRecordFileMatchesTheSocketRecording(t *testing.T) {
	in := start(t)
	in.post("ok", batch)
	in.post("429:20", batch)

	data, err := os.ReadFile(in.recordPath)
	if err != nil {
		t.Fatalf("read %s: %v", in.recordPath, err)
	}
	var fromFile []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record file line is not JSON: %v", err)
		}
		fromFile = append(fromFile, record)
	}
	fromSocket := in.records()
	if len(fromFile) != len(fromSocket) {
		t.Fatalf("record file has %d lines, socket has %d records", len(fromFile), len(fromSocket))
	}
	for i := range fromFile {
		if fmt.Sprint(fromFile[i]["seq"]) != fmt.Sprint(fromSocket[i]["seq"]) ||
			fmt.Sprint(fromFile[i]["body"]) != fmt.Sprint(fromSocket[i]["body"]) {
			t.Fatalf("record %d differs between file and socket:\n file %v\nsocket %v", i, fromFile[i], fromSocket[i])
		}
	}
}

func TestRecordingPaginates(t *testing.T) {
	in := start(t)
	for i := 0; i < 4; i++ {
		in.post("ok", batch)
	}
	reply := in.mustCall(`{"cmd":"recording","since":1,"limit":3}`)
	records, _ := reply["records"].([]any)
	if len(records) != 3 {
		t.Fatalf("limit 3 returned %d records", len(records))
	}
	next, _ := reply["next"].(float64)
	if next != 4 {
		t.Fatalf("next = %v, want 4: a truncating limit must resume after the last record returned", next)
	}
	reply = in.mustCall(fmt.Sprintf(`{"cmd":"recording","since":%d}`, int(next)))
	rest, _ := reply["records"].([]any)
	if len(rest) != 5 {
		t.Fatalf("the second page has %d records, want the remaining 5", len(rest))
	}
}

func TestNothingIsLoggedToStdoutAfterTheReadyLine(t *testing.T) {
	// The ready line is the only thing on stdout: a runner parses it, and a
	// stray line there would be parsed as one.
	in := start(t)
	in.post("teapot", batch) // provokes an error log
	in.mustCall(`{"cmd":"mode","mode":"429"}`)
	if stderr := in.stderr(); !strings.Contains(stderr, "mockd ready") {
		t.Fatalf("slog is not writing to stderr: %q", stderr)
	}
	if strings.Contains(in.stderr(), "\"addr\":") {
		t.Error("the ready line was written to stderr as well as stdout")
	}
}
