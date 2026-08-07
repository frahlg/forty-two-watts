package appenroll

// The box code: the way back in without a camera.
//
// A phone with no printed QR and no other device already paired still has to
// be able to get back into its home. The floor is somebody standing at the
// box, reading eight characters down the phone. Everything here exists to make
// those eight characters safe to say out loud.
//
// It is redeemed exactly where the QR code's payload is redeemed — inside
// Noise handshake message 1 — so there is no new endpoint, no new carrier and
// no HTTP path. The app decodes the characters back to five bytes and sends
// those. The wire does not change at all.
//
// What it is not, and the box's own page says so on the screen: it is not a
// way in for a phone that has never seen this box. These characters are the
// pairing code and nothing else. The box's static key and its rendezvous
// secret travel only in the QR payload, so a phone with no record of this box
// can neither find it nor be sure the box answering is the right one. This
// re-admits a phone that already holds those.
//
// What it proves is not that the phone is trusted. It is that somebody was
// standing at the box: the code is minted only through the LAN-only pairing
// route and shown only on the box's own screen.

import (
	"errors"
	"strings"
)

const (
	// SpokenCodeBytes is forty bits of entropy.
	//
	// Not thirty: the failure counter in Authorise already holds a guesser to
	// five tries per minting, so thirty would very likely do. Forty costs one
	// extra breath to say and removes the argument entirely. Not sixteen
	// bytes like the QR code's — nobody reads sixteen bytes aloud.
	SpokenCodeBytes = 5

	// SpokenCodeChars is what those bytes become. Forty bits divides by five
	// exactly, so there is no padding and no remainder to explain.
	SpokenCodeChars = 8

	// spokenAlphabet is Crockford base32.
	//
	// Chosen because "read it aloud, down a phone, once" is the whole
	// requirement. It leaves out I, L and O — the characters people mishear
	// as 1 and 0 — and decoding folds those back rather than refusing, so a
	// listener who wrote down IO still gets in. U is left out too, so a
	// random draw cannot spell something a household would rather not read to
	// their neighbour.
	spokenAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// ErrBadSpokenCode is a typed code that is not eight characters of the
// alphabet above, once the folding has been applied.
var ErrBadSpokenCode = errors.New("appenroll: that is not a box code")

// encodeSpoken turns five bytes into eight characters, most significant
// first.
func encodeSpoken(raw []byte) string {
	var n uint64
	for _, b := range raw {
		n = n<<8 | uint64(b)
	}
	out := make([]byte, SpokenCodeChars)
	for i := SpokenCodeChars - 1; i >= 0; i-- {
		out[i] = spokenAlphabet[n&0x1f]
		n >>= 5
	}
	return string(out)
}

// GroupSpokenCode is how a code is shown and read: XXXX-XXXX.
//
// The hyphen is for the reader, not for the code. DecodeSpokenCode throws it
// away again, along with spaces, so somebody who types what they hear without
// it is not punished for it.
func GroupSpokenCode(code string) string {
	if len(code) != SpokenCodeChars {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// DecodeSpokenCode turns what somebody typed back into five bytes.
//
// This is the box's own copy of the rule the app applies before it sends the
// bytes. It lives here because the folding is the part that has to match
// exactly on both sides, and a test that encodes and decodes is what stops the
// two drifting.
func DecodeSpokenCode(typed string) ([]byte, error) {
	var cleaned strings.Builder
	for _, r := range typed {
		switch r {
		case ' ', '-', '\t':
			continue // written for the reader, meaningless to the code
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if strings.IndexRune(spokenAlphabet, r) < 0 {
			return nil, ErrBadSpokenCode
		}
		cleaned.WriteRune(r)
	}

	chars := cleaned.String()
	if len(chars) != SpokenCodeChars {
		return nil, ErrBadSpokenCode
	}

	var n uint64
	for i := 0; i < len(chars); i++ {
		n = n<<5 | uint64(strings.IndexByte(spokenAlphabet, chars[i]))
	}
	out := make([]byte, SpokenCodeBytes)
	for i := SpokenCodeBytes - 1; i >= 0; i-- {
		out[i] = byte(n & 0xff)
		n >>= 8
	}
	return out, nil
}
