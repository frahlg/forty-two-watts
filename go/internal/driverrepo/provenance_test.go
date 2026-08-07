package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// householdEnvelope signs the fixture's manifest with the household's own key
// and files it under FTW's key id, which is the closest a config can get to
// claiming FTW's signature. Every check the installer makes passes.
func householdEnvelope(t *testing.T, f *signedFixture) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ManifestEnvelope{
		SchemaVersion: components.ComponentManifestSchemaVersion,
		KeyID:         config.DefaultDriverRepositorySigningKeyID,
		Payload:       payload,
		Signature:     base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A signature is not the question. Whose signature is.
//
// A household can hold a signing key, publish a manifest with it and list that
// key as trusted, and every check the installer makes will pass — as it
// should, because the household is entitled to run its own drivers. What it
// does not get is FTW vouching for the names it chose, so the install record
// says so, and the fleet ping reads that record.
func TestAHouseholdsOwnKeyDoesNotRecordAsFTWSigned(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &signedFixture{private: private}
	// TLS, so the repository needs neither allow_unsigned nor allow_insecure.
	// The only thing separating it from FTW's own is whose key signed it.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(householdEnvelope(t, fixture))
		case "/demo.lua":
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			_, _ = w.Write(fixture.driver)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fixture.setVersion(server.URL, "1.0.0")

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager := New(&config.DeviceRepository{
		Enabled: true,
		Repositories: []config.DriverRepositorySource{{
			ID: "mine", Enabled: true, ManifestURL: server.URL + "/manifest.json",
			TrustedKeys: map[string]string{
				config.DefaultDriverRepositorySigningKeyID: base64.StdEncoding.EncodeToString(public),
			},
		}},
	}, dir, store)
	manager.client = server.Client()
	if err := manager.Refresh(context.Background(), "mine"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	installed, err := manager.Install(context.Background(), "mine", "demo", "1.0.0")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.FTWSigned {
		t.Error("a manifest signed with the household's own key recorded as FTW-signed")
	}

	// And the record is what survives, not the return value.
	active, err := store.ActiveDriverRepoInstall("drivers/demo.lua")
	if err != nil {
		t.Fatalf("read the install record back: %v", err)
	}
	if active.FTWSigned {
		t.Error("the stored install record claims FTW signed it")
	}
}

// The conditions, one at a time, on the function the installer records with.
// Each line here is a way a config could otherwise have talked its way into
// FTW's name.
func TestOnlyFTWsOwnKeyCountsAsFTWSigned(t *testing.T) {
	ftwKeys := func() map[string]string {
		return map[string]string{
			config.DefaultDriverRepositorySigningKeyID: config.DefaultDriverRepositoryPublicKey,
		}
	}
	somebodyElse := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

	withExtraKey := ftwKeys()
	withExtraKey["households-own"] = somebodyElse

	cases := []struct {
		name string
		repo config.DriverRepositorySource
		want bool
	}{
		{"FTW's key and nothing else", config.DriverRepositorySource{
			ID: config.DefaultDriverRepositoryID, TrustedKeys: ftwKeys()}, true},
		{"the pinned beta channel, even after a config claimed its id",
			officialBetaRepository([]config.DriverRepositorySource{
				{ID: config.DefaultDriverRepositoryBetaID}}), true},
		{"nothing signed at all", config.DriverRepositorySource{
			ID: "mine", AllowUnsigned: true, TrustedKeys: ftwKeys()}, false},
		{"the fetch rules dropped", config.DriverRepositorySource{
			ID: "mine", AllowInsecure: true, TrustedKeys: ftwKeys()}, false},
		{"FTW's key id over somebody else's key", config.DriverRepositorySource{
			ID: config.DefaultDriverRepositoryID, TrustedKeys: map[string]string{
				config.DefaultDriverRepositorySigningKeyID: somebodyElse}}, false},
		{"FTW's key listed beside one the household holds", config.DriverRepositorySource{
			ID: config.DefaultDriverRepositoryID, TrustedKeys: withExtraKey}, false},
		{"no trusted keys", config.DriverRepositorySource{ID: "mine"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ftwSigned(tc.repo); got != tc.want {
				t.Errorf("ftwSigned = %v, want %v", got, tc.want)
			}
		})
	}
}
