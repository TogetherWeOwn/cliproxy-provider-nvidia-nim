package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
)

type catalog struct {
	cache *verdictCache
	quota *quotaProvider
}

func newCatalog(q *quotaProvider) catalog { return catalog{cache: newVerdictCache(), quota: q} }

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
	digest := sha256.Sum256([]byte(key))
	alive := make([]bool, len(candidates))
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, m := range candidates {
		id := strings.TrimPrefix(m.ID, modelPrefix)
		if v, ok := c.cache.get(digest, id); ok {
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
			if definitive {
				alive[i] = usable
				c.cache.put(digest, id, usable)
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
// 403/404/410 mean the model is not usable by this key (not entitled, no
// function deployed for the account, end of life). 2xx proves it is usable.
// Anything else is inconclusive.
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
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, true
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return false, true
	}
	return true, false
}

// verdictCache remembers definitive per-key model verdicts so hot reloads and
// repeated discovery do not re-probe the whole catalog. Keys are SHA-256
// digests; no credential is retained.
type verdictCache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[[32]byte]map[string]verdict
}
type verdict struct {
	usable  bool
	expires time.Time
}

func newVerdictCache() *verdictCache {
	return &verdictCache{now: time.Now, m: make(map[[32]byte]map[string]verdict)}
}
func (c *verdictCache) get(digest [32]byte, id string) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[digest][id]
	if !ok || !c.now().Before(v.expires) {
		return false, false
	}
	return v.usable, true
}
func (c *verdictCache) put(digest [32]byte, id string, usable bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[digest] == nil {
		c.m[digest] = make(map[string]verdict)
	}
	c.m[digest][id] = verdict{usable: usable, expires: c.now().Add(verdictTTL)}
}
