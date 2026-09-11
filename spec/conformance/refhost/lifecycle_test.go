package main

import (
	"fmt"
	"testing"
)

func TestInstallDeadlineFlushesWithoutAnotherPublicCall(t *testing.T) {
	for _, status := range []int{202, 400, 402, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newSleepHarness(t, status)
			h.store.Load()
			h.store.Update(func(state *State) {
				state.InstallClaimed = false
				state.InstallDueAt = formatBig(after(h.sdk.clock.Now(), 10000))
				state.LastHeartbeatDay = h.sdk.clock.DayIndex()
			})
			h.sdk.Init("prd_conform001", "")
			h.host.sleep(3000)
			if len(h.sent()) != 0 {
				t.Fatal("unexpected initial traffic")
			}
			h.host.sleep(8000)
			sent := h.sent()
			if len(sent) != 1 || len(sent[0]) != 1 || sent[0][0].N != "install" {
				t.Fatalf("scheduled install did not flush: %+v", sent)
			}
			h.host.sleep(4000)
			want := 1
			if status == 503 {
				want = 2
			}
			sent = h.sent()
			if len(sent) != want {
				t.Fatalf("status %d: got %d requests, want %d", status, len(sent), want)
			}
			if status == 503 && (sent[0][0].ID != sent[1][0].ID || string(sent[0][0].T) != string(sent[1][0].T)) {
				t.Fatal("retry changed the install")
			}
			if h.store.Get().InstallClaimed != (status == 202) {
				t.Fatal("incorrect install claim")
			}
			h.sdk.Disable()
		})
	}
}

func TestDeadlineExcludesPastAndAlreadyScheduledInstall(t *testing.T) {
	h := newSleepHarness(t)
	now := h.sdk.clock.Now()
	h.store.Load()
	h.store.Update(func(state *State) {
		state.InstallDueAt = formatBig(after(now, -10000))
		state.BackoffNextAt = formatBig(after(now, -1000))
	})
	if got := h.sdk.deadline(now, nil); got != nil {
		t.Fatalf("claimed idle install has deadline %s", got)
	}
	h.store.Update(func(state *State) {
		state.InstallClaimed = false
		state.InstallDueAt = formatBig(after(now, 1000))
		state.BackoffNextAt = formatBig(after(now, 5000))
	})
	if got := h.sdk.deadline(now, nil); got == nil || got.Cmp(after(now, 1000)) != 0 {
		t.Fatalf("lost install wake: %v", got)
	}
	h.sdk.mu.Lock()
	h.sdk.installEnqueuedThisRun = true
	h.sdk.mu.Unlock()
	if got := h.sdk.deadline(now, nil); got == nil || got.Cmp(after(now, 5000)) != 0 {
		t.Fatalf("wrong retry wake: %v", got)
	}
}
