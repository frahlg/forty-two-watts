package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseNextLink(t *testing.T) {
	cases := []struct {
		name, header, base, want string
	}{
		{"empty", "", "https://ghcr.io", ""},
		{
			name:   "absolute next",
			header: `<https://ghcr.io/v2/foo/tags/list?n=200&last=v0.5.0>; rel="next"`,
			base:   "https://ghcr.io",
			want:   "https://ghcr.io/v2/foo/tags/list?n=200&last=v0.5.0",
		},
		{
			name:   "relative next is resolved against base",
			header: `</v2/foo/tags/list?n=200&last=v0.5.0>; rel="next"`,
			base:   "https://ghcr.io",
			want:   "https://ghcr.io/v2/foo/tags/list?n=200&last=v0.5.0",
		},
		{
			name:   "rel=prev is ignored",
			header: `</v2/foo/tags/list?n=200&first=v0.1.0>; rel="prev"`,
			base:   "https://ghcr.io",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNextLink(tc.header, tc.base); got != tc.want {
				t.Errorf("parseNextLink = %q, want %q", got, tc.want)
			}
		})
	}
}

// Empty tag list is the race window between GH publishing a release
// and the build workflow pushing the image — must report "not present"
// rather than erroring, so Check can defer the dispatch to the next
// probe instead of surfacing a noisy error to the operator.
func TestRegistryProbe_EmptyTagListIsNotAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"x"}`))
	})
	mux.HandleFunc("/v2/anything/tags/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"anything","tags":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rp := &registryProbe{
		httpClient: http.DefaultClient,
		base:       srv.URL,
		repo:       "anything",
		service:    "ghcr.io",
	}
	ok, err := rp.hasTag(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("empty tag list shouldn't error: %v", err)
	}
	if ok {
		t.Error("hasTag must report false when tag isn't in the list")
	}
}
