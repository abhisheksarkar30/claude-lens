// Package catalog supplies the model_catalog table's data: a live
// GET /v1/models refresh, with a shipped fallback so an offline start
// still has a catalogue.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Entry is one model_catalog row.
type Entry struct {
	ModelID         string
	DisplayName     string
	MaxInputTokens  int
	MaxOutputTokens int
	Capabilities    string
	FetchedAt       time.Time
	Source          string // "live" | "shipped"
}

// ShippedCatalog is the built-in fallback used when GET /v1/models is
// unreachable. Context windows are the first-party documented defaults;
// a live refresh supersedes these whenever it succeeds.
func ShippedCatalog() []Entry {
	now := time.Now()
	models := []struct {
		id, name      string
		maxIn, maxOut int
	}{
		{"claude-fable-5-1", "Claude Fable 5.1", 200_000, 64_000},
		{"claude-fable-5", "Claude Fable 5", 200_000, 64_000},
		{"claude-mythos-5-1", "Claude Mythos 5.1", 200_000, 64_000},
		{"claude-mythos-5", "Claude Mythos 5", 200_000, 64_000},
		{"claude-opus-5", "Claude Opus 5", 200_000, 32_000},
		{"claude-opus-4-8", "Claude Opus 4.8", 200_000, 32_000},
		{"claude-opus-4-7", "Claude Opus 4.7", 200_000, 32_000},
		{"claude-opus-4-6", "Claude Opus 4.6", 200_000, 32_000},
		{"claude-sonnet-5", "Claude Sonnet 5", 200_000, 64_000},
		{"claude-sonnet-4-6", "Claude Sonnet 4.6", 200_000, 64_000},
		{"claude-haiku-4-5", "Claude Haiku 4.5", 200_000, 64_000},
	}
	entries := make([]Entry, 0, len(models))
	for _, m := range models {
		entries = append(entries, Entry{
			ModelID:         m.id,
			DisplayName:     m.name,
			MaxInputTokens:  m.maxIn,
			MaxOutputTokens: m.maxOut,
			FetchedAt:       now,
			Source:          "shipped",
		})
	}
	return entries
}

// modelsResponse is the narrow shape of GET /v1/models' body. Anthropic's
// public listing carries id/display_name reliably; context-window and
// capability fields are read if present and left zero otherwise, so an API
// response narrower than expected degrades rather than fails parsing.
type modelsResponse struct {
	Data []struct {
		ID              string   `json:"id"`
		DisplayName     string   `json:"display_name"`
		MaxInputTokens  int      `json:"max_input_tokens"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Capabilities    []string `json:"capabilities"`
	} `json:"data"`
}

// Fetch calls GET {baseURL}/v1/models and returns its entries with
// Source="live". Any failure -- network, non-200, or a body that does not
// parse -- returns ShippedCatalog() instead: an unreachable catalogue
// refresh must never block startup or return an error the caller has to
// handle (fail open).
func Fetch(ctx context.Context, client *http.Client, baseURL, apiKey string) []Entry {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return ShippedCatalog()
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return ShippedCatalog()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ShippedCatalog()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ShippedCatalog()
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ShippedCatalog()
	}
	if len(parsed.Data) == 0 {
		return ShippedCatalog()
	}

	now := time.Now()
	entries := make([]Entry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		var capabilities string
		if len(m.Capabilities) > 0 {
			capabilities = fmt.Sprint(m.Capabilities)
		}
		entries = append(entries, Entry{
			ModelID:         m.ID,
			DisplayName:     m.DisplayName,
			MaxInputTokens:  m.MaxInputTokens,
			MaxOutputTokens: m.MaxOutputTokens,
			Capabilities:    capabilities,
			FetchedAt:       now,
			Source:          "live",
		})
	}
	if len(entries) == 0 {
		return ShippedCatalog()
	}
	return entries
}
