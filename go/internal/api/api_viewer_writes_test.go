package api

// Every write the passthrough exposes, attempted by a viewer.
//
// The other viewer test picks one route and proves the box did not move. This
// one is the sweep: it reads every route this box registers out of the source
// that registers them, asks each one through a real viewer session, and
// insists on a refusal. A write route added next year is in this test the day
// it is written, without anybody remembering to add it.
//
// It is deliberately not a list. A list of routes in a test is the same
// maintenance burden as a list of routes in the app, and it rots the same way.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// handleCall matches one line of routes(): the method and the path, exactly as
// the mux is given them.
var handleCall = regexp.MustCompile(`s\.handle\("\s*([A-Z]+)\s+(/api/[^"]*)"`)

type route struct{ method, path string }

// registeredRoutes reads the route table out of api.go.
//
// Out of the source rather than out of the mux, because net/http's ServeMux
// does not enumerate what it holds, and a hand-kept copy in this file would be
// the very list this test exists to avoid.
func registeredRoutes(t *testing.T) []route {
	t.Helper()
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("reading the route table: %v", err)
	}
	matches := handleCall.FindAllStringSubmatch(string(raw), -1)
	if len(matches) < 100 {
		t.Fatalf("found %d routes; the pattern above has stopped matching routes()", len(matches))
	}

	out := make([]route, 0, len(matches))
	for _, m := range matches {
		out = append(out, route{method: m[1], path: m[2]})
	}
	return out
}

// concrete fills in a pattern's wildcards, so the request reaches the same
// pattern the mux would pick for a real one.
func concrete(path string) string {
	for _, name := range []string{"{id}", "{name}", "{driver}", "{key}"} {
		path = strings.ReplaceAll(path, name, "x")
	}
	// Anything left is a wildcard this test has not seen. Filled with a
	// harmless segment rather than left as braces, which would not route.
	for strings.Contains(path, "{") {
		open := strings.Index(path, "{")
		close := strings.Index(path[open:], "}")
		if close < 0 {
			break
		}
		path = path[:open] + "x" + path[open+close+1:]
	}
	return path
}

// awaitID waits for the first frame carrying this request id, so one session
// can be swept across every route without the answers running together.
func awaitID(t *testing.T, frames *appFrames, id uint32) appproto.Envelope {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, env := range frames.snapshot() {
			if env.ID != nil && *env.ID == id {
				return env
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no answer to request %d", id)
	return appproto.Envelope{}
}

// A viewer is refused at every door that changes anything.
//
// The refusal may be any of five, and which one is not the point — what
// matters is that no handler ran. E_SCOPE_DENIED is the role gate, E_USE_CMD
// is a route that moves energy, E_WHOLE_DOCUMENT is a route that replaces a
// document, E_LOCAL_ONLY is a route the session does not carry, E_UNKNOWN_OP
// is a path it does not have at all.
func TestAViewerIsRefusedAtEveryWrite(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleViewer)
	srv := New(&Deps{Version: "test", WebDir: t.TempDir()})

	refusals := map[string]bool{
		appproto.ErrScopeDenied:   true,
		appproto.ErrUseCmd:        true,
		appproto.ErrWholeDocument: true,
		appproto.ErrLocalOnly:     true,
		appproto.ErrUnknownOp:     true,
	}

	var id uint32
	var swept int
	for _, r := range registeredRoutes(t) {
		path := concrete(r.path)

		// What the box itself says this route costs. Reads are a viewer's
		// right and are not what this test is about — and because the tier now
		// comes from the handler rather than from the verb, a GET that hands
		// out a credential is not a read and is swept here with the writes.
		probe := httptest.NewRequest(r.method, path, nil)
		if srv.Route(probe).Tier == apiauth.TierRead {
			continue
		}
		swept++

		id++
		rig.send(t, appproto.MsgAPIReq, id, appproto.APIReq{
			Method: r.method,
			Path:   path,
			// Step-up asserted, so a refusal cannot be the step-up gate
			// standing in for the role gate. The box cannot verify this and
			// never claims to; here it only removes one reason to refuse.
			StepUp: true,
		})

		env := awaitID(t, rig.frames, id)
		if env.T != appproto.MsgError {
			t.Fatalf("%s %s answered %s; a viewer reached a handler",
				r.method, path, env.T)
		}
		body := decode[appproto.ErrorBody](t, env)
		if !refusals[body.Code] {
			t.Fatalf("%s %s was refused with %q, which is not a refusal to write",
				r.method, path, body.Code)
		}

		// On a route that is only configuration, the refusal has to be the
		// role gate itself. Without this the sweep would still pass if every
		// route were refused for some other reason — a missing dependency on
		// this bare test box, say — and would prove nothing about roles.
		facts := srv.Route(probe)
		if facts.Tier == apiauth.TierConfigure && !facts.ReplacesAll && !facts.Static {
			if body.Code != appproto.ErrScopeDenied {
				t.Fatalf("%s %s is plain configuration but refused a viewer with %q, "+
					"not the role gate", r.method, path, body.Code)
			}
			if body.Args["needRole"] != apiauth.RoleOwner {
				t.Fatalf("%s %s refused without naming the role it needs: %v",
					r.method, path, body.Args)
			}
		}
	}

	if swept < 50 {
		t.Fatalf("only %d write routes were swept; the tier probe is letting writes through", swept)
	}

	// Not one handler ever answered. This is the assertion that survives
	// somebody deleting the role gate and leaving the error codes alone.
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("a viewer's write reached a handler")
	}
	if rig.saved.count() != 0 {
		t.Fatalf("a viewer wrote the configuration %d times", rig.saved.count())
	}
	if rig.ctrl.BatteryCoversEV {
		t.Fatal("a viewer changed a dispatch setting")
	}
}

// What an app session may do with the sharing routes lives in
// api_app_link_session_test.go, against the box's real enrolment.
//
// It used to be asserted here as a blanket 403 on all six of them, which was
// right about the danger — a phone on the far side of a relay must not mint an
// OWNER's code, or every claim about physical presence becomes false — and
// wrong about the remedy. Refusing the whole surface left the app's sharing
// screen showing three buttons that answered 403 to the household's own
// owner, and it hid the defect underneath: the app asked for a viewer in a
// query string the box never read, so opening the door alone would have minted
// owners. Both halves are tested there now, on what the guest actually became.

// A viewer may read. Refusing everything would pass the sweep above and be a
// different product: sharing exists so somebody can watch the house.
//
// The counterpart — an owner writing and the box changing — is
// TestAnOwnerConfiguresAndTheBoxChanges. It is one route on purpose: a sweep
// that ran every write handler as an owner would restart drivers, start a
// network scan and take an update channel with it.
func TestAViewerMayStillRead(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleViewer)

	// Several, because one read working could be one handler that happens to
	// need nothing. These are the readings a guest is shared the house for.
	for i, path := range []string{"/api/health", "/api/mode", "/api/modes", "/api/system/info"} {
		id := uint32(i + 1)
		rig.send(t, appproto.MsgAPIReq, id, appproto.APIReq{
			Method: appproto.APIGet, Path: path,
		})
		env := awaitID(t, rig.frames, id)
		if env.T != appproto.MsgAPIHead {
			t.Fatalf("a viewer's read of %s answered %s, want a head", path, env.T)
		}
		if head := decode[appproto.APIHeadMsg](t, env); head.Status != http.StatusOK {
			t.Fatalf("a viewer's read of %s answered %d, want 200", path, head.Status)
		}
	}
}
