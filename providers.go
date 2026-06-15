package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)


// ProviderManager handles multi-provider routing, discovery, and health checks.
type ProviderManager struct {
	mu   sync.RWMutex
	cfg  *ConfigStore
	http *http.Client
}

// NewProviderManager creates a new provider manager.
func NewProviderManager(cfg *ConfigStore) *ProviderManager {
	return &ProviderManager{
		cfg: cfg,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// OAIModelResponse represents the OpenAI /v1/models response.
type OAIModelResponse struct {
	Object string     `json:"object"`
	Data   []OAIModel `json:"data"`
}

// OAIModel represents a single model from the OpenAI API.
// context_window can be either an integer (standard) or an object like llm7.io's {"tokens":240000}
type OAIModel struct {
	ID            string          `json:"id"`
	Object        string          `json:"object"`
	OwnedBy       string          `json:"owned_by,omitempty"`
	Created       int64           `json:"created,omitempty"`
	ContextWindow json.RawMessage `json:"context_window,omitempty"`
}

// contextWindowTokens extracts the context window size in tokens from various formats.
func (m *OAIModel) contextWindowTokens() int {
	if len(m.ContextWindow) == 0 {
		return 0
	}
	// Try plain integer first: context_window: 128000
	var n int
	if json.Unmarshal(m.ContextWindow, &n) == nil {
		return n
	}
	// Try object format: context_window: {"tokens": 240000, "chars": null}
	var obj struct {
		Tokens *int `json:"tokens"`
		Chars  *int `json:"chars"`
	}
	if json.Unmarshal(m.ContextWindow, &obj) == nil {
		if obj.Tokens != nil {
			return *obj.Tokens
		}
		if obj.Chars != nil {
			return *obj.Chars
		}
	}
	return 0
}


// DiscoverModels fetches available models from all providers.
func (pm *ProviderManager) DiscoverModels() {
	appCfg := pm.cfg.Get()
	var allModels []ModelInfo

	for i, p := range appCfg.Providers {
		if p.URL == "" {
			pm.cfg.UpdateProviderStatus(i, "offline", 0)
			continue
		}

		start := time.Now()
		models, err := pm.fetchModels(p, i)
		latency := int(time.Since(start).Milliseconds())

		if err != nil {
			// If it's an auth error, don't pretend it's online
			if strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
				pm.cfg.UpdateProviderStatus(i, "offline", latency)
				continue
			}
			// /v1/models failed for other reasons — try a lightweight ping
			if pm.pingProvider(p) {
				// Endpoint reachable but /v1/models unsupported — mark online with configured model
				pm.cfg.UpdateProviderStatus(i, "online", latency)
				if p.Model != "" {
					allModels = append(allModels, ModelInfo{
						ID:          p.Model,
						Provider:    p.Name,
						Object:      "model",
						Latency:     latency,
						DisplayName: p.Model,
						Pricing:     analyzePricing(p.Name, p.Model),
					})
				}
			} else {
				pm.cfg.UpdateProviderStatus(i, "offline", latency)
			}
			continue
		}

		// Provider responded to /v1/models — always mark online
		pm.cfg.UpdateProviderStatus(i, "online", latency)

		// If model list is empty, add the configured model as a fallback
		if len(models) == 0 && p.Model != "" {
			allModels = append(allModels, ModelInfo{
				ID:          p.Model,
				Provider:    p.Name,
				Object:      "model",
				Latency:     latency,
				DisplayName: p.Model,
				Pricing:     analyzePricing(p.Name, p.Model),
			})
		}

		// Tag models with provider info and pricing analysis
		for _, m := range models {
			mi := ModelInfo{
				ID:       m.ID,
				Provider: p.Name,
				Object:   "model",
				Latency:  latency,
			}
			if m.OwnedBy != "" {
				mi.OwnedBy = m.OwnedBy
			}
			if m.Created > 0 {
				mi.Created = m.Created
			}
			if ctxSize := m.contextWindowTokens(); ctxSize > 0 {
				mi.ContextSize = ctxSize
			}
			mi.Pricing = analyzePricing(p.Name, m.ID)
			mi.DisplayName = m.ID
			allModels = append(allModels, mi)
		}

	}

	pm.cfg.SetModels(allModels)
}

// pingProvider does a minimal HEAD/GET to check if the base URL is reachable.
func (pm *ProviderManager) pingProvider(p Provider) bool {
	url := strings.TrimRight(p.URL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	if p.Key != "" {
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 200 OK means it works. 404 means models endpoint doesn't exist but server is up.
	// 401/403 means auth failed (so server is up but unusable without valid key).
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound
}

// fetchModels calls the /v1/models endpoint for a specific provider.
// Handles both standard OpenAI format {"object":"list","data":[...]}
// and bare array format [...] used by some providers (e.g. llm7.io).
func (pm *ProviderManager) fetchModels(p Provider, idx int) ([]OAIModel, error) {
	url := strings.TrimRight(p.URL, "/") + "/models"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if p.Key != "" {
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := pm.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Read the raw body so we can try multiple decode strategies
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read models body: %w", err)
	}

	// Strategy 1: Standard OpenAI envelope {"object":"list","data":[...]}
	var oaiResp OAIModelResponse
	if json.Unmarshal(bodyBytes, &oaiResp) == nil && len(oaiResp.Data) > 0 {
		return oaiResp.Data, nil
	}

	// Strategy 2: Bare array [...] (used by llm7.io and some others)
	var directList []OAIModel
	if json.Unmarshal(bodyBytes, &directList) == nil {
		return directList, nil
	}

	// Strategy 3: Wrapped in another structure with "models" key
	var wrapped struct {
		Models []OAIModel `json:"models"`
		Data   []OAIModel `json:"data"`
	}
	if json.Unmarshal(bodyBytes, &wrapped) == nil {
		if len(wrapped.Models) > 0 {
			return wrapped.Models, nil
		}
		if len(wrapped.Data) > 0 {
			return wrapped.Data, nil
		}
	}

	// Couldn't parse but endpoint responded — return empty (not an error)
	return []OAIModel{}, nil
}


// TestConnection tests connectivity to a specific provider.
func (pm *ProviderManager) TestConnection(idx int) (status string, latency int, modelCount int, errMsg string) {
	p, ok := pm.cfg.GetProviderByIndex(idx)
	if !ok {
		return "error", 0, 0, "provider not found"
	}

	if p.URL == "" {
		return "offline", 0, 0, "missing URL"
	}

	start := time.Now()

	// Try fetching models as a connectivity test
	models, err := pm.fetchModels(p, idx)
	latency = int(time.Since(start).Milliseconds())

	if err != nil {
		// /v1/models failed — check if endpoint is at least reachable
		if pm.pingProvider(p) {
			pm.cfg.UpdateProviderStatus(idx, "online", latency)
			return "online", latency, 0, "endpoint reachable but /v1/models not supported"
		}
		pm.cfg.UpdateProviderStatus(idx, "offline", latency)
		return "offline", latency, 0, err.Error()
	}

	// Always mark online if /v1/models succeeded (even empty list)
	pm.cfg.UpdateProviderStatus(idx, "online", latency)
	return "online", latency, len(models), ""
}

// GetProviderForRequest returns the best provider index for a request.
// Uses active provider; if it's offline and failover is enabled, picks next best.
func (pm *ProviderManager) GetProviderForRequest() int {
	appCfg := pm.cfg.Get()
	if len(appCfg.Providers) == 0 {
		return -1
	}

	active := appCfg.ActiveIdx
	if active < 0 || active >= len(appCfg.Providers) {
		active = 0
	}

	// Check if active provider is online
	p := appCfg.Providers[active]
	if p.Status == "online" || p.Status == "degraded" {
		return active
	}

	// Active is offline — find next best by priority
	if appCfg.Failover {
		for i, prov := range appCfg.Providers {
			if i != active && (prov.Status == "online" || prov.Status == "degraded") {
				return i
			}
		}
	}

	// No online provider found — use active anyway (will fail gracefully)
	return active
}

// analyzePricing determines if a model is likely free or paid based on heuristics.
func analyzePricing(providerName, modelID string) string {
	prov := strings.ToLower(providerName)
	id := strings.ToLower(modelID)

	// 1. Providers that are fully free (or mostly free API tiers)
	freeProviders := []string{
		"groq", "gemini", "ollama", "github", 
		"glhf", "hyperbolic", "scaleway", "cerebras", "sambanova",
	}
	for _, fp := range freeProviders {
		if strings.Contains(prov, fp) {
			return "free"
		}
	}

	// 2. Explicitly tagged free models (e.g., openrouter ":free")
	if strings.Contains(id, ":free") || strings.Contains(id, "-free") {
		return "free"
	}

	// 3. Known paid model patterns on generic/paid providers
	paidPatterns := []string{
		"opus", "sonnet", "haiku", "claude",
		"gpt-4", "gpt-3.5", "o1", "o3",
		"grok", "deepseek", "llama", "qwen",
		"mistral", "mixtral", "gemma", "phi", "gemini",
	}

	for _, pattern := range paidPatterns {
		if strings.Contains(id, pattern) {
			return "paid"
		}
	}

	return "unknown"
}

// HealthCheck runs periodic health checks on all providers.
func (pm *ProviderManager) HealthCheck() {
	appCfg := pm.cfg.Get()
	for i := range appCfg.Providers {
		pm.TestConnection(i)
	}
	// Re-discover models after health check
	pm.DiscoverModels()
}

// StartHealthChecker starts a periodic health check goroutine.
func (pm *ProviderManager) StartHealthChecker(interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			pm.HealthCheck()
		}
	}()
}
