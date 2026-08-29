package ocpp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// requestFrom builds the request the library hands checkClient: basic auth in
// the header, and the local address net/http puts on every connection context.
func requestFrom(t *testing.T, user, pass, arrivedOn string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/garage", nil)
	if user != "" || pass != "" {
		r.SetBasicAuth(user, pass)
	}
	if arrivedOn != "" {
		addr, err := net.ResolveTCPAddr("tcp", arrivedOn)
		if err != nil {
			t.Fatalf("resolve %q: %v", arrivedOn, err)
		}
		ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, addr)
		r = r.WithContext(ctx)
	}
	return r
}

// A charger with a credential of its own cannot be impersonated by something
// holding only the shared password. That is the whole point of the feature:
// identity is client-chosen, so authenticating has never proved which device
// is on the other end.
func TestPerChargerCredentialBlocksImpersonation(t *testing.T) {
	a := newAuthorizer(&Config{
		Username:       "ftw",
		Password:       "shared-secret",
		ChargerSecrets: map[string]string{"garage": "garage-only-secret"},
	})

	t.Run("its own credential is accepted", func(t *testing.T) {
		if !a.basicAuth("garage", "garage-only-secret") {
			t.Error("basic auth rejected the charger's own credential")
		}
		if !a.checkClient("garage", requestFrom(t, "garage", "garage-only-secret", "")) {
			t.Error("checkClient rejected the charger's own credential")
		}
	})

	t.Run("the shared credential no longer buys its name", func(t *testing.T) {
		// Basic auth passes — the shared secret is real — and the identity
		// binding is what refuses.
		if !a.basicAuth("ftw", "shared-secret") {
			t.Fatal("the shared credential should still authenticate")
		}
		if a.checkClient("garage", requestFrom(t, "ftw", "shared-secret", "")) {
			t.Error("the shared password claimed a charger that has its own credential")
		}
	})

	t.Run("its own password under another username is refused", func(t *testing.T) {
		if a.checkClient("garage", requestFrom(t, "ftw", "garage-only-secret", "")) {
			t.Error("accepted the charger's password presented under another identity")
		}
	})

	t.Run("a wrong password for its own name is refused", func(t *testing.T) {
		if a.basicAuth("garage", "shared-secret") {
			t.Error("a charger with its own credential fell back to the shared one")
		}
		if a.checkClient("garage", requestFrom(t, "garage", "nope", "")) {
			t.Error("accepted a wrong password")
		}
	})

	t.Run("chargers without one keep using the shared credential", func(t *testing.T) {
		if !a.basicAuth("ftw", "shared-secret") {
			t.Error("shared credential rejected")
		}
		if !a.checkClient("carport", requestFrom(t, "ftw", "shared-secret", "")) {
			t.Error("a charger with no credential of its own should use the shared one")
		}
	})
}

// Bind is enforced at the handshake because the socket cannot be pinned: the
// library builds its listen address from the port alone.
func TestBindAddressIsEnforcedAtTheHandshake(t *testing.T) {
	tests := []struct {
		name      string
		bind      string
		arrivedOn string
		want      bool
	}{
		{name: "same address", bind: "192.168.1.10", arrivedOn: "192.168.1.10:8887", want: true},
		{name: "another interface", bind: "192.168.1.10", arrivedOn: "10.8.0.1:8887", want: false},
		{name: "loopback when bound to the LAN", bind: "192.168.1.10", arrivedOn: "127.0.0.1:8887", want: false},
		{name: "unspecified accepts anything", bind: "0.0.0.0", arrivedOn: "10.8.0.1:8887", want: true},
		{name: "empty accepts anything", bind: "", arrivedOn: "10.8.0.1:8887", want: true},
		// A dual-stack listener reports an IPv4 connection as v4-mapped v6.
		{name: "v4-mapped v6 matches its v4", bind: "192.168.1.10", arrivedOn: "[::ffff:192.168.1.10]:8887", want: true},
		// Not knowing where it landed must not lock every charger out.
		{name: "no local address is allowed", bind: "192.168.1.10", arrivedOn: "", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthorizer(&Config{Bind: tc.bind})
			got := a.checkClient("garage", requestFrom(t, "", "", tc.arrivedOn))
			if got != tc.want {
				t.Errorf("checkClient on %s with bind %q: got %v, want %v",
					tc.arrivedOn, tc.bind, got, tc.want)
			}
		})
	}
}

// With no credentials configured the basic-auth handler must stay unregistered
// — the library answers 401 to a charger that sends none whenever a handler
// exists, so registering it would lock out every charger instead of admitting
// them all.
func TestNoCredentialsMeansNoBasicAuthHandler(t *testing.T) {
	if newAuthorizer(&Config{}).requiresCredential() {
		t.Error("an OCPP section with no credentials should not demand one")
	}
	if !newAuthorizer(&Config{Username: "ftw", Password: "x"}).requiresCredential() {
		t.Error("a shared credential should be demanded")
	}
	if !newAuthorizer(&Config{ChargerSecrets: map[string]string{"garage": "x"}}).requiresCredential() {
		t.Error("a per-charger credential should be demanded")
	}
}

// TLS has to fail loudly. An operator who asked for wss:// and silently got
// ws:// would have no way to tell the link was never encrypted.
func TestTLSMisconfigurationRefusesToStart(t *testing.T) {
	tests := []struct {
		name string
		tls  *TLSConfig
	}{
		{name: "cert without key", tls: &TLSConfig{CertFile: "cert.pem"}},
		{name: "key without cert", tls: &TLSConfig{KeyFile: "key.pem"}},
		{name: "cert file missing", tls: &TLSConfig{CertFile: "no-such-cert.pem", KeyFile: "no-such-key.pem"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Enabled: true, Bind: "127.0.0.1", Port: freePort(t), TLS: tc.tls}
			if _, err := Start(context.Background(), cfg, telemetry.NewStore()); err == nil {
				t.Fatal("a broken TLS config started anyway, serving plaintext")
			}
		})
	}
}

func TestSchemeFollowsTLS(t *testing.T) {
	if got := (&Config{}).Scheme(); got != "ws" {
		t.Errorf("plaintext scheme: got %q, want ws", got)
	}
	cfg := &Config{TLS: &TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}}
	if got := cfg.Scheme(); got != "wss" {
		t.Errorf("TLS scheme: got %q, want wss", got)
	}
}

// End to end over a real connection: the library must actually consult both
// gates, in the order that makes the identity binding effective.
func TestPerChargerCredentialOverTheWire(t *testing.T) {
	port := freePort(t)
	cfg := &Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               port,
		HeartbeatIntervalS: 60,
		Username:           "ftw",
		Password:           "shared-secret",
		ChargerSecrets:     map[string]string{"garage": "garage-only-secret"},
		ApprovedIDs:        []string{"garage"},
	}
	srv, err := Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	waitForListener(t, port)

	connect := func(t *testing.T, id, user, pass string) error {
		t.Helper()
		client := ws.NewClient()
		client.SetBasicAuth(user, pass)
		cp := ocpp16.NewChargePoint(id, nil, client)
		err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port))
		if err == nil {
			t.Cleanup(cp.Stop)
		}
		return err
	}

	if err := connect(t, "garage", "garage", "garage-only-secret"); err != nil {
		t.Fatalf("the charger's own credential was refused: %v", err)
	}
	if err := connect(t, "garage-impostor", "ftw", "shared-secret"); err != nil {
		t.Fatalf("a charger without its own credential should still connect: %v", err)
	}
	// The gate this test exists for. It also catches the library detail that
	// makes it fragile: ocppj.Server.Start replaces the connection check the
	// ws.Server was given, so a gate registered on the raw server is silently
	// discarded and every impersonation attempt succeeds.
	if err := connect(t, "garage", "ftw", "shared-secret"); err == nil {
		t.Fatal("the shared password connected as a charger that has its own credential")
	}
}

// waitForListener blocks until the port accepts, so a client never races the
// listener goroutine.
func waitForListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not bind on port %d within deadline", port)
}
