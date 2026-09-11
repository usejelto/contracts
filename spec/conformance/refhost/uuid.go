package main

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
)

// NilUUID is the one UUID spec/wire-v1.md §3 forbids in `id` and §5.2 forbids
// in `iid`. It is named so the code can be seen never to send it.
const NilUUID = "00000000-0000-0000-0000-000000000000"

// newUUIDv4 is the install_id of RFC-0001 §8.2 item 2 ("create a UUIDv4 if
// absent"). C2 asserts a v4 specifically.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// §8.3 item 10: never throw into the host. A read from crypto/rand
		// cannot fail on any supported platform; if it did, an id derived from
		// nothing is still better than a panic, and it is still not nil.
		b[0] |= 1
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

// newUUIDv7 is the per-event `id` of spec/wire-v1.md §3 ("UUID (v7
// preferred)"). The 48-bit timestamp is the SDK clock's milliseconds, taken
// modulo 2^48 so that C15b's pre-epoch and past-int64 clocks still produce a
// well-formed, non-nil v7 -- an `id` is a dedup key, not a second timestamp,
// and §8.5 does not let the SDK move the clock to make one pretty.
func newUUIDv7(now *big.Int) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		b[0] |= 1
	}
	millis := new(big.Int).Mod(now, big.NewInt(1<<48))
	stamp := millis.Uint64()
	b[0] = byte(stamp >> 40)
	b[1] = byte(stamp >> 32)
	b[2] = byte(stamp >> 24)
	b[3] = byte(stamp >> 16)
	b[4] = byte(stamp >> 8)
	b[5] = byte(stamp)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80
	out := formatUUID(b)
	if out == NilUUID {
		// Unreachable: the version and variant nibbles are non-zero. Kept
		// because §3 makes "never the nil UUID" the rule, and a rule with no
		// enforcement is a comment.
		return newUUIDv4()
	}
	return out
}

func formatUUID(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
