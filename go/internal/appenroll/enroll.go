// Package appenroll owns the box's half of app enrollment: the long-lived
// keys behind the QR code on the lid, and the record of which phones have been
// let in.
//
// Three secrets, three different lifetimes, and confusing any two of them is
// the mistake this package exists to prevent:
//
//   - the Noise static key, which the app pins optically and which must never
//     change, because changing it invalidates every pairing at once;
//   - the rendezvous secret, which is long-lived but replaceable, and which
//     only ever feeds the HKDF that names this box on the relay;
//   - the pairing code, which is single-use with a short life and exists only
//     so the box can tell the first handshake from a stranger's.
//
// None of them is stored in SQLite. They are boot-time material that has to be
// readable before the state store is open, and go/internal/state is for site
// data, not for keys.
package appenroll

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appwire"
)

const (
	// PairingCodeBytes matches the app's PAIRING_CODE_BYTES. It is an
	// unguessable token, not a passphrase — nobody types it.
	PairingCodeBytes = 16

	// RendezvousSecretBytes matches the app's RENDEZVOUS_SECRET_BYTES. It is
	// HKDF input keying material, and 32 bytes is what a SHA-256 PRF wants.
	RendezvousSecretBytes = 32

	// PairingTTL is how long a minted code stays usable.
	//
	// Ten minutes is long enough to find the QR code, open the app and hold
	// the phone steady, and short enough that a photograph of the lid taken
	// last year is worth nothing. The code is also spent on first use, so
	// this only bounds the window before anyone uses it at all.
	PairingTTL = 10 * time.Minute

	// InviteTTL is how long a shared, view-only code stays usable.
	//
	// The same ten minutes as an owner's, and deliberately not dressed up as
	// less. An invite is not a weaker kind of code — it is the same code
	// carrying a different role, and shortening its life would suggest the
	// household is protected by the clock when what protects them is the
	// role the box stamps at redemption.
	InviteTTL = PairingTTL

	// SpokenTTL is how long a box code stays usable.
	//
	// Half an owner code's life, and this one is a real difference: forty
	// bits read down a phone is less entropy than sixteen random bytes
	// scanned off a screen, and it is read out while both people are on the
	// line. Five minutes is the length of that conversation.
	SpokenTTL = 5 * time.Minute

	// MaxPairingAttempts is how many wrong guesses a code survives.
	//
	// This is the number that makes a spoken code sound out loud: forty bits
	// with unlimited tries is a lock somebody can pick given a weekend, and
	// forty bits with five tries is one nobody can pick at all. See the
	// counter in Authorise for why it is on the code and not on the caller.
	MaxPairingAttempts = 5

	// FileName is where the material lives, beside nova.key.
	FileName = "applink.json"
)

var (
	// ErrNoPairing is a handshake that offered nothing and came from a key
	// this box has never authorised.
	ErrNoPairing = errors.New("appenroll: no pairing code and an unknown app key")
	// ErrBadPairing is a code that is wrong, expired, already spent or has
	// been guessed at too many times. One error for all four on purpose: a
	// guesser who could tell "wrong" from "expired" would learn when to stop
	// wasting attempts, and none of the four is a different sentence to the
	// household — they all mean ask for a new code.
	ErrBadPairing = errors.New("appenroll: pairing code is not valid")
	// ErrUnknownRole is a role that is not in contract/registry.yaml.
	ErrUnknownRole = errors.New("appenroll: no such role")
	// ErrLastOwnerProtected is an attempt to demote the only owner, or to
	// remove it from somewhere other than the box itself.
	ErrLastOwnerProtected = errors.New("appenroll: that is the only owner")
)

// stored is the on-disk shape. Base64 rather than hex only because the app's
// QR payload is base64url and one encoding across the pair is one less thing
// to get wrong.
type stored struct {
	NoiseSecret      string `json:"noiseSecret"`
	RendezvousSecret string `json:"rendezvousSecret"`
	// AuthorisedApps holds every phone that has completed a pairing, with
	// enough metadata to tell them apart on the settings page. A key here
	// needs no code on later handshakes, which is what makes the code
	// single-use without breaking reconnects.
	//
	// Earlier versions stored a bare list of key strings; load() reads both
	// so an update never silently unpairs a house.
	AuthorisedApps []storedApp `json:"authorisedApps"`
}

type storedApp struct {
	Key        string `json:"key"`
	AddedAtMs  int64  `json:"addedAtMs,omitempty"`
	LastSeenMs int64  `json:"lastSeenMs,omitempty"`
	// Role is what this phone may do. Absent on a row written before roles
	// existed, and load() reads that as owner: a box updating from a version
	// with no roles must not silently demote every paired phone to viewer.
	Role string `json:"role,omitempty"`
	// Epoch counts how many times this grant has been given a different
	// role. It is what a log line names to say which version of a grant a
	// request ran under; it is not what makes a change bite. That is
	// GrantFor, which every privileged request calls. Absent, or zero, reads
	// as the first epoch.
	Epoch uint64 `json:"epoch,omitempty"`
}

// UnmarshalJSON accepts the legacy bare-string form beside the current one.
func (a *storedApp) UnmarshalJSON(raw []byte) error {
	var key string
	if err := json.Unmarshal(raw, &key); err == nil {
		*a = storedApp{Key: key}
		return nil
	}
	type plain storedApp
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*a = storedApp(v)
	return nil
}

// DeviceInfo is one paired phone, as the settings page needs to see it.
//
// The ID is a prefix of the key, not the key: enough to name a row and to
// revoke it, not enough to impersonate anyone even in a leaked screenshot.
type DeviceInfo struct {
	ID         string
	AddedAtMs  int64
	LastSeenMs int64
	// Role is what this phone may do. It belongs in the list because sharing
	// and locking out are the same screen: a household that cannot see which
	// phone is a guest cannot decide which one to remove.
	Role string
	// LastOwner marks the only phone left that can change anything here. It
	// cannot be stepped down at either door, and cannot be removed over a
	// session — so a screen can say what it is before somebody presses a
	// button rather than after.
	LastOwner bool
}

// deviceID is the row name: the first eight characters of the base64 key.
func deviceID(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// Identity is the box's app-facing enrollment state.
//
// Safe for concurrent use: the uplink authorises handshakes on its own
// goroutine while the API may be minting a code for the screen.
type Identity struct {
	path string

	mu     sync.Mutex
	static appwire.KeyPair
	// staticSecret is kept beside the key pair because appwire deliberately
	// exposes only the public half; persisting the private one is this
	// package's job and nobody else's.
	staticSecret []byte
	rendezvous   []byte
	authorised   map[string]*appMeta
	pairing      *pairingCode
	now          func() time.Time
	// randRead is the entropy source. Injectable so a test can prove the
	// failure path instead of only the happy one.
	randRead func([]byte) (int, error)
}

// appMeta is what the box remembers about one paired phone.
type appMeta struct {
	addedAtMs  int64
	lastSeenMs int64
	role       string
	epoch      uint64
}

// firstEpoch is where a fresh enrolment starts.
//
// One and not zero, so a row loaded from a file written before epochs existed
// is the first epoch rather than an unwritten field.
const firstEpoch uint64 = 1

// Grant is what a finished handshake earned: which device, what it may do,
// and which version of that answer it was told.
type Grant struct {
	DeviceID string
	Role     string
	Epoch    uint64
}

type pairingCode struct {
	code      []byte
	expiresAt time.Time
	// role is what the code lets in. Held by the box and stamped when the
	// code is spent, never carried in the QR payload: a role in the fragment
	// is a claim its holder can edit.
	role string
	// kind is "qr" or "spoken", and it exists to be logged and shown rather
	// than to be checked. Both kinds are redeemed identically — the same
	// field of the same handshake message — and the only thing that differs
	// is how many bytes were drawn and how long they last.
	kind string
	// attempts counts wrong guesses against this code. See Authorise.
	attempts int
}

// Kinds of pairing code.
const (
	// PairingKindQR is sixteen bytes, scanned off a screen.
	PairingKindQR = "qr"
	// PairingKindSpoken is five bytes, read aloud. See boxcode.go.
	PairingKindSpoken = "spoken"
)

// LoadOrCreate reads the material beside keyPath, minting whatever is missing.
//
// Minting is idempotent in the sense that matters: an existing static key is
// never replaced. Replacing it would silently unpair every phone in the house,
// and a box that has forgotten who it is should say so rather than quietly
// become a different box.
func LoadOrCreate(keyPath string) (*Identity, error) {
	path := filepath.Join(filepath.Dir(keyPath), FileName)

	id := &Identity{
		path:       path,
		authorised: map[string]*appMeta{},
		now:        time.Now,
		randRead:   rand.Read,
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := id.load(raw); err != nil {
			return nil, err
		}
		return id, nil
	case errors.Is(err, os.ErrNotExist):
		if err := id.mint(); err != nil {
			return nil, err
		}
		return id, id.save()
	default:
		return nil, fmt.Errorf("appenroll: reading %s: %w", path, err)
	}
}

func (i *Identity) load(raw []byte) error {
	var s stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("appenroll: %s is not readable: %w", i.path, err)
	}

	secret, err := base64.RawURLEncoding.DecodeString(s.NoiseSecret)
	if err != nil {
		return fmt.Errorf("appenroll: static key in %s is not base64: %w", i.path, err)
	}
	static, err := appwire.KeyPairFromSecret(secret)
	if err != nil {
		return fmt.Errorf("appenroll: static key in %s: %w", i.path, err)
	}
	rendezvous, err := base64.RawURLEncoding.DecodeString(s.RendezvousSecret)
	if err != nil {
		return fmt.Errorf("appenroll: rendezvous secret in %s is not base64: %w", i.path, err)
	}
	if len(rendezvous) != RendezvousSecretBytes {
		return fmt.Errorf("appenroll: rendezvous secret in %s is %d bytes, need %d",
			i.path, len(rendezvous), RendezvousSecretBytes)
	}

	i.static = static
	i.staticSecret = secret
	i.rendezvous = rendezvous
	for _, app := range s.AuthorisedApps {
		role := app.Role
		if role == "" {
			// A row from before roles existed. Owner, because these are the
			// phones the household already paired and trusts; reading them
			// as viewers would lock a house out of its own box on an update.
			role = apiauth.RoleOwner
		}
		epoch := app.Epoch
		if epoch == 0 {
			epoch = firstEpoch
		}
		i.authorised[app.Key] = &appMeta{
			addedAtMs:  app.AddedAtMs,
			lastSeenMs: app.LastSeenMs,
			role:       role,
			epoch:      epoch,
		}
	}
	return nil
}

func (i *Identity) mint() error {
	// Drawn here rather than by appwire.GenerateKeyPair because the secret
	// has to be written to disk, and appwire never hands one back — a key
	// that cannot be exported is the right default for a crypto package.
	secret := make([]byte, appwire.DHBytes)
	if _, err := i.randRead(secret); err != nil {
		return fmt.Errorf("appenroll: minting the static key: %w", err)
	}
	static, err := appwire.KeyPairFromSecret(secret)
	if err != nil {
		return fmt.Errorf("appenroll: minting the static key: %w", err)
	}
	rendezvous := make([]byte, RendezvousSecretBytes)
	if _, err := i.randRead(rendezvous); err != nil {
		return fmt.Errorf("appenroll: minting the rendezvous secret: %w", err)
	}

	i.static = static
	i.staticSecret = secret
	i.rendezvous = rendezvous
	return nil
}

// save writes through a temporary file and a rename, so a power cut during the
// write leaves the old material rather than half of the new. A box that loses
// its static key is a box every phone in the house has to be re-paired with.
func (i *Identity) save() error {
	i.mu.Lock()
	s := stored{
		NoiseSecret:      base64.RawURLEncoding.EncodeToString(i.staticSecret),
		RendezvousSecret: base64.RawURLEncoding.EncodeToString(i.rendezvous),
		AuthorisedApps:   make([]storedApp, 0, len(i.authorised)),
	}
	for key, meta := range i.authorised {
		s.AuthorisedApps = append(s.AuthorisedApps, storedApp{
			Key: key, AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
			Role: meta.role, Epoch: meta.epoch,
		})
	}
	// Deterministic on disk, so two saves of the same state are the same
	// bytes and a diff of applink.json says something.
	sort.Slice(s.AuthorisedApps, func(a, b int) bool {
		return s.AuthorisedApps[a].Key < s.AuthorisedApps[b].Key
	})
	i.mu.Unlock()

	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("appenroll: encoding %s: %w", i.path, err)
	}

	tmp := i.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("appenroll: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, i.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("appenroll: replacing %s: %w", i.path, err)
	}
	return nil
}

// StaticKey is the box's Noise static key pair.
func (i *Identity) StaticKey() appwire.KeyPair {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.static
}

// RendezvousSecret is the HKDF input the relay handle is derived from.
func (i *Identity) RendezvousSecret() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]byte(nil), i.rendezvous...)
}

// RotateRendezvousSecret replaces the secret and returns the new one.
//
// Every paired phone has to be told the new value before it can find this box
// again, so the caller owns that half. Provided because the alternative — a
// secret fixed for the life of the box — is the thing hourly rotation exists
// to avoid, one level up.
func (i *Identity) RotateRendezvousSecret() ([]byte, error) {
	secret := make([]byte, RendezvousSecretBytes)
	if _, err := i.randRead(secret); err != nil {
		return nil, fmt.Errorf("appenroll: minting a rendezvous secret: %w", err)
	}

	i.mu.Lock()
	i.rendezvous = secret
	i.mu.Unlock()

	if err := i.save(); err != nil {
		return nil, err
	}
	return append([]byte(nil), secret...), nil
}

// MintPairingCode issues a fresh single-use code and forgets any previous one.
//
// Forgetting matters: two live codes means two strangers can pair, and the
// second one was minted by whoever pressed the button last. One code at a time
// is the whole safety property, and it holds across kinds — minting an invite
// cancels a pending owner code and the reverse. The screen already copes with
// a code expiring, so it copes with this.
//
// The role is remembered here rather than carried in the QR payload, because a
// role the holder can edit is not a role. Nothing about the payload changes
// between an owner's code and a viewer's: the box remembers which it minted
// and stamps it when the code is spent.
//
// What stops the code being replayed, in the order it stops it:
//   - it is spent at the first success, so the second presentation of a
//     photographed code finds no live code at all;
//   - it lives for ttl and no longer;
//   - it survives MaxPairingAttempts wrong guesses and is then burned;
//   - it rides inside Noise handshake message 1, encrypted to this box's
//     static key, so the relay carrying it never sees it — and a replay of
//     that whole message earns nothing, because the replayer holds neither
//     the initiator's ephemeral nor its static private key and so cannot
//     derive the session keys the box answers with.
func (i *Identity) MintPairingCode(role string, ttl time.Duration) ([]byte, time.Time, error) {
	if err := knownRole(role); err != nil {
		return nil, time.Time{}, err
	}
	code := make([]byte, PairingCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return nil, time.Time{}, fmt.Errorf("appenroll: minting a pairing code: %w", err)
	}
	expires := i.now().Add(ttl)

	i.mu.Lock()
	i.pairing = &pairingCode{code: code, expiresAt: expires, role: role, kind: PairingKindQR}
	i.mu.Unlock()

	return append([]byte(nil), code...), expires, nil
}

// MintSpokenCode issues a box code: eight characters somebody can read aloud.
//
// It replaces any live code, exactly as MintPairingCode does, and it is spent,
// timed and counted by the same machinery. The only differences are that it
// carries forty bits rather than a hundred and twenty-eight, and that it lives
// for five minutes rather than ten.
//
// Returned grouped as XXXX-XXXX, ready to be shown and read. The bytes are
// what travels; the characters are for the person.
func (i *Identity) MintSpokenCode(role string) (string, time.Time, error) {
	if err := knownRole(role); err != nil {
		return "", time.Time{}, err
	}
	code := make([]byte, SpokenCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return "", time.Time{}, fmt.Errorf("appenroll: minting a box code: %w", err)
	}
	expires := i.now().Add(SpokenTTL)

	i.mu.Lock()
	i.pairing = &pairingCode{code: code, expiresAt: expires, role: role, kind: PairingKindSpoken}
	i.mu.Unlock()

	return GroupSpokenCode(encodeSpoken(code)), expires, nil
}

// knownRole refuses a role the registry does not define.
//
// Checked at every door into the stored state rather than trusted from the
// caller, because a role nobody recognises expands to no scopes at all — so a
// typo would not fail loudly, it would quietly make a phone that can do
// nothing and give nobody a reason why.
func knownRole(role string) error {
	if _, ok := apiauth.RoleScopes[role]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	return nil
}

// Authorise decides whether a finished handshake may become a session, and
// says what the session may do.
//
// appStatic is the app's static key as the handshake authenticated it, never
// as it was claimed; payload is handshake message 1's plaintext.
//
// A key already on the list needs no code, which is what lets a phone
// reconnect after every dropped socket without the box handing out a second
// pairing code. Anything else needs a live code, and spending one records the
// key so the next reconnect takes the first branch.
//
// The Grant is the seam. This function has always known which device it just
// authenticated; until now it threw that away and answered only yes or no,
// which is why nothing downstream could tell two phones apart.
func (i *Identity) Authorise(appStatic, payload []byte) (Grant, error) {
	key := base64.RawURLEncoding.EncodeToString(appStatic)

	i.mu.Lock()
	if meta := i.authorised[key]; meta != nil {
		// A reconnect. The stamp is what lets the settings page tell a
		// phone in daily use from a key that paired once and vanished —
		// which is exactly the row someone wants to revoke.
		meta.lastSeenMs = i.now().UnixMilli()
		grant := Grant{DeviceID: deviceID(key), Role: meta.role, Epoch: meta.epoch}
		i.mu.Unlock()
		// A stamp that could not be persisted must not block a session.
		_ = i.save()
		return grant, nil
	}
	// The rest happens under the lock, because counting a wrong guess is a
	// read and a write of the same code and two handshakes racing must not
	// each see four attempts and let a fifth and a sixth through.
	if len(payload) == 0 {
		i.mu.Unlock()
		return Grant{}, ErrNoPairing
	}
	pairing := i.pairing
	if pairing == nil || !i.now().Before(pairing.expiresAt) {
		i.mu.Unlock()
		return Grant{}, ErrBadPairing
	}
	// Constant time, because a byte-at-a-time comparison against a live code
	// is exactly the oracle an attacker with a relay socket would want.
	if subtle.ConstantTimeCompare(payload, pairing.code) != 1 {
		// A failure counter on the code itself, and this is the layer that
		// makes forty bits safe to say out loud: a guesser gets five tries
		// per minting, and every minting needs a person standing at the box.
		//
		// Deliberately not keyed on the caller's address. docs/architecture.md
		// records that the relay's own limiter keys on the socket address,
		// which behind the documented TLS terminator is a single address for
		// the whole fleet — an address-keyed counter here would inherit that
		// bug and count the world as one guesser.
		//
		// The cost is real and accepted: anyone who can reach this box can
		// burn a live code by guessing at it, and the household has to ask
		// for another. Denying somebody a code they can re-mint in a second
		// is a far smaller harm than a code that can be ground down at
		// leisure.
		pairing.attempts++
		if pairing.attempts >= MaxPairingAttempts {
			i.pairing = nil
		}
		i.mu.Unlock()
		return Grant{}, ErrBadPairing
	}

	now := i.now().UnixMilli()
	// The role travels with the code rather than with the handshake, so a
	// code minted for a viewer stamps a viewer here — and nothing the app
	// sends can change it.
	role := pairing.role
	if role == "" {
		role = apiauth.RoleOwner
	}
	if role != apiauth.RoleOwner && !i.hasOwnerLocked() {
		// Whoever creates a home owns it. The first enrolment is an owner
		// whatever its code said, because a box with no owner cannot be
		// administered by anybody, from anywhere, ever again — and a
		// household that handed out a view-only code before pairing its own
		// phone would have built exactly that.
		role = apiauth.RoleOwner
	}
	i.authorised[key] = &appMeta{addedAtMs: now, lastSeenMs: now, role: role, epoch: firstEpoch}
	// Spent. A second phone offering the same photographed code now meets
	// ErrBadPairing rather than a pairing.
	i.pairing = nil
	i.mu.Unlock()

	return Grant{DeviceID: deviceID(key), Role: role, Epoch: firstEpoch}, i.save()
}

// hasOwnerLocked reports whether any enrolment can still administer this box.
// The caller holds the mutex.
func (i *Identity) hasOwnerLocked() bool {
	for _, meta := range i.authorised {
		if meta.role == apiauth.RoleOwner {
			return true
		}
	}
	return false
}

// ownerCountLocked is how many enrolments can administer this box. The caller
// holds the mutex.
func (i *Identity) ownerCountLocked() int {
	n := 0
	for _, meta := range i.authorised {
		if meta.role == apiauth.RoleOwner {
			n++
		}
	}
	return n
}

// findLocked returns the enrolment behind a device id. The caller holds the
// mutex.
func (i *Identity) findLocked(id string) (string, *appMeta) {
	for key, meta := range i.authorised {
		if deviceID(key) == id {
			return key, meta
		}
	}
	return "", nil
}

// GrantFor is one enrolment as it stands right now.
//
// Read on every privileged request rather than at the handshake, because a
// socket outlives both a revoke and a role change. Three layers stop a revoked
// phone and none of them is enough alone: the session is torn down, the next
// handshake fails, and this — the one that closes the window where a socket
// outlives the revoke. It is also the only thing that makes a demotion bite
// on a session that is already open and working.
//
// The second result is false when the enrolment is gone entirely, which is a
// revoke. A grant that merely changed comes back with its new role, and the
// caller is expected to start using it rather than to end the session: a
// demoted owner is still a viewer, and telling them their access was withdrawn
// would be a sentence the code does not mean.
//
// The honest limit belongs beside it: revocation is immediate at the box.
// Nothing can un-send bytes already in a phone's cache.
func (i *Identity) GrantFor(id string) (Grant, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	key, meta := i.findLocked(id)
	if meta == nil {
		return Grant{}, false
	}
	return Grant{DeviceID: deviceID(key), Role: meta.role, Epoch: meta.epoch}, true
}

// SetRole changes what one paired phone may do.
//
// It takes effect at once on a session that is already open, and the mechanism
// is GrantFor rather than anything here: both doors re-read the grant on every
// privileged request, so a phone demoted while it is connected loses its writes
// at the very next one rather than at its next reconnect. The epoch moves
// alongside, as the count of how many times this grant has changed.
//
// Refused for the only owner, here rather than in the API layer — otherwise
// the box's own web UI could do what the app cannot, and the protection would
// be a property of the screen instead of a property of the box.
//
// Refused at both doors, the box's own page included. Removing the last owner
// there is allowed — see Revoke — because a household whose phone is gone has
// to be able to empty the list and start again. Stepping the last owner down
// leaves that same box carrying a list of phones that can only look, which is
// the shape of the problem rather than a way out of it.
func (i *Identity) SetRole(id, role string) error {
	if err := knownRole(role); err != nil {
		return err
	}

	i.mu.Lock()
	_, meta := i.findLocked(id)
	if meta == nil {
		i.mu.Unlock()
		return ErrUnknownDevice
	}
	if meta.role == role {
		// Nothing changed, so nothing is bumped. An epoch that moved for a
		// write that changed nothing would make every live session re-read a
		// grant that is the same as the one it holds.
		i.mu.Unlock()
		return nil
	}
	if meta.role == apiauth.RoleOwner && i.ownerCountLocked() == 1 {
		i.mu.Unlock()
		return ErrLastOwnerProtected
	}
	meta.role = role
	meta.epoch++
	i.mu.Unlock()

	return i.save()
}

// Devices lists every paired phone, most recently seen first.
func (i *Identity) Devices() []DeviceInfo {
	i.mu.Lock()
	lastOwner := i.ownerCountLocked() == 1
	out := make([]DeviceInfo, 0, len(i.authorised))
	for key, meta := range i.authorised {
		out = append(out, DeviceInfo{
			ID: deviceID(key), AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
			Role:      meta.role,
			LastOwner: lastOwner && meta.role == apiauth.RoleOwner,
		})
	}
	i.mu.Unlock()

	sort.Slice(out, func(a, b int) bool {
		if out[a].LastSeenMs != out[b].LastSeenMs {
			return out[a].LastSeenMs > out[b].LastSeenMs
		}
		return out[a].ID < out[b].ID
	})
	return out
}

// ErrUnknownDevice is a revoke aimed at an id no paired phone carries.
var ErrUnknownDevice = errors.New("appenroll: no such device")

// Presence says whether whoever is asking is standing at the box.
//
// It is not a role and it is not a scope: an owner's phone holds every scope
// this box grants and still cannot prove where it is. Presence is the one
// thing only the LAN door establishes, and here it settles exactly one
// question — whether the last owner may go.
type Presence bool

const (
	// AtTheBox is a request off the box's own page, on the home network.
	AtTheBox Presence = true
	// OverASession is an enrolled phone, which could be anywhere on earth.
	OverASession Presence = false
)

// Revoke forgets a phone by its device id and returns the full key, so the
// caller can also tear down any session that key is running right now. The
// next handshake from it meets ErrNoPairing like any stranger's.
//
// Removing a guest and locking out a phone are the same action, deliberately.
// Sharing does not get its own gesture with its own bugs: a shared phone is a
// row in the same list, removed by the same button.
//
// Refused for the only owner over a session, and allowed at the box.
//
// What the refusal guards against is a phone locking a household out of its
// own box from anywhere in the world: remove the last owner remotely and
// nobody can administer the box, and nothing done remotely can undo that.
// Presence is the way back — whoever is standing at the box can mint a fresh
// owner's code on the same page the Remove button is on. So at the box the
// refusal protects nothing, and all it does is stop a household emptying the
// list after a phone is lost or starting clean.
//
// The decision stays here rather than in the API layer. What is new is that
// the box is told which door the request came through; which screen drew the
// button still decides nothing.
func (i *Identity) Revoke(id string, from Presence) ([]byte, error) {
	i.mu.Lock()
	fullKey, meta := i.findLocked(id)
	if meta == nil {
		i.mu.Unlock()
		return nil, ErrUnknownDevice
	}
	if from == OverASession && meta.role == apiauth.RoleOwner && i.ownerCountLocked() == 1 {
		i.mu.Unlock()
		return nil, ErrLastOwnerProtected
	}
	delete(i.authorised, fullKey)
	i.mu.Unlock()

	if err := i.save(); err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(fullKey)
	if err != nil {
		// The key was minted by EncodeToString; this cannot happen outside a
		// corrupted file, and the revoke itself already stuck.
		return nil, nil
	}
	return raw, nil
}

// AuthorisedCount is for the API and for tests. The keys themselves stay here.
func (i *Identity) AuthorisedCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.authorised)
}
