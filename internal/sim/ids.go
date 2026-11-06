package sim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// MinIDKeyLen is the minimum length of the secret ID key.
const MinIDKeyLen = 16

// DeriveID returns the n-th identifier in a domain ("entity", "player", ...)
// as a keyed pseudo-random function: HMAC-SHA256(key, domain || 0 || n)
// truncated to 64 bits.
//
// Identifiers are public, but they must not leak how many objects exist or
// in which order they were created. Without the key, outputs are
// indistinguishable from random, so observing any number of IDs reveals
// nothing about the counter or other IDs. With the key, the sequence is
// fully deterministic, which keeps replays exact.
//
// A non-cryptographic generator (such as Rand) would not do: SplitMix64 is
// invertible, so a single observed ID would expose the counter.
func DeriveID(key []byte, domain string, n uint64) uint64 {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	var buf [9]byte
	binary.BigEndian.PutUint64(buf[1:], n)
	mac.Write(buf[:])
	return binary.BigEndian.Uint64(mac.Sum(nil)[:8])
}
