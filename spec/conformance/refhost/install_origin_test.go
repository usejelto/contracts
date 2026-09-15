package main

import (
	"os"
	"testing"
)

func TestInstallOriginWireAndLegacyState(t *testing.T) {
	for _, tc := range []struct {
		name, hint, want string
		legacy           bool
	}{
		{"new", "new", "new", false},
		{"existing", "existing", "existing", false},
		{"omitted", "", "unknown", false},
		{"invalid", "first-launch-2026-09-15", "unknown", false},
		{"legacy", "new", "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSleepHarness(t)
			if tc.legacy {
				h.store.Load()
				h.store.Update(func(state *State) { state.InstallClaimed = false })
			} else if err := os.Remove(h.store.statePath()); err != nil {
				t.Fatal(err)
			}
			h.sdk.Init("prd_conform001", "", tc.hint)
			h.host.sleep(3000)
			if got := h.sdk.Export().InstallOrigin; got != tc.want {
				t.Fatalf("origin %q, want %q", got, tc.want)
			}
			found := false
			for _, batch := range h.sent() {
				for _, event := range batch {
					if event.N == "install" {
						found = true
						if event.Props["install_origin"] != tc.want {
							t.Fatalf("install props: %v", event.Props)
						}
					} else if _, ok := event.Props["install_origin"]; ok {
						t.Fatalf("origin leaked to %s", event.N)
					}
				}
			}
			if !found {
				t.Fatal("missing install")
			}
			h.sdk.Reset()
			if h.sdk.Export().InstallOrigin != "unknown" {
				t.Fatal("reset invented app installation age")
			}
		})
	}
}

func TestInstallOriginWaitsForDurableClaimState(t *testing.T) {
	h := newSleepHarness(t)
	if err := os.Remove(h.store.statePath()); err != nil {
		t.Fatal(err)
	}
	temporary := h.store.statePath() + ".tmp"
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	// Unknown versions do not independently commit a version baseline.
	h.sdk.platform.AppVersion = ""
	h.sdk.Init("prd_conform001", "", "existing")
	h.host.sleep(3000)
	before := h.store.Get()
	if before.InstallOrigin != "existing" || before.InstallFirstTry != "" {
		t.Fatalf("failed claim commit mutated captured state: %+v", before)
	}
	if len(h.sent()) != 0 || h.sdk.queue.Contains("install") {
		t.Fatal("published or queued a claim before its state was durable")
	}
	if _, err := os.Stat(h.store.statePath()); !os.IsNotExist(err) {
		t.Fatalf("unexpected durable state before recovery: %v", err)
	}
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	h.host.sleep(3000)
	after := NewStore(h.store.Dir()).Load()
	if after.InstallOrigin != "existing" || after.InstallID != before.InstallID || after.InstallDueAt != before.InstallDueAt || after.InstallFirstTry == "" {
		t.Fatalf("recovery changed or failed to persist claim state: %+v", after)
	}
	claims := 0
	for _, batch := range h.sent() {
		for _, event := range batch {
			if event.N == "install" {
				claims++
				if event.Props["install_origin"] != "existing" {
					t.Fatalf("recovered claim props: %v", event.Props)
				}
			}
		}
	}
	if claims != 1 {
		t.Fatalf("got %d recovered claims, want exactly one", claims)
	}
}
