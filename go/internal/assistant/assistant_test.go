package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSplitsAnswerAndIssue(t *testing.T) {
	got := Parse("## Answer\nBattery is idle because the plan is in a cheap-hour wait.\n\n## Issue title\n\n## Issue body\n")
	if !strings.Contains(got.Answer, "Battery is idle") {
		t.Fatalf("answer = %q", got.Answer)
	}
	if got.IssueTitle != "" || got.IssueBody != "" {
		t.Fatalf("expected no issue, got %+v", got)
	}
}

func TestParseKeepsIssueWhenPresent(t *testing.T) {
	got := Parse("## Answer\nDriver is offline.\n## Issue title\n[bug] sungrow poll timeout\n## Issue body\nThe Sungrow driver has been offline for 20 minutes.\n")
	if got.IssueTitle != "[bug] sungrow poll timeout" {
		t.Fatalf("title = %q", got.IssueTitle)
	}
	if !strings.Contains(got.IssueBody, "Sungrow") {
		t.Fatalf("body = %q", got.IssueBody)
	}
}

func TestParseBareProseIsTheAnswer(t *testing.T) {
	got := Parse("The house is importing 400 W and that matches the plan.")
	if got.Answer != "The house is importing 400 W and that matches the plan." {
		t.Fatalf("answer = %q", got.Answer)
	}
	if got.IssueTitle != "" {
		t.Fatalf("unexpected title %q", got.IssueTitle)
	}
}

func TestRedactStripsAddressesAndKeys(t *testing.T) {
	in := "Talks to 192.168.1.10 with sk-or-v1-secret and Bearer abcdef"
	got := Redact(in)
	if strings.Contains(got, "192.168.1.10") || strings.Contains(got, "sk-or-v1") || strings.Contains(got, "abcdef") {
		t.Fatalf("not redacted: %q", got)
	}
}

func TestCompletePostsChatCompletions(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "openrouter/free",
			"choices": []map[string]any{
				{"message": map[string]string{
					"role":    "assistant",
					"content": "## Answer\nIdle as planned.\n\n## Issue title\n\n## Issue body\n",
				}},
			},
		})
	}))
	defer srv.Close()

	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "sk-or-v1-test",
		Model:    DefaultModel,
		BaseURL:  srv.URL,
		Question: "why idle?",
		Report:   "# FTW help report\nIdle.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-or-v1-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBody, "why idle?") || !strings.Contains(gotBody, "FTW help report") {
		t.Fatalf("prompt missing question or report: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"role":"system"`) {
		t.Fatalf("system skill missing from %s", gotBody)
	}
	if reply.Answer != "Idle as planned." {
		t.Fatalf("answer = %q", reply.Answer)
	}
	if strings.Contains(reply.Answer, "sk-or-v1-test") {
		t.Fatal("API key leaked into the reply")
	}
}

func TestCompleteMapsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cli := &Client{HTTP: srv.Client()}
	_, err := cli.Complete(context.Background(), Request{
		APIKey: "bad", Model: DefaultModel, BaseURL: srv.URL, Report: "x",
	})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteRejectsEmptyKey(t *testing.T) {
	cli := &Client{}
	_, err := cli.Complete(context.Background(), Request{Report: "x"})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusConflict {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteRunsReadOnlyTools(t *testing.T) {
	var saw []string
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		saw = append(saw, string(raw))
		if strings.Contains(string(raw), `"role":"tool"`) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "openrouter/free",
				"choices": []map[string]any{
					{"message": map[string]string{
						"role":    "assistant",
						"content": "## Answer\nSungrow is offline.\n\n## Issue title\n\n## Issue body\n",
					}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{
							"id":   "call_1",
							"type": "function",
							"function": map[string]string{
								"name":      "get_driver_health",
								"arguments": `{"name":"sungrow"}`,
							},
						},
						{
							"id":   "call_bad",
							"type": "function",
							"function": map[string]string{
								"name":      "driver_command",
								"arguments": `{"action":"battery"}`,
							},
						},
					},
				}},
			},
		})
	}))
	defer srv.Close()

	cli := &Client{HTTP: srv.Client()}
	reply, err := cli.Complete(context.Background(), Request{
		APIKey:   "k",
		BaseURL:  srv.URL,
		Question: "why is sungrow down?",
		Trigger:  "driver sungrow is offline",
		Run: func(name string, args json.RawMessage) (string, error) {
			called = append(called, name)
			return "sungrow status=offline last_success=12 min ago", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "Sungrow is offline." {
		t.Fatalf("answer = %q", reply.Answer)
	}
	if reply.ToolRounds != 2 {
		t.Fatalf("tool rounds = %d, want 2 (health + rejected write)", reply.ToolRounds)
	}
	if len(called) != 1 || called[0] != ToolDriverHealth {
		t.Fatalf("runner called %v, want only get_driver_health", called)
	}
	if !strings.Contains(saw[0], `"name":"get_driver_health"`) {
		t.Fatalf("first request missing tools: %s", saw[0])
	}
	if !strings.Contains(saw[0], "driver sungrow is offline") {
		t.Fatalf("first request missing trigger: %s", saw[0])
	}
	if !strings.Contains(saw[1], "unknown tool; Ask why is read-only") {
		t.Fatalf("write tool was not refused: %s", saw[1])
	}
	if !strings.Contains(saw[1], "sungrow status=offline") {
		t.Fatalf("health result missing: %s", saw[1])
	}
}

func TestAllowedToolIsReadOnly(t *testing.T) {
	if AllowedTool("driver_command") || AllowedTool("set_mode") {
		t.Fatal("write names must not be allowed")
	}
	if !AllowedTool(ToolSupportReport) || !AllowedTool(ToolPlanNow) {
		t.Fatal("read tools must be allowed")
	}
}
