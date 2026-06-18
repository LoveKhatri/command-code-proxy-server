package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

const (
	// upstreamModelsURL is Command Code's Provider API models endpoint
	upstreamModelsURL = "https://api.commandcode.ai/provider/v1/models"

	// modelCacheTTL is how often we refresh the model list from upstream
	modelCacheTTL = 6 * time.Hour

	// modelFetchTimeout is the per-request timeout for upstream model fetching
	modelFetchTimeout = 10 * time.Second
)

// UpstreamModel represents a single model in the upstream Command Code API
// response (OpenAI-compatible /v1/models format).
type UpstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// UpstreamModelList is the response wrapper for /v1/models
type UpstreamModelList struct {
	Object string           `json:"object"`
	Data   []UpstreamModel  `json:"data"`
}

// ModelCache holds the dynamically-fetched model list with thread-safe access.
type ModelCache struct {
	mu          sync.RWMutex
	models      []api.OpenAIModel
	fetchedAt   time.Time
	httpClient  *http.Client
	fetchingNow bool // prevents concurrent refreshes
}

// NewModelCache creates an empty cache. Call Refresh() to populate it.
func NewModelCache() *ModelCache {
	return &ModelCache{
		models:     nil,
		httpClient: &http.Client{Timeout: modelFetchTimeout},
	}
}

// containsModel checks if a model ID is already in the list (used to avoid
// double-adding injected models).
func containsModel(list []api.OpenAIModel, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Get returns a copy of the current cached models. If cache is empty,
// returns the static fallback list from getStaticModels().
func (c *ModelCache) Get() []api.OpenAIModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.models) == 0 {
		return getStaticModels()
	}
	// Return a copy to prevent callers from mutating the cache
	out := make([]api.OpenAIModel, len(c.models))
	copy(out, c.models)
	return out
}

// IsStale returns true if the cache needs refreshing.
func (c *ModelCache) IsStale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.fetchedAt) > modelCacheTTL || len(c.models) == 0
}

// Refresh fetches the upstream model list and rebuilds the cache.
// On error, logs but keeps the existing cache (or static fallback if empty).
func (c *ModelCache) Refresh(apiKey string) error {
	c.mu.Lock()
	if c.fetchingNow {
		c.mu.Unlock()
		return nil // another goroutine is already refreshing
	}
	c.fetchingNow = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.fetchingNow = false
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamModelsURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "command-code-proxy/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch upstream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var upstream UpstreamModelList
	if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
		return fmt.Errorf("decode upstream: %w", err)
	}

	// Convert upstream models to our OpenAI model format, enriching with
	// context_length from our local override map.
	enriched := make([]api.OpenAIModel, 0, len(upstream.Data)+1)
	for _, m := range upstream.Data {
		enriched = append(enriched, api.OpenAIModel{
			ID:            m.ID,
			Object:        m.Object,
			Created:       m.Created,
			OwnedBy:       m.OwnedBy,
			ContextLength: ContextLengthFor(m.ID),
		})
	}

	// Inject Command Code's internal "taste-1" model — not exposed via upstream
	// /v1/models API but available to all CLI users. Skip if already present.
	if !containsModel(enriched, tasteOneModelID) {
		enriched = append(enriched, api.OpenAIModel{
			ID:            tasteOneModelID,
			Object:        "model",
			Created:       0,
			OwnedBy:       "commandcode",
			ContextLength: tasteOneContextLength,
		})
		log.Printf("[models] injected internal model: %s", tasteOneModelID)
	}

	// Inject Claude variants that may not appear in upstream response:
	// - claude-haiku-4-5 (bare name, no date suffix)
	// - claude-opus-4-6 (alternate Opus variant)
	// Skip if already present in upstream response.
	for _, m := range []api.OpenAIModel{
		{ID: "claude-haiku-4-5", Object: "model", Created: 0, OwnedBy: "anthropic", ContextLength: ContextLengthFor("claude-haiku-4-5")},
		{ID: "claude-opus-4-6", Object: "model", Created: 0, OwnedBy: "anthropic", ContextLength: ContextLengthFor("claude-opus-4-6")},
		{ID: "MiniMaxAI/MiniMax-M3-Promo", Object: "model", Created: 0, OwnedBy: "minimaxai", ContextLength: ContextLengthFor("MiniMaxAI/MiniMax-M3-Promo")},
	} {
		if !containsModel(enriched, m.ID) {
			enriched = append(enriched, m)
			log.Printf("[models] injected upstream-missing model: %s", m.ID)
		}
	}

	c.mu.Lock()
	c.models = enriched
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	log.Printf("[models] refreshed cache: %d models from upstream", len(enriched))
	return nil
}

// StartBackgroundRefresh launches a goroutine that periodically refreshes
// the cache. Safe to call once at startup.
func (c *ModelCache) StartBackgroundRefresh(apiKey string) {
	go func() {
		// Initial fetch
		if err := c.Refresh(apiKey); err != nil {
			log.Printf("[models] initial refresh failed (using static fallback): %v", err)
		}

		ticker := time.NewTicker(modelCacheTTL)
		defer ticker.Stop()
		for range ticker.C {
			if err := c.Refresh(apiKey); err != nil {
				log.Printf("[models] refresh failed (keeping existing cache): %v", err)
			}
		}
	}()
}
