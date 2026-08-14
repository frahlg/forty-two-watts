package main

import (
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/srcfl/ftw/go/internal/api"
)

const minAPITokenLength = 32

// apiMutationPolicy keeps the legacy local-LAN workflow open while requiring
// an explicit secret for protected requests addressed through public/FQDN
// hostnames. An invalid configured token fails closed for remote requests.
func apiMutationPolicy() api.MutationPolicy {
	token := strings.TrimSpace(os.Getenv("FTW_API_TOKEN"))
	if token != "" && len(token) < minAPITokenLength {
		slog.Error("FTW_API_TOKEN is too short; remote API mutations remain disabled",
			"minimum_characters", minAPITokenLength)
		token = ""
	}
	return api.MutationPolicy{
		RequireTokenForRemote: true,
		Token:                 token,
	}
}

// lanAuthLookups is filled after state.db opens. Until then both funcs
// report off, so the boot-phase listener and the setup wizard stay open.
type lanAuthLookups struct {
	mu      sync.RWMutex
	enabled func() bool
	verify  func(string) bool
}

func (l *lanAuthLookups) Enabled() bool {
	l.mu.RLock()
	fn := l.enabled
	l.mu.RUnlock()
	return fn != nil && fn()
}

func (l *lanAuthLookups) Verify(secret string) bool {
	l.mu.RLock()
	fn := l.verify
	l.mu.RUnlock()
	return fn != nil && fn(secret)
}

func (l *lanAuthLookups) Bind(enabled func() bool, verify func(string) bool) {
	l.mu.Lock()
	l.enabled = enabled
	l.verify = verify
	l.mu.Unlock()
}
