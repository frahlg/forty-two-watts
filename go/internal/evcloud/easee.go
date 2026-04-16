package evcloud

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func init() { Register("easee", Easee{}) }

// Easee implements Provider for the Easee Cloud API.
type Easee struct{}

// Bounded timeout so a stalled TCP connection to api.easee.com cannot tie
// up the HTTP handler goroutine indefinitely. 15s matches what the rest
// of the codebase uses for external HTTP calls.
var easeeClient = &http.Client{Timeout: 15 * time.Second}

func (Easee) ListChargers(email, password string) ([]Charger, error) {
	token, err := easeeLogin(email, password)
	if err != nil {
		return nil, err
	}
	return easeeListChargers(token)
}

func easeeLogin(email, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"userName": email, "password": password})
	resp, err := easeeClient.Post("https://api.easee.com/api/accounts/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("login: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("login: no token in response")
	}
	return tok.AccessToken, nil
}

func easeeListChargers(token string) ([]Charger, error) {
	req, _ := http.NewRequest("GET", "https://api.easee.com/api/chargers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := easeeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chargers request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chargers: HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("chargers: decode: %w", err)
	}
	out := make([]Charger, len(list))
	for i, ch := range list {
		out[i] = Charger{ID: ch.ID, Name: ch.Name}
	}
	return out, nil
}
