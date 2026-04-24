package api

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"
)

// YAML-document complexity caps. Guards against alias-bomb / billion-laughs
// style inputs that stay under the 64 KB byte cap but expand catastrophically
// during Unmarshal. Numbers chosen well above any legit operator config
// (hand-reviewed real configs top out at ~8 anchors and depth ~6).
const (
	rawConfigMaxAnchors = 64
	rawConfigMaxDepth   = 64
)

// scanYAMLComplexity rejects documents that exceed the anchor or depth
// caps. Returns nil on acceptable input. Runs pre-Unmarshal so the parser
// never sees a pathological document.
func scanYAMLComplexity(body []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	for {
		var root yaml.Node
		if err := dec.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			// Malformed YAML — let the real Unmarshal produce the
			// operator-facing line number. Scan phase just bails.
			return nil
		}
		var walk func(n *yaml.Node, depth int, anchors *int) error
		walk = func(n *yaml.Node, depth int, anchors *int) error {
			if n == nil {
				return nil
			}
			if depth > rawConfigMaxDepth {
				return fmt.Errorf("yaml nesting exceeds %d levels", rawConfigMaxDepth)
			}
			if n.Anchor != "" {
				*anchors++
				if *anchors > rawConfigMaxAnchors {
					return fmt.Errorf("yaml defines > %d anchors", rawConfigMaxAnchors)
				}
			}
			for _, c := range n.Content {
				if err := walk(c, depth+1, anchors); err != nil {
					return err
				}
			}
			return nil
		}
		anchors := 0
		if err := walk(&root, 0, &anchors); err != nil {
			return err
		}
	}
}

// strictYAMLUnmarshal parses into a typed struct with KnownFields(true) so
// typo'd keys (e.g. `max_anps`) are rejected instead of silently dropped.
// Shared by POST and Validate so both gates stop the same classes of bad
// input.
func strictYAMLUnmarshal(body []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if err == io.EOF {
			return fmt.Errorf("empty document")
		}
		return err
	}
	return nil
}

// Raw-YAML endpoints backing the Settings → Advanced tab. Pipes the
// on-disk config.yaml (with secrets resolved from state.db) through
// CodeMirror on the client, accepts edits back as text/yaml, and
// runs the same Validate + atomic-save path as POST /api/config.
//
// Why a separate file: CLAUDE.md for this package says new features
// land as api_<feature>.go rather than accreting in api.go. Routes
// are still registered in api.go's routes() table.

const (
	// contentTypeYAML is the media type we accept + emit for the raw
	// endpoint. Matches the IETF draft for YAML media types; existing
	// clients (curl, the web UI) are indifferent to the exact string
	// but we set it consistently so the browser's dev tools show
	// something sensible.
	contentTypeYAML = "application/yaml"

	// rawConfigMaxBytes caps the request body. A typical config is
	// 3-8 KB; 64 KB is ~8x headroom for power users with many drivers.
	rawConfigMaxBytes = 64 * 1024
)

// handleGetConfigRaw returns the current config.yaml with secrets
// resolved from state.db. We re-marshal from the in-memory struct
// rather than returning the file bytes verbatim so the output is
// always the canonical representation (same shape SaveAtomic would
// write). That trades user-authored YAML comments for predictable
// round-trips — the existing form-based POST has the same loss, so
// this is consistent with what operators already see.
func (s *Server) handleGetConfigRaw(w http.ResponseWriter, r *http.Request) {
	s.deps.CfgMu.RLock()
	cfgCopy := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()

	// Marshal the struct first. EVCharger.Password has `yaml:"-"` so
	// it's omitted here — we re-inject the plaintext under ev_charger
	// via a second generic-map pass below. The user asked for "allow
	// editing secrets", so the editor sees the real password.
	data, err := marshalCanonicalYAML(&cfgCopy)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "marshal: " + err.Error()})
		return
	}
	if s.deps.State != nil && cfgCopy.EVCharger != nil {
		if pw, ok := s.deps.State.LoadConfig(evPasswordKey); ok && pw != "" {
			if withPw, ierr := injectEVPassword(data, pw); ierr == nil {
				data = withPw
			}
		}
	}

	w.Header().Set("Content-Type", contentTypeYAML)
	w.Header().Set("Cache-Control", "no-store")
	// CORS: mirror what writeJSON sets, since the raw endpoint bypasses
	// it (text payload, not JSON).
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(data)
}

// stripEVPassword returns a copy of the YAML body with
// ev_charger.password removed. Needed because strict-unmarshal (the
// KnownFields gate against typos) would otherwise reject the field —
// config.EVCharger tags password as yaml:"-" since it persists in
// state.db, not config.yaml. Callers should pair this with
// extractEVPassword to re-inject the value into the parsed struct.
// Returns the original bytes unchanged when the document has no
// ev_charger block or any parse hiccup — downstream unmarshal will
// produce the real error.
func stripEVPassword(body []byte) []byte {
	var m map[string]any
	if err := yaml.Unmarshal(body, &m); err != nil {
		return body
	}
	ev, ok := m["ev_charger"].(map[string]any)
	if !ok {
		return body
	}
	if _, present := ev["password"]; !present {
		return body
	}
	delete(ev, "password")
	out, err := yaml.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// extractEVPassword pulls ev_charger.password out of a raw YAML
// body via a generic-map unmarshal. The typed config.EVCharger
// struct tags the field with yaml:"-" so the standard Unmarshal
// never populates it; this is how the raw-config editor supports
// password edits without giving every code path the ability to
// persist a password into config.yaml by accident.
//
// Returns ("", false) when the document has no ev_charger section,
// when ev_charger is not a mapping, or when password is absent/empty.
func extractEVPassword(body []byte) (string, bool) {
	var m map[string]any
	if err := yaml.Unmarshal(body, &m); err != nil {
		return "", false
	}
	ev, ok := m["ev_charger"].(map[string]any)
	if !ok {
		return "", false
	}
	pw, ok := ev["password"].(string)
	if !ok || pw == "" {
		return "", false
	}
	return pw, true
}

// injectEVPassword unmarshals a YAML document into a generic map,
// writes ev_charger.password = pw, and re-marshals. Used to surface
// the EV charger password in the editor even though config.EVCharger
// tags the field with yaml:"-" for on-disk files (the password lives
// in state.db, not config.yaml). Returns (original, nil) when the
// document has no ev_charger block — nothing to inject into.
func injectEVPassword(data []byte, pw string) ([]byte, error) {
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	ev, ok := m["ev_charger"].(map[string]any)
	if !ok {
		return data, nil
	}
	ev["password"] = pw
	return yaml.Marshal(m)
}

// handlePostConfigRaw applies a YAML payload: parse → preserve/update
// secrets → Validate → atomic save → propagate. Shares the same
// post-save wiring as handlePostConfig (update Ctrl + Cfg under their
// respective locks) so the two entry points are behaviorally
// equivalent; the only difference is wire format.
func (s *Server) handlePostConfigRaw(w http.ResponseWriter, r *http.Request) {
	body, err := readYAMLBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// Anchor / depth guard before we hand the bytes to yaml.v3 — bails
	// out billion-laughs-style documents that stay under the byte cap
	// but expand pathologically on unmarshal.
	if err := scanYAMLComplexity(body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	// Extract EV charger password out of the body first: it's tagged
	// yaml:"-" on the struct (persisted in state.db, not config.yaml),
	// which KnownFields(true) would flag as an unknown field. Scrub it
	// from the body, then strict-unmarshal the rest.
	extractedPw, hasExtracted := extractEVPassword(body)
	strictBody := body
	if hasExtracted {
		strictBody = stripEVPassword(body)
	}

	var newCfg config.Config
	// Strict unmarshal (KnownFields=true) — typo'd keys like
	// `max_anps: 16` would otherwise be silently dropped and Validate()
	// would pass, leaving the operator's actual intent unapplied.
	if err := strictYAMLUnmarshal(strictBody, &newCfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": formatYAMLError(err)})
		return
	}
	// Fill zero-valued defaults (SafetyMarginAmps etc.) the same way
	// Parse() does for on-disk loads. Without this, the brief window
	// between in-memory swap and fsnotify reload runs with reduced
	// safety-margin budgets.
	config.ApplyDefaults(&newCfg)

	// Re-inject the extracted password into the parsed struct so
	// downstream Validate() + state.db persistence see it.
	if hasExtracted && newCfg.EVCharger != nil {
		newCfg.EVCharger.Password = extractedPw
	}

	// Preserve masked placeholders for any secret the UI didn't touch.
	// Belt-and-braces: GET returns plaintext so editors normally hold
	// the real password, but if a user manually replaced it with the
	// masked placeholder we shouldn't overwrite state.db with that.
	s.deps.CfgMu.RLock()
	newCfg.PreserveMaskedSecrets(s.deps.Cfg)
	s.deps.CfgMu.RUnlock()

	// EV charger password: route to state.db, not config.yaml. Same
	// contract as handlePostConfig.
	if s.deps.State != nil && newCfg.EVCharger != nil {
		pw := newCfg.EVCharger.Password
		if pw != "" && pw != maskedPlaceholder {
			if err := s.deps.State.SaveConfig(evPasswordKey, pw); err != nil {
				slog.Warn("raw-yaml: failed to persist ev_charger_password", "err", err)
			}
		}
		if stored, ok := s.deps.State.LoadConfig(evPasswordKey); ok {
			newCfg.EVCharger.Password = stored
		}
	}

	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}

	if err := s.deps.SaveConfig(s.deps.ConfigPath, &newCfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}

	// Apply control-level changes immediately. Mirrors handlePostConfig.
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetGridTarget(newCfg.Site.GridTargetW)
	s.deps.Ctrl.GridToleranceW = newCfg.Site.GridToleranceW
	s.deps.Ctrl.SlewRateW = newCfg.Site.SlewRateW
	s.deps.Ctrl.MinDispatchIntervalS = newCfg.Site.MinDispatchIntervalS
	s.deps.CtrlMu.Unlock()

	s.deps.CfgMu.Lock()
	*s.deps.Cfg = newCfg
	s.deps.CfgMu.Unlock()

	slog.Info("config updated via /api/config/raw")
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleValidateConfigRaw is the dry-run companion to POST: parse +
// Validate only, plus a unified diff against the canonical current
// config. Lets the Advanced tab show the user exactly what will
// change before they click Apply.
//
// Response shape:
//
//	{ "ok": true,  "diff": "..." }                 // valid; diff may be empty
//	{ "ok": false, "error": "..." [, "line": N] }  // parse/validation error
func (s *Server) handleValidateConfigRaw(w http.ResponseWriter, r *http.Request) {
	body, err := readYAMLBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	if err := scanYAMLComplexity(body); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// See handlePostConfigRaw for why we extract+scrub ev_charger.password
	// before strict-unmarshal.
	extractedPw, hasExtracted := extractEVPassword(body)
	strictBody := body
	if hasExtracted {
		strictBody = stripEVPassword(body)
	}

	var proposed config.Config
	if err := strictYAMLUnmarshal(strictBody, &proposed); err != nil {
		resp := map[string]any{"ok": false, "error": formatYAMLError(err)}
		if line := yamlErrorLine(err); line > 0 {
			resp["line"] = line
		}
		writeJSON(w, 200, resp)
		return
	}
	config.ApplyDefaults(&proposed)
	if hasExtracted && proposed.EVCharger != nil {
		proposed.EVCharger.Password = extractedPw
	}

	s.deps.CfgMu.RLock()
	cfgCopy := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	proposed.PreserveMaskedSecrets(&cfgCopy)

	// Merge stored password into the proposed config so the diff
	// compares like-for-like against the canonical GET output. Don't
	// persist anything.
	if s.deps.State != nil && proposed.EVCharger != nil {
		if stored, ok := s.deps.State.LoadConfig(evPasswordKey); ok && stored != "" {
			pw := proposed.EVCharger.Password
			if pw == "" || pw == maskedPlaceholder {
				cp := *proposed.EVCharger
				cp.Password = stored
				proposed.EVCharger = &cp
			}
		}
	}

	if err := proposed.Validate(); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "validation: " + err.Error()})
		return
	}

	// Canonical-marshal both sides and produce a unified diff. Using
	// canonical text on both sides means "no changes" really means
	// "no semantic changes" even if the user only moved whitespace.
	currentData, err := marshalCanonicalYAML(&cfgCopy)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": "marshal current: " + err.Error()})
		return
	}
	// Apply the same secret-resolution we do on GET so both sides of
	// the diff carry the plaintext password. Otherwise an unchanged
	// password would always appear as a diff hunk (current: absent;
	// proposed: present, since user edited through the injected view).
	if s.deps.State != nil {
		if pw, ok := s.deps.State.LoadConfig(evPasswordKey); ok && pw != "" {
			if withPw, ierr := injectEVPassword(currentData, pw); ierr == nil {
				currentData = withPw
			}
		}
	}
	proposedData, err := marshalCanonicalYAML(&proposed)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": "marshal proposed: " + err.Error()})
		return
	}
	if s.deps.State != nil && proposed.EVCharger != nil && proposed.EVCharger.Password != "" {
		if withPw, ierr := injectEVPassword(proposedData, proposed.EVCharger.Password); ierr == nil {
			proposedData = withPw
		}
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(currentData)),
		B:        difflib.SplitLines(string(proposedData)),
		FromFile: "current",
		ToFile:   "proposed",
		Context:  3,
	}
	diffText, _ := difflib.GetUnifiedDiffString(diff)

	writeJSON(w, 200, map[string]any{"ok": true, "diff": diffText})
}

// marshalCanonicalYAML runs config.SaveAtomic's marshal step without
// writing to disk — the output is what would land on disk if we
// called SaveAtomic. Used on both sides of the validate-diff so the
// comparison is apples-to-apples.
func marshalCanonicalYAML(c *config.Config) ([]byte, error) {
	// We intentionally don't call SaveAtomic with a tmp path because
	// we don't want to touch disk. yaml.Marshal on a copy matches
	// what SaveAtomic does after its relDriverPath rewrite; driver
	// paths come back as whatever Load() resolved them to. For the
	// diff/editor that's fine — the worst case is an absolute path
	// showing up where a relative one was, which SaveAtomic
	// normalizes back at the actual save step.
	return yaml.Marshal(c)
}

// readYAMLBody enforces the body cap and returns the raw bytes.
// Keeping the parse separate from the read lets POST and Validate
// share the read but diverge on error handling.
func readYAMLBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("empty body")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, rawConfigMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > rawConfigMaxBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", rawConfigMaxBytes)
	}
	return data, nil
}

// formatYAMLError normalizes a yaml.v3 error message. The library
// already includes "yaml: line N:" — we keep that as-is so the
// frontend lint panel can echo the exact line number the parser
// flagged.
func formatYAMLError(err error) string {
	if err == nil {
		return ""
	}
	return "yaml: " + strings.TrimPrefix(err.Error(), "yaml: ")
}

// yamlErrorLine extracts the line number from a yaml.v3 error message
// like "yaml: line 37: mapping values are not allowed in this
// context". Returns 0 when no line is present — the frontend falls
// back to highlighting the first line in that case.
func yamlErrorLine(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	const prefix = "yaml: line "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return 0
	}
	rest := msg[i+len(prefix):]
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// unused silences the os import in case we later want to read the
// file directly. Kept ready for a future iteration that preserves
// user-authored comments by returning the file bytes on GET.
var _osRead = os.ReadFile
