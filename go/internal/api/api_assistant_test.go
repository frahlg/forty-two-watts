package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func assistantTestServer(t *testing.T, asst *config.Assistant, httpClient *http.Client) *Server {
	t.Helper()
	st := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	tel.Update("meter", telemetry.DerMeter, 500, nil, nil)
	return New(&Deps{
		Ctrl:          st,
		CtrlMu:        &sync.Mutex{},
		Tel:           tel,
		Cfg:           &config.Config{Assistant: asst},
		CfgMu:         &sync.RWMutex{},
		Version:       "test-version",
		AssistantHTTP: httpClient,
	})
}

func postAssistantAsk(question string) *http.Request {
	body := `{"question":` + jsonString(question) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAssistantAskIsLocal(t *testing.T) {
	srv := assistantTestServer(t, nil, nil)
	req := postAssistantAsk("")
	if facts := srv.Route(req); facts.Tier != apiauth.TierLocal {
		t.Fatalf("tier = %v, want Local — this path ships the help report off-box", facts.Tier)
	}
}

func TestAssistantStatusIsRead(t *testing.T) {
	srv := assistantTestServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/assistant/status", nil)
	if facts := srv.Route(req); facts.Tier != apiauth.TierRead {
		t.Fatalf("tier = %v, want Read", facts.Tier)
	}
}

func TestAssistantStatusWithoutConfig(t *testing.T) {
	srv := assistantTestServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/assistant/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got assistantStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ready || got.Enabled || got.Configured {
		t.Fatalf("empty config should be unavailable: %+v", got)
	}
	if got.Model != config.DefaultAssistantModel {
		t.Fatalf("model = %q", got.Model)
	}
	if !strings.Contains(got.Unavailable, "Turn on") {
		t.Fatalf("unavailable = %q", got.Unavailable)
	}
}

func TestAssistantAskWithoutKey(t *testing.T) {
	srv := assistantTestServer(t, &config.Assistant{Enabled: true}, nil)
	req := postAssistantAsk("why?")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-or-") {
		t.Fatal("response leaked a key")
	}
}

func TestAssistantAskExplainsReport(t *testing.T) {
	var sawPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		sawPrompt = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "meta-llama/llama-3.3-70b-instruct:free",
			"choices": []map[string]any{
				{"message": map[string]string{
					"role":    "assistant",
					"content": "## Answer\nThe site meter is live and the house is importing about 500 W.\n\n## Issue title\n[bug] meter idle mismatch\n\n## Issue body\nFTW test-version. Site meter reads 500 W import while the plan expected idle.\nTalks to 10.0.0.5.\n",
				}},
			},
		})
	}))
	defer upstream.Close()

	asst := &config.Assistant{
		Enabled: true,
		APIKey:  "sk-or-v1-secret-key",
		Model:   "openrouter/free",
		BaseURL: upstream.URL,
	}
	srv := assistantTestServer(t, asst, upstream.Client())
	req := postAssistantAsk("why import?")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sawPrompt, "why import?") {
		t.Fatal("prompt missing the question")
	}
	if !strings.Contains(sawPrompt, "test-version") && !strings.Contains(sawPrompt, "FTW help report") {
		t.Fatalf("prompt missing the help report: %s", sawPrompt)
	}
	if strings.Contains(sawPrompt, "sk-or-v1-secret-key") {
		t.Fatal("API key was sent in the prompt body")
	}

	var got assistantAskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Answer, "500 W") {
		t.Fatalf("answer = %q", got.Answer)
	}
	if got.IssueTitle != "[bug] meter idle mismatch" {
		t.Fatalf("title = %q", got.IssueTitle)
	}
	if strings.Contains(got.IssueBody, "10.0.0.5") {
		t.Fatalf("issue body kept an IP: %q", got.IssueBody)
	}
	if !strings.Contains(got.IssueURL, "github.com/srcfl/ftw/issues/new") {
		t.Fatalf("issue url = %q", got.IssueURL)
	}
	if strings.Contains(rec.Body.String(), "sk-or-v1-secret-key") {
		t.Fatal("API key leaked into the HTTP response")
	}
	if strings.Contains(got.Answer, "# FTW help report") {
		t.Fatal("response echoed the raw help report")
	}
}

func TestAssistantAskRejectsSecondCall(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "## Answer\nok\n"}},
			},
		})
	}))
	defer upstream.Close()
	asst := &config.Assistant{Enabled: true, APIKey: "k", BaseURL: upstream.URL}
	srv := assistantTestServer(t, asst, upstream.Client())

	done := make(chan int, 1)
	go func() {
		req := postAssistantAsk("one")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		done <- rec.Code
	}()
	<-started
	req := postAssistantAsk("two")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second ask = %d body=%s", rec.Code, rec.Body.String())
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first ask = %d", code)
	}
}
