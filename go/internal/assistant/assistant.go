// Package assistant calls an OpenAI-compatible chat API (OpenRouter by
// default) to explain a local FTW help report. It never issues commands.
package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultModel   = "openrouter/free"
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	// Timeout covers a slow free-tier completion. The HTTP handler holds
	// one in-flight ask, so a long wait does not pile up.
	Timeout = 90 * time.Second
	// maxReportRunes keeps the prompt inside a free-model context.
	maxReportRunes = 80_000
	maxQuestion    = 2000
	maxTokens      = 2500
	maxIssueTitle  = 80
)

// Request is one Ask why turn: a question plus the already-built help report.
type Request struct {
	APIKey   string
	Model    string
	BaseURL  string
	Question string
	Report   string
}

// Reply is what the UI shows. Issue fields are empty when the model does
// not think this is a bug.
type Reply struct {
	Answer        string
	IssueTitle    string
	IssueBody     string
	Model         string
	ResolvedModel string
}

// APIError is an outbound failure the HTTP layer can map to a status code.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// Client posts chat completions. HTTP may be nil (a 90s client is used).
type Client struct {
	HTTP *http.Client
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete asks the model to explain the report. It does not stream.
func (c *Client) Complete(ctx context.Context, req Request) (Reply, error) {
	var zero Reply
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		return zero, &APIError{Status: http.StatusConflict, Msg: "Ask why needs an API key in Settings → System"}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultModel
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		question = "What is going on right now?"
	}
	if utf8.RuneCountInString(question) > maxQuestion {
		return zero, &APIError{Status: http.StatusBadRequest, Msg: "question is too long"}
	}
	report := strings.TrimSpace(req.Report)
	if report == "" {
		return zero, &APIError{Status: http.StatusServiceUnavailable, Msg: "help report is empty"}
	}
	if utf8.RuneCountInString(report) > maxReportRunes {
		runes := []rune(report)
		report = string(runes[:maxReportRunes]) + "\n\n[truncated]\n"
	}

	body, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: Skill},
			{Role: "user", Content: "Question:\n" + question + "\n\nSite report:\n\n" + report},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.2,
	})
	if err != nil {
		return zero, err
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: Timeout}
	}

	endpoint, err := url.JoinPath(base, "chat", "completions")
	if err != nil {
		return zero, &APIError{Status: http.StatusBadRequest, Msg: "assistant.base_url is not a valid URL"}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("HTTP-Referer", "https://github.com/srcfl/ftw")
	httpReq.Header.Set("X-Title", "FTW")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "could not reach the model API"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return zero, &APIError{Status: http.StatusUnauthorized, Msg: "OpenRouter rejected the API key"}
	}
	if resp.StatusCode == http.StatusPaymentRequired {
		return zero, &APIError{Status: http.StatusPaymentRequired, Msg: "this model needs credits; use openrouter/free or add credit"}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return zero, &APIError{Status: http.StatusTooManyRequests, Msg: "model rate limit; try again in a minute"}
	}
	if resp.StatusCode >= 500 {
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "model API is unavailable"}
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = fmt.Sprintf("model API returned HTTP %d", resp.StatusCode)
		}
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "model API error"}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "model API returned unreadable JSON"}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "model API error"}
	}
	if len(parsed.Choices) == 0 {
		return zero, &APIError{Status: http.StatusBadGateway, Msg: "model returned no answer"}
	}
	reply := Parse(parsed.Choices[0].Message.Content)
	reply.Model = model
	reply.ResolvedModel = strings.TrimSpace(parsed.Model)
	if reply.ResolvedModel == "" {
		reply.ResolvedModel = model
	}
	return reply, nil
}

// Parse splits the model's markdown into answer and optional issue fields.
func Parse(content string) Reply {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSpace(content)
	sections := splitSections(content)
	answer := strings.TrimSpace(sections["answer"])
	if answer == "" {
		answer = content
	}
	title := cleanIssueTitle(sections["issue title"])
	body := Redact(strings.TrimSpace(sections["issue body"]))
	if title == "" {
		body = ""
	}
	return Reply{Answer: answer, IssueTitle: title, IssueBody: body}
}

func splitSections(content string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(content, "\n")
	var current string
	var buf []string
	flush := func() {
		if current == "" {
			return
		}
		out[current] = strings.TrimSpace(strings.Join(buf, "\n"))
	}
	for _, line := range lines {
		if name, ok := headingName(line); ok {
			flush()
			current = name
			buf = buf[:0]
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

func headingName(line string) (string, bool) {
	s := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(s, "#"):
		s = strings.TrimSpace(strings.TrimLeft(s, "#"))
	case strings.HasPrefix(s, "**") && strings.HasSuffix(s, "**"):
		s = strings.TrimSpace(strings.Trim(s, "*"))
	default:
		return "", false
	}
	if s == "" {
		return "", false
	}
	switch strings.ToLower(s) {
	case "answer", "issue title", "issue body":
		return strings.ToLower(s), true
	default:
		return "", false
	}
}

func cleanIssueTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"'")
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "none", "n/a", "na", "-", "empty", "not a bug":
		return ""
	}
	if utf8.RuneCountInString(s) > maxIssueTitle {
		s = string([]rune(s)[:maxIssueTitle-1]) + "…"
	}
	return s
}

var (
	ipv4Re   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	openKey  = regexp.MustCompile(`sk-or-v1-\S+`)
	bearerRe = regexp.MustCompile(`(?i)bearer\s+\S+`)
)

// Redact strips the obvious secret and address patterns from issue text.
func Redact(s string) string {
	s = ipv4Re.ReplaceAllString(s, "[ip omitted]")
	s = openKey.ReplaceAllString(s, "[key omitted]")
	s = bearerRe.ReplaceAllString(s, "Bearer [omitted]")
	return s
}
