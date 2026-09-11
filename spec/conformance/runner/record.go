package main

import (
	"encoding/json"
)

// Record keeps wire numbers such as t as json.RawMessage; float64 decoding
// would round C15b values and lose their original exponent syntax.
type Record struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	ConnID int64  `json:"conn_id"`

	AtUnixMS     int64 `json:"at_unix_ms"`
	SinceStartUS int64 `json:"since_start_us"`
	RespondedUS  int64 `json:"responded_us"`

	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`

	Body          string `json:"body"`
	BodyBase64    string `json:"body_base64"`
	BodyBytes     int64  `json:"body_bytes"`
	BodyTruncated bool   `json:"body_truncated"`

	Envelope      *Envelope `json:"envelope"`
	EnvelopeError string    `json:"envelope_error"`

	Mode       string `json:"mode"`
	ModeSource string `json:"mode_source"`
	MockError  string `json:"mock_error"`

	Response *Response `json:"response"`
}

type Envelope struct {
	V          json.RawMessage `json:"v"`
	P          string          `json:"p"`
	EventCount int             `json:"event_count"`
	Events     []Event         `json:"events"`
}

// Event is mockd's per-event summary. `t` stays a raw literal: C15 and C15b are
// assertions about the exact decimal an SDK put on the wire.
type Event struct {
	N   string          `json:"n"`
	ID  string          `json:"id"`
	T   json.RawMessage `json:"t"`
	S   string          `json:"s"`
	V   string          `json:"v"`
	IID string          `json:"iid"`
}

type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	Bytes   int                 `json:"bytes"`
	DelayUS int64               `json:"delay_us"`
	Hangup  bool                `json:"hangup"`
}

// Requests filters the ingest requests out of a recording, in arrival order.
// A connection record is not a request and an OPTIONS is not one either: a
// scenario counting "batches" means POST /v1/e.
func Requests(records []Record) []Record {
	var out []Record
	for _, record := range records {
		if record.Kind == "request" && record.Method == "POST" && record.Path == "/v1/e" {
			out = append(out, record)
		}
	}
	return out
}

// AllRequests includes the wrong methods and wrong paths, so a scenario can
// assert an SDK never made one.
func AllRequests(records []Record) []Record {
	var out []Record
	for _, record := range records {
		if record.Kind == "request" {
			out = append(out, record)
		}
	}
	return out
}

func Connections(records []Record) []Record {
	var out []Record
	for _, record := range records {
		if record.Kind == "connection" {
			out = append(out, record)
		}
	}
	return out
}

// header reads one header value case-insensitively (mockd stores them
// canonicalised by net/http, but a runner should not depend on that).
func header(values map[string][]string, name string) (string, bool) {
	for key, list := range values {
		if len(list) == 0 {
			continue
		}
		if equalFold(key, name) {
			return list[0], true
		}
	}
	return "", false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
