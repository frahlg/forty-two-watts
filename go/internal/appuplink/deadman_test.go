package appuplink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/notifications"
)

// testVAPIDKey mirrors nova.Identity's signing surface, the same double the
// notifications tests use, so the rows below carry real ES256 headers.
type testVAPIDKey struct{ priv *ecdsa.PrivateKey }

func newTestVAPIDKey(t *testing.T) *testVAPIDKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &testVAPIDKey{priv: priv}
}

func (k *testVAPIDKey) PublicKeyHex() string {
	out := make([]byte, 64)
	k.priv.X.FillBytes(out[:32])
	k.priv.Y.FillBytes(out[32:])
	return hex.EncodeToString(out)
}

func (k *testVAPIDKey) SignRawHex(msg string) (string, error) {
	hash := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, hash[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return hex.EncodeToString(sig), nil
}

func (k *testVAPIDKey) publicB64() string {
	out := make([]byte, 65)
	out[0] = 0x04
	k.priv.X.FillBytes(out[1:33])
	k.priv.Y.FillBytes(out[33:])
	return base64.RawURLEncoding.EncodeToString(out)
}

// testClock is a movable now, shared between the uplink and the row source
// so a test can let six hours pass without waiting for them.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// stubDeadman is main.go's row source in miniature: it holds the current
// subscriptions' rows and signs each row's auth freshly on every call,
// through the same exported builder production uses — which is what lets
// the refresh test below prove a re-post carries a new signature rather
// than a replay of the old one.
type stubDeadman struct {
	key  notifications.VAPIDKey
	now  func() time.Time
	mu   sync.Mutex
	rows []DeadmanRow
}

func (s *stubDeadman) DeadmanRows() ([]DeadmanRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeadmanRow, 0, len(s.rows))
	for _, row := range s.rows {
		auth, err := notifications.DeadmanAuthorization(s.key, row.Endpoint, s.now())
		if err != nil {
			return nil, err
		}
		row.Auth = auth
		out = append(out, row)
	}
	return out, nil
}

func (s *stubDeadman) set(rows []DeadmanRow) {
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
}

// newDeadmanRig is newRig with the switch armed. now drives the epoch (and
// should be the same clock the source signs with); refresh shortens the
// six-hour re-posting cadence, zero keeping production's.
func newDeadmanRig(t *testing.T, epoch int64, source DeadmanSource, now func() time.Time, refresh time.Duration) *rig {
	t.Helper()

	relay := newFakeRelay(epoch)
	t.Cleanup(relay.close)

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	uplink, err := New(Options{
		Endpoint:       relay.url(),
		Enroll:         enroll,
		Handler:        newHandler,
		Deadman:        source,
		Logger:         slog.New(slog.DiscardHandler),
		Now:            now,
		Random:         func() float64 { return 0 },
		DeadmanRefresh: refresh,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- uplink.Run(ctx) }()

	return &rig{relay: relay, enroll: enroll, uplink: uplink, cancel: cancel, done: done}
}

func fixedNow(epoch int64) func() time.Time {
	now := time.UnixMilli(epoch * EpochMs)
	return func() time.Time { return now }
}

func waitForDeadman(t *testing.T, relay *fakeRelay, ok func(rows map[string]deadmanRowWire, claims []string) bool) (map[string]deadmanRowWire, []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, claims := relay.deadmanState()
		if ok(rows, claims) {
			return rows, claims
		}
		if time.Now().After(deadline) {
			t.Fatalf("dead man state never settled: rows=%v claims=%v", rows, claims)
		}
		select {
		case <-relay.deadman:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// vapidClaims unpacks a stored row's Authorization header: the JWT's aud
// and exp, and the k= public key it names.
func vapidClaims(t *testing.T, auth string) (aud string, exp int64, k string) {
	t.Helper()
	if !strings.HasPrefix(auth, "vapid t=") {
		t.Fatalf("auth = %q", auth)
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "vapid t="), ", k=", 2)
	if len(parts) != 2 {
		t.Fatalf("auth = %q", auth)
	}
	segments := strings.Split(parts[0], ".")
	if len(segments) != 3 {
		t.Fatalf("JWT has %d segments", len(segments))
	}
	raw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Aud, claims.Exp, parts[1]
}

// The id is 32 lowercase hex chars, stable across calls, different per
// subscription and per secret — the relay must be able to key on it and
// learn nothing from it.
func TestDeadmanIDShape(t *testing.T) {
	secret := make([]byte, 32)
	a := DeadmanID(secret, "sub1")
	if len(a) != 32 {
		t.Fatalf("id is %d chars, want 32", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("id is not hex: %v", err)
	}
	if DeadmanID(secret, "sub1") != a {
		t.Fatal("the id is not deterministic")
	}
	if DeadmanID(secret, "sub2") == a {
		t.Fatal("two subscriptions share an id")
	}
	other := make([]byte, 32)
	other[0] = 1
	if DeadmanID(other, "sub1") == a {
		t.Fatal("two secrets share an id")
	}
}

// On connect the box posts one row per subscription — id, endpoint,
// pre-encrypted ct, auth, deadline, exactly the contract's body — and
// claims every id on the socket it just opened. Each row's auth is a VAPID
// header the push service could verify: aud is that row's own endpoint
// origin, exp the full 24 hours out, k the box's key.
func TestDeadmanRowsPostedAndClaimedOnConnect(t *testing.T) {
	const epoch = 481234
	key := newTestVAPIDKey(t)
	now := fixedNow(epoch)
	source := &stubDeadman{key: key, now: now}
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push-a.example/a", CT: []byte("ct-one")},
		{SubscriptionID: "s2", Endpoint: "https://push-b.example/b", CT: []byte("ct-two")},
	})
	r := newDeadmanRig(t, epoch, source, now, 0)

	rows, claims := waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, claims []string) bool {
		return len(rows) == 2 && len(claims) >= 2
	})

	secret := r.enroll.RendezvousSecret()
	for _, want := range []struct {
		sub, endpoint, origin, ct string
	}{
		{"s1", "https://push-a.example/a", "https://push-a.example", "ct-one"},
		{"s2", "https://push-b.example/b", "https://push-b.example", "ct-two"},
	} {
		id := DeadmanID(secret, want.sub)
		row, ok := rows[id]
		if !ok {
			t.Fatalf("no row for subscription %s (id %s): %v", want.sub, id, rows)
		}
		if row.Endpoint != want.endpoint {
			t.Fatalf("row endpoint = %q, want %q", row.Endpoint, want.endpoint)
		}
		ct, err := base64.StdEncoding.DecodeString(row.CT)
		if err != nil || string(ct) != want.ct {
			t.Fatalf("row ct = %q (%v), want %q", row.CT, err, want.ct)
		}
		if row.DeadlineS != DeadmanDeadlineS {
			t.Fatalf("deadline_s = %d, want %d", row.DeadlineS, DeadmanDeadlineS)
		}
		aud, exp, k := vapidClaims(t, row.Auth)
		if aud != want.origin {
			t.Fatalf("auth aud = %q, want this row's own origin %q", aud, want.origin)
		}
		if wantExp := now().Add(24 * time.Hour).Unix(); exp != wantExp {
			t.Fatalf("auth exp = %d, want 24h out (%d)", exp, wantExp)
		}
		if k != key.publicB64() {
			t.Fatalf("auth k = %q, want the box's key %q", k, key.publicB64())
		}
		claimed := false
		for _, c := range claims {
			if c == id {
				claimed = true
			}
		}
		if !claimed {
			t.Fatalf("id %s was posted but never claimed: %v", id, claims)
		}
	}
}

// A removed subscription's row is deleted on resync — the relay must not
// keep a farewell for a phone that no longer exists — and the remaining
// ids are re-claimed.
func TestDeadmanResyncDeletesRemovedRows(t *testing.T) {
	const epoch = 481234
	key := newTestVAPIDKey(t)
	now := fixedNow(epoch)
	source := &stubDeadman{key: key, now: now}
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push.example/a", CT: []byte("ct-one")},
		{SubscriptionID: "s2", Endpoint: "https://push.example/b", CT: []byte("ct-two")},
	})
	r := newDeadmanRig(t, epoch, source, now, 0)
	waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, claims []string) bool {
		return len(rows) == 2 && len(claims) >= 2
	})

	// The phone behind s2 was revoked; its subscription went with it.
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push.example/a", CT: []byte("ct-one")},
	})
	r.uplink.ResyncDeadman()

	secret := r.enroll.RendezvousSecret()
	gone := DeadmanID(secret, "s2")
	kept := DeadmanID(secret, "s1")
	rows, _ := waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, claims []string) bool {
		_, still := rows[gone]
		return len(rows) == 1 && !still
	})
	if _, ok := rows[kept]; !ok {
		t.Fatalf("the surviving subscription's row went too: %v", rows)
	}
}

// While the connection lasts, the rows are re-posted on the refresh cadence
// with a freshly signed header each time: six hours after the first post
// the relay must hold a JWT expiring six hours later too, or the header
// would one day expire in the relay's hands and the switch would fire with
// a voice the push service refuses.
func TestDeadmanRefreshRepostsWithFreshJWT(t *testing.T) {
	const epoch = 481234
	key := newTestVAPIDKey(t)
	clock := &testClock{t: time.UnixMilli(epoch * EpochMs)}
	source := &stubDeadman{key: key, now: clock.Now}
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push.example/a", CT: []byte("ct-one")},
	})
	r := newDeadmanRig(t, epoch, source, clock.Now, 40*time.Millisecond)

	id := DeadmanID(r.enroll.RendezvousSecret(), "s1")
	rows, _ := waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, _ []string) bool {
		_, ok := rows[id]
		return ok
	})
	_, exp0, _ := vapidClaims(t, rows[id].Auth)

	// Six hours pass while the connection holds. The next refresh tick must
	// re-post the row signed against the moved clock — a replayed header
	// would keep the old exp.
	clock.Advance(6 * time.Hour)
	rows, _ = waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, _ []string) bool {
		row, ok := rows[id]
		if !ok {
			return false
		}
		_, exp, _ := vapidClaims(t, row.Auth)
		return exp != exp0
	})
	_, exp1, _ := vapidClaims(t, rows[id].Auth)
	if got, want := exp1-exp0, int64(6*3600); got != want {
		t.Fatalf("exp moved by %ds, want the clock's 6h (%ds)", got, want)
	}
}

// With the switch unarmed the box says no text word at all — a relay that
// predates the contract must see exactly the traffic it always saw.
func TestUnarmedUplinkSaysNoTextWords(t *testing.T) {
	r := newRig(t, 481234)
	select {
	case <-r.relay.joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the box never joined")
	}
	time.Sleep(100 * time.Millisecond)
	rows, claims := r.relay.deadmanState()
	if len(rows) != 0 || len(claims) != 0 {
		t.Fatalf("an unarmed box spoke: rows=%v claims=%v", rows, claims)
	}
}
