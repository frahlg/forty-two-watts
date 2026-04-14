// Package libcheck is a one-off smoke test proving each dep works
// as expected. Deleted once real modules are in place. Run with:
//
//	go test ./internal/libcheck -v
package libcheck

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/fsnotify/fsnotify"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/simonvetter/modbus"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	"database/sql"
)

// ---- SQLite (pure Go, modernc) ----

func TestSQLite(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE history_hot (ts_ms INTEGER PRIMARY KEY, grid_w REAL, pv_w REAL);`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		_, _ = db.Exec(`INSERT INTO history_hot VALUES (?, ?, ?)`, i*5000, float64(i)*10, float64(i)*-5)
	}
	// SQL range query + bucketed aggregation — the thing our custom tiered code does
	row := db.QueryRow(`
		SELECT ((ts_ms/15000)*15000)+7500 AS bucket_mid, AVG(grid_w), COUNT(*)
		FROM history_hot
		WHERE ts_ms BETWEEN 0 AND 50000
		GROUP BY ts_ms/15000
		ORDER BY bucket_mid DESC LIMIT 1;
	`)
	var bucket int64
	var avg float64
	var n int
	if err := row.Scan(&bucket, &avg, &n); err != nil {
		t.Fatal(err)
	}
	t.Logf("sqlite bucketed: bucket=%d avg=%.1f n=%d — aggregates cleanly, schema <10 lines", bucket, avg, n)
}

// ---- YAML (gopkg.in/yaml.v3) ----

type site struct {
	Name          string  `yaml:"name"`
	GridTarget    float64 `yaml:"grid_target_w"`
	SmoothingAlpha float64 `yaml:"smoothing_alpha"`
}
type cfg struct {
	Site site `yaml:"site"`
}

func TestYAML(t *testing.T) {
	src := []byte(`
site:
  name: Test
  grid_target_w: -100
  smoothing_alpha: 0.3
`)
	var c cfg
	if err := yaml.Unmarshal(src, &c); err != nil {
		t.Fatal(err)
	}
	if c.Site.Name != "Test" || c.Site.GridTarget != -100 {
		t.Fatalf("parse mismatch: %+v", c)
	}
	out, _ := yaml.Marshal(&c)
	t.Logf("yaml roundtrip: %s", string(out))
}

// ---- fsnotify (file watcher) ----

func TestFsnotify(t *testing.T) {
	tmp := t.TempDir()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Add(tmp); err != nil {
		t.Fatal(err)
	}
	// Touch a file, expect an event within 1s
	path := filepath.Join(tmp, "config.yaml")
	done := make(chan fsnotify.Event, 1)
	go func() {
		select {
		case ev := <-w.Events:
			done <- ev
		case <-time.After(1500 * time.Millisecond):
		}
	}()
	_ = os.WriteFile(path, []byte("x"), 0644)
	select {
	case ev := <-done:
		t.Logf("fsnotify event: %s %s", ev.Op, ev.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// ---- Mochi MQTT broker (embedded for tests) + paho client ----

func TestMochiBrokerAndPaho(t *testing.T) {
	// Start embedded broker on a random port
	s := mqttserver.New(&mqttserver.Options{InlineClient: true})
	_ = s.AddHook(new(auth.AllowHook), nil) // no auth for test
	tcp := listeners.NewTCP(listeners.Config{ID: "t1", Address: "127.0.0.1:21883"})
	if err := s.AddListener(tcp); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	defer s.Close()
	time.Sleep(100 * time.Millisecond)

	// Paho client connect
	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:21883").SetClientID("smoke")
	c := mqtt.NewClient(opts)
	if tok := c.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer c.Disconnect(100)

	got := make(chan string, 1)
	c.Subscribe("test/#", 0, func(_ mqtt.Client, m mqtt.Message) { got <- string(m.Payload()) })
	time.Sleep(100 * time.Millisecond)
	c.Publish("test/smoke", 0, false, "hello")
	select {
	case p := <-got:
		t.Logf("mqtt roundtrip via embedded broker: %q", p)
	case <-time.After(2 * time.Second):
		t.Fatal("no message received")
	}
}

// ---- Modbus server + client ----

func TestModbusLoopback(t *testing.T) {
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:        "tcp://127.0.0.1:15502",
		Timeout:    5 * time.Second,
		MaxClients: 4,
	}, &stubHandler{vals: map[uint16]uint16{100: 1234, 101: 5678}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	cli, err := modbus.NewClient(&modbus.ClientConfiguration{URL: "tcp://127.0.0.1:15502", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Open(); err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	regs, err := cli.ReadRegisters(100, 2, modbus.HOLDING_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("modbus TCP roundtrip (own-process server+client): %v", regs)
	if regs[0] != 1234 || regs[1] != 5678 {
		t.Fatalf("values mismatch: %v", regs)
	}
}

type stubHandler struct{ vals map[uint16]uint16 }

func (s *stubHandler) HandleCoils(_ *modbus.CoilsRequest) ([]bool, error) { return nil, modbus.ErrIllegalFunction }
func (s *stubHandler) HandleDiscreteInputs(_ *modbus.DiscreteInputsRequest) ([]bool, error) { return nil, modbus.ErrIllegalFunction }
func (s *stubHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		for i, v := range req.Args {
			s.vals[req.Addr+uint16(i)] = v
		}
		return nil, nil
	}
	out := make([]uint16, req.Quantity)
	for i := range out {
		out[i] = s.vals[req.Addr+uint16(i)]
	}
	return out, nil
}
func (s *stubHandler) HandleInputRegisters(_ *modbus.InputRegistersRequest) ([]uint16, error) {
	return make([]uint16, 1), nil
}

// ---- wazero (WASM runtime) — tiny WAT module that doubles its input ----

//go:embed testdata/double.wasm
var doubleWasm []byte

func TestWazero(t *testing.T) {
	if len(doubleWasm) == 0 {
		t.Skip("doubleWasm not embedded; will generate in phase 4")
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	mod, err := r.Instantiate(ctx, doubleWasm)
	if err != nil {
		t.Fatal(err)
	}
	fn := mod.ExportedFunction("double")
	out, err := fn.Call(ctx, 21)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != 42 {
		t.Fatalf("expected 42, got %d", out[0])
	}
	t.Log("wazero: double(21) → 42 ✓")
}
