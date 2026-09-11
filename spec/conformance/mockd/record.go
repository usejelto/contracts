package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

// Record is one line of the recording. doc.go, "The recording", says which §4
// scenario each field exists for; this file only builds them.
type Record struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"` // "request" or "connection"
	ConnID int64  `json:"conn_id"`

	At          string `json:"at"`             // RFC3339 with nanoseconds, wall clock
	AtUnixMS    int64  `json:"at_unix_ms"`     // wall clock, for the runner's own frame
	SinceStart  int64  `json:"since_start_us"` // MONOTONIC, and the field a schedule is measured in
	RespondedUS int64  `json:"responded_us,omitempty"`

	Remote string `json:"remote"`

	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Query   string              `json:"query,omitempty"`
	Proto   string              `json:"proto,omitempty"`
	Host    string              `json:"host,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`

	Body          string `json:"body,omitempty"`
	BodyBase64    string `json:"body_base64,omitempty"`
	BodyBytes     int64  `json:"body_bytes"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`

	Envelope      *Envelope `json:"envelope,omitempty"`
	EnvelopeError string    `json:"envelope_error,omitempty"`

	Mode       string `json:"mode,omitempty"`
	ModeSource string `json:"mode_source,omitempty"` // header | script | default
	MockError  string `json:"mock_error,omitempty"`

	Response *Response `json:"response"`
}

// Response is what the client was told. A scenario asserts on this rather than
// re-deriving it from the mode, which is what lets C8b's two arms be compared
// as "the same run differing only in a header".
type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
	Bytes   int                 `json:"bytes"`
	DelayUS int64               `json:"delay_us,omitempty"`
	Hangup  bool                `json:"hangup,omitempty"`
}

// Envelope is the convenience summary of a readable request body. The raw body
// stays the authority; nothing here is re-serialised back onto the wire.
type Envelope struct {
	V          json.RawMessage `json:"v,omitempty"`
	P          string          `json:"p,omitempty"`
	EventCount int             `json:"event_count"`
	Events     []Event         `json:"events,omitempty"`
}

// Event keeps `t` as its JSON LITERAL. wire §3 makes a whole-number field one
// whose VALUE is whole, so `1.7855784e12` is a legal `t`, and C15b sends one
// that saturates int64 -- both survive a string and neither survives float64.
type Event struct {
	N   string          `json:"n,omitempty"`
	ID  string          `json:"id,omitempty"`
	T   json.RawMessage `json:"t,omitempty"`
	S   string          `json:"s,omitempty"`
	V   string          `json:"v,omitempty"`
	IID string          `json:"iid,omitempty"`
}

// Recording is the ordered log. It is the whole point of mockd: a gap here is
// a §4 scenario that cannot be written.
type Recording struct {
	mu      sync.Mutex
	records []*Record
	seq     int64
	conns   int64

	requests    int64
	connections int64
	mockErrors  int64
	dropped     int64

	max int

	file   *os.File
	writer *bufio.Writer
}

func NewRecording(max int, path string) (*Recording, error) {
	recording := &Recording{max: max}
	if path == "" {
		return recording, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	recording.file, recording.writer = file, bufio.NewWriter(file)
	return recording, nil
}

// NextConnID assigns a connection an id and records that it was accepted. C5
// asserts a socket was never opened, which no per-request log can answer.
func (r *Recording) NextConnID(remote string, at time.Time, sinceStart time.Duration) int64 {
	r.mu.Lock()
	r.conns++
	r.connections++
	id := r.conns
	record := &Record{
		Kind:       "connection",
		ConnID:     id,
		Remote:     remote,
		At:         at.UTC().Format(time.RFC3339Nano),
		AtUnixMS:   at.UnixMilli(),
		SinceStart: sinceStart.Microseconds(),
	}
	r.appendLocked(record)
	r.writeLineLocked(record)
	r.mu.Unlock()
	return id
}

// Begin appends a request record at ARRIVAL, before the response is decided,
// so that `seq` is arrival order and an `await` unblocks on a request that a
// `slow:` mode has not answered yet. Such a record carries "response": null.
func (r *Recording) Begin(record *Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record.Kind = "request"
	r.requests++
	if record.MockError != "" {
		r.mockErrors++
	}
	r.appendLocked(record)
}

// Complete fills in the response half of a record already begun, and only then
// writes the line to the -record file: a line in that file is always whole.
func (r *Recording) Complete(record *Record, response *Response, respondedUS int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record.Response = response
	record.RespondedUS = respondedUS
	r.writeLineLocked(record)
}

func (r *Recording) appendLocked(record *Record) {
	r.seq++
	record.Seq = r.seq
	r.records = append(r.records, record)
	if r.max > 0 && len(r.records) > r.max {
		drop := len(r.records) - r.max
		r.records = append(r.records[:0], r.records[drop:]...)
		r.dropped += int64(drop)
	}
}

func (r *Recording) writeLineLocked(record *Record) {
	if r.writer == nil {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	_, _ = r.writer.Write(line)
	_ = r.writer.WriteByte('\n')
	_ = r.writer.Flush()
}

// Since returns the records with seq >= since (0 meaning all), at most limit
// (0 meaning all), and the seq a caller should pass next. They come back
// already marshalled, under the lock: a record whose response half a `slow:`
// request is still filling in must not be encoded from another goroutine.
func (r *Recording) Since(since, limit int64) ([]json.RawMessage, int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]json.RawMessage, 0, len(r.records))
	last := int64(0)
	for _, record := range r.records {
		if record.Seq < since {
			continue
		}
		line, err := json.Marshal(record)
		if err != nil {
			continue
		}
		out = append(out, line)
		last = record.Seq
		if limit > 0 && int64(len(out)) >= limit {
			break
		}
	}
	next := since
	if next < 1 {
		next = 1
	}
	switch {
	case last > 0:
		// A truncating `limit` must resume at the record after the last one
		// returned, never at the head of the log.
		next = last + 1
	case r.seq >= next:
		next = r.seq + 1
	}
	return out, next, r.dropped
}

// CountRequestsSince is what `await` blocks on: connection records do not
// count, because a scenario waiting for "the second batch" is waiting for a
// request and a keep-alive connection carries several.
func (r *Recording) CountRequestsSince(since int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, record := range r.records {
		if record.Seq >= since && record.Kind == "request" {
			count++
		}
	}
	return count
}

type Stats struct {
	Requests    int64 `json:"requests"`
	Connections int64 `json:"connections"`
	MockErrors  int64 `json:"mock_errors"`
	Dropped     int64 `json:"dropped"`
}

func (r *Recording) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Requests: r.requests, Connections: r.connections, MockErrors: r.mockErrors, Dropped: r.dropped}
}

// Reset is the scenario boundary. seq restarts at 1 and the -record file is
// truncated, so a scenario's recording is exactly its own.
func (r *Recording) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
	r.seq, r.conns = 0, 0
	r.requests, r.connections, r.mockErrors, r.dropped = 0, 0, 0, 0
	if r.file != nil {
		_ = r.writer.Flush()
		_ = r.file.Truncate(0)
		_, _ = r.file.Seek(0, 0)
	}
}

func (r *Recording) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.writer.Flush()
		_ = r.file.Close()
		r.file, r.writer = nil, nil
	}
}

// setBody stores the raw bytes. A body that is valid UTF-8 is kept as a string
// so a recording is readable; anything else is base64, so that a body which is
// not text is still byte-exact.
func (record *Record) setBody(body []byte, total int64, truncated bool) {
	record.BodyBytes = total
	record.BodyTruncated = truncated
	if len(body) == 0 {
		return
	}
	if utf8.Valid(body) {
		record.Body = string(body)
		return
	}
	record.BodyBase64 = base64.StdEncoding.EncodeToString(body)
}

// summarise reads the envelope for the convenience summary. It is deliberately
// lenient: every field is pulled out on its own, so one unreadable field does
// not cost the whole summary, and a body that is not an envelope at all leaves
// envelope_error rather than an empty record.
func summarise(body []byte) (*Envelope, string) {
	if len(body) == 0 {
		return nil, "empty body"
	}
	var raw struct {
		V json.RawMessage   `json:"v"`
		P json.RawMessage   `json:"p"`
		E []json.RawMessage `json:"e"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err.Error()
	}
	envelope := &Envelope{V: raw.V, P: text(raw.P), EventCount: len(raw.E)}
	for _, item := range raw.E {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			envelope.Events = append(envelope.Events, Event{})
			continue
		}
		envelope.Events = append(envelope.Events, Event{
			N:   text(fields["n"]),
			ID:  text(fields["id"]),
			T:   fields["t"],
			S:   text(fields["s"]),
			V:   text(fields["v"]),
			IID: text(fields["iid"]),
		})
	}
	return envelope, ""
}

// text unquotes a JSON string, or returns the literal when the value is not a
// string -- an `n` of `123` is recorded as "123" rather than dropped, and the
// raw body remains available to say which it was.
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return string(raw)
}
