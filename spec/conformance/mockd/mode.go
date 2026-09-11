package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Mode is a parsed `spec/sdk-conformance.md` §2 mode: one or more `+`-joined
// clauses, resolved into everything the response writer needs. See doc.go for
// the grammar and for which clauses are extensions.
type Mode struct {
	Raw string

	Delay time.Duration // slow:<ms>

	Down   bool // the listener is closed; connections are refused
	Hangup bool // accept, record, close without answering

	// Exactly one of the following decides the response, except that Reject
	// and Stop compose into a single 202 body.
	Status   int  // 0 when no status clause was given
	Garbage  bool // a body that is not JSON
	Huge     bool
	HugeSize int

	Reject       bool
	RejectReason string
	RejectField  string

	Stop        bool
	StopSeconds int64
	StopScope   string

	// RetryAfter is sent verbatim when Has is true. `429:` parses to
	// Has == false: an empty value omits the header entirely rather than
	// sending an empty one (§2).
	RetryAfter    string
	HasRetryAfter bool

	// ErrorCode overrides the §2a body for a 400 (400:<code>).
	ErrorCode string
}

const (
	defaultHugeSize = 8 << 20
	// defaultRetryAfter is §2's fixed value for a bare `429` / `503`. The
	// production server answers `1`; §2 says `2` and §2 is what an SDK author
	// reads when writing against the mock, so the mock says 2.
	defaultRetryAfter = "2"
)

// statusBodies are the exact bytes `spec/wire-v1.md` §2a gives each status. A
// status §2a does not name answers `{}`; 204 and 405 carry no body at all.
var statusBodies = map[int]string{
	202: "{}",
	204: "",
	400: `{"error":"malformed"}`,
	402: `{"error":"payment_required"}`,
	405: "",
	413: `{"error":"too_large"}`,
	429: "{}",
	500: `{"error":"internal"}`,
	503: "{}",
}

// envelopeErrorCodes are §2a's four `400` bodies. The two fixture-mode codes
// §2a lists for completeness are deliberately absent: nothing a production
// sender can provoke, and an SDK has no branch for them.
var envelopeErrorCodes = map[string]bool{
	"malformed":           true,
	"unsupported_version": true,
	"empty":               true,
	"too_many_events":     true,
}

// ParseMode parses a mode string. An empty string is `ok`.
func ParseMode(raw string) (Mode, error) {
	mode := Mode{Raw: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		mode.Raw = "ok"
		mode.Status = 202
		return mode, nil
	}
	mode.Raw = trimmed

	var bodyClause string // the clause that decided the status, for conflict messages
	claim := func(name string) error {
		if bodyClause != "" {
			return fmt.Errorf("clause %q conflicts with %q: at most one clause may decide the response", name, bodyClause)
		}
		bodyClause = name
		return nil
	}

	for _, clause := range strings.Split(trimmed, "+") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			return Mode{}, errors.New("empty clause: a mode is one or more `+`-joined clauses")
		}
		name, value, hasValue := strings.Cut(clause, ":")

		switch {
		case name == "ok":
			if hasValue {
				return Mode{}, errors.New("`ok` takes no value")
			}
			if err := claim("ok"); err != nil {
				return Mode{}, err
			}
			mode.Status = 202

		case name == "reject":
			if !hasValue || value == "" {
				return Mode{}, errors.New("`reject` needs a reason: reject:<reason> (spec/wire-v1.md §6)")
			}
			if mode.Reject {
				return Mode{}, errors.New("`reject` given twice")
			}
			reason, field, _ := strings.Cut(value, ":")
			if reason == "" {
				return Mode{}, errors.New("`reject` needs a reason: reject:<reason>")
			}
			// The reason is NOT checked against §6's enum. A mock that closed
			// the set could not be pointed at a reason the server grew, and a
			// scenario asserting what an SDK does with an unknown reason could
			// not be written at all.
			if strings.ContainsAny(reason, "+ \t") || strings.ContainsAny(field, "+ \t") {
				return Mode{}, fmt.Errorf("reject:%q: a reason and a field carry no `+` and no space", value)
			}
			// reject composes with stop, and with nothing else.
			if bodyClause != "" && bodyClause != "stop" {
				return Mode{}, fmt.Errorf("clause %q conflicts with %q", clause, bodyClause)
			}
			if bodyClause == "" {
				bodyClause = "reject"
			}
			mode.Reject, mode.RejectReason, mode.RejectField = true, reason, field
			mode.Status = 202

		case name == "stop":
			if !hasValue {
				return Mode{}, errors.New("`stop` needs a duration: stop:<seconds>")
			}
			if mode.Stop {
				return Mode{}, errors.New("`stop` given twice")
			}
			secondsText, scope, hasScope := strings.Cut(value, ":")
			seconds, err := strconv.ParseInt(secondsText, 10, 64)
			if err != nil {
				return Mode{}, fmt.Errorf("stop:%q: seconds must be a whole number", value)
			}
			if !hasScope || scope == "" {
				scope = "app"
			}
			if scope != "app" && scope != "web" {
				return Mode{}, fmt.Errorf("stop scope %q: spec/wire-v1.md §8 has only `app` and `web`", scope)
			}
			if bodyClause != "" && bodyClause != "reject" {
				return Mode{}, fmt.Errorf("clause %q conflicts with %q", clause, bodyClause)
			}
			if bodyClause == "" {
				bodyClause = "stop"
			}
			mode.Stop, mode.StopSeconds, mode.StopScope = true, seconds, scope
			mode.Status = 202

		case name == "slow":
			if !hasValue {
				return Mode{}, errors.New("`slow` needs a delay: slow:<ms>")
			}
			if mode.Delay != 0 {
				return Mode{}, errors.New("`slow` given twice")
			}
			millis, err := strconv.Atoi(value)
			if err != nil || millis < 0 {
				return Mode{}, fmt.Errorf("slow:%q: milliseconds must be a whole number >= 0", value)
			}
			mode.Delay = time.Duration(millis) * time.Millisecond

		case name == "down":
			if hasValue {
				return Mode{}, errors.New("`down` takes no value")
			}
			mode.Down = true

		case name == "hangup":
			if hasValue {
				return Mode{}, errors.New("`hangup` takes no value")
			}
			if err := claim("hangup"); err != nil {
				return Mode{}, err
			}
			mode.Hangup = true

		case name == "garbage":
			if hasValue {
				return Mode{}, errors.New("`garbage` takes no value")
			}
			if err := claim("garbage"); err != nil {
				return Mode{}, err
			}
			mode.Garbage, mode.Status = true, 202

		case name == "huge":
			if err := claim("huge"); err != nil {
				return Mode{}, err
			}
			size := defaultHugeSize
			if hasValue {
				parsed, err := strconv.Atoi(value)
				if err != nil || parsed <= 0 {
					return Mode{}, fmt.Errorf("huge:%q: bytes must be a whole number > 0", value)
				}
				size = parsed
			}
			mode.Huge, mode.HugeSize, mode.Status = true, size, 202

		case isStatusName(name):
			status, _ := strconv.Atoi(name)
			if err := claim(name); err != nil {
				return Mode{}, err
			}
			mode.Status = status
			switch {
			case !hasValue:
				if status == 429 || status == 503 {
					mode.RetryAfter, mode.HasRetryAfter = defaultRetryAfter, true
				}
			case status == 429 || status == 503:
				// The literal value, verbatim and unclamped. Empty omits the
				// header: `429:` is the "absent" edge of wire §9, and an empty
				// Retry-After is a DIFFERENT thing from no Retry-After.
				if value != "" {
					mode.RetryAfter, mode.HasRetryAfter = value, true
				}
			case status == 400:
				if !envelopeErrorCodes[value] {
					return Mode{}, fmt.Errorf("400:%q: spec/wire-v1.md §2a's envelope errors are malformed, unsupported_version, empty, too_many_events", value)
				}
				mode.ErrorCode = value
			default:
				return Mode{}, fmt.Errorf("%s:%q: a value is only meaningful on 429 and 503 (Retry-After) and on 400 (the error code)", name, value)
			}

		default:
			return Mode{}, fmt.Errorf("unknown mode clause %q; spec/sdk-conformance.md §2 has ok, reject:, 429, 503, 429:<v>, 503:<v>, 400, 402, stop:<seconds>, slow:<ms>, down", clause)
		}
	}

	if mode.Down && mode.Raw != "down" {
		return Mode{}, errors.New("`down` composes with nothing: the connection is refused before any of it could apply")
	}
	if mode.Status == 0 && !mode.Down && !mode.Hangup {
		// `slow:500` on its own delays an `ok`.
		mode.Status = 202
	}
	return mode, nil
}

func isStatusName(name string) bool {
	if len(name) != 3 {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	status, err := strconv.Atoi(name)
	return err == nil && status >= 100
}

// Body returns the exact bytes this mode answers with, given how many events
// the request's envelope carried and the `now` from which a stop: is measured.
func (m Mode) Body(eventCount int, now time.Time) string {
	switch {
	case m.Garbage:
		// 202, Content-Type: application/json, and a body that is not JSON:
		// C10's "invalid JSON". Truncated rather than random so it is obvious
		// in a log what it was meant to be.
		return `{"rejected":[{"i":0,"reason":`
	case m.Huge:
		return hugeBody(m.HugeSize)
	case m.Reject || m.Stop:
		return acceptedBody(m, eventCount, now)
	case m.Status == 400 && m.ErrorCode != "":
		return `{"error":"` + m.ErrorCode + `"}`
	}
	if body, ok := statusBodies[m.Status]; ok {
		return body
	}
	return "{}"
}

// acceptedBody builds the one 202 body that carries a rejection list, a stop,
// or both. It is assembled by hand rather than by json.Marshal so that the
// bytes on the wire are the bytes in this file -- a mock whose body shape is a
// struct's field order is a mock whose contract moved when somebody reordered
// the struct.
func acceptedBody(m Mode, eventCount int, now time.Time) string {
	var builder strings.Builder
	builder.WriteByte('{')
	if m.Reject {
		if eventCount < 1 {
			// A body mockd could not read still gets an answer; the runner
			// sees envelope_error on the record and knows why there is one
			// entry rather than n.
			eventCount = 1
		}
		builder.WriteString(`"rejected":[`)
		for i := 0; i < eventCount; i++ {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(`{"i":`)
			builder.WriteString(strconv.Itoa(i))
			builder.WriteString(`,"reason":"`)
			builder.WriteString(m.RejectReason)
			builder.WriteByte('"')
			if m.RejectField != "" {
				builder.WriteString(`,"field":"`)
				builder.WriteString(m.RejectField)
				builder.WriteByte('"')
			}
			builder.WriteByte('}')
		}
		builder.WriteByte(']')
	}
	if m.Stop {
		if m.Reject {
			builder.WriteByte(',')
		}
		builder.WriteString(`"stop":{"until":`)
		builder.WriteString(strconv.FormatInt(now.Unix()+m.StopSeconds, 10))
		builder.WriteString(`,"scope":"`)
		builder.WriteString(m.StopScope)
		builder.WriteString(`"}`)
	}
	builder.WriteByte('}')
	return builder.String()
}

func hugeBody(size int) string {
	const head = `{"rejected":[],"pad":"`
	const tail = `"}`
	padding := size - len(head) - len(tail)
	if padding < 1 {
		padding = 1
	}
	var builder strings.Builder
	builder.Grow(size + 2)
	builder.WriteString(head)
	builder.WriteString(strings.Repeat("x", padding))
	builder.WriteString(tail)
	return builder.String()
}
