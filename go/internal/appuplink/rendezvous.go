package appuplink

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The rendezvous handle: what the box and the app call each other on the relay.
//
// It is the one piece of protocol metadata the relay unavoidably sees, so what
// it is made of decides how much the protocol itself reveals. A stable handle
// — a hash of the box's static key, say — would work perfectly and hand the
// operator a household identifier good for years: every connection, every
// outage, every holiday, joined up under one string.
//
// So it is derived per epoch from a secret the relay never sees:
//
//	handle = HKDF-SHA256(secret, info = "ftw/rendezvous/v1/<epoch>")[0..16]
//
// HKDF-Expand is a PRF, so without the secret two epochs' handles are two
// cryptographically unrelated strings and there is no function the relay can
// compute from the handles alone that links them. This removes a stable
// protocol identifier; it does not hide source IP, timing or connection
// continuity. The relay's only contribution to the derivation is the epoch
// *number*, which it announces to everyone equally because it is its own clock.
//
// This must agree byte for byte with srcfl/ftw-webapp
// src/lib/carrier/rendezvous.ts. Disagreeing means the box and the app sit in
// two different rooms and nobody's house appears on their phone.
const (
	// EpochMs is how long a stable protocol handle lives. An hour limits that
	// identifier's lifetime and keeps rotations rare next to the reconnects a
	// phone does anyway. Network metadata may still correlate the sockets.
	EpochMs = 3_600_000

	// HandleBytes is 128 bits: long enough that handles never collide,
	// short enough for a URL.
	HandleBytes = 16
)

// CurrentEpoch is the epoch a peer guesses from its own clock.
//
// A guess, not an answer: the relay is the authority and says so in the close
// reason when this is wrong. A Pi with a dead RTC reads 1970, guesses epoch 0
// and is corrected on its first connect — which is the whole reason the guess
// is allowed to be wrong.
func CurrentEpoch(nowMs int64) int64 {
	// Floor, not truncation. Go divides toward zero and the app uses
	// Math.floor, so a clock before 1970 would put the two on different
	// epochs — the one case this function exists to survive.
	if nowMs < 0 {
		return -((-nowMs + EpochMs - 1) / EpochMs)
	}
	return nowMs / EpochMs
}

// Handle derives this epoch's handle. The secret never leaves the box.
func Handle(secret []byte, epoch int64) (string, error) {
	if len(secret) < 16 {
		return "", fmt.Errorf("appuplink: rendezvous secret is %d bytes, too short to be a secret", len(secret))
	}

	// Extract with no salt, matching @noble/hashes hkdf(sha256, ikm,
	// undefined, info, len), which uses a zero-filled salt of hash length.
	prk, err := hkdf.Extract(sha256.New, secret, nil)
	if err != nil {
		return "", fmt.Errorf("appuplink: hkdf extract: %w", err)
	}
	okm, err := hkdf.Expand(sha256.New, prk, fmt.Sprintf("ftw/rendezvous/v1/%d", epoch), HandleBytes)
	if err != nil {
		return "", fmt.Errorf("appuplink: hkdf expand: %w", err)
	}
	return hex.EncodeToString(okm), nil
}
