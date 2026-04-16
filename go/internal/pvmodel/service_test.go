package pvmodel

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// TestResetPersistsSurvivesRestart verifies that calling Reset() writes the
// clean model to SQLite so that a fresh NewService (simulating a restart)
// loads the reset state, not the old trained state.
func TestResetPersistsSurvivesRestart(t *testing.T) {
	db := openTestDB(t)

	ratedW := 5000.0
	cs := func(time.Time) float64 { return 500 }
	cl := func(time.Time) (float64, bool) { return 20, true }

	// --- Phase 1: seed a "trained" model and persist it ---
	svc := NewService(db, nil, cs, cl, ratedW)
	// Mutate the model to simulate training.
	svc.mu.Lock()
	svc.model.Samples = 200
	svc.model.MAE = 42
	svc.model.Beta[0] = 999 // clearly non-default
	svc.mu.Unlock()
	svc.persist()

	// Verify the trained state is in the DB.
	js, ok := db.LoadConfig(stateKey)
	if !ok || js == "" {
		t.Fatal("trained model not persisted")
	}
	var trained Model
	if err := json.Unmarshal([]byte(js), &trained); err != nil {
		t.Fatal(err)
	}
	if trained.Samples != 200 {
		t.Fatalf("expected 200 samples in stored model, got %d", trained.Samples)
	}

	// --- Phase 2: reset ---
	svc.Reset()

	// Verify the reset state is now in the DB (samples=0, fresh beta).
	js2, ok := db.LoadConfig(stateKey)
	if !ok || js2 == "" {
		t.Fatal("reset model not persisted")
	}
	var reset Model
	if err := json.Unmarshal([]byte(js2), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Samples != 0 {
		t.Fatalf("expected 0 samples after reset, got %d", reset.Samples)
	}
	if reset.Beta[0] != 0 {
		t.Fatalf("expected Beta[0]=0 after reset, got %f", reset.Beta[0])
	}

	// --- Phase 3: simulate restart ---
	svc2 := NewService(db, nil, cs, cl, ratedW)
	m := svc2.Model()
	if m.Samples != 0 {
		t.Fatalf("after restart: expected 0 samples, got %d", m.Samples)
	}
	if m.Beta[0] != 0 {
		t.Fatalf("after restart: expected Beta[0]=0, got %f", m.Beta[0])
	}
	// Cold-start beta[2] should be ratedW/1000.
	expected := ratedW / 1000
	if m.Beta[2] != expected {
		t.Fatalf("after restart: expected Beta[2]=%f, got %f", expected, m.Beta[2])
	}
}

func openTestDB(t *testing.T) *state.Store {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
