package wirecheck

import (
	"strings"
)

// C17 debug lines use "jelto: "; payloads use "jelto: POST " plus the exact body.
// Wire string escaping keeps each body on one line. Skip unrelated application logs.

const (
	linePrefix = "jelto: "
	postPrefix = "jelto: POST "
)

// Kind classifies one captured log line.
type Kind uint8

const (
	// KindOther is a line the SDK did not write.
	KindOther Kind = iota
	// KindPost is a request body, to be validated.
	KindPost
	// KindClientLoss is a value rejected before sending, invisible to server counters.
	KindClientLoss
	// KindServerRejection echoes a rejection already counted by the server (wire §6).
	KindServerRejection
	// KindBatchDropped identifies a whole-batch 400/402. Envelope failures may occur
	// before product resolution and therefore have no attributable server counter.
	KindBatchDropped
	// KindFlushGaveUp is data lost at process exit (RFC-0001 §8.3 item 7).
	KindFlushGaveUp
	// KindOperational is a retry, a kill switch, a backoff note. Not a loss.
	KindOperational
)

// lossRule matches one client-side loss message. Every row cites the SDK source
// that writes it, so this table can be re-derived by grepping `log.log(`.
type lossRule struct {
	match  string // a prefix of the message, or a substring when `contains`
	source string
	lost   string // what did not reach the server
	// contains makes `match` a substring test instead of a prefix test, for the
	// two messages whose distinguishing text is not at the front.
	contains bool
}

// Match the complete SDK loss vocabulary, including messages without a "drop"
// prefix. Unsupported platforms can suppress all traffic without a drop-prefixed line.
// Re-derive the table from SDK log calls when the diagnostic vocabulary changes.
var lossRules = []lossRule{
	{match: "drop event ", source: "Wire.swift eventName / validateTrackProps; Jelto.swift track", lost: "the whole event"},
	{match: "drop onboarding step ", source: "Wire.swift WireGate.onboarding", lost: "the whole event"},
	{match: "drop install property ", source: "Wire.swift WireGate.installProps", lost: "one install property; the heartbeat is still sent without it"},
	{match: "drop setprops: ", source: "Wire.swift WireGate.withinPropCap", lost: "the whole setProps call"},
	{match: "drop app slug ", source: "Wire.swift WireGate.appSlug", lost: "the `a` field; `app` falls back to `os`"},
	{match: "client version ", source: "Wire.swift WireGate.clientVersion (W4)", lost: "the `v` field; the Ops card cannot name the release", contains: false},
	{match: "`os` is macos|windows|linux", source: "Wire.swift Platform.detect", lost: "EVERYTHING -- this build can never send an event", contains: true},
	{match: "`arch` is arm64|x64|x86", source: "Wire.swift Platform.detect", lost: "EVERYTHING -- this build can never send an event", contains: true},
}

// Line is one classified line of a capture.
type Line struct {
	Number  int
	Kind    Kind
	Message string // the line with "jelto: " stripped; for KindPost, the body
	Lost    string // KindClientLoss only: what did not reach the server
	Source  string // KindClientLoss only: the SDK call site
}

// Classify decides what one raw capture line is. `number` is 1-indexed and is
// carried through so a report can point at the capture.
func Classify(number int, raw string) Line {
	line := Line{Number: number, Kind: KindOther}
	if !strings.HasPrefix(raw, linePrefix) {
		return line
	}
	if strings.HasPrefix(raw, postPrefix) {
		line.Kind = KindPost
		line.Message = raw[len(postPrefix):]
		return line
	}
	message := raw[len(linePrefix):]
	line.Message = message

	for _, rule := range lossRules {
		hit := strings.HasPrefix(message, rule.match)
		if rule.contains {
			hit = strings.Contains(message, rule.match)
		}
		if !hit {
			continue
		}
		// `client version ` also prefixes no other message, but assert the tail
		// so a future non-loss line starting the same way is not miscounted.
		if rule.match == "client version " && !strings.Contains(message, "is omitted") {
			continue
		}
		line.Kind = KindClientLoss
		line.Lost = rule.lost
		line.Source = rule.source
		return line
	}

	switch {
	case strings.HasPrefix(message, "event ") && strings.Contains(message, " rejected: "):
		line.Kind = KindServerRejection
	case strings.HasPrefix(message, "batch dropped: "):
		line.Kind = KindBatchDropped
	case strings.HasPrefix(message, "termination flush gave up"):
		line.Kind = KindFlushGaveUp
	default:
		line.Kind = KindOperational
	}
	return line
}
