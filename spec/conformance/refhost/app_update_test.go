package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func interruptedUpdate() QueuedEvent {
	return QueuedEvent{
		ID: "01991ba0-4000-7000-8000-000000000001", N: "app_updated", T: json.RawMessage(pinnedMS),
		Props:    map[string]any{"from_version": "A", "to_version": "B"},
		Metadata: &EventMetadata{Platform: Platform{AppVersion: "B", OS: "macos", OSVersion: "14", Arch: "arm64", Slug: "original"}, Version: "refhost/1", InstallID: "11111111-1111-4111-8111-111111111111"},
	}
}

func TestRecoverUpdateCommitAtBothQueueBoundaries(t *testing.T) {
	for _, alreadyQueued := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent-only", true: "intent-and-queue"}[alreadyQueued], func(t *testing.T) {
			h := newSleepHarness(t)
			event := interruptedUpdate()
			h.store.Load()
			h.store.Update(func(state *State) {
				state.LastAppVersion, state.PendingUpdate = "B", &event
				state.LastHeartbeatDay = h.sdk.clock.DayIndex()
			})
			if alreadyQueued && !h.sdk.queue.Recover(event) {
				t.Fatal("seed queue")
			}
			h.sdk.platform.AppVersion = "C"
			h.sdk.Init("prd_conform001", "later")
			if got := h.sdk.Export().LastAppVersion; got != "C" {
				t.Fatalf("baseline=%s", got)
			}
			queued := h.sdk.queue.Head(100)
			if len(queued) != 2 || queued[0].ID != event.ID || queued[1].Props["from_version"] != "B" {
				t.Fatalf("recovered=%+v", queued)
			}
			if h.store.Get().PendingUpdate != nil {
				t.Fatal("intent not retired")
			}
			raw, err := buildEvent(queued[0], h.sdk.platform, "refhost/99", "changed", nil)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"id": event.ID, "av": "B", "a": "original", "v": "refhost/1", "osv": "14", "iid": event.Metadata.InstallID} {
				if wire[key] != want {
					t.Fatalf("%s=%v want %s", key, wire[key], want)
				}
			}
		})
	}
}

func TestFailedUpdateStateCommitBlocksDeliveryAndKeepsBaseline(t *testing.T) {
	h := newSleepHarness(t)
	h.store.Load()
	h.store.Update(func(state *State) { state.LastAppVersion = "A"; state.LastHeartbeatDay = h.sdk.clock.DayIndex() })
	temporary := h.store.statePath() + ".tmp"
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	h.sdk.platform.AppVersion = "B"
	h.sdk.Init("prd_conform001", "")
	h.host.sleep(3000)
	if got := h.sdk.Export().LastAppVersion; got != "A" {
		t.Fatalf("failed commit advanced to %s", got)
	}
	if len(h.sent()) != 0 {
		t.Fatal("sent without committing transition")
	}
	if got := NewStore(h.store.Dir()).Load().LastAppVersion; got != "A" {
		t.Fatalf("disk baseline=%s", got)
	}
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	h.host.sleep(3000)
	if got := h.sdk.Export().LastAppVersion; got != "B" {
		t.Fatalf("recovery baseline=%s", got)
	}
	if len(h.sent()) != 1 {
		t.Fatalf("requests=%d", len(h.sent()))
	}
}

func TestFailedUpdateQueueCommitKeepsIntentAndDoesNotSend(t *testing.T) {
	h := newSleepHarness(t)
	event := interruptedUpdate()
	h.store.Load()
	h.store.Update(func(state *State) {
		state.LastAppVersion, state.PendingUpdate = "B", &event
		state.LastHeartbeatDay = h.sdk.clock.DayIndex()
	})
	temporary := filepath.Join(h.store.Dir(), "queue.jsonl.tmp")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	h.sdk.platform.AppVersion = "B"
	h.sdk.Init("prd_conform001", "")
	h.host.sleep(3000)
	if len(h.sent()) != 0 || h.store.Get().PendingUpdate == nil {
		t.Fatal("failed handoff retired intent or sent")
	}
	if h.sdk.LegacyVersion() {
		t.Fatal("legacyversion accepted pending intent")
	}
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	h.host.sleep(3000)
	if len(h.sent()) != 1 || h.store.Get().PendingUpdate != nil {
		t.Fatal("successful handoff did not deliver exactly once")
	}
}

func TestAcknowledgementAfterCapEvictionKeepsUnsentUpdates(t *testing.T) {
	queue := NewQueue(filepath.Join(t.TempDir(), "queue.jsonl"))
	queue.Append(QueuedEvent{ID: "sent", N: "old", T: json.RawMessage("0")})
	sent := queue.Head(1)
	for i := 0; i < queueMaxEvents; i++ {
		queue.Append(QueuedEvent{ID: string(rune(i + 100)), N: "app_updated", T: json.RawMessage("0")})
	}
	queue.RemoveEvents(sent)
	if queue.Len() != queueMaxEvents {
		t.Fatalf("ack removed unsent update: %d", queue.Len())
	}
	queue.Load()
	if queue.Len() != queueMaxEvents {
		t.Fatal("ack removed unsent update from disk")
	}
}

func TestRecoveryDoesNotResurrectAnEvictedUpdate(t *testing.T) {
	h := newSleepHarness(t)
	event := interruptedUpdate()
	h.store.Load()
	h.store.Update(func(state *State) {
		state.LastAppVersion, state.PendingUpdate = "B", &event
		state.LastHeartbeatDay = h.sdk.clock.DayIndex()
	})
	if !h.sdk.queue.Recover(event) {
		t.Fatal("seed queue handoff")
	}
	// The queue handoff succeeded, but retiring the state intent failed.
	for i := 0; i < queueMaxEvents; i++ {
		h.sdk.queue.Append(QueuedEvent{ID: "new-" + strconv.Itoa(i), N: "ordinary", T: event.T})
	}
	if h.sdk.queue.Contains("app_updated") {
		t.Fatal("oldest-first cap did not evict the update")
	}
	h.sdk.platform.AppVersion = "B"
	h.sdk.Init("prd_conform001", "")
	h.host.sleep(3000)
	for _, batch := range h.sent() {
		for _, item := range batch {
			if item.N == "app_updated" {
				t.Fatal("recovery resurrected an intentionally evicted update")
			}
		}
	}
	if h.store.Get().PendingUpdate != nil {
		t.Fatal("completed handoff did not retire the intent")
	}
}

func TestFailedRecoveryCheckpointKeepsPreviousQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.jsonl")
	queue := NewQueue(path)
	for i := 0; i < queueMaxEvents; i++ {
		queue.Append(QueuedEvent{ID: "old-" + strconv.Itoa(i), N: "ordinary", T: json.RawMessage("0")})
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if queue.Recover(interruptedUpdate()) {
		t.Fatal("checkpoint succeeded despite blocked temporary file")
	}
	for _, reload := range []bool{false, true} {
		if reload {
			queue.Load()
		}
		if queue.Contains("app_updated") || queue.Len() != queueMaxEvents || queue.Head(1)[0].ID != "old-0" {
			t.Fatal("failed recovery mutated the committed queue")
		}
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if !queue.Recover(interruptedUpdate()) || !queue.Contains("app_updated") || queue.Head(1)[0].ID != "old-1" {
		t.Fatal("recovery did not resume with oldest-first eviction")
	}
}

func TestResetQueueCheckpointRetainsRecoveryReceipt(t *testing.T) {
	queue := NewQueue(filepath.Join(t.TempDir(), "queue.jsonl"))
	event := interruptedUpdate()
	if !queue.Recover(event) || !queue.DiscardIdentityEvents() {
		t.Fatal("seed reset checkpoint")
	}
	// A failed state commit leaves the old intent even though reset discarded the queue entry.
	queue.Load()
	if !queue.Recover(event) || queue.Contains("app_updated") {
		t.Fatal("reset resurrected an intentionally discarded update")
	}
	if queue.Export().Bytes != 0 {
		t.Fatal("queue byte cap counts recovery bookkeeping as queued events")
	}
	queue.Delete()
	if !queue.Recover(event) || !queue.Contains("app_updated") {
		t.Fatal("delete retained the previous identity's receipt")
	}
}

func TestLegacyRecoveryRemembersHandoffBeforeFailedCheckpointAndEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.jsonl")
	legacy := NewQueue(path)
	event := interruptedUpdate()
	legacy.Append(event)
	for i := 0; i < queueMaxEvents-1; i++ {
		legacy.Append(QueuedEvent{ID: "old-" + strconv.Itoa(i), N: "ordinary", T: event.T})
	}
	queue := NewQueue(path)
	queue.Load(event.ID)
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if queue.Recover(event) {
		t.Fatal("checkpoint succeeded despite blocked temporary file")
	}
	queue.Append(QueuedEvent{ID: "new-0", N: "ordinary", T: event.T})
	if queue.Contains("app_updated") {
		t.Fatal("oldest-first cap did not evict update")
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	queue.Append(QueuedEvent{ID: "new-1", N: "ordinary", T: event.T})
	queue.Load()
	if !queue.Recover(event) || queue.Contains("app_updated") {
		t.Fatal("legacy recovery resurrected an intentionally evicted update")
	}
}
