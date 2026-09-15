package main

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
)

// Queue caps from RFC-0001 §8.3 item 6: "append-only file, cap 1 MB or 1 000
// events; oldest dropped first" (C6).
const (
	queueMaxBytes  = 1 << 20
	queueMaxEvents = 1000
)

// QueuedEvent is one line of queue.jsonl. Only what cannot be recomputed at
// send time is stored: spec/wire-v1.md §4 says a heartbeat "carries the app's
// CURRENT values every time", so install properties are resolved when the
// request is built and never frozen into the queue (C22's first heartbeat
// carries a `license` set after the heartbeat was enqueued).
type QueuedEvent struct {
	ID    string          `json:"id"`
	N     string          `json:"n"`
	T     json.RawMessage `json:"t"`
	Props map[string]any  `json:"props,omitempty"`
	// Heartbeat marks the events whose props come from the install properties
	// at send time rather than from this record.
	Heartbeat bool           `json:"hb,omitempty"`
	Metadata  *EventMetadata `json:"metadata,omitempty"`
}

type EventMetadata struct {
	Platform  Platform `json:"platform"`
	Version   string   `json:"version"`
	InstallID string   `json:"install_id,omitempty"`
}

// Queue is the append-only event file plus the in-memory mirror the sender
// batches from. Both are always in step: every mutation rewrites the file.
type Queue struct {
	mu   sync.Mutex
	path string
	// entries keeps each event beside the line it encodes to, so appending is
	// one Marshal and the byte cap is a running sum rather than a re-encode of
	// the whole file per call. C6 appends 1 500 times against a 1 000-event
	// cap; the naive form is 1.5 million Marshals.
	entries []entry
	bytes   int
	// dropped counts what the cap discarded, for the debug log.
	dropped int
	// The last durable handoff remains recognized after cap eviction, until the
	// state intent is retired. Only one intent can be in flight at a time.
	recoveredUpdateID string
}

type entry struct {
	event QueuedEvent
	line  []byte
}

func NewQueue(path string) *Queue { return &Queue{path: path} }

// Load reads the file if it exists. Like Load on the state store it creates
// nothing: before `init` the SDK touches no file (§8.2 item 5).
func (q *Queue) Load(pendingUpdateID ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries, q.bytes, q.recoveredUpdateID = nil, 0, ""
	raw, err := os.ReadFile(q.path)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			QueuedEvent
			RecoveredUpdateID string `json:"recovered_update_id"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			// A half-written line from a killed process costs that event, not
			// the file (§8.3 item 10).
			continue
		}
		if record.RecoveredUpdateID != "" {
			q.recoveredUpdateID = record.RecoveredUpdateID
			continue
		}
		// Legacy checkpoints lack a receipt. Finding the exact pending ID proves
		// handoff even if a failed recovery write is followed by cap eviction.
		if q.recoveredUpdateID == "" && len(pendingUpdateID) > 0 && record.ID == pendingUpdateID[0] {
			q.recoveredUpdateID = record.ID
		}
		q.appendLocked(record.QueuedEvent, append([]byte(nil), line...))
	}
}

func (q *Queue) appendLocked(event QueuedEvent, line []byte) {
	q.entries = append(q.entries, entry{event: event, line: line})
	q.bytes += len(line) + 1
}

// Append enqueues one event and applies the cap, oldest first.
func (q *Queue) Append(event QueuedEvent) (dropped int) {
	line, err := json.Marshal(event)
	if err != nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.appendLocked(event, line)
	dropped = q.trimLocked()
	q.dropped += dropped
	q.persistLocked()
	return dropped
}

// trimLocked caps serialized live events; recovery receipts are bookkeeping,
// excluded from the byte cap by spec/sdk-conformance.md §3.2.
func (q *Queue) trimLocked() int {
	dropped := 0
	for len(q.entries) > queueMaxEvents || (len(q.entries) > 1 && q.bytes > queueMaxBytes) {
		q.bytes -= len(q.entries[0].line) + 1
		q.entries = q.entries[1:]
		dropped++
	}
	return dropped
}

// Head returns up to n events from the front without removing them: a batch is
// only removed once the server has accepted it (§8.3 item 8 retries the SAME
// batch, and spec/wire-v1.md §6 forbids altering its `id`s).
func (q *Queue) Head(n int) []QueuedEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > len(q.entries) {
		n = len(q.entries)
	}
	out := make([]QueuedEvent, n)
	for i := 0; i < n; i++ {
		out[i] = q.entries[i].event
	}
	return out
}

// Remove drops the first n events. Called only on a 202 or on a final refusal.
func (q *Queue) Remove(n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > len(q.entries) {
		n = len(q.entries)
	}
	for i := 0; i < n; i++ {
		q.bytes -= len(q.entries[i].line) + 1
	}
	q.entries = q.entries[n:]
	q.persistLocked()
}

func (q *Queue) RemoveEvents(sent []QueuedEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make(map[string]bool, len(sent))
	for _, event := range sent {
		ids[event.ID] = true
	}
	kept := q.entries[:0]
	q.bytes = 0
	for _, entry := range q.entries {
		if ids[entry.event.ID] {
			continue
		}
		kept = append(kept, entry)
		q.bytes += len(entry.line) + 1
	}
	q.entries = kept
	q.persistLocked()
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Contains reports whether an event with this name is queued. `install` is
// enqueued once per launch at most, and only when the previous launch's copy
// is not still waiting (RFC-0001 §8.2 item 4, C4 "exactly one install event").
func (q *Queue) Contains(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.entries {
		if q.entries[i].event.N == name {
			return true
		}
	}
	return false
}

func (q *Queue) persistLocked() {
	if len(q.entries) == 0 && q.recoveredUpdateID == "" {
		_ = os.Remove(q.path)
		return
	}
	atomicWrite(q.path, q.contentsLocked(), false)
}

func (q *Queue) receiptLineLocked() []byte {
	if q.recoveredUpdateID == "" {
		return nil
	}
	line, _ := json.Marshal(struct {
		RecoveredUpdateID string `json:"recovered_update_id"`
	}{q.recoveredUpdateID})
	return append(line, '\n')
}

func (q *Queue) contentsLocked() []byte {
	receipt := q.receiptLineLocked()
	buffer := bytes.NewBuffer(make([]byte, 0, q.bytes+len(receipt)))
	buffer.Write(receipt)
	for i := range q.entries {
		buffer.Write(q.entries[i].line)
		buffer.WriteByte('\n')
	}
	return buffer.Bytes()
}

func (q *Queue) Recover(event QueuedEvent) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	previousEntries, previousBytes, previousReceipt := q.entries, q.bytes, q.recoveredUpdateID
	q.entries = append([]entry(nil), q.entries...)
	found := false
	for _, entry := range q.entries {
		if entry.event.ID == event.ID {
			found = true
			break
		}
	}
	if q.recoveredUpdateID != event.ID && !found {
		line, err := json.Marshal(event)
		if err != nil {
			return false
		}
		q.appendLocked(event, line)
	}
	q.recoveredUpdateID = event.ID
	q.trimLocked()
	if atomicWrite(q.path, q.contentsLocked(), true) {
		return true
	}
	q.entries, q.bytes, q.recoveredUpdateID = previousEntries, previousBytes, previousReceipt
	return false
}

func (q *Queue) DiscardIdentityEvents() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := make([]entry, 0, len(q.entries))
	total := 0
	var buffer bytes.Buffer
	buffer.Write(q.receiptLineLocked())
	for _, entry := range q.entries {
		if entry.event.N == "app_updated" || entry.event.N == "install" {
			continue
		}
		kept = append(kept, entry)
		total += len(entry.line) + 1
		buffer.Write(entry.line)
		buffer.WriteByte('\n')
	}
	if !atomicWrite(q.path, buffer.Bytes(), true) {
		return false
	}
	q.entries = kept
	q.bytes = total
	return true
}

// Delete is disable()'s half of §8.7 item 18.
func (q *Queue) Delete() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries, q.bytes, q.recoveredUpdateID = nil, 0, ""
	_ = os.Remove(q.path)
}
