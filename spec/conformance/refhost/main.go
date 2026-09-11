package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reply is the one JSON line refhost writes to stdout per command
// (spec/sdk-conformance.md §3.1, "One reply per command").
type Reply struct {
	Cmd   string `json:"cmd"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Value string `json:"value,omitempty"`

	// State is §3.2's export and rides on `dumpstate` alone. §3.1 keeps the
	// two apart: `value` carries what a PRINTING command printed, `state`
	// carries §3.2's object.
	State *Export `json:"state,omitempty"`

	// Micros is what C1 means by "init returns in < 5 ms (host measures)".
	Micros int64 `json:"us,omitempty"`

	// The exit summary. C11 asks the host to measure per-track wall time and
	// its own memory; a Go runtime cannot meet C11's 2 MB RSS budget, so these
	// are REPORTED and the runner decides what to do with them.
	Tracks       int   `json:"tracks,omitempty"`
	TrackP50US   int64 `json:"track_p50_us,omitempty"`
	TrackP99US   int64 `json:"track_p99_us,omitempty"`
	HeapDeltaKiB int64 `json:"heap_delta_kib,omitempty"`
}

func main() {
	code := run(os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(stdin io.Reader, stdout, stderr io.Writer) int {
	debug := NewDebug(stderr, os.Getenv("JELTO_DEBUG") == "1")

	endpoint := os.Getenv("JELTO_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(stderr, "refhost: JELTO_ENDPOINT is required (spec/sdk-conformance.md §3)")
		return 2
	}
	stateDir := os.Getenv("JELTO_STATE_DIR")
	if stateDir == "" {
		fmt.Fprintln(stderr, "refhost: JELTO_STATE_DIR is required (spec/sdk-conformance.md §3)")
		return 2
	}

	clock := NewClock()
	if pin, ok := os.LookupEnv("JELTO_NOW"); ok {
		// The presence of the variable is the pin, so JELTO_NOW=0 is a legal
		// 1970-01-01 clock (C15) and not "unset".
		value, ok := new(big.Int).SetString(strings.TrimSpace(pin), 10)
		if !ok {
			fmt.Fprintf(stderr, "refhost: JELTO_NOW=%q is not a whole number of milliseconds\n", pin)
			return 2
		}
		clock.Pin(value)
	}

	appVersion, setAppVersion := os.LookupEnv("JELTO_APP_VERSION")
	if !setAppVersion {
		appVersion = "1.0.0"
	}
	platform, err := detectPlatform(appVersion)
	if err != nil {
		fmt.Fprintln(stderr, "refhost:", err)
		return 2
	}

	// W4: an SDK "built with a version string it did not choose". The grammar
	// is spec/wire-v1.md §3's and it is the whole request that dies when a
	// value misses it, so a value refhost cannot send is OMITTED, never sent
	// as a bare number.
	version := os.Getenv("JELTO_CLIENT_VERSION")
	if _, set := os.LookupEnv("JELTO_CLIENT_VERSION"); !set {
		version = "refhost/0.1.0"
	}
	if version != "" && !reClientVersion.MatchString(version) {
		debug.Printf("client version %q does not match spec/wire-v1.md §3's ^[a-z]+/[0-9A-Za-z.+-]{1,24}$; `v` is omitted", version)
		version = ""
	}

	seed := time.Now().UnixNano()
	if raw := os.Getenv("JELTO_SEED"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			seed = parsed
		}
	}

	sdk := NewSDK(debug, clock, NewStore(stateDir), endpoint, os.Getenv("JELTO_MOCK"), version, platform, seed)
	host := &host{sdk: sdk, clock: clock, out: stdout, err: stderr}

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	host.heapBefore = int64(before.HeapAlloc)

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if stop := host.dispatch(line); stop {
			return 0
		}
	}
	// EOF on stdin is a termination, and §8.3 item 7 flushes best effort.
	host.reply(Reply{Cmd: "eof", OK: true})
	sdk.Stop()
	return 0
}

type host struct {
	sdk   *SDK
	clock *Clock
	out   io.Writer
	err   io.Writer

	trackTimes []int64
	heapBefore int64
}

func (h *host) reply(reply Reply) {
	line, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_, _ = h.out.Write(line)
	_, _ = io.WriteString(h.out, "\n")
	if flusher, ok := h.out.(interface{ Sync() error }); ok {
		_ = flusher.Sync()
	}
}

func (h *host) dispatch(line string) (stop bool) {
	tokens := tokenize(line)
	if len(tokens) == 0 {
		return false
	}
	switch tokens[0] {
	case "init":
		if len(tokens) < 2 {
			h.reply(Reply{Cmd: "init", Error: "init <key> [app]"})
			return false
		}
		slug := ""
		if len(tokens) > 2 {
			slug = tokens[2]
		}
		start := time.Now()
		h.sdk.Init(tokens[1], slug)
		h.reply(Reply{Cmd: "init", OK: true, Micros: time.Since(start).Microseconds()})

	case "track":
		if len(tokens) < 2 {
			h.reply(Reply{Cmd: "track", Error: "track <name> [json-props]"})
			return false
		}
		props, err := parseProps(tokens, 2)
		if err != nil {
			h.reply(Reply{Cmd: "track", Error: err.Error()})
			return false
		}
		start := time.Now()
		h.sdk.Track(tokens[1], props)
		elapsed := time.Since(start).Microseconds()
		h.trackTimes = append(h.trackTimes, elapsed)
		h.reply(Reply{Cmd: "track", OK: true, Micros: elapsed})

	case "onboarding":
		if len(tokens) < 3 {
			h.reply(Reply{Cmd: "onboarding", Error: "onboarding <step> <ok|fail|skip> [reason]"})
			return false
		}
		reason := ""
		if len(tokens) > 3 {
			reason = tokens[3]
		}
		h.sdk.Onboarding(tokens[1], tokens[2], reason)
		h.reply(Reply{Cmd: "onboarding", OK: true})

	case "setprops":
		if len(tokens) < 2 {
			// spec/sdk-conformance.md §3: the argument is not optional, and
			// a bare `setprops` is a scenario error rather than "set nothing".
			// Until v0.14 this host called SetProps(nil) and replied ok while
			// the Swift host refused, and the spec said which was right only
			// by the shape of the command language.
			h.reply(Reply{Cmd: "setprops", Error: "setprops <json>"})
			return false
		}
		props, err := parseProps(tokens, 1)
		if err != nil {
			h.reply(Reply{Cmd: "setprops", Error: err.Error()})
			return false
		}
		h.sdk.SetProps(props)
		h.reply(Reply{Cmd: "setprops", OK: true})

	case "installid":
		h.reply(Reply{Cmd: "installid", OK: true, Value: h.sdk.InstallID()})

	case "dumpstate":
		// §3.2's state export. It reads what the SDK already holds and creates,
		// loads and writes nothing, so C5's `dumpstate` before `init` prints an
		// empty export and leaves JELTO_STATE_DIR as empty as it found it.
		export := h.sdk.Export()
		h.reply(Reply{Cmd: "dumpstate", OK: true, State: &export})

	case "reset":
		h.sdk.Reset()
		h.reply(Reply{Cmd: "reset", OK: true})

	case "legacyversion":
		ok := h.sdk.LegacyVersion()
		reply := Reply{Cmd: "legacyversion", OK: ok}
		if !ok {
			reply.Error = "legacyversion requires initialized state with no pending transition"
		}
		h.reply(reply)

	case "disable":
		h.sdk.Disable()
		h.reply(Reply{Cmd: "disable", OK: true})

	case "sleep":
		if len(tokens) < 2 {
			h.reply(Reply{Cmd: "sleep", Error: "sleep <ms>"})
			return false
		}
		millis, err := strconv.ParseInt(tokens[1], 10, 64)
		if err != nil || millis < 0 {
			h.reply(Reply{Cmd: "sleep", Error: "sleep <ms>: a whole number >= 0"})
			return false
		}
		h.sleep(millis)
		h.reply(Reply{Cmd: "sleep", OK: true})

	case "exit":
		h.sdk.Stop()
		h.reply(h.summary("exit"))
		return true

	default:
		h.reply(Reply{Cmd: tokens[0], Error: "unknown command; spec/sdk-conformance.md §3 has init, track, onboarding, setprops, installid, dumpstate, reset, disable, sleep, exit"})
	}
	return false
}

// Pinned sleep settles before and after advancing: bootstrap must finish on the
// old clock, then all work due on the new clock must complete. Both barriers
// share one real-time budget.
func (h *host) sleep(millis int64) {
	if !h.clock.Pinned() {
		time.Sleep(time.Duration(millis) * time.Millisecond)
		return
	}
	deadline := time.Now().Add(settleBudget)
	if !h.settle(deadline) {
		return
	}
	h.clock.Advance(millis)
	h.settle(deadline)
}

// settleBudget bounds one `sleep` in REAL time, however many barriers it takes.
// A wedged scenario has to fail with a message rather than hang.
const settleBudget = 30 * time.Second

// settle waits for the pump's explicit idle acknowledgement before deadline.
// Quiet polling cannot prove scheduled work ran (TODO.md §5).
func (h *host) settle(deadline time.Time) bool {
	barrier := h.sdk.openBarrier()
	if h.sdk.awaitBarrier(barrier, time.Until(deadline)) {
		return true
	}
	fmt.Fprintln(h.err, "refhost: sleep did not settle within 30 s of real time")
	return false
}

func (h *host) summary(cmd string) Reply {
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	reply := Reply{Cmd: cmd, OK: true, Tracks: len(h.trackTimes)}
	reply.HeapDeltaKiB = (int64(after.HeapAlloc) - h.heapBefore) / 1024
	if len(h.trackTimes) > 0 {
		sorted := append([]int64(nil), h.trackTimes...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		reply.TrackP50US = sorted[len(sorted)*50/100]
		reply.TrackP99US = sorted[min(len(sorted)*99/100, len(sorted)-1)]
	}
	return reply
}

func parseProps(tokens []string, at int) (map[string]any, error) {
	if len(tokens) <= at {
		return nil, nil
	}
	raw := tokens[at]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var props map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&props); err != nil {
		return nil, fmt.Errorf("props must be a JSON object: %w", err)
	}
	return props, nil
}

// tokenize splits a command line. Double quotes group a token (W2's
// `track "Bad Name!"`, C20b's `onboarding x ok "Free text reason"`), and a
// token that opens a JSON object or array takes the REST of the line, because
// W3's `track x {"k":"..."}` is one argument containing anything.
func tokenize(line string) []string {
	var tokens []string
	i, n := 0, len(line)
	for i < n {
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		switch line[i] {
		case '"':
			i++
			var token strings.Builder
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n {
					i++
				}
				token.WriteByte(line[i])
				i++
			}
			i++ // the closing quote
			tokens = append(tokens, token.String())
		case '{', '[':
			tokens = append(tokens, strings.TrimSpace(line[i:]))
			return tokens
		default:
			start := i
			for i < n && line[i] != ' ' && line[i] != '\t' {
				i++
			}
			tokens = append(tokens, line[start:i])
		}
	}
	return tokens
}
