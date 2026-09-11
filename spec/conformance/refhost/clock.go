package main

import (
	"math/big"
	"sync"
	"time"
)

// millisPerDay is the UTC day the daily heartbeat of RFC-0001 §8.2 item 3 is
// keyed on. A day index is floor(ms / 86_400_000), which is the UTC calendar
// day for every value including negative ones (math/big's Div is Euclidean, so
// it floors for a positive divisor) -- and a pre-epoch clock is exactly what
// C15b sends.
var millisPerDay = big.NewInt(86_400_000)

// Clock is the SDK's clock abstraction (spec/sdk-conformance.md §3). It is
// arbitrary precision because RFC-0001 §8.5 forbids correcting the host clock
// and C15b sends one past int64: saturating into an int64 would be a
// correction.
type Clock struct {
	mu     sync.Mutex
	pinned bool
	now    *big.Int // virtual milliseconds, when pinned
}

// NewClock returns the real wall clock, or a pinned one at unixMS.
func NewClock() *Clock { return &Clock{} }

// Pin fixes the clock at a decimal millisecond literal. It is the JELTO_NOW
// frame: from here the clock moves only when Advance is called.
func (c *Clock) Pin(millis *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned, c.now = true, new(big.Int).Set(millis)
}

func (c *Clock) Pinned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinned
}

// Now is the client wall clock in milliseconds (spec/wire-v1.md §3 `t`).
func (c *Clock) Now() *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pinned {
		return big.NewInt(time.Now().UnixMilli())
	}
	return new(big.Int).Set(c.now)
}

// Advance moves a pinned clock forward. It is what `sleep <ms>` does under
// JELTO_NOW; on the real clock it does nothing and the caller really sleeps.
func (c *Clock) Advance(millis int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pinned {
		return
	}
	c.now.Add(c.now, big.NewInt(millis))
}

// DayIndex is the UTC day the clock is in, as a decimal string. A string
// rather than a formatted date because a clock past int64 has no
// representation as a time.Time, and the heartbeat rule only needs the days to
// be DISTINCT, not printable.
func (c *Clock) DayIndex() string {
	return new(big.Int).Div(c.Now(), millisPerDay).String()
}

// deadline is an absolute instant on the SDK's clock. The zero value means
// "no deadline set", which is why it is a pointer everywhere it is stored.
type deadline = big.Int

// after returns now + millis.
func after(now *big.Int, millis int64) *big.Int {
	return new(big.Int).Add(now, big.NewInt(millis))
}

// due reports whether now has reached at (a nil `at` is never due).
func due(now, at *big.Int) bool {
	return at != nil && now.Cmp(at) >= 0
}

// untilMillis returns at - now clamped to [0, ...] as an int64 of
// milliseconds, saturating rather than wrapping. It is only ever used to size a
// REAL timer, so saturating at a value far beyond any test's patience is
// harmless; the authoritative comparison is always `due` on the big values.
func untilMillis(now, at *big.Int) int64 {
	if at == nil {
		return -1
	}
	delta := new(big.Int).Sub(at, now)
	if delta.Sign() <= 0 {
		return 0
	}
	if !delta.IsInt64() {
		return int64(1) << 40
	}
	return delta.Int64()
}
