package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The dump exists to be handed to somebody else, so the archive has to
// open with no extra tooling. This asserts it is a real zip, not just a
// renamed one.
func TestSupportDumpIsAReadableZip(t *testing.T) {
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	ring := telemetry.NewLogRing()
	ring.Append(telemetry.LogEntry{Level: "WARN", Msg: "something to log"})
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: ring, Version: "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/support/dump", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/dump = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Errorf("Content-Disposition = %q, want a .zip filename", cd)
	}

	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("archive does not open as a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			t.Errorf("entry %s will not open: %v", f.Name, err)
			continue
		}
		rc.Close()
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"manifest.json", "drivers.json", "logs/global.log"} {
		if !strings.Contains(joined, want) {
			t.Errorf("archive is missing %s; has %v", want, names)
		}
	}
}
