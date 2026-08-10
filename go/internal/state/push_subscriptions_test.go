package state

import (
	"path/filepath"
	"testing"
)

func pushTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPushSubscriptionCRUD(t *testing.T) {
	st := pushTestStore(t)

	id, err := st.SavePushSubscription("https://push.example/a", "keyA", "authA", "dev1")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == "" {
		t.Fatal("no id returned")
	}

	// Re-subscribing the same endpoint is the same subscription with
	// fresh keys — the id must hold, and the keys must move.
	again, err := st.SavePushSubscription("https://push.example/a", "keyA2", "authA2", "dev1")
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if again != id {
		t.Fatalf("endpoint upsert minted a new id: %q then %q", id, again)
	}
	rows, err := st.PushSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].P256dh != "keyA2" || rows[0].Auth != "authA2" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].DeviceID != "dev1" || rows[0].CreatedAtMs == 0 {
		t.Fatalf("row = %+v", rows[0])
	}

	if err := st.DeletePushSubscription(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeletePushSubscription(id); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}
	if rows, _ := st.PushSubscriptions(); len(rows) != 0 {
		t.Fatalf("rows after delete = %+v", rows)
	}
}

// Revoking a device sweeps its subscriptions and nobody else's — and an
// empty device id sweeps nothing, because the unclaimed rows are exactly
// the ones revocation must not touch.
func TestPushSubscriptionDeviceSweep(t *testing.T) {
	st := pushTestStore(t)

	if _, err := st.SavePushSubscription("https://push.example/a", "k", "a", "phone1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SavePushSubscription("https://push.example/b", "k", "a", "phone1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SavePushSubscription("https://push.example/c", "k", "a", "phone2"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SavePushSubscription("https://push.example/d", "k", "a", ""); err != nil {
		t.Fatal(err)
	}

	n, err := st.DeletePushSubscriptionsByDevice("phone1")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept %d rows, want 2", n)
	}
	if n, err := st.DeletePushSubscriptionsByDevice(""); err != nil || n != 0 {
		t.Fatalf("empty device sweep = %d, %v; want 0, nil", n, err)
	}
	rows, _ := st.PushSubscriptions()
	if len(rows) != 2 {
		t.Fatalf("rows after sweep = %+v", rows)
	}
	for _, r := range rows {
		if r.DeviceID == "phone1" {
			t.Fatalf("phone1 row survived the sweep: %+v", r)
		}
	}
}
