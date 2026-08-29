package ocpp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// Config controls the OCPP Central System.
//
// Charge points connect to ws://<bind>:<port>/<chargerId>, or wss:// when TLS
// is configured. When a loadpoint names that chargerId (see ApprovedIDs) it
// becomes the driver name in telemetry.Store and shows up in /api/devices and
// /api/status.drivers; otherwise the charger stays pending and appears only in
// /api/ocpp/chargers.
type Config struct {
	Enabled bool `yaml:"enabled"`

	// Bind is the address the listener is offered on. The OCPP library
	// builds its own listen address from the port alone, so the socket is
	// unavoidably open on every interface; what this does is refuse the
	// WebSocket handshake for any connection that arrived somewhere else
	// (see authorizer.checkClient). That is an access control, not a
	// smaller attack surface — a port scan still finds the port. Empty or
	// unspecified (0.0.0.0 / ::) accepts every interface.
	Bind string `yaml:"bind"`

	Port               int    `yaml:"port"`
	PortV201           int    `yaml:"port_v201"`
	Path               string `yaml:"path"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s"`

	// TLS, when set, serves wss:// instead of ws://. Optional client
	// certificate verification implements OCPP 2.0.1 security profile 3,
	// where the certificate — not a shared password — is what identifies a
	// charge point.
	TLS *TLSConfig `yaml:"tls"`

	// ChargerSecrets maps a charge point identity to a password of its own.
	// A charger listed here must present that password under that identity,
	// so the shared password no longer buys an attacker its name. Derived by
	// the caller from the config's per-charger entries, never read from YAML
	// here.
	ChargerSecrets map[string]string `yaml:"-"`

	// ApprovedIDs is the set of charger identities that are part of the site
	// — the ids loadpoints name in config. Derived by the caller, never read
	// from YAML. A charger connecting under any other identity is accepted at
	// the protocol level but quarantined as "pending": visible in the API and
	// UI so an operator can adopt it, withheld from telemetry so it cannot
	// influence dispatch. Empty means every charger is pending.
	ApprovedIDs []string `yaml:"-"`
}

// TLSConfig points at the certificate this server presents, and optionally at
// the CA that signs the charge points allowed to connect.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// ClientCAFile, when set, requires every charge point to present a
	// certificate signed by this CA and rejects the handshake otherwise.
	// That is a far stronger identity than a shared password: it cannot be
	// copied out of one charger's config and replayed by another device
	// unless its private key was copied too.
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether TLS is fully configured. Both files are required —
// a certificate without its key cannot serve.
func (t *TLSConfig) Enabled() bool {
	return t != nil && t.CertFile != "" && t.KeyFile != ""
}

// configured reports whether the operator asked for TLS at all, however
// incompletely. Enabled answers "can we serve it"; this answers "were we meant
// to", and the gap between the two is a misconfiguration that must fail loudly
// rather than quietly fall back to ws://.
func (t *TLSConfig) configured() bool {
	return t != nil && (t.CertFile != "" || t.KeyFile != "" || t.ClientCAFile != "")
}

// serverTLS builds the tls.Config for the listener, reading the client CA from
// disk when one is configured.
//
// Returns an error rather than falling back to plaintext: an operator who
// asked for TLS and got ws:// because a path was wrong would have no way to
// tell, and would believe the link was encrypted.
func (t *TLSConfig) serverTLS() (*tls.Config, error) {
	if !t.Enabled() {
		return nil, errors.New("ocpp: tls needs both cert_file and key_file")
	}
	if _, err := os.Stat(t.CertFile); err != nil {
		return nil, fmt.Errorf("ocpp: tls cert_file: %w", err)
	}
	if _, err := os.Stat(t.KeyFile); err != nil {
		return nil, fmt.Errorf("ocpp: tls key_file: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.ClientCAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(t.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("ocpp: tls client_ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ocpp: tls client_ca_file %s contains no certificate", t.ClientCAFile)
	}
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	cfg.ClientCAs = pool
	return cfg, nil
}

// Defaults fills in any unset fields with safe values.
func (c *Config) Defaults() {
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 8887
	}
	if c.Path == "" {
		c.Path = "/"
	}
	if c.HeartbeatIntervalS == 0 {
		c.HeartbeatIntervalS = 60
	}
}

// Scheme is the URL scheme charge points must dial, for logs and the UI.
func (c *Config) Scheme() string {
	if c != nil && c.TLS.Enabled() {
		return "wss"
	}
	return "ws"
}
