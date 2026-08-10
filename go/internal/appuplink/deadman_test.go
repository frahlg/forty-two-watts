package appuplink

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appenroll"
)

type stubDeadman struct {
	mu   sync.Mutex
	rows []DeadmanRow
}

func (s *stubDeadman) DeadmanRows() ([]DeadmanRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DeadmanRow(nil), s.rows...), nil
}

func (s *stubDeadman) set(rows []DeadmanRow) {
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
}

// newDeadmanRig is newRig with the switch armed.
func newDeadmanRig(t *testing.T, epoch int64, source DeadmanSource) *rig {
	t.Helper()

	relay := newFakeRelay(epoch)
	t.Cleanup(relay.close)

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	now := time.UnixMilli(epoch * EpochMs)
	uplink, err := New(Options{
		Endpoint: relay.url(),
		Enroll:   enroll,
		Handler:  newHandler,
		Deadman:  source,
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
		Random:   func() float64 { return 0 },
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
// pre-encrypted ct, deadline, exactly the contract's body — and claims
// every id on the socket it just opened.
func TestDeadmanRowsPostedAndClaimedOnConnect(t *testing.T) {
	source := &stubDeadman{}
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push.example/a", CT: []byte("ct-one")},
		{SubscriptionID: "s2", Endpoint: "https://push.example/b", CT: []byte("ct-two")},
	})
	r := newDeadmanRig(t, 481234, source)

	rows, claims := waitForDeadman(t, r.relay, func(rows map[string]deadmanRowWire, claims []string) bool {
		return len(rows) == 2 && len(claims) >= 2
	})

	secret := r.enroll.RendezvousSecret()
	for _, want := range []struct {
		sub, endpoint, ct string
	}{
		{"s1", "https://push.example/a", "ct-one"},
		{"s2", "https://push.example/b", "ct-two"},
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
	source := &stubDeadman{}
	source.set([]DeadmanRow{
		{SubscriptionID: "s1", Endpoint: "https://push.example/a", CT: []byte("ct-one")},
		{SubscriptionID: "s2", Endpoint: "https://push.example/b", CT: []byte("ct-two")},
	})
	r := newDeadmanRig(t, 481234, source)
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
