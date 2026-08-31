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
