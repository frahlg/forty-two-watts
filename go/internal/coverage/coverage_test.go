package coverage

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/prices"
)

func TestForecastProvidersAreWorldwide(t *testing.T) {
	for _, id := range []string{"met_no", "openweather", "open_meteo", "forecast_solar"} {
		s, ok := ByID(id)
		if !ok {
			t.Fatalf("%s: not registered", id)
		}
		if !s.Worldwide() {
			t.Errorf("%s: want worldwide", id)
		}
		if !s.Covers(-33.87, 151.21) {
			t.Errorf("%s: worldwide source must cover Sydney", id)
		}
	}
}

// The whole point of #726: price data is Europe-only. If someone adds a global
// price provider this test should be updated deliberately, not incidentally.
func TestPriceProvidersAreEuropeOnly(t *testing.T) {
	got := ForKind(KindPrice)
	if len(got) == 0 {
		t.Fatal("no price sources registered")
	}
	for _, s := range got {
		if s.Worldwide() {
			t.Errorf("%s: price sources are not worldwide", s.ID)
		}
		if s.Covers(-33.87, 151.21) {
			t.Errorf("%s: must not claim to cover Sydney", s.ID)
		}
		if s.Covers(40.71, -74.01) {
			t.Errorf("%s: must not claim to cover New York", s.ID)
		}
	}
}

func TestSwedishPriceProviderIsNarrowerThanEuropean(t *testing.T) {
	if Covers("elprisetjustnu", 52.52, 13.40) {
		t.Error("elprisetjustnu must not claim Berlin")
	}
	if !Covers("sourceful", 52.52, 13.40) {
		t.Error("sourceful should cover Berlin")
	}
	if !Covers("elprisetjustnu", 59.33, 18.07) {
		t.Error("elprisetjustnu should cover Stockholm")
	}
	if Covers("elprisetjustnu", 69.65, 18.96) {
		t.Error("elprisetjustnu must not claim Tromsø")
	}
}

func TestUnknownSourceIsNotCovered(t *testing.T) {
	if Covers("does_not_exist", 59.33, 18.07) {
		t.Error("unknown source must report not covered")
	}
	if _, ok := ByID("does_not_exist"); ok {
		t.Error("unknown source must not resolve")
	}
}

func TestBBoxContainsIsInclusive(t *testing.T) {
	b := BBox{MinLat: 10, MinLon: 20, MaxLat: 30, MaxLon: 40}
	for _, c := range []struct {
		lat, lon float64
		want     bool
	}{
		{10, 20, true},
		{30, 40, true},
		{20, 30, true},
		{9.99, 30, false},
		{20, 40.01, false},
	} {
		if got := b.Contains(c.lat, c.lon); got != c.want {
			t.Errorf("Contains(%v,%v) = %v, want %v", c.lat, c.lon, got, c.want)
		}
	}
}

func TestBBoxDoesNotWrapLongitude(t *testing.T) {
	b := BBox{MinLat: -90, MinLon: -180, MaxLat: 90, MaxLon: 180}
	if b.Contains(0, 200) {
		t.Error("lon 200 must not wrap to -160")
	}
}

func TestRegistryIsInternallyConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range All() {
		if s.ID == "" || s.Label == "" || s.Area == "" {
			t.Errorf("%+v: id, label and area are all required", s)
		}
		if seen[s.ID] {
			t.Errorf("%s: duplicate id", s.ID)
		}
		seen[s.ID] = true
		if s.BBox != nil {
			if s.BBox.MinLat > s.BBox.MaxLat || s.BBox.MinLon > s.BBox.MaxLon {
				t.Errorf("%s: inverted bbox %+v", s.ID, *s.BBox)
			}
		}
	}
}

func TestAllReturnsACopy(t *testing.T) {
	got := All()
	original := got[0].ID
	got[0].ID = "mutated"
	if All()[0].ID != original {
		t.Fatal("All() exposed the backing array")
	}
}

// zoneCountryISO is the country-name → ISO 3166-1 alpha-2 map for every
// country currently in the price zone table. A new country in zones.go that
// is missing here is a coverage-registry miss, not a silent expansion.
var zoneCountryISO = map[string]string{
	"Austria":        "AT",
	"Belgium":        "BE",
	"Bulgaria":       "BG",
	"Croatia":        "HR",
	"Czech Republic": "CZ",
	"Denmark":        "DK",
	"Estonia":        "EE",
	"Finland":        "FI",
	"France":         "FR",
	"Germany":        "DE",
	"Greece":         "GR",
	"Hungary":        "HU",
	"Italy":          "IT",
	"Latvia":         "LV",
	"Lithuania":      "LT",
	"Luxembourg":     "LU",
	"Montenegro":     "ME",
	"Netherlands":    "NL",
	"Norway":         "NO",
	"Poland":         "PL",
	"Portugal":       "PT",
	"Romania":        "RO",
	"Serbia":         "RS",
	"Slovakia":       "SK",
	"Slovenia":       "SI",
	"Spain":          "ES",
	"Sweden":         "SE",
	"Switzerland":    "CH",
	"Ukraine":        "UA",
}

func TestEuropeanPriceCountriesMatchZoneTable(t *testing.T) {
	src, ok := ByID("sourceful")
	if !ok {
		t.Fatal("sourceful not registered")
	}
	have := map[string]bool{}
	for _, c := range src.Countries {
		have[c] = true
	}
	for _, z := range prices.Zones() {
		code, mapped := zoneCountryISO[z.Country]
		if !mapped {
			t.Errorf("zone country %q has no ISO mapping; add it to coverage", z.Country)
			continue
		}
		if !have[code] {
			t.Errorf("sourceful countries missing %s (%s) from the zone table", code, z.Country)
		}
	}
}
