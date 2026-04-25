package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// GET /api/system/info must return 200 + a decodable sysInfoResponse.
// Host probes (cpu/mem/disk/host.Info) may return zero values in
// sandboxed CI — the test only asserts fields that don't depend on
// the host probes succeeding (Cores from runtime, Disk.Path from
// diskRoot()), so it won't flake on a locked-down runner.
func TestHandleSysInfoSmoke(t *testing.T) {
	srv := New(&Deps{})

	req := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	var resp sysInfoResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, rr.Body.String())
	}

	if resp.CPU.Cores != runtime.NumCPU() {
		t.Errorf("CPU.Cores = %d, want %d", resp.CPU.Cores, runtime.NumCPU())
	}
	if resp.Disk.Path != diskRoot() {
		t.Errorf("Disk.Path = %q, want %q", resp.Disk.Path, diskRoot())
	}
	if resp.Network == nil {
		t.Errorf("Network is nil — expected a slice (possibly empty) so JSON shape stays stable")
	}
}

func TestDiskRootByGOOS(t *testing.T) {
	got := diskRoot()
	switch runtime.GOOS {
	case "windows":
		if got == "/" {
			t.Errorf("diskRoot() on windows = %q, want a drive root like C:\\", got)
		}
	default:
		if got != "/" {
			t.Errorf("diskRoot() on %s = %q, want %q", runtime.GOOS, got, "/")
		}
	}
}
