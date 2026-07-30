package drivers

import (
	"context"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const arpTestDriver = `
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) end
`

// recordingARP stands in for arp.Lookup and remembers every host it was
// asked about, so a test can assert on what was NOT probed.
type recordingARP struct {
	mu    sync.Mutex
	hosts []string
	macs  map[string]string
}

func (r *recordingARP) lookup(host string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	mac, ok := r.macs[host]
	return mac, ok
}

func (r *recordingARP) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func addARPTestDriver(t *testing.T, arp *recordingARP, cfg config.Driver) *Registry {
	t.Helper()
	r := NewRegistry(telemetry.NewStore())
	r.ARPLookup = arp.lookup
	cfg.Lua = writeTestDriver(t, arpTestDriver)
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { r.remove(cfg.Name, true) })
	return r
}

// An HTTP driver sets no endpoint, so without an ARP-derived MAC a LAN
// device that has not yet reported a serial has no stable identity at all.
func TestAddResolvesMACForHTTPDriver(t *testing.T) {
	arp := &recordingARP{macs: map[string]string{"192.168.1.50": "aa:bb:cc:dd:ee:ff"}}
	r := addARPTestDriver(t, arp, config.Driver{
		Name:         "heatpump",
		Capabilities: config.Capabilities{HTTP: &config.HTTPCapability{}},
		Config:       map[string]any{"host": "192.168.1.50"},
	})

	env := r.Env("heatpump")
	if env == nil {
		t.Fatal("Env = nil after Add")
	}
	if _, _, mac, _ := env.FullIdentity(); mac != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("mac = %q, want aa:bb:cc:dd:ee:ff", mac)
	}
}

// The merged allowlist also names vendor cloud endpoints. Probing those
// would be both useless (never on our L2) and an unsolicited connection
// to a third party, so only the driver's own address may be asked about.
func TestHTTPDriverARPProbesOnlyItsOwnHost(t *testing.T) {
	arp := &recordingARP{macs: map[string]string{"192.168.1.50": "aa:bb:cc:dd:ee:ff"}}
	addARPTestDriver(t, arp, config.Driver{
		Name: "heatpump",
		Capabilities: config.Capabilities{HTTP: &config.HTTPCapability{
			AllowedHosts: []string{"api.vendor.example", "telemetry.vendor.example"},
		}},
		Config: map[string]any{"host": "192.168.1.50"},
	})

	asked := arp.asked()
	if len(asked) != 1 || asked[0] != "192.168.1.50" {
		t.Errorf("ARP probed %v, want exactly [192.168.1.50]", asked)
	}
}

func TestCloudOnlyHTTPDriverIsNotProbed(t *testing.T) {
	arp := &recordingARP{}
	addARPTestDriver(t, arp, config.Driver{
		Name: "easee",
		Capabilities: config.Capabilities{HTTP: &config.HTTPCapability{
			AllowedHosts: []string{"api.easee.cloud"},
		}},
	})

	if asked := arp.asked(); len(asked) != 0 {
		t.Errorf("ARP probed %v for a driver with no device address, want none", asked)
	}
}

func TestHTTPDriverSurvivesUnresolvableMAC(t *testing.T) {
	arp := &recordingARP{} // every lookup misses
	r := addARPTestDriver(t, arp, config.Driver{
		Name:         "heatpump",
		Capabilities: config.Capabilities{HTTP: &config.HTTPCapability{}},
		Config:       map[string]any{"host": "192.168.1.50"},
	})

	if _, _, mac, _ := r.Env("heatpump").FullIdentity(); mac != "" {
		t.Errorf("mac = %q, want empty when ARP misses", mac)
	}
}

func TestHTTPDeviceHost(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"nil config", nil, ""},
		{"empty config", map[string]any{}, ""},
		{"bare host", map[string]any{"host": "192.168.1.50"}, "192.168.1.50"},
		{"host with port", map[string]any{"host": "192.168.1.50:8080"}, "192.168.1.50"},
		{"host is trimmed and lowercased", map[string]any{"host": "  Inverter.Local  "}, "inverter.local"},
		{"blank host", map[string]any{"host": "   "}, ""},
		{"host wins over url", map[string]any{
			"host": "192.168.1.50", "url": "https://api.vendor.example",
		}, "192.168.1.50"},
		{"url fallback", map[string]any{"url": "http://192.168.1.60/api"}, "192.168.1.60"},
		{"url port is stripped", map[string]any{"url": "http://192.168.1.60:8080/api"}, "192.168.1.60"},
		{"url with credentials", map[string]any{"url": "http://user:pw@192.168.1.60/api"}, "192.168.1.60"},
		{"bracketed ipv6 url is unwrapped", map[string]any{"url": "http://[fd00::5]:8080/"}, "fd00::5"},
		{"url without host", map[string]any{"url": "/local/path"}, ""},
		{"non-string host is ignored", map[string]any{"host": 42}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpDeviceHost(tc.cfg); got != tc.want {
				t.Errorf("httpDeviceHost(%v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}
