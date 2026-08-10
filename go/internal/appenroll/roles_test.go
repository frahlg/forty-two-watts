package appenroll

// Roles, sharing and the box code, at the layer that decides them.
//
// Everything here asserts on the stored grant rather than on a returned error,
// because an error message is the one thing that stays right when the check
// behind it is deleted.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// pair walks one phone through a freshly minted code of the given role and
// returns the grant the box stamped on it.
func pair(t *testing.T, id *Identity, role string) Grant {
	t.Helper()
	code, _, err := id.MintPairingCode(role, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode(%q): %v", role, err)
	}
	grant, err := id.Authorise(appKey(t), code)
	if err != nil {
		t.Fatalf("pairing a %s: %v", role, err)
	}
	return grant
}

// roleOf reads one row's role back out of the device list.
func roleOf(t *testing.T, id *Identity, deviceID string) string {
	t.Helper()
	for _, d := range id.Devices() {
		if d.ID == deviceID {
			return d.Role
		}
	}
	t.Fatalf("no device %q in the list", deviceID)
	return ""
}

// --------------------------------------------------------------------------
// An invite is a capability to become an enrolment
// --------------------------------------------------------------------------

// The whole of sharing, in one test: a code minted for a viewer makes a
// viewer, works exactly once, and the second holder of the same code gets
// nothing.
func TestAnInviteIsRedeemedOnceAndRefusedTwice(t *testing.T) {
	id, _ := newIdentity(t)
	owner := pair(t, id, apiauth.RoleOwner)

	code, _, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	guest, err := id.Authorise(appKey(t), code)
	if err != nil {
		t.Fatalf("redeeming the invite: %v", err)
	}
	if guest.Role != apiauth.RoleViewer {
		t.Fatalf("the invite made a %q, want a viewer", guest.Role)
	}
	if guest.DeviceID == owner.DeviceID {
		t.Fatal("the guest landed on the owner's row")
	}

	// The same code again, from a different phone. This is the photographed
	// screen, the forwarded screenshot, the second person in the room.
	second := appKey(t)
	if _, err := id.Authorise(second, code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a spent invite was redeemed again: %v", err)
	}
	// And the box did not quietly enrol them anyway, which is the assertion
	// that survives someone deleting the check and keeping the error.
	if n := id.AuthorisedCount(); n != 2 {
		t.Fatalf("%d enrolments after a replayed invite, want 2", n)
	}
}

func TestAnExpiredInviteIsRefused(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	now := time.UnixMilli(1_760_000_000_000)
	id.now = func() time.Time { return now }

	code, expires, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	// One millisecond past the stamp the screen showed. The boundary is the
	// interesting part: a check written with >= would let this through.
	now = expires.Add(time.Millisecond)
	if _, err := id.Authorise(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("an expired invite was redeemed: %v", err)
	}
	if n := id.AuthorisedCount(); n != 1 {
		t.Fatalf("%d enrolments after an expired invite, want 1", n)
	}
}

// One live code at a time, across kinds. Minting an invite cancels a pending
// owner code, or a household that pressed both buttons has handed out two.
func TestMintingAnInviteCancelsAPendingOwnerCode(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	ownerCode, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, _, err := id.MintSpokenCode(apiauth.RoleViewer); err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}

	if _, err := id.Authorise(appKey(t), ownerCode); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a superseded owner code still worked: %v", err)
	}
	if n := id.AuthorisedCount(); n != 1 {
		t.Fatalf("%d enrolments, want 1 — the superseded code enrolled somebody", n)
	}
}

// A role the registry does not define is refused at the door rather than
// stored. Stored, it would expand to no scopes and give nobody a reason why.
func TestAnUnknownRoleIsRefusedEverywhere(t *testing.T) {
	id, _ := newIdentity(t)
	owner := pair(t, id, apiauth.RoleOwner)
	pair(t, id, apiauth.RoleOwner)

	if _, _, err := id.MintPairingCode("administrator", PairingTTL); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("minting for an unknown role: %v", err)
	}
	if _, _, err := id.MintSpokenCode("administrator"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("minting a box code for an unknown role: %v", err)
	}
	if err := id.SetRole(owner.DeviceID, "administrator"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("setting an unknown role: %v", err)
	}
	if got := roleOf(t, id, owner.DeviceID); got != apiauth.RoleOwner {
		t.Fatalf("the row is now %q; an unknown role was written", got)
	}
}

// --------------------------------------------------------------------------
// Whoever creates a home owns it
// --------------------------------------------------------------------------

// A box with no owner cannot be administered by anybody ever again. The first
// enrolment is an owner whatever its code said.
func TestTheFirstEnrolmentIsAnOwnerEvenFromAViewerCode(t *testing.T) {
	id, _ := newIdentity(t)

	code, _, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	grant, err := id.Authorise(appKey(t), code)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if grant.Role != apiauth.RoleOwner {
		t.Fatalf("the first phone on the box is a %q; the home has no owner", grant.Role)
	}

	// And only the first. The second viewer code makes a viewer, or the rule
	// above would quietly promote every guest.
	next, _, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	guest, err := id.Authorise(appKey(t), next)
	if err != nil {
		t.Fatalf("pairing a guest: %v", err)
	}
	if guest.Role != apiauth.RoleViewer {
		t.Fatalf("the second phone is a %q, want a viewer", guest.Role)
	}
}

// --------------------------------------------------------------------------
// The last owner
// --------------------------------------------------------------------------

// A phone cannot leave the household with nothing that can change anything.
// It is holding a session that proves enrolment and says nothing about where
// it is, and a box with no owner cannot be mended from a phone anywhere.
func TestTheLastOwnerCannotBeRemovedOrDemotedOverASession(t *testing.T) {
	id, _ := newIdentity(t)
	owner := pair(t, id, apiauth.RoleOwner)
	guest := pair(t, id, apiauth.RoleViewer)

	if err := id.SetRole(owner.DeviceID, apiauth.RoleViewer); !errors.Is(err, ErrLastOwnerProtected) {
		t.Fatalf("demoting the only owner: %v", err)
	}
	if _, err := id.Revoke(owner.DeviceID, OverASession); !errors.Is(err, ErrLastOwnerProtected) {
		t.Fatalf("revoking the only owner: %v", err)
	}
	// The box, not the error. A check that returns the right error and writes
	// anyway is the failure this project keeps rediscovering.
	if got := roleOf(t, id, owner.DeviceID); got != apiauth.RoleOwner {
		t.Fatalf("the last owner is now a %q", got)
	}
	if id.Devices()[0].LastOwner != true && id.Devices()[1].LastOwner != true {
		t.Fatal("no row is marked as the last owner, so no screen can explain the refusal")
	}

	// The guest is not the last owner and goes, from the same door. The rule
	// is about the one row, not about sessions removing phones.
	if _, err := id.Revoke(guest.DeviceID, OverASession); err != nil {
		t.Fatalf("revoking a guest over a session: %v", err)
	}

	// Promote a second owner, and the first is free to go.
	second := pair(t, id, apiauth.RoleViewer)
	if err := id.SetRole(second.DeviceID, apiauth.RoleOwner); err != nil {
		t.Fatalf("promoting the second phone: %v", err)
	}
	if _, err := id.Revoke(owner.DeviceID, OverASession); err != nil {
		t.Fatalf("revoking an owner once there are two: %v", err)
	}
	if n := id.AuthorisedCount(); n != 1 {
		t.Fatalf("%d enrolments, want 1", n)
	}
	if got := roleOf(t, id, second.DeviceID); got != apiauth.RoleOwner {
		t.Fatalf("the surviving phone is a %q, want the owner it was promoted to", got)
	}
}

// At the box, the last phone goes and the list ends empty.
//
// The refusal above is against a lockout nothing remote can mend. Standing at
// the box is the mending: the same page mints a fresh owner's code. So a
// household whose only phone was lost, or one that wants to start clean, gets
// its list back to nothing here and nowhere else.
func TestTheLastPhoneCanBeRemovedAtTheBox(t *testing.T) {
	id, _ := newIdentity(t)
	owner := pair(t, id, apiauth.RoleOwner)

	if _, err := id.Revoke(owner.DeviceID, AtTheBox); err != nil {
		t.Fatalf("removing the last phone at the box: %v", err)
	}
	if n := id.AuthorisedCount(); n != 0 {
		t.Fatalf("%d enrolments left, want an empty list", n)
	}
	if rows := id.Devices(); len(rows) != 0 {
		t.Fatalf("the roster still lists %+v", rows)
	}

	// And the box is not stranded: a code minted here admits an owner again,
	// which is the whole reason the removal above is safe.
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("minting a code after the list was emptied: %v", err)
	}
	grant, err := id.Authorise(appKey(t), code)
	if err != nil {
		t.Fatalf("pairing a phone with the emptied box: %v", err)
	}
	if grant.Role != apiauth.RoleOwner {
		t.Fatalf("the phone that came back is a %q, want an owner", grant.Role)
	}
}

// Stepping the last owner down is refused at the box too.
//
// Not an oversight of the rule above. Removing the last phone leaves a list
// somebody at the box can fill again; demoting it leaves a list of phones that
// can only look, which is the lockout rather than a way out of it.
func TestTheLastOwnerCannotBeSteppedDownAtTheBox(t *testing.T) {
	id, _ := newIdentity(t)
	owner := pair(t, id, apiauth.RoleOwner)
	pair(t, id, apiauth.RoleViewer)

	if err := id.SetRole(owner.DeviceID, apiauth.RoleViewer); !errors.Is(err, ErrLastOwnerProtected) {
		t.Fatalf("demoting the only owner: %v", err)
	}
	if got := roleOf(t, id, owner.DeviceID); got != apiauth.RoleOwner {
		t.Fatalf("the last owner is now a %q", got)
	}
}

// --------------------------------------------------------------------------
// A role change moves the epoch, which is what makes it bite mid-session
// --------------------------------------------------------------------------

func TestChangingARoleMovesTheEpoch(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	guest := pair(t, id, apiauth.RoleViewer)

	before, ok := id.GrantFor(guest.DeviceID)
	if !ok {
		t.Fatal("the guest has no grant")
	}
	if err := id.SetRole(guest.DeviceID, apiauth.RoleOwner); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	after, ok := id.GrantFor(guest.DeviceID)
	if !ok {
		t.Fatal("the guest lost their grant to a promotion")
	}
	if after.Role != apiauth.RoleOwner {
		t.Fatalf("role is %q, want owner", after.Role)
	}
	if after.Epoch <= before.Epoch {
		t.Fatalf("epoch went %d → %d; a live session would never notice",
			before.Epoch, after.Epoch)
	}

	// Setting the same role again changes nothing, so nothing moves. An epoch
	// that jumped for a write that changed nothing would make every open
	// session re-read a grant identical to the one it holds.
	if err := id.SetRole(guest.DeviceID, apiauth.RoleOwner); err != nil {
		t.Fatalf("SetRole, unchanged: %v", err)
	}
	same, _ := id.GrantFor(guest.DeviceID)
	if same.Epoch != after.Epoch {
		t.Fatalf("epoch moved %d → %d for a change that was not one", after.Epoch, same.Epoch)
	}
}

// A role survives a restart. Without this, every box update would re-read its
// guests as whatever the loader defaults to.
func TestRolesSurviveARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	guest := pair(t, id, apiauth.RoleViewer)

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	reloaded, ok := again.GrantFor(guest.DeviceID)
	if !ok {
		t.Fatal("the guest is gone after a restart")
	}
	if reloaded.Role != apiauth.RoleViewer {
		t.Fatalf("the guest came back as a %q, want a viewer", reloaded.Role)
	}
	if reloaded.Epoch != guest.Epoch {
		t.Fatalf("epoch %d after a restart, want %d — every session would be refused",
			reloaded.Epoch, guest.Epoch)
	}
}

// --------------------------------------------------------------------------
// The box code
// --------------------------------------------------------------------------

// The round trip, including everything a listener is likely to mishear. A
// decode that did not fold these would send five wrong bytes and burn one of
// the five attempts on a person who heard correctly.
func TestABoxCodeSurvivesBeingReadAloud(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	shown, _, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}
	if len(shown) != SpokenCodeChars+1 || shown[4] != '-' {
		t.Fatalf("code %q is not grouped as XXXX-XXXX", shown)
	}

	want, err := DecodeSpokenCode(shown)
	if err != nil {
		t.Fatalf("decoding what the box showed: %v", err)
	}
	if len(want) != SpokenCodeBytes {
		t.Fatalf("decoded to %d bytes, want %d", len(want), SpokenCodeBytes)
	}

	// Written down by ear: lower case, no hyphen, spaces. These hold for any
	// code; the folding of misheard characters is pinned separately below,
	// against a code known to contain the characters in question.
	bare := strings.ReplaceAll(shown, "-", "")
	for _, typed := range []string{
		strings.ToLower(shown),
		bare,
		bare[:4] + " " + bare[4:],
	} {
		got, err := DecodeSpokenCode(typed)
		if err != nil {
			t.Fatalf("decoding %q: %v", typed, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%q decoded to different bytes than %q", typed, shown)
		}
	}

	// And it opens the door, which is the only thing that matters.
	grant, err := id.Authorise(appKey(t), want)
	if err != nil {
		t.Fatalf("redeeming the box code: %v", err)
	}
	if grant.Role != apiauth.RoleViewer {
		t.Fatalf("the box code let in a %q, want the viewer it was minted for", grant.Role)
	}
}

// The folding, against a code chosen to contain the characters it folds.
//
// Built from fixed bytes rather than from a minted code on purpose: a random
// forty bits very often contains no 0 and no 1 at all, and a test that
// substitutes characters a code does not have asserts nothing while appearing
// to assert everything. That is the shape of test this project has lost
// several rounds to, so it is written out longhand here.
func TestABoxCodeFoldsTheCharactersPeopleMishear(t *testing.T) {
	// Forty bits of 0x0000000001, which encodes to seven noughts and a one:
	// every character this alphabet folds, in one code.
	canonical := encodeSpoken([]byte{0, 0, 0, 0, 1})
	if canonical != "00000001" {
		t.Fatalf("encodeSpoken = %q, want 00000001", canonical)
	}

	want, err := DecodeSpokenCode(canonical)
	if err != nil {
		t.Fatalf("decoding the canonical form: %v", err)
	}

	for _, c := range []struct{ name, typed string }{
		{"O heard as nought", "OOOOOOO1"},
		{"o heard as nought", "ooooooo1"},
		{"I heard as one", "0000000I"},
		{"l heard as one", "0000000l"},
		{"L heard as one", "0000000L"},
		{"all of them at once", "OOOOOOOI"},
		{"grouped and misheard", "OOOO-OOOL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The variant must really be a variant, or the case below is a
			// test of nothing.
			if strings.ReplaceAll(c.typed, "-", "") == canonical {
				t.Fatalf("%q is the canonical form; this case folds nothing", c.typed)
			}
			got, err := DecodeSpokenCode(c.typed)
			if err != nil {
				t.Fatalf("decoding %q: %v", c.typed, err)
			}
			if string(got) != string(want) {
				t.Fatalf("%q decoded to %x, want %x", c.typed, got, want)
			}
		})
	}
}

// Encoding and decoding agree across the whole range, one byte at a time.
// The app implements the same folding on its side, and this is the table it
// has to match.
func TestSpokenCodesRoundTrip(t *testing.T) {
	for _, raw := range [][]byte{
		{0, 0, 0, 0, 0},
		{0, 0, 0, 0, 1},
		{0xff, 0xff, 0xff, 0xff, 0xff},
		{0x80, 0, 0, 0, 0},
		{0x01, 0x23, 0x45, 0x67, 0x89},
		{0xde, 0xad, 0xbe, 0xef, 0x42},
	} {
		encoded := encodeSpoken(raw)
		if len(encoded) != SpokenCodeChars {
			t.Fatalf("%x encoded to %q, %d characters", raw, encoded, len(encoded))
		}
		got, err := DecodeSpokenCode(GroupSpokenCode(encoded))
		if err != nil {
			t.Fatalf("decoding %q: %v", encoded, err)
		}
		if string(got) != string(raw) {
			t.Fatalf("%x → %q → %x", raw, encoded, got)
		}
	}

	// Every one of the 32 characters is reachable and decodes back to the
	// value it stands for. The last character carries the low five bits, so
	// counting through 0..31 walks the whole alphabet.
	for i := 0; i < 32; i++ {
		encoded := encodeSpoken([]byte{0, 0, 0, 0, byte(i)})
		last := rune(encoded[SpokenCodeChars-1])
		if !strings.ContainsRune(spokenAlphabet, last) {
			t.Fatalf("value %d encoded to %q, which is not in the alphabet", i, last)
		}
		got, err := DecodeSpokenCode(encoded)
		if err != nil {
			t.Fatalf("value %d encoded to %q, which will not decode: %v", i, encoded, err)
		}
		if got[SpokenCodeBytes-1] != byte(i) {
			t.Fatalf("value %d round-tripped to %d", i, got[SpokenCodeBytes-1])
		}
	}

	// And the four characters Crockford leaves out are not in it, or the
	// folding above would be ambiguous.
	for _, excluded := range "ILOU" {
		if strings.ContainsRune(spokenAlphabet, excluded) {
			t.Fatalf("%q is in the alphabet; folding it would be ambiguous", excluded)
		}
	}
}

func TestRubbishIsNotABoxCode(t *testing.T) {
	for _, typed := range []string{
		"",
		"ABCD-EFG",   // seven characters
		"ABCD-EFGHJ", // nine
		"ABCD-EFGU",  // U is not in the alphabet
		"ABCD-EFG!",  // punctuation
		"ABCD-EFGé",  // not ASCII
	} {
		if _, err := DecodeSpokenCode(typed); !errors.Is(err, ErrBadSpokenCode) {
			t.Fatalf("DecodeSpokenCode(%q) = %v, want a refusal", typed, err)
		}
	}
}

// The property the whole box code rests on: a guesser gets MaxPairingAttempts
// tries per minting, and every minting needs somebody standing at the box.
//
// Forty bits with unlimited tries is a lock that opens given a weekend. Forty
// bits with five is one that never opens at all — a guesser expecting to
// succeed needs about 2^40/5 mintings, each of which is a person walking to
// the box, so the code expires unguessed every time.
func TestABoxCodeCannotBeGuessedFasterThanItExpires(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	shown, _, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}
	real, err := DecodeSpokenCode(shown)
	if err != nil {
		t.Fatalf("DecodeSpokenCode: %v", err)
	}

	// Wrong guesses, all well within the five minutes and all correctly
	// shaped — this is somebody working through the space, not fumbling.
	for i := 0; i < MaxPairingAttempts; i++ {
		guess := append([]byte(nil), real...)
		guess[0] ^= byte(i + 1)
		if _, err := id.Authorise(appKey(t), guess); !errors.Is(err, ErrBadPairing) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}

	// The code is burned, so even the right answer buys nothing. Somebody has
	// to walk back to the box.
	if _, err := id.Authorise(appKey(t), real); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("the real code still worked after %d wrong guesses: %v", MaxPairingAttempts, err)
	}
	if n := id.AuthorisedCount(); n != 1 {
		t.Fatalf("%d enrolments, want 1 — a guesser got in", n)
	}

	// A fresh minting starts the counter again, or a household would be
	// locked out for good by anyone who could reach the box.
	next, _, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}
	fresh, err := DecodeSpokenCode(next)
	if err != nil {
		t.Fatalf("DecodeSpokenCode: %v", err)
	}
	if _, err := id.Authorise(appKey(t), fresh); err != nil {
		t.Fatalf("a freshly minted code was refused: %v", err)
	}
}

// The counter is on the code, not on the caller. Five phones guessing once
// each must burn the code exactly as one phone guessing five times does — the
// relay's own limiter already keys on an address that is one address for the
// whole fleet, and an address-keyed counter here would inherit that.
func TestTheAttemptCounterFollowsTheCodeNotTheCaller(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	shown, _, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}
	real, err := DecodeSpokenCode(shown)
	if err != nil {
		t.Fatalf("DecodeSpokenCode: %v", err)
	}

	for i := 0; i < MaxPairingAttempts; i++ {
		guess := append([]byte(nil), real...)
		guess[4] ^= byte(i + 1)
		// A different key every time: a different phone, or the same phone
		// pretending to be one.
		if _, err := id.Authorise(appKey(t), guess); !errors.Is(err, ErrBadPairing) {
			t.Fatalf("guess %d from a fresh key: %v", i, err)
		}
	}
	if _, err := id.Authorise(appKey(t), real); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("spreading the guesses across keys kept the code alive: %v", err)
	}
}

// The QR code is counted too. It has enough entropy not to need it, but one
// rule for both kinds is one rule to get wrong.
func TestAQRCodeIsBurnedByGuessingToo(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	code, _, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	for i := 0; i < MaxPairingAttempts; i++ {
		guess := append([]byte(nil), code...)
		guess[0] ^= byte(i + 1)
		if _, err := id.Authorise(appKey(t), guess); !errors.Is(err, ErrBadPairing) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if _, err := id.Authorise(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a guessed-at QR code survived: %v", err)
	}
}

// A box code lives half as long as a scanned one, and the difference is real
// rather than decorative: fewer bits, read out while two people are talking.
func TestABoxCodeExpiresSoonerThanAScannedOne(t *testing.T) {
	if SpokenTTL >= PairingTTL {
		t.Fatalf("SpokenTTL is %s and PairingTTL is %s; the comment above the "+
			"constant claims a shorter life", SpokenTTL, PairingTTL)
	}

	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)

	now := time.UnixMilli(1_760_000_000_000)
	id.now = func() time.Time { return now }

	shown, expires, err := id.MintSpokenCode(apiauth.RoleOwner)
	if err != nil {
		t.Fatalf("MintSpokenCode: %v", err)
	}
	if got := expires.Sub(now); got != SpokenTTL {
		t.Fatalf("a box code lives %s, want %s", got, SpokenTTL)
	}
	real, err := DecodeSpokenCode(shown)
	if err != nil {
		t.Fatalf("DecodeSpokenCode: %v", err)
	}

	now = expires.Add(time.Millisecond)
	if _, err := id.Authorise(appKey(t), real); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("an expired box code was redeemed: %v", err)
	}
}
