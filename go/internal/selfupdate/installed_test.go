package selfupdate

import (
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
)

func collectInstalled(bus *events.Bus) func() []events.UpdateInstalled {
	var mu sync.Mutex
	var got []events.UpdateInstalled
	bus.Subscribe(events.KindUpdateInstalled, func(e events.Event) {
		if ev, ok := e.(events.UpdateInstalled); ok {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	})
	return func() []events.UpdateInstalled {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.UpdateInstalled(nil), got...)
	}
}

// The first boot of a new version announces it — once, because the recorded
// version moves before the announcement, so the next boot has nothing new
// to say.
func TestAnnounceInstalledFiresOncePerVersionChange(t *testing.T) {
	store := newMemStore()
	bus := events.NewBus()
	installed := collectInstalled(bus)
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }

	// The previous run wrote its version down before it was replaced.
	New(Config{CurrentVersion: "v1.16.1", Bus: bus, Now: now}, store).announceInstalled()
	if got := installed(); len(got) != 0 {
		t.Fatalf("first boot announced %v; nothing was updated", got)
	}

	// The updated process boots.
	c := New(Config{CurrentVersion: "v1.17.0", Bus: bus, Now: now}, store)
	c.announceInstalled()
	got := installed()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1", len(got))
	}
	if got[0].Version != "v1.17.0" || got[0].PreviousVersion != "v1.16.1" {
		t.Fatalf("event = %+v", got[0])
	}

	// A plain restart of the same version says nothing.
	c.announceInstalled()
	New(Config{CurrentVersion: "v1.17.0", Bus: bus, Now: now}, store).announceInstalled()
	if got := installed(); len(got) != 1 {
		t.Fatalf("announcements = %d after restarts, want 1", len(got))
	}
}

// A dev binary changes identity every build without installing anything;
// it records itself but never announces.
func TestAnnounceInstalledIgnoresDevBuilds(t *testing.T) {
	store := newMemStore()
	bus := events.NewBus()
	installed := collectInstalled(bus)

	New(Config{CurrentVersion: "dev", Bus: bus}, store).announceInstalled()
	New(Config{CurrentVersion: "v1.17.0", Bus: bus}, store).announceInstalled()
	if got := installed(); len(got) != 0 {
		t.Fatalf("dev transitions announced %v", got)
	}
}
