package main

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sync"
)

// State is refhost's private storage. Export translates it into conformance §3.2.
// Store instants as decimal strings to preserve arbitrary-precision clock values (C15b).
type State struct {
	InstallID      string       `json:"install_id"`
	LastAppVersion string       `json:"last_app_version,omitempty"`
	PendingUpdate  *QueuedEvent `json:"pending_update,omitempty"`

	LastHeartbeatDay string `json:"last_heartbeat_day,omitempty"` // UTC day index, decimal

	InstallOrigin   string            `json:"install_origin,omitempty"` // wire §5.2; frozen with the claim
	InstallClaimed  bool              `json:"install_claimed"`
	InstallDueAt    string            `json:"install_due_at,omitempty"`    // §8.2 item 4, the draw instant, persisted once (C4c)
	InstallFirstTry string            `json:"install_first_try,omitempty"` // when attempts began; +30 d claims it anyway (C4b)
	InstallProps    map[string]string `json:"install_props,omitempty"`     // §8.1 setProps, persisted (C22)

	BackoffStepMS int64  `json:"backoff_step_ms,omitempty"`  // §8.3 item 8; 0 means "not in backoff"
	BackoffNextAt string `json:"backoff_next_at,omitempty"`  // absolute, persisted across launches
	BackoffFails  int    `json:"backoff_failures,omitempty"` // consecutive refusals; C8c turns on the tenth

	StopUntilMS  string `json:"stop_until,omitempty"`     // §8.6 / wire §8, absolute ms on the SDK clock
	StopProbeDue bool   `json:"stop_probe_due,omitempty"` // wire §8's single-heartbeat re-check is owed
}

// Store is the state directory. Nothing here touches the filesystem until
// Load is called, which happens on `init` and never before: RFC-0001 §8.2
// item 5, "no file is created" (C5).
type Store struct {
	dir string

	mu    sync.Mutex
	state State
}

const (
	stateFile = "state.json"
	queueFile = "queue.jsonl"
)

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Dir() string { return s.dir }

func (s *Store) statePath() string { return filepath.Join(s.dir, stateFile) }
func (s *Store) queuePath() string { return filepath.Join(s.dir, queueFile) }

// Load reads state.json, creating neither the file nor the directory when it
// is absent. A file that cannot be parsed is treated as absent: §8.3 item 10
// swallows every failure, and a corrupt state costs an install id, not a
// crash.
func (s *Store) Load() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		s.state = State{}
		return s.state
	}
	var loaded State
	if err := json.Unmarshal(raw, &loaded); err != nil {
		s.state = State{}
		return s.state
	}
	s.state = loaded
	return s.state
}

func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Update mutates and persists under one lock, so a crash between the two is
// the only way they can disagree.
func (s *Store) Update(mutate func(*State)) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.state)
	s.persistLocked()
	return s.state
}

func (s *Store) persistLocked() {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		return
	}
	atomicWrite(s.statePath(), raw, true)
}

// Commit keeps memory at the old committed baseline when storage refuses the write.
func (s *Store) Commit(mutate func(*State)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	mutate(&next)
	raw, err := json.Marshal(next)
	if err != nil || !atomicWrite(s.statePath(), raw, true) {
		return false
	}
	s.state = next
	return true
}

// Wipe is RFC-0001 §8.7 item 18: disable() deletes the queue file and the
// install_id. C18 asserts the state DIRECTORY is empty afterwards, so
// everything refhost put there goes, not only the two fields.
func (s *Store) Wipe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{}
	_ = os.Remove(s.statePath())
	_ = os.Remove(s.queuePath())
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(s.dir, entry.Name()))
	}
}

// parseBig reads one of the decimal-string instants back. An unreadable value
// is "not set", which is the safe direction for every field that uses one.
func parseBig(text string) *big.Int {
	if text == "" {
		return nil
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil
	}
	return value
}

func formatBig(value *big.Int) string {
	if value == nil {
		return ""
	}
	return value.String()
}
