package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// IngestPath is the one route an SDK is allowed to call: spec/wire-v1.md §1.
const IngestPath = "/v1/e"

// MockHeader is the per-request mode channel of spec/sdk-conformance.md §2:
// "the X-Mock header the host forwards from JELTO_MOCK".
const MockHeader = "X-Mock"

type connKey struct{}

type connInfo struct {
	id   int64
	conn net.Conn
}

// scriptStep is one entry of the control socket's per-request program.
type scriptStep struct {
	Mode  string `json:"mode"`
	Times int    `json:"times"`
}

// Server is mockd. The HTTP listener is torn down and rebuilt by `down` and
// `reset`, so it is held behind a mutex rather than owned by main.
type Server struct {
	log       *slog.Logger
	recording *Recording
	started   time.Time
	maxBody   int64

	mu sync.Mutex

	defaultMode Mode
	script      []Mode // consumed front to back
	clockPin    time.Time
	clockAt     time.Time

	addr      string
	listener  net.Listener
	http      *http.Server
	listening bool

	waiters chan struct{}
}

func NewServer(log *slog.Logger, recording *Recording, initial Mode, maxBody int64) *Server {
	return &Server{
		log:         log,
		recording:   recording,
		started:     time.Now(),
		maxBody:     maxBody,
		defaultMode: initial,
	}
}

// Listen binds the HTTP listener and remembers the RESOLVED address, so that a
// `down` followed by an `ok` comes back on the same port a scenario's host is
// already configured for. `-addr 127.0.0.1:0` resolves once, here.
func (s *Server) Listen(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.addr = listener.Addr().String()
	s.mu.Unlock()
	s.serve(listener)
	return nil
}

func (s *Server) serve(listener net.Listener) {
	server := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 30 * time.Second,
		// ConnContext runs at accept, before ConnState(StateNew) and before a
		// byte is read, so it is where a connection earns its id and its
		// record. C5 asserts a socket was never opened.
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			now := time.Now()
			id := s.recording.NextConnID(conn.RemoteAddr().String(), now, now.Sub(s.started))
			s.notify()
			return context.WithValue(ctx, connKey{}, connInfo{id: id, conn: conn})
		},
	}
	s.mu.Lock()
	s.listener, s.http, s.listening = listener, server, true
	s.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("mockd http listener stopped", "error", err)
		}
	}()
}

// Addr is the resolved HTTP address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *Server) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listening
}

// BringDown implements the `down` mode by closing the listener and every open
// connection: the kernel then REFUSES a connect, which is what §2 says `down`
// is. The cost is stated in doc.go -- an attempt made while down is invisible
// to the recording, because mockd never sees it.
func (s *Server) BringDown() {
	s.mu.Lock()
	server, listening := s.http, s.listening
	s.listening, s.http, s.listener = false, nil, nil
	s.mu.Unlock()
	if !listening || server == nil {
		return
	}
	_ = server.Close()
	s.log.Info("mockd is down: the listener is closed and connections are refused")
}

// BringUp rebinds the same address. http.Server cannot be served twice after
// Close, so a fresh one is built each time.
func (s *Server) BringUp() error {
	s.mu.Lock()
	if s.listening {
		s.mu.Unlock()
		return nil
	}
	addr := s.addr
	s.mu.Unlock()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rebind %s: %w", addr, err)
	}
	s.serve(listener)
	s.log.Info("mockd is up", "addr", addr)
	return nil
}

func (s *Server) Close() {
	s.BringDown()
	s.recording.Close()
}

// --- mode state -------------------------------------------------------------

// SetMode sets the scenario default and clears any script.
func (s *Server) SetMode(mode Mode) error {
	s.mu.Lock()
	s.defaultMode, s.script = mode, nil
	s.mu.Unlock()
	if mode.Down {
		s.BringDown()
		return nil
	}
	return s.BringUp()
}

// SetScript installs the per-request program. `fallback` replaces the default
// mode when it is set; steps are expanded by `times` here so that consumption
// is a single slice index at request time.
func (s *Server) SetScript(steps []Mode, fallback *Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = steps
	if fallback != nil {
		s.defaultMode = *fallback
	}
}

func (s *Server) ScriptRemaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.script)
}

func (s *Server) DefaultMode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.defaultMode
}

// SetClock pins the `now` from which stop:<seconds> computes `until`. The pin
// advances with real time; unix_ms 0 returns to the real clock.
func (s *Server) SetClock(unixMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if unixMS <= 0 {
		s.clockPin, s.clockAt = time.Time{}, time.Time{}
		return
	}
	s.clockPin, s.clockAt = time.UnixMilli(unixMS), time.Now()
}

func (s *Server) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clockPin.IsZero() {
		return time.Now()
	}
	return s.clockPin.Add(time.Since(s.clockAt))
}

// Reset is the scenario boundary; see doc.go.
func (s *Server) Reset() error {
	s.mu.Lock()
	s.defaultMode, s.script = Mode{Raw: "ok", Status: 202}, nil
	s.clockPin, s.clockAt = time.Time{}, time.Time{}
	s.mu.Unlock()
	s.recording.Reset()
	return s.BringUp()
}

// resolveMode implements doc.go's precedence: the request's own X-Mock header,
// then the next unconsumed script step, then the scenario default. A
// header-driven request does not consume a script step.
func (s *Server) resolveMode(header string) (Mode, string, string) {
	if trimmed := strings.TrimSpace(header); trimmed != "" {
		mode, err := ParseMode(trimmed)
		if err != nil {
			return Mode{}, "header", err.Error()
		}
		if mode.Down {
			// Refusing a connection is decided before any header exists, so
			// `down` is a control-socket mode only (doc.go, "Precedence").
			// Answering `ok` here instead would let a scenario believe it had
			// taken the endpoint away.
			return Mode{}, "header", "`down` cannot be set per request: the connection is already open by the time X-Mock is read. Set it on the control socket"
		}
		return mode, "header", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.script) > 0 {
		mode := s.script[0]
		s.script = s.script[1:]
		return mode, "script", ""
	}
	return s.defaultMode, "default", ""
}

// --- await ------------------------------------------------------------------

// notify wakes anything blocked in Await. The channel is replaced rather than
// signalled per waiter so a broadcast needs no bookkeeping.
func (s *Server) notify() {
	s.mu.Lock()
	if s.waiters != nil {
		close(s.waiters)
		s.waiters = nil
	}
	s.mu.Unlock()
}

func (s *Server) changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiters == nil {
		s.waiters = make(chan struct{})
	}
	return s.waiters
}

// Await blocks until `count` request records exist at or after `since`, or the
// deadline passes. It reports whether the count was reached.
func (s *Server) Await(since int64, count int, timeout time.Duration) bool {
	if count <= 0 {
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		changed := s.changed()
		if s.recording.CountRequestsSince(since) >= count {
			return true
		}
		select {
		case <-changed:
		case <-deadline.C:
			return s.recording.CountRequestsSince(since) >= count
		}
	}
}

// --- request handling -------------------------------------------------------

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	arrival := time.Now()
	info, _ := r.Context().Value(connKey{}).(connInfo)

	record := &Record{
		ConnID:     info.id,
		At:         arrival.UTC().Format(time.RFC3339Nano),
		AtUnixMS:   arrival.UnixMilli(),
		SinceStart: arrival.Sub(s.started).Microseconds(),
		Remote:     r.RemoteAddr,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Proto:      r.Proto,
		Host:       r.Host,
		Headers:    r.Header.Clone(),
	}

	body, total, truncated := s.readBody(r.Body)
	record.setBody(body, total, truncated)

	eventCount := 0
	if r.Method == http.MethodPost {
		envelope, envelopeErr := summarise(body)
		record.Envelope, record.EnvelopeError = envelope, envelopeErr
		if envelope != nil {
			eventCount = envelope.EventCount
		}
	}

	// A mode is resolved only for the one request a mode is about. A preflight,
	// a wrong method or a wrong path must not consume a script step: C8c's
	// program is positional, and a step spent on an OPTIONS would shift every
	// assertion after it by one.
	var (
		mode    Mode
		source  string
		modeErr string
	)
	if r.Method == http.MethodPost && r.URL.Path == IngestPath {
		mode, source, modeErr = s.resolveMode(r.Header.Get(MockHeader))
		record.Mode, record.ModeSource, record.MockError = mode.Raw, source, modeErr
		if modeErr != "" {
			// An X-Mock nobody can parse must not quietly behave like `ok`: a
			// scenario would then pass while testing nothing. 500 is not a
			// status §2 hands a mode, so it cannot be mistaken for one being
			// exercised.
			record.Mode = r.Header.Get(MockHeader)
		}
	}

	s.recording.Begin(record)
	s.notify()

	response := s.respond(w, r, record, mode, modeErr, eventCount, info)
	s.recording.Complete(record, response, time.Since(s.started).Microseconds())
	s.notify()
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, record *Record, mode Mode, modeErr string, eventCount int, info connInfo) *Response {
	cors(w)

	if modeErr != "" {
		s.log.Error("mockd cannot parse a mode", "x_mock", r.Header.Get(MockHeader), "error", modeErr)
		w.Header().Set("X-Mock-Error", oneLine(modeErr))
		return write(w, http.StatusInternalServerError, `{"error":"mockd_bad_mode"}`, 0)
	}

	// The route contract, ahead of the mode: an SDK that calls the wrong path
	// or the wrong method is answered the way the server answers, and is
	// recorded either way.
	switch {
	case r.Method == http.MethodOptions:
		// wire §1: a permissive preflight. §2a gives it 204 and no body.
		return write(w, http.StatusNoContent, "", 0)
	case r.URL.Path != IngestPath:
		return write(w, http.StatusNotFound, `{"error":"not_found"}`, 0)
	case r.Method != http.MethodPost:
		// §2a: any method other than POST or OPTIONS is 405 with no body.
		return write(w, http.StatusMethodNotAllowed, "", 0)
	}

	delay := mode.Delay
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return &Response{Status: 0, DelayUS: delay.Microseconds()}
		}
	}

	if mode.Hangup {
		// Accept, record, then close without answering: an attempt that `down`
		// would have made invisible (doc.go, "What the recording cannot
		// express").
		s.hangup(w, info)
		return &Response{Status: 0, DelayUS: delay.Microseconds(), Hangup: true}
	}

	if mode.HasRetryAfter {
		w.Header().Set("Retry-After", mode.RetryAfter)
	}
	status := mode.Status
	if status == 0 {
		status = http.StatusAccepted
	}
	body := mode.Body(eventCount, s.now())
	return write(w, status, body, delay)
}

func (s *Server) hangup(w http.ResponseWriter, info connInfo) {
	if hijacker, ok := w.(http.Hijacker); ok {
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
			return
		}
	}
	if info.conn != nil {
		_ = info.conn.Close()
	}
}

func write(w http.ResponseWriter, status int, body string, delay time.Duration) *Response {
	if body != "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if body != "" {
		_, _ = io.WriteString(w, body)
	}
	return &Response{
		Status:  status,
		Headers: w.Header().Clone(),
		Body:    body,
		Bytes:   len(body),
		DelayUS: delay.Microseconds(),
	}
}

// readBody keeps at most maxBody bytes but counts every one, so a recording can
// still say how large an over-cap body was. W1 bounds a body at 64 KB and C6 at
// a 1 MB queue file; both are assertions about the length.
func (s *Server) readBody(reader io.Reader) ([]byte, int64, bool) {
	if reader == nil {
		return nil, 0, false
	}
	limit := s.maxBody
	if limit <= 0 {
		limit = 1 << 20
	}
	kept, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return kept, int64(len(kept)), false
	}
	total := int64(len(kept))
	if int64(len(kept)) < limit {
		return kept, total, false
	}
	rest, err := io.Copy(io.Discard, reader)
	if err != nil {
		return kept, total + rest, true
	}
	return kept, total + rest, rest > 0
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

func oneLine(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}
