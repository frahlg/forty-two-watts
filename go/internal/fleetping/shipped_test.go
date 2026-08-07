package fleetping

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/fleetping/genshipped"
)

const pinPath = "../../../drivers/BUNDLED_SOURCE.json"

// The compiled-in names must be what the pin currently says. A snapshot
// updated without rerunning the generator is a shipped catalogue that names a
// driver this build does not have, or misses one it does — and the second of
// those quietly reports a real driver as "other".
func TestShippedGenIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatalf("read the bundled-driver pin: %v", err)
	}
	want, err := genshipped.Generate(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("shipped_gen.go")
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("shipped_gen.go is stale; run: go generate ./internal/fleetping/...\n"+
			"(pin: %s)", filepath.Clean(pinPath))
	}
}

// The claim the whole rule rests on: this set is fixed when the binary is
// built. Nothing a box does at run time may change it, so the caller gets a
// copy and the copy is not the thing itself.
func TestShippedCatalogueCannotBeWrittenTo(t *testing.T) {
	first := ShippedCatalogue()
	if len(first) == 0 {
		t.Fatal("the shipped catalogue is empty; every configured driver would report as other")
	}
	first[0] = "pappas-laddbox"

	second := ShippedCatalogue()
	if len(second) != len(shippedDrivers) {
		t.Fatalf("a caller changed the shipped catalogue's length: %d, want %d", len(second), len(shippedDrivers))
	}
	for i, name := range second {
		if name != shippedDrivers[i] {
			t.Fatalf("a caller changed the shipped catalogue: %q at %d, want %q", name, i, shippedDrivers[i])
		}
	}
}

// The pin is the list of drivers CI fetches, and the fleet ping now treats it
// as the list of names FTW publishes. Anything in it that is not a plain
// driver id would travel verbatim, so the generator refuses it — and this is
// the check that the pin as committed passes.
func TestThePinHoldsOnlyDriverIDs(t *testing.T) {
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatalf("read the bundled-driver pin: %v", err)
	}
	var pin struct {
		Drivers []string `json:"drivers"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatalf("parse the pin: %v", err)
	}
	if len(pin.Drivers) != len(shippedDrivers) {
		t.Fatalf("the pin lists %d drivers, the build carries %d", len(pin.Drivers), len(shippedDrivers))
	}
	for _, name := range pin.Drivers {
		if !slicesHas(shippedDrivers[:], name) {
			t.Errorf("%q is pinned but not compiled in", name)
		}
	}
}

// The generator is the gate, so the things it must refuse are worth naming.
func TestTheGeneratorRefusesWhatIsNotADriverID(t *testing.T) {
	cases := []struct {
		name string
		pin  string
	}{
		{"a path", `{"drivers": ["../../home/pi/pappas-laddbox"]}`},
		{"a filename", `{"drivers": ["sungrow.lua"]}`},
		{"a household's own name", `{"drivers": ["pappas-laddbox"]}`},
		{"a name with a space", `{"drivers": ["sungrow vasagatan"]}`},
		{"the same name twice", `{"drivers": ["sungrow", "sungrow"]}`},
		{"no drivers at all", `{"drivers": []}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := genshipped.Generate([]byte(tc.pin)); err == nil {
				t.Error("the generator accepted it")
			}
		})
	}
}

func slicesHas(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
