package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

type dataSource struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Label       string   `json:"label"`
	Area        string   `json:"area"`
	Countries   []string `json:"countries"`
	Worldwide   bool     `json:"worldwide"`
	RequiresKey bool     `json:"requires_key"`
	Note        string   `json:"note"`
	Covers      *bool    `json:"covers"`
}

type dataSourcesResp struct {
	Latitude  *float64     `json:"latitude"`
	Longitude *float64     `json:"longitude"`
	Sources   []dataSource `json:"sources"`
}

func getDataSources(t *testing.T, deps *Deps, query string) dataSourcesResp {
	t.Helper()
	srv := New(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/data-sources"+query, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp dataSourcesResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func depsAt(lat, lon float64) *Deps {
	cfg := &config.Config{Weather: &config.Weather{Latitude: lat, Longitude: lon}}
	return &Deps{Cfg: cfg, CfgMu: &sync.RWMutex{}}
}

func findSource(t *testing.T, resp dataSourcesResp, id string) dataSource {
	t.Helper()
	for _, s := range resp.Sources {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("source %q missing from response", id)
	return dataSource{}
}

func TestDataSourcesListsEveryShippedSource(t *testing.T) {
	resp := getDataSources(t, depsAt(59.33, 18.07), "")
	for _, id := range []string{
		"met_no", "openweather", "open_meteo", "forecast_solar",
		"sourceful", "elprisetjustnu", "entsoe",
	} {
		findSource(t, resp, id)
	}
}

func TestDataSourcesCoversNordicSite(t *testing.T) {
	resp := getDataSources(t, depsAt(59.33, 18.07), "")
	for _, id := range []string{"elprisetjustnu", "sourceful", "open_meteo"} {
		s := findSource(t, resp, id)
		if s.Covers == nil || !*s.Covers {
			t.Errorf("%s: want covers=true for Stockholm", id)
		}
	}
}

// The case that motivated this endpoint: outside Europe the forecast still
// works, but every price provider does not.
func TestDataSourcesExplainsWhySydneyIsLimited(t *testing.T) {
	resp := getDataSources(t, depsAt(-33.87, 151.21), "")

	for _, id := range []string{"met_no", "openweather", "open_meteo", "forecast_solar"} {
		s := findSource(t, resp, id)
		if s.Covers == nil || !*s.Covers {
			t.Errorf("%s: forecast providers are worldwide, want covers=true", id)
		}
	}
	for _, id := range []string{"sourceful", "elprisetjustnu", "entsoe"} {
		s := findSource(t, resp, id)
		if s.Covers == nil || *s.Covers {
			t.Errorf("%s: want covers=false in Sydney", id)
		}
		if s.Note == "" && s.Area == "" {
			t.Errorf("%s: an uncovered source must still explain its area", id)
		}
	}
}

func TestDataSourcesQueryOverridesConfiguredSite(t *testing.T) {
	deps := depsAt(59.33, 18.07) // configured: Stockholm
	resp := getDataSources(t, deps, "?lat=-33.87&lon=151.21")
	if s := findSource(t, resp, "sourceful"); s.Covers == nil || *s.Covers {
		t.Error("query lat/lon should override config and report not covered")
	}
	if resp.Latitude == nil || *resp.Latitude != -33.87 {
		t.Errorf("latitude = %v, want the overridden -33.87", resp.Latitude)
	}
}

func TestDataSourcesOmitsCoversWithoutASite(t *testing.T) {
	resp := getDataSources(t, &Deps{}, "")
	if len(resp.Sources) == 0 {
		t.Fatal("sources should still be listed without a site")
	}
	for _, s := range resp.Sources {
		if s.Covers != nil {
			t.Errorf("%s: covers should be omitted when no site is known", s.ID)
		}
	}
	if resp.Latitude != nil || resp.Longitude != nil {
		t.Error("latitude/longitude should be omitted when no site is known")
	}
}

func TestDataSourcesCarriesRegionMetadata(t *testing.T) {
	resp := getDataSources(t, depsAt(59.33, 18.07), "")

	sf := findSource(t, resp, "sourceful")
	if sf.Worldwide {
		t.Error("sourceful must not be reported worldwide")
	}
	if sf.Area == "" || len(sf.Countries) == 0 {
		t.Error("sourceful should carry an area and country list")
	}
	if sf.RequiresKey {
		t.Error("sourceful needs no API key")
	}

	if ow := findSource(t, resp, "openweather"); !ow.RequiresKey {
		t.Error("openweather requires an API key")
	}
	if mn := findSource(t, resp, "met_no"); !mn.Worldwide {
		t.Error("met_no is worldwide")
	}
	if ep := findSource(t, resp, "entsoe"); !ep.RequiresKey {
		t.Error("entsoe requires an API key")
	}
}
