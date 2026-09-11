package main

import (
	"encoding/json"
	"strings"
)

// Export exposes the semantic facts required by conformance §3.2, independently
// of storage format. Omit BackoffFails: it is recomputable host bookkeeping,
// not a required persisted fact.
type Export struct {
	InstallID      string `json:"install_id"` // §8.2 item 2; "" before init and after disable()
	LastAppVersion string `json:"last_app_version,omitempty"`

	LastHeartbeatDay string `json:"last_heartbeat_day,omitempty"` // §8.2 item 3, floor(ms / 86 400 000)

	InstallClaimed  bool              `json:"install_claimed"`             // §8.2 item 4
	InstallDueAt    string            `json:"install_due_at,omitempty"`    // §8.2 item 4's deadline, not a countdown (C4c)
	InstallFirstTry string            `json:"install_first_try,omitempty"` // §8.2 item 4's 30-day window (C4b)
	InstallProps    map[string]string `json:"install_props,omitempty"`     // §8.1 setProps (C22, C22d)

	BackoffStepMS int64  `json:"backoff_step_ms,omitempty"` // §8.3 item 8; 0 or absent means "not in backoff"
	BackoffNextAt string `json:"backoff_next_at,omitempty"` // §8.3 item 8; a step does not say WHEN (C8c)

	StopUntil    string `json:"stop_until,omitempty"`     // §8.6 item 16 and wire §8 (C16)
	StopProbeDue bool   `json:"stop_probe_due,omitempty"` // wire §8's single-heartbeat probe, owed across a relaunch

	Queue QueueExport `json:"queue"` // §8.3 item 6 (C6, C4b, C18)
}

// QueueExport is §3.2's `queue`: what the SDK is holding, oldest first, and how
// much it counts that as. There is no count field beside `events` on purpose --
// §3.2: "a count and a list that disagreed would leave the runner to choose
// between them on exactly the row (C6) that is testing the cap".
type QueueExport struct {
	Bytes  int             `json:"bytes"`
	Events []ExportedEvent `json:"events"`
}

// ExportedEvent carries the three fields the wire fixes when an event is
// ENQUEUED and which the SDK must not regenerate afterwards (§3.2): `id`
// (wire §6, a retry resends the same batch with the same `id`s), `n` and `t`.
// Properties are not exported; they are asserted in mockd's recording, where
// they are visible.
type ExportedEvent struct {
	ID string `json:"id"`
	N  string `json:"n"`
	T  string `json:"t"`
}

// Export reads held state without loading, creating, writing, or inferring values.
// Before init it must leave the state directory untouched (C5).
func (s *SDK) Export() Export {
	s.mu.Lock()
	started, ready := s.started, s.ready
	s.mu.Unlock()
	if started && ready != nil {
		// init is asynchronous (§8.2 item 1), so a `dumpstate` that overtook
		// bootstrap would read a state the SDK has not finished loading and
		// report facts it does in fact hold as absent. This waits for the load
		// `init` already started; it starts none, and before `init` there is
		// nothing to wait for.
		<-ready
	}
	state := s.store.Get()
	props := map[string]string{}
	for key, value := range state.InstallProps {
		props[key] = value
	}
	return Export{
		InstallID:        state.InstallID,
		LastAppVersion:   state.LastAppVersion,
		LastHeartbeatDay: state.LastHeartbeatDay,
		InstallClaimed:   state.InstallClaimed,
		InstallDueAt:     state.InstallDueAt,
		InstallFirstTry:  state.InstallFirstTry,
		InstallProps:     props,
		BackoffStepMS:    state.BackoffStepMS,
		BackoffNextAt:    state.BackoffNextAt,
		StopUntil:        state.StopUntilMS,
		StopProbeDue:     state.StopProbeDue,
		Queue:            s.queue.Export(),
	}
}

// Export is the queue half of §3.2. `bytes` is "what the SDK counts against its
// own cap, not what a file system reports": it is the same running sum
// trimLocked caps against, so C6's 1 MB half is read off the figure the cap
// actually binds on rather than off a stat of a file no other SDK has to have.
func (q *Queue) Export() QueueExport {
	q.mu.Lock()
	defer q.mu.Unlock()
	events := make([]ExportedEvent, 0, len(q.entries))
	for i := range q.entries {
		events = append(events, ExportedEvent{
			ID: q.entries[i].event.ID,
			N:  q.entries[i].event.N,
			T:  instantString(q.entries[i].event.T),
		})
	}
	return QueueExport{Bytes: q.bytes, Events: events}
}

// instantString renders a stored `t` as §3.2's decimal string. QueuedEvent.T is
// a json.RawMessage precisely so the digits are never decoded: C15b's clock is
// past int64, and a value that went through float64 would come back rounded --
// `1.7855784e12` as 1785578400000, `99999999999999999999` as
// 100000000000000000000, the trap mockd's doc.go records for the wire's own
// `t`. Nothing here parses; it re-encodes the bytes the queue already holds.
func instantString(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}
