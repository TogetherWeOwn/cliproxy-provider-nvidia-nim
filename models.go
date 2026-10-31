package main

import (
	"context"
	"encoding/json"
	"hash/maphash"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The hosted /v1/models list is a static catalog, not per-account availability:
// on 2026-10-31 it listed 85 IDs for one key, and a one-token chat probe of each
// returned 404 "function ... not found" for 60 of them; retired IDs answer 410.
// ModelsForAuth therefore validates every candidate with the chat endpoint the
// executor uses and advertises only models the gateway does not definitively
// reject. Unknown outcomes (timeouts, 429, 5xx, transport errors) keep the model
// so a transient upstream problem never empties the catalog.
const (
	probeConcurrency = 8
	probeTimeout     = 5 * time.Second
	discoveryBudget  = 30 * time.Second
	verdictTTL       = 6 * time.Hour
	// Inconclusive probes keep the model and are retried only after this, so
	// every host client reload does not spend RPM re-probing slow models.
	inconclusiveTTL = 15 * time.Minute
)

type catalog struct {
	cache *verdictCache
	quota *quotaProvider
	// probes is shared by every discovery so overlapping ModelsForAuth calls
	// stay within probeConcurrency in total.
	probes chan struct{}
}

func newCatalog(q *quotaProvider) catalog {
	return catalog{cache: newVerdictCache(), quota: q, probes: make(chan struct{}, probeConcurrency)}
}

// Fallback IDs are published chat models, not an entitlement/free-tier claim.
// Wire/API references are recorded in docs/SOURCES.md. Fallbacks are validated
// like discovered IDs before being advertised for an auth.
func fallbackModels() []pluginapi.ModelInfo {
	ids := []string{"stepfun-ai/step-5-preview"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, modelInfo(id))
	}
	return out
}
func modelInfo(id string) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id}
}
func (catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (c catalog) ModelsForAuth(ctx context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	key, err := authKey(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	if r.HTTPClient == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	candidates, err := discover(ctx, r.HTTPClient, key)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	models, err := c.validate(ctx, r.HTTPClient, key, candidates)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return pluginapi.ModelResponse{Provider: provider, Models: models}, nil
}

// discover returns the upstream catalog, or the static fallback when the
// catalog is unavailable or unusable. Credential/rate errors are surfaced.
func discover(ctx context.Context, client pluginapi.HostHTTPClient, key string) ([]pluginapi.ModelInfo, error) {
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: selectedBase(upstreamBase) + "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return fallbackModels(), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 401, 402, 403, 429:
			return nil, classifyResponse(resp)
		}
		return fallbackModels(), nil
	}
	var wire struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if len(resp.Body) > 1<<20 || json.Unmarshal(resp.Body, &wire) != nil {
		return fallbackModels(), nil
	}
	var out []pluginapi.ModelInfo
	seen := make(map[string]bool)
	for _, m := range wire.Data {
		id := strings.TrimSpace(m.ID)
		lower := strings.ToLower(id)
		// This executor implements chat, not embedding/reranking APIs.
		if id == "" || strings.ContainsAny(id, " \r\n\t") || strings.Contains(lower, "embed") || strings.Contains(lower, "rerank") || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, modelInfo(id))
	}
	if len(out) == 0 {
		return fallbackModels(), nil
	}
	return out, nil
}

// validate drops candidates the gateway definitively rejects for this key and
// keeps catalog order. Each probe is a real one-token chat dispatch, so it is
// counted on the display-only quota card like any other request.
func (c catalog) validate(ctx context.Context, client pluginapi.HostHTTPClient, key string, candidates []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	budget, cancel := context.WithTimeout(ctx, discoveryBudget)
	defer cancel()
	cache := c.cache
	tag := cache.tag(key)
	alive := make([]bool, len(candidates))
	sem := c.probes
	if sem == nil {
		sem = make(chan struct{}, probeConcurrency)
	}
	var wg sync.WaitGroup
	for i, m := range candidates {
		id := strings.TrimPrefix(m.ID, modelPrefix)
		if v, ok := cache.get(tag, id); ok {
			alive[i] = v
			continue
		}
		alive[i] = true // unknown until proven otherwise
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-budget.Done():
				return
			}
			defer func() { <-sem }()
			c.quota.record(key)
			usable, definitive := probeModel(budget, client, key, id)
			alive[i] = usable
			if definitive {
				cache.put(tag, id, usable, verdictTTL)
			} else if budget.Err() == nil {
				cache.put(tag, id, true, inconclusiveTTL)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]pluginapi.ModelInfo, 0, len(candidates))
	for i, m := range candidates {
		if alive[i] {
			out = append(out, m)
		}
	}
	return out, nil
}

// probeModel sends a one-token streaming chat request and decides on the
// response status alone; the stream is cancelled as soon as headers arrive.
// 400/422 mean the model rejects a minimal chat request (not a chat model);
// 403/404/410 mean it is not usable by this key (not entitled, no function
// deployed for the account, end of life). 2xx other than 202 proves it is
// usable. Anything else (202 pending, 429, 5xx, timeouts) is inconclusive.
func probeModel(ctx context.Context, client pluginapi.HostHTTPClient, key, id string) (usable, definitive bool) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": id, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": 1, "stream": true})
	resp, err := client.DoStream(pctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: selectedBase(upstreamBase) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"}}, Body: body})
	if err != nil {
		return true, false
	}
	cancel() // stop the upstream generation; only the status was needed
	if resp.Chunks != nil {
		for range resp.Chunks {
		}
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return true, false // NVCF "pending": nothing was proven
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, true
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return false, true
	}
	return true, false
}

// verdictCache remembers per-key model verdicts (definitive for 6 h,
// inconclusive for 15 min) so hot reloads and repeated discovery do not
// re-probe the catalog. Entries are keyed by a process-seeded maphash of the
// key (not a stored credential and not a password hash): a changed key is a
// new entry, and expired entries are pruned on write.
type verdictCache struct {
	mu   sync.Mutex
	now  func() time.Time
	seed maphash.Seed
	m    map[uint64]map[string]verdict
}
type verdict struct {
	usable  bool
	expires time.Time
}

func newVerdictCache() *verdictCache {
	return &verdictCache{now: time.Now, seed: maphash.MakeSeed(), m: make(map[uint64]map[string]verdict)}
}
func (c *verdictCache) tag(key string) uint64 {
	if c == nil {
		return 0
	}
	return maphash.String(c.seed, key)
}
func (c *verdictCache) get(tag uint64, id string) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[tag][id]
	if !ok || !c.now().Before(v.expires) {
		return false, false
	}
	return v.usable, true
}
func (c *verdictCache) put(tag uint64, id string, usable bool, ttl time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for t, entries := range c.m {
		for model, v := range entries {
			if !now.Before(v.expires) {
				delete(entries, model)
			}
		}
		if len(entries) == 0 {
			delete(c.m, t)
		}
	}
	if c.m[tag] == nil {
		c.m[tag] = make(map[string]verdict)
	}
	c.m[tag][id] = verdict{usable: usable, expires: now.Add(ttl)}
}
