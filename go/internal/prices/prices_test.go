package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// ---- Applier ----

func TestApplierComputesConsumerPrice(t *testing.T) {
	// 1.50 SEK/kWh spot, 30 öre/kWh grid tariff, 25% VAT
	// spot in öre = 150, total = (150 + 30) * 1.25 = 225
	a := Applier{GridTariffOreKwh: 30, VATPercent: 25}
	spot, total := a.Apply(1.5)
	if spot != 150 { t.Errorf("spot: %f, want 150", spot) }
	if total != 225 { t.Errorf("total: %f, want 225", total) }
}

func TestApplierZeroGridTariff(t *testing.T) {
	a := Applier{GridTariffOreKwh: 0, VATPercent: 25}
	_, total := a.Apply(2.0)
	// 200 * 1.25 = 250
	if total != 250 { t.Errorf("%f", total) }
}

func TestApplierZeroVAT(t *testing.T) {
	a := Applier{GridTariffOreKwh: 10, VATPercent: 0}
	_, total := a.Apply(1.0)
	if total != 110 { t.Errorf("%f", total) }
}

// ---- Elpriser parse ----

func TestElpriserDetects15MinFromTimeEnd(t *testing.T) {
	// Real elprisetjustnu response shape: time_end + time_start explicitly give the slot
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{
			{"SEK_per_kWh": 1.25, "time_start": "2026-04-14T00:00:00+02:00", "time_end": "2026-04-14T00:15:00+02:00"},
			{"SEK_per_kWh": 1.20, "time_start": "2026-04-14T00:15:00+02:00", "time_end": "2026-04-14T00:30:00+02:00"},
			{"SEK_per_kWh": 1.10, "time_start": "2026-04-14T00:30:00+02:00", "time_end": "2026-04-14T00:45:00+02:00"},
			{"SEK_per_kWh": 1.05, "time_start": "2026-04-14T00:45:00+02:00", "time_end": "2026-04-14T01:00:00+02:00"},
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil { t.Fatal(err) }
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (15-min slots)", len(rows))
	}
	for i, r := range rows {
		if r.SlotLenMin != 15 {
			t.Errorf("row %d: slot_len_min=%d, want 15", i, r.SlotLenMin)
		}
	}
}

func TestElpriserDetects60MinFromSpacing(t *testing.T) {
	// Legacy hourly response (no time_end) — spacing-based detection kicks in
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{
			{"SEK_per_kWh": 1.25, "time_start": "2026-04-14T00:00:00+02:00"},
			{"SEK_per_kWh": 1.10, "time_start": "2026-04-14T01:00:00+02:00"},
			{"SEK_per_kWh": 0.90, "time_start": "2026-04-14T02:00:00+02:00"},
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil { t.Fatal(err) }
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].SlotLenMin != 60 {
		t.Errorf("hourly data should be tagged as 60-min, got %d", rows[0].SlotLenMin)
	}
}

func TestElpriserHandles404(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	rows, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err != nil { t.Fatalf("404 should not error: %v", err) }
	if len(rows) != 0 { t.Errorf("404 should return empty, got %d", len(rows)) }
}

func TestElpriserURL(t *testing.T) {
	captured := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { case captured <- r.URL.Path: default: }
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	_, _ = p.Fetch(context.Background(), "SE3", day)
	got := <-captured
	want := "/2026/04-14_SE3.json"
	if got != want {
		t.Errorf("URL path: got %q, want %q", got, want)
	}
}

// ---- Service integration: fetch → save → load ----

func TestServiceFetchesAndStores(t *testing.T) {
	// Always use today's local date so the test doesn't break on a
	// calendar boundary. fetchAndStore queries elprisetjustnu's URL
	// shape for the current date — match it dynamically.
	today := time.Now().In(time.Local)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	todayPath := today.Format("01-02_") // MM-DD_ — same fragment fetchAndStore uses
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, todayPath) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"SEK_per_kWh": 1.0, "time_start": today.Format(time.RFC3339), "time_end": today.Add(15 * time.Minute).Format(time.RFC3339)},
				{"SEK_per_kWh": 2.0, "time_start": today.Add(15 * time.Minute).Format(time.RFC3339), "time_end": today.Add(30 * time.Minute).Format(time.RFC3339)},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil { t.Fatal(err) }
	defer st.Close()

	s := &Service{
		Provider: &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL},
		Store:    st,
		Zone:     "SE3",
		Applier:  Applier{GridTariffOreKwh: 30, VATPercent: 25},
	}
	s.fetchAndStore(context.Background())

	pts, err := st.LoadPrices("SE3", today.UnixMilli(), today.Add(time.Hour).UnixMilli())
	if err != nil { t.Fatal(err) }
	if len(pts) != 2 {
		t.Fatalf("got %d prices, want 2", len(pts))
	}
	if pts[0].SpotOreKwh != 100 {
		t.Errorf("spot: %f", pts[0].SpotOreKwh)
	}
	if pts[0].TotalOreKwh != 162.5 {
		t.Errorf("total: %f", pts[0].TotalOreKwh)
	}
	if pts[0].SlotLenMin != 15 {
		t.Errorf("slot: %d", pts[0].SlotLenMin)
	}
}

// ---- FromConfig ----

func TestFromConfigNilWhenDisabled(t *testing.T) {
	if FromConfig(nil, nil, nil) != nil { t.Error("nil cfg → nil service") }
	if FromConfig(&config.Price{Provider: "none"}, nil, nil) != nil { t.Error("none → nil service") }
	if FromConfig(&config.Price{Provider: ""}, nil, nil) != nil { t.Error("empty → nil service") }
}

func TestFromConfigDefaultsZoneAndVAT(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := FromConfig(&config.Price{Provider: "elprisetjustnu"}, st, nil)
	if s == nil { t.Fatal("expected service") }
	if s.Zone != "SE3" { t.Errorf("default zone: %s", s.Zone) }
	if s.Applier.VATPercent != 25 { t.Errorf("default VAT: %f", s.Applier.VATPercent) }
}

// ---- ENTSOE minimal checks ----

func TestENTSOERequiresAPIKey(t *testing.T) {
	p := NewENTSOE("")
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestENTSOERejectsUnknownZone(t *testing.T) {
	p := NewENTSOE("any-key")
	_, err := p.Fetch(context.Background(), "ZZZ", time.Now())
	if err == nil {
		t.Error("expected error for unknown zone")
	}
}

// ---- Error paths ----

func TestElpriserHandles500(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "oops")
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil {
		t.Error("expected error for 5xx")
	}
}
