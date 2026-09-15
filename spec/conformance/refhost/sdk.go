package main

import (
	"bytes"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The numbers of RFC-0001 §8.3, in one place so each can be read against its
// rule.
const (
	initFlushDelayMS = 2_000           // item 7, "2 s after init"
	trackDebounceMS  = 5_000           // item 7, "5 s debounce after track"
	requestTimeout   = 5 * time.Second // item 8, "Request timeout 5 s"
	backoffFirstMS   = 1_000           // item 8, "Backoff 1 s x 2"
	backoffCapMS     = 3_600_000       // item 8, "up to 1 h"; wire §9 clamps Retry-After to the same ceiling
	jitterFraction   = 0.20            // item 8, "+-20 % jitter"

	installClaimAfter = 30 * 24 * 60 * 60 * 1000 // §8.2 item 4, "or after 30 days of attempts"

	// Bound best-effort termination flushes so a blocked request cannot prevent exit (C1).
	terminationBudget = 600 * time.Millisecond

	// Cap response buffering so an oversized server response cannot exhaust the SDK budget (C10).
	responseReadCap = 1 << 20
)

// SDK runs deadlines and requests on one background pump.
type SDK struct {
	log           *Debug
	clock         *Clock
	store         *Store
	queue         *Queue
	client        *http.Client
	endpoint      string
	mock          string
	platform      Platform
	version       string
	installOrigin string
	random        *rand.Rand

	mu                  sync.Mutex
	started             bool
	disabled            bool
	key                 string
	install             string
	props               map[string]string
	observationMu       sync.Mutex
	observedAt          *big.Int
	observationComplete bool
	stagedUpdate        *QueuedEvent

	initFlushAt            *big.Int
	trackFlushAt           *big.Int
	pending                bool
	installEnqueuedThisRun bool // guarded by mu; final refusal cannot regenerate an install this run

	// The pump acknowledges idleWant only after observing its clock advance and
	// finding no due work. Closing idleWake wakes waiters without polling.
	// This host-only handshake does not affect send or queue policy (TODO.md §5).
	idleMu   sync.Mutex
	idleWant uint64
	idleAck  uint64
	idleWake chan struct{}

	ready chan struct{}
	wake  chan struct{}
	quit  chan struct{}
	done  chan struct{}
}

func NewSDK(log *Debug, clock *Clock, store *Store, endpoint, mock, version string, platform Platform, seed int64) *SDK {
	return &SDK{
		log:      log,
		clock:    clock,
		store:    store,
		queue:    NewQueue(store.queuePath()),
		endpoint: endpoint,
		mock:     mock,
		platform: platform,
		version:  version,
		random:   rand.New(rand.NewSource(seed)),
		props:    map[string]string{},
	}
}

// --- surface (RFC-0001 §8.1) -------------------------------------------------

// Init is §8.1's init(key, app?). It returns without touching the filesystem
// or the network: everything real happens on the pump (§8.2 item 1, C1).
func (s *SDK) Init(key, slug string, origin ...string) {
	s.mu.Lock()
	if s.started && !s.disabled {
		// §8.1: init happens "once".
		s.mu.Unlock()
		return
	}
	restart := s.started && s.disabled
	s.started, s.disabled, s.key = true, false, key
	s.installOrigin = "unknown"
	if len(origin) > 0 {
		s.installOrigin = normalizeInstallOrigin(origin[0])
	}
	if slug != "" {
		// §5.2 `a`. A slug outside the grammar is dropped rather than sent:
		// the server would answer invalid_field for every event carrying it.
		if reAppSlug.MatchString(slug) {
			s.platform.Slug = slug
		} else {
			s.log.Printf("drop app slug %q: spec/wire-v1.md §5.2 `a` is ^[a-z0-9-]{1,32}$", slug)
		}
	}
	s.ready = make(chan struct{})
	if restart {
		ready := s.ready
		s.mu.Unlock()
		go func() {
			s.bootstrap(ready)
			s.notify()
		}()
		return
	}
	s.wake = make(chan struct{}, 1)
	s.quit = make(chan struct{})
	s.done = make(chan struct{})
	s.client = &http.Client{Timeout: requestTimeout}
	ready, quit, done := s.ready, s.quit, s.done
	s.mu.Unlock()

	go func() {
		s.bootstrap(ready)
		s.pump(quit, done)
	}()
}

// bootstrap is everything init would otherwise have done synchronously.
func (s *SDK) bootstrap(ready chan struct{}) {
	loaded := s.store.Load()
	if loaded.PendingUpdate != nil {
		s.queue.Load(loaded.PendingUpdate.ID)
	} else {
		s.queue.Load()
	}
	now := s.clock.Now()
	s.observationMu.Lock()
	s.observedAt, s.observationComplete, s.stagedUpdate = new(big.Int).Set(now), false, nil
	s.observationMu.Unlock()

	state := s.store.Update(func(state *State) {
		// §8.2 item 2: load the install_id, create a UUIDv4 if absent.
		if state.InstallID == "" || state.InstallID == NilUUID {
			state.InstallID = newUUIDv4()
			state.InstallOrigin = s.installOrigin
		}
		state.InstallOrigin = normalizeInstallOrigin(state.InstallOrigin)
		delete(state.InstallProps, "install_origin")
		if state.InstallProps == nil {
			state.InstallProps = map[string]string{}
		}
		// §8.2 item 4: persist the immediate deadline once, at the draw instant,
		// so a relaunch resumes it even after an abrupt death (C4c).
		if !state.InstallClaimed && state.InstallDueAt == "" {
			state.InstallDueAt = formatBig(now)
		}
	})

	s.mu.Lock()
	s.install = state.InstallID
	s.props = map[string]string{}
	for key, value := range state.InstallProps {
		s.props[key] = value
	}
	// §8.3 item 7: the first flush trigger.
	s.initFlushAt = after(now, initFlushDelayMS)
	s.installEnqueuedThisRun = s.queue.Contains("install")
	s.mu.Unlock()

	s.observeAppVersion()

	// §8.2 item 3: one heartbeat per UTC day.
	day := s.clock.DayIndex()
	if state.LastHeartbeatDay != day {
		s.store.Update(func(state *State) { state.LastHeartbeatDay = day })
		s.enqueue(QueuedEvent{ID: newUUIDv7(now), N: "heartbeat", T: literal(now), Heartbeat: true})
	}

	close(ready)
}

// Track is §8.1's track(name, props?). Everything it can refuse, it refuses
// here, in the client, with a debug line: W2 and W3.
func (s *SDK) Track(name string, props map[string]any) {
	if !s.awaitReady() {
		// §8.1: "queued; no-op before init" (C5).
		return
	}
	if !reEventName.MatchString(name) {
		s.log.Printf("drop event %q: spec/wire-v1.md §3 `n` is ^[a-z0-9_:.-]{1,64}$", name)
		return
	}
	if rawOrigin, hasOrigin := props["install_origin"]; hasOrigin {
		origin, ok := rawOrigin.(string)
		if name != "install" || !ok || (origin != "new" && origin != "existing" && origin != "unknown") {
			s.log.Printf("drop event %q: install_origin is an install-only enum", name)
			return
		}
	}
	if err := validateProps(props); err != nil {
		s.log.Printf("drop event %q: %v", name, err)
		return
	}
	now := s.clock.Now()
	s.enqueue(QueuedEvent{ID: newUUIDv7(now), N: name, T: literal(now), Props: props})
	s.mu.Lock()
	// §8.3 item 7: a 5 s debounce, reset by every track.
	s.trackFlushAt = after(now, trackDebounceMS)
	s.mu.Unlock()
	s.notify()
}

// Onboarding validates helper arguments and uses the same Track path to preserve C20 equivalence.
func (s *SDK) Onboarding(step, status, reason string) {
	if !s.awaitReady() {
		return
	}
	if !reOnboardingStep.MatchString(step) {
		s.log.Printf("drop onboarding step %q: spec/wire-v1.md §4 `<step>` is ^[a-z0-9_-]{1,32}$", step)
		return
	}
	if status != "ok" && status != "fail" && status != "skip" {
		s.log.Printf("drop onboarding step %q: spec/wire-v1.md §4 `status` is ok|fail|skip, not %q", step, status)
		return
	}
	props := map[string]any{"status": status}
	if reason != "" {
		if len(reason) > maxReasonChars || !reOnboardingReason.MatchString(reason) {
			s.log.Printf("drop onboarding step %q: spec/wire-v1.md §4 `reason` is ^[a-z0-9_.-]+$ and <= %d chars, not %q", step, maxReasonChars, reason)
			return
		}
		props["reason"] = reason
	}
	s.Track("onboarding:"+step, props)
}

// SetProps is §8.1's setProps: "install properties (license, edition ...):
// persisted, sent with every heartbeat, an immediate heartbeat on change".
func (s *SDK) SetProps(props map[string]any) {
	if !s.awaitReady() {
		return
	}
	accepted := map[string]string{}
	for key, value := range props {
		if key == "install_origin" {
			continue
		} // reserved for the immutable install claim
		text, ok := value.(string)
		if !ok {
			// §4: every install-property value matches ^[a-z0-9_.-]{1,24}$,
			// which is a string grammar.
			s.log.Printf("drop install property %q: spec/wire-v1.md §4 install-property values are strings matching ^[a-z0-9_.-]{1,24}$", key)
			continue
		}
		if !rePropKey.MatchString(key) {
			s.log.Printf("drop install property %q: spec/wire-v1.md §3 `props` keys are ^[a-z0-9_]{1,32}$", key)
			continue
		}
		if !reInstallPropValue.MatchString(text) {
			s.log.Printf("drop install property %q: spec/wire-v1.md §4 install-property values are ^[a-z0-9_.-]{1,24}$, not %q", key, text)
			continue
		}
		accepted[key] = text
	}

	s.mu.Lock()
	merged := map[string]string{}
	for key, value := range s.props {
		merged[key] = value
	}
	for key, value := range accepted {
		merged[key] = value
	}
	changed := len(merged) != len(s.props)
	if !changed {
		for key, value := range merged {
			if s.props[key] != value {
				changed = true
				break
			}
		}
	}
	if len(merged) > maxProps {
		s.mu.Unlock()
		s.log.Printf("drop setprops: %d install properties, spec/wire-v1.md §4 caps them at %d", len(merged), maxProps)
		return
	}
	s.props = merged
	s.mu.Unlock()

	s.store.Update(func(state *State) { state.InstallProps = merged })
	if !changed {
		// C22b: the same value twice is not a change, so no extra heartbeat.
		return
	}
	now := s.clock.Now()
	s.enqueue(QueuedEvent{ID: newUUIDv7(now), N: "heartbeat", T: literal(now), Heartbeat: true})
	s.mu.Lock()
	s.pending = true // "an immediate heartbeat on change" (C22's <= 2 s)
	s.mu.Unlock()
	s.notify()
}

// InstallID is §8.1's installId. It is empty after disable() (C18).
func (s *SDK) InstallID() string {
	if !s.awaitReady() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.install
}

// Reset is §8.1's "rotate". NO RULE names what else it rotates; RFC-0001 §8.1
// says only "view, rotate, wipe (queue + id)", and no C-scenario exercises it.
// refhost rotates the id and, because a new id is a new install, forgets the
// install claim and the heartbeat day with it.
func (s *SDK) Reset() {
	if !s.awaitReady() {
		return
	}
	now := s.clock.Now()
	id := newUUIDv4()
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if !s.queue.DiscardIdentityEvents() {
		return
	}
	if !s.store.Commit(func(state *State) {
		state.PendingUpdate = nil
		state.LastAppVersion = ""
		if knownAppVersion(s.platform.AppVersion) {
			state.LastAppVersion = s.platform.AppVersion
		}
		state.InstallID = id
		state.InstallOrigin = "unknown"
		state.InstallClaimed = false
		state.InstallDueAt = formatBig(now)
		state.InstallFirstTry = ""
		state.LastHeartbeatDay = ""
	}) {
		return
	}
	s.stagedUpdate, s.observationComplete = nil, true
	s.mu.Lock()
	s.install = id
	s.installEnqueuedThisRun = s.queue.Contains("install")
	s.mu.Unlock()
	s.notify()
}

func (s *SDK) LegacyVersion() bool {
	if !s.awaitReady() {
		return false
	}
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if s.store.Get().PendingUpdate != nil {
		return false
	}
	return s.store.Commit(func(state *State) { state.LastAppVersion = "" })
}

// Disable is §8.7 item 18: delete the queue file and the install_id;
// subsequent calls are no-ops until the next init.
func (s *SDK) Disable() {
	if !s.awaitReady() {
		return
	}
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	s.mu.Lock()
	s.disabled, s.install, s.props = true, "", map[string]string{}
	s.initFlushAt, s.trackFlushAt, s.pending = nil, nil, false
	s.installEnqueuedThisRun = false
	s.mu.Unlock()
	s.queue.Delete()
	s.store.Wipe()
	s.notify()
}

// Stop is the host process going away: §8.3 item 7's best-effort termination
// flush, bounded so C1's "process exits within 1 s" holds.
func (s *SDK) Stop() {
	s.mu.Lock()
	started, quit, done := s.started, s.quit, s.done
	if started && !s.disabled {
		s.pending = true
	}
	s.mu.Unlock()
	if !started {
		return
	}
	s.notify()

	deadline := time.After(terminationBudget)
	for {
		if s.queue.Len() == 0 {
			break
		}
		select {
		case <-deadline:
			s.log.Printf("termination flush gave up with %d events queued (best effort, RFC-0001 §8.3 item 7)", s.queue.Len())
			goto stop
		case <-time.After(5 * time.Millisecond):
		}
	}
stop:
	close(quit)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Abandon in-flight requests at exit rather than waiting out the network timeout.
	}
}

// --- the pump ----------------------------------------------------------------

// awaitReady rejects calls before init and after disable until the next init (C5, C18).
func (s *SDK) awaitReady() bool {
	s.mu.Lock()
	ready, started := s.ready, s.started
	s.mu.Unlock()
	if !started {
		return false
	}
	<-ready
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.disabled
}

func (s *SDK) notify() {
	s.mu.Lock()
	wake := s.wake
	s.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// openBarrier requests acknowledgement of the current clock. Virtual sleep calls
// it before and after advancing so bootstrap cannot observe the later time early.
func (s *SDK) openBarrier() uint64 {
	s.idleMu.Lock()
	s.idleWant++
	seq := s.idleWant
	s.idleMu.Unlock()
	// Wake the pump to observe the new barrier; an existing wake token is sufficient.
	s.notify()
	return seq
}

// observeBarrier is what the pump has been asked. The pump calls it before it
// reads the clock and never afterwards.
func (s *SDK) observeBarrier() uint64 {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	return s.idleWant
}

// ackBarrier is the pump's answer: nothing was due, so every barrier up to seq
// is settled.
func (s *SDK) ackBarrier(seq uint64) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if seq <= s.idleAck {
		return
	}
	s.idleAck = seq
	if s.idleWake != nil {
		close(s.idleWake)
		s.idleWake = nil
	}
}

// barrierState returns the last acknowledged sequence together with a channel
// that closes the next time it moves.
func (s *SDK) barrierState() (uint64, <-chan struct{}) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.idleWake == nil {
		s.idleWake = make(chan struct{})
	}
	return s.idleAck, s.idleWake
}

// awaitBarrier uses a real-time budget so wedged virtual-clock scenarios fail instead of hanging.
func (s *SDK) awaitBarrier(seq uint64, budget time.Duration) bool {
	s.mu.Lock()
	started, done := s.started, s.done
	s.mu.Unlock()
	if !started {
		// Before init there is no pump, so nothing can become due and there is
		// nothing to wait for (§8.2 item 5, C5).
		return true
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		ack, wake := s.barrierState()
		if ack >= seq {
			return true
		}
		select {
		case <-wake:
		case <-done:
			// The pump has stopped (`exit`); nothing further can become due.
			return true
		case <-timer.C:
			return false
		}
	}
}

// Check readiness without blocking the pump on another goroutine.
func (s *SDK) bootstrapped() bool {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready == nil {
		return false
	}
	select {
	case <-ready:
		return true
	default:
		return false
	}
}

func (s *SDK) enqueue(event QueuedEvent) {
	if event.Metadata == nil {
		_, _, version, platform, _ := s.snapshot()
		event.Metadata = &EventMetadata{Platform: platform, Version: version}
	}
	if dropped := s.queue.Append(event); dropped > 0 {
		s.log.Printf("queue cap reached: dropped %d oldest event(s) (RFC-0001 §8.3 item 6)", dropped)
	}
	s.notify()
}

func knownAppVersion(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 32
}

func (s *SDK) observeAppVersion() bool {
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if s.observationComplete {
		return true
	}
	for {
		if pending := s.store.Get().PendingUpdate; pending != nil {
			if !s.queue.Recover(*pending) {
				return false
			}
			if !s.store.Commit(func(state *State) { state.PendingUpdate = nil }) {
				return false
			}
		}
		_, install, version, platform, _ := s.snapshot()
		current, previous := platform.AppVersion, s.store.Get().LastAppVersion
		if !knownAppVersion(current) || current == previous {
			s.observationComplete = true
			return true
		}
		if !knownAppVersion(previous) {
			if !s.store.Commit(func(state *State) { state.LastAppVersion = current }) {
				return false
			}
			s.observationComplete = true
			return true
		}
		if s.stagedUpdate == nil {
			s.stagedUpdate = &QueuedEvent{
				ID: newUUIDv7(s.observedAt), N: "app_updated", T: literal(s.observedAt),
				Props:    map[string]any{"from_version": previous, "to_version": current},
				Metadata: &EventMetadata{Platform: platform, Version: version, InstallID: install},
			}
		}
		event := s.stagedUpdate
		if !s.store.Commit(func(state *State) { state.LastAppVersion, state.PendingUpdate = current, event }) {
			return false
		}
		s.stagedUpdate = nil
	}
}

// Process one due action per iteration so large clock advances preserve action ordering.
func (s *SDK) pump(quit, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-quit:
			return
		default:
		}
		// Read the barrier before the clock so observing a barrier also observes its preceding clock advance.
		barrier := s.observeBarrier()
		now := s.clock.Now()
		next, acted := s.step(now)
		if acted {
			continue
		}
		// Acknowledge only after bootstrap has enqueued its work and nothing remains due.
		// Re-init bootstrap notifies the existing pump when ready.
		if s.bootstrapped() {
			s.ackBarrier(barrier)
		}
		s.mu.Lock()
		wake := s.wake
		s.mu.Unlock()
		if s.clock.Pinned() {
			// A pinned clock only moves when `sleep` moves it, so there is
			// nothing to wait for but a wake-up.
			select {
			case <-quit:
				return
			case <-wake:
			}
			continue
		}
		wait := untilMillis(now, next)
		if wait < 0 {
			select {
			case <-quit:
				return
			case <-wake:
			}
			continue
		}
		timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
		select {
		case <-quit:
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// step performs at most one due action and returns the next deadline.
func (s *SDK) step(now *big.Int) (*big.Int, bool) {
	s.mu.Lock()
	disabled, started := s.disabled, s.started
	s.mu.Unlock()
	if disabled || !started {
		return nil, false
	}
	if !s.observeAppVersion() {
		return after(now, 1000), false
	}
	// Serialize install persistence with reset/disable so an immediate deadline
	// cannot recreate a queue or state that a concurrent disable just wiped.
	s.observationMu.Lock()
	s.mu.Lock()
	disabled = s.disabled
	s.mu.Unlock()
	if disabled {
		s.observationMu.Unlock()
		return nil, false
	}
	state := s.store.Get()

	// §8.2 item 4: "Mark claimed on 202, or after 30 days of attempts."
	firstTry := parseBig(state.InstallFirstTry)
	if !state.InstallClaimed && firstTry != nil && due(now, after(firstTry, installClaimAfter)) {
		s.store.Update(func(state *State) { state.InstallClaimed = true })
		s.log.Printf("install claimed after 30 days of attempts without a 202 (RFC-0001 §8.2 item 4)")
		s.observationMu.Unlock()
		return nil, true
	}

	// A queued install is already the retry; never enqueue another copy (C4b).
	installDue := parseBig(state.InstallDueAt)
	s.mu.Lock()
	installEnqueued := s.installEnqueuedThisRun
	s.mu.Unlock()
	if !state.InstallClaimed && !installEnqueued && due(now, installDue) && !s.queue.Contains("install") {
		if !s.store.Commit(func(state *State) {
			if state.InstallFirstTry == "" {
				state.InstallFirstTry = formatBig(now)
			}
		}) {
			// Persist the captured origin and deadline before publishing the claim,
			// even when no version change required a separate state commit.
			s.observationMu.Unlock()
			return after(now, 1000), false
		}
		s.mu.Lock()
		s.installEnqueuedThisRun = true
		// Queue immediately while preserving §8.3 item 7's initial flush timer.
		if s.initFlushAt == nil {
			s.pending = true
		}
		s.mu.Unlock()
		s.enqueue(QueuedEvent{ID: newUUIDv7(now), N: "install", T: literal(now), Props: map[string]any{"install_origin": normalizeInstallOrigin(state.InstallOrigin)}})
		s.observationMu.Unlock()
		return nil, true
	}

	s.observationMu.Unlock()

	// §8.6 / wire §8: the kill switch. Nothing is sent while it is on; the
	// queue keeps accepting up to its cap.
	stopUntil := parseBig(state.StopUntilMS)
	if stopUntil != nil && !due(now, stopUntil) {
		return s.deadline(now, stopUntil), false
	}
	if stopUntil != nil {
		s.store.Update(func(state *State) { state.StopUntilMS = "" })
		s.log.Printf("kill switch elapsed (spec/wire-v1.md §8)")
		return nil, true
	}

	// The retry gate (§8.3 item 8). It governs the probe as well: a probe that
	// failed on the network is retried, not repeated at once.
	retryAt := parseBig(state.BackoffNextAt)
	if retryAt != nil && !due(now, retryAt) {
		return s.deadline(now, retryAt), false
	}

	// After a stop, await one heartbeat probe sent alone before draining queued events (wire §8).
	if state.StopProbeDue {
		s.sendProbe(now)
		return nil, true
	}

	s.mu.Lock()
	if due(now, s.initFlushAt) {
		s.initFlushAt, s.pending = nil, true
	}
	if due(now, s.trackFlushAt) {
		s.trackFlushAt, s.pending = nil, true
	}
	pending := s.pending
	s.mu.Unlock()

	if pending && s.queue.Len() > 0 {
		s.sendBatch(now)
		return nil, true
	}
	if pending {
		s.mu.Lock()
		s.pending = false
		s.mu.Unlock()
	}
	return s.deadline(now, nil), false
}

// deadline is the earliest instant the pump has to wake for.
func (s *SDK) deadline(now *big.Int, extra *big.Int) *big.Int {
	state := s.store.Get()
	s.mu.Lock()
	candidates := []*big.Int{s.initFlushAt, s.trackFlushAt, extra}
	installEnqueued := s.installEnqueuedThisRun
	s.mu.Unlock()
	candidates = append(candidates, parseBig(state.BackoffNextAt), parseBig(state.StopUntilMS))
	if !state.InstallClaimed && !installEnqueued {
		candidates = append(candidates, parseBig(state.InstallDueAt))
	}
	if firstTry := parseBig(state.InstallFirstTry); firstTry != nil && !state.InstallClaimed {
		candidates = append(candidates, after(firstTry, installClaimAfter))
	}
	var best *big.Int
	for _, candidate := range candidates {
		if candidate == nil || candidate.Cmp(now) <= 0 {
			continue
		}
		if best == nil || candidate.Cmp(best) < 0 {
			best = candidate
		}
	}
	return best
}

// --- sending -----------------------------------------------------------------

func (s *SDK) snapshot() (key, install, version string, platform Platform, props map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	props = make(map[string]string, len(s.props))
	for key, value := range s.props {
		props[key] = value
	}
	return s.key, s.install, s.version, s.platform, props
}

// sendProbe sends wire §8's single-heartbeat re-check.
func (s *SDK) sendProbe(now *big.Int) {
	probe := QueuedEvent{ID: newUUIDv7(now), N: "heartbeat", T: literal(now), Heartbeat: true}
	key, install, version, platform, props := s.snapshot()
	rendered, err := buildEvent(probe, platform, version, install, props)
	if err != nil {
		s.store.Update(func(state *State) { state.StopProbeDue = false })
		return
	}
	body, used := buildEnvelope(key, [][]byte{rendered})
	if used != 1 {
		s.store.Update(func(state *State) { state.StopProbeDue = false })
		return
	}
	outcome := s.post(body)
	// Measure Retry-After from response arrival, not request start.
	answered := s.clock.Now()
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if !s.sameInstall(install) {
		return
	}
	switch {
	case outcome.retryable:
		s.backoff(answered, outcome)
	case outcome.status == 202:
		s.applyAccepted(answered, outcome, false)
		s.clearBackoff()
		s.store.Update(func(state *State) {
			if state.StopUntilMS == "" {
				// Only clear the probe when the switch did not come straight
				// back on: an `until` in the response re-arms it and the next
				// lift owes another probe.
				state.StopProbeDue = false
			}
		})
	default:
		s.finalRefusal(outcome)
		s.clearBackoff()
		s.store.Update(func(state *State) { state.StopProbeDue = false })
	}
}

func (s *SDK) sendBatch(now *big.Int) {
	key, install, version, platform, props := s.snapshot()
	queued := s.queue.Head(maxEventsPerReq)
	rendered := make([][]byte, 0, len(queued))
	for i := range queued {
		event, err := buildEvent(queued[i], platform, version, install, props)
		if err != nil {
			continue
		}
		rendered = append(rendered, event)
	}
	if len(rendered) == 0 {
		s.queue.Remove(len(queued))
		return
	}
	body, used := buildEnvelope(key, rendered)
	if used == 0 {
		s.queue.Remove(1)
		return
	}
	outcome := s.post(body)
	answered := s.clock.Now() // see sendProbe: both floors run from the answer
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if !s.sameInstall(install) {
		return
	}
	switch {
	case outcome.retryable:
		// Keep retry IDs unchanged to preserve server deduplication (wire §6, C8).
		s.backoff(answered, outcome)
	case outcome.status == 202:
		s.queue.RemoveEvents(queued[:used])
		s.applyAccepted(answered, outcome, s.batchHasInstall(queued[:used]))
		s.clearBackoff()
		s.mu.Lock()
		s.pending = s.queue.Len() > 0
		s.mu.Unlock()
	default:
		// Only network errors, 429, and 503 retry; all other statuses are final (C9, C9b).
		s.finalRefusal(outcome)
		s.queue.RemoveEvents(queued[:used])
		s.clearBackoff()
		s.mu.Lock()
		s.pending = s.queue.Len() > 0
		s.mu.Unlock()
	}
}

func (s *SDK) sameInstall(install string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.disabled && s.install == install
}

func (s *SDK) batchHasInstall(events []QueuedEvent) bool {
	for i := range events {
		if events[i].N == "install" {
			return true
		}
	}
	return false
}

type outcome struct {
	status     int
	body       []byte
	retryAfter string
	hasHeader  bool
	err        error
	retryable  bool
}

func (s *SDK) post(body []byte) outcome {
	// §8.7 item 17: "prints every payload before it is sent".
	s.log.Payload(body)

	request, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return outcome{err: err, retryable: true}
	}
	request.Header.Set("Content-Type", "application/json")
	if s.mock != "" {
		// spec/sdk-conformance.md §2: the host forwards JELTO_MOCK into X-Mock.
		request.Header.Set("X-Mock", s.mock)
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	response, err := client.Do(request)
	if err != nil {
		s.log.Printf("request failed: %v (network error, retryable per RFC-0001 §8.3 item 8)", err)
		return outcome{err: err, retryable: true}
	}
	defer func() { _ = response.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, responseReadCap))
	result := outcome{status: response.StatusCode, body: payload}
	if values := response.Header.Values("Retry-After"); len(values) > 0 {
		result.retryAfter, result.hasHeader = values[0], true
	}
	result.retryable = response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable
	return result
}

// applyAccepted reads a 202 body for the two things §6 and §8 put there.
func (s *SDK) applyAccepted(now *big.Int, result outcome, hadInstall bool) {
	parsed, err := decodeResponse(result.body)
	if err != nil {
		// §8.3 item 10: swallowed and logged. The status was 202, so the
		// envelope was accepted whatever the body said (C10's invalid JSON).
		s.log.Printf("202 with a body that is not JSON: %v", err)
		if hadInstall {
			s.store.Update(func(state *State) { state.InstallClaimed = true })
		}
		return
	}
	if hadInstall {
		// §8.2 item 4: "Mark claimed on 202".
		s.store.Update(func(state *State) { state.InstallClaimed = true })
	}
	for _, rejection := range parsed.Rejected {
		s.log.Printf("event %d rejected: %s%s (spec/wire-v1.md §6)", rejection.I, rejection.Reason, fieldSuffix(rejection.Field))
	}
	if parsed.Stop == nil {
		return
	}
	// wire §8: `scope` is app or web; a client matching the scope MUST stop.
	// C16b: an app SDK ignores a web-scoped stop.
	if parsed.Stop.Scope == "web" {
		s.log.Printf("ignoring a stop scoped to web (spec/wire-v1.md §8; this client is s=app)")
		return
	}
	seconds, ok := new(big.Int).SetString(parsed.Stop.Until.String(), 10)
	if !ok {
		s.log.Printf("ignoring a stop whose `until` is not whole seconds: %q", parsed.Stop.Until.String())
		return
	}
	until := new(big.Int).Mul(seconds, big.NewInt(1000))
	if !due(until, now) {
		// An `until` already in the past is a switch that was never on. It is
		// still honoured -- trivially -- and the probe is still owed, because
		// wire §8's MUST is about the first request AFTER `until`.
		s.log.Printf("stop until %s is already past (now %s)", until.String(), now.String())
	}
	s.store.Update(func(state *State) {
		state.StopUntilMS = formatBig(until)
		state.StopProbeDue = true
	})
	s.log.Printf("kill switch: no request until %s ms, scope %s (spec/wire-v1.md §8)", until.String(), parsed.Stop.Scope)
}

func fieldSuffix(field string) string {
	if field == "" {
		return ""
	}
	return " (" + field + ")"
}

func (s *SDK) finalRefusal(result outcome) {
	message := ""
	if parsed, err := decodeResponse(result.body); err == nil && parsed.Error != "" {
		message = " " + parsed.Error
	}
	s.log.Printf("batch dropped: status=%d%s -- final, not retried (RFC-0001 §8.3 items 8-9, spec/wire-v1.md §2a)", result.status, message)
}

// backoff implements RFC-0001 §8.3 item 8 composed with spec/wire-v1.md §9:
// both are floors and the next attempt is at the later of the two.
func (s *SDK) backoff(now *big.Int, result outcome) {
	state := s.store.Get()
	step := state.BackoffStepMS
	if step <= 0 {
		step = backoffFirstMS
	}
	s.mu.Lock()
	jitter := 1 - jitterFraction + 2*jitterFraction*s.random.Float64()
	s.mu.Unlock()
	wait := int64(float64(step) * jitter)
	if wait > backoffCapMS {
		wait = backoffCapMS
	}
	source := "backoff"

	if header, ok := parseRetryAfter(result.retryAfter, result.hasHeader); ok {
		// §9: "Clamped to the same 3 600 s ceiling C8 puts on its own backoff."
		if header > backoffCapMS {
			s.log.Printf("Retry-After %q exceeds the 3600 s ceiling and is clamped (spec/wire-v1.md §9)", result.retryAfter)
			header = backoffCapMS
		}
		if header > wait {
			wait, source = header, "Retry-After"
		}
	} else if result.hasHeader {
		// §9: "Absent, unparseable, or not delay-seconds -> treated as absent,
		// never as zero."
		s.log.Printf("Retry-After %q is not delay-seconds and is treated as absent (spec/wire-v1.md §9)", result.retryAfter)
	}

	// Round the deadline up by 1 ms: a floored clock puts actual response arrival
	// in [now, now+1), and wire §9 forbids retrying before the full interval elapses.
	next := after(now, wait+1)
	nextStep := step * 2
	if nextStep > backoffCapMS {
		nextStep = backoffCapMS
	}
	s.store.Update(func(state *State) {
		// §8.3 item 8: persisted across launches, so a new process continues
		// the schedule rather than restarting at 1 s (C8).
		state.BackoffStepMS = nextStep
		state.BackoffNextAt = formatBig(next)
		state.BackoffFails++
	})
	s.log.Printf("retry in %d ms (%s governs; step was %d ms, refusal %d) status=%d", wait, source, step, state.BackoffFails+1, result.status)
}

func (s *SDK) clearBackoff() {
	state := s.store.Get()
	if state.BackoffStepMS == 0 && state.BackoffNextAt == "" && state.BackoffFails == 0 {
		return
	}
	s.store.Update(func(state *State) {
		state.BackoffStepMS, state.BackoffNextAt, state.BackoffFails = 0, "", 0
	})
}

// parseRetryAfter reads RFC 9110 §10.2.3's delay-seconds form and nothing
// else: spec/wire-v1.md §9 says "the HTTP-date form is never sent", and
// anything unparseable is absent.
func parseRetryAfter(value string, present bool) (int64, bool) {
	if !present {
		return 0, false
	}
	trimmed := strings.TrimSpace(value)
	seconds, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return seconds * 1000, true
}

// literal renders the clock as the JSON number `t` carries. It is the decimal
// the clock holds, never a float64: RFC-0001 §8.5 forbids correcting it and
// C15b sends one past int64.
func literal(now *big.Int) []byte {
	return []byte(now.String())
}

func normalizeInstallOrigin(value string) string {
	switch value {
	case "new", "existing":
		return value
	default:
		return "unknown"
	}
}
