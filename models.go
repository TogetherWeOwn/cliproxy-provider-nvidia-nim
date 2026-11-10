package main

import (
	"context"
	"encoding/json"
	"errors"
	"hash/maphash"
	"net/http"
	"strings"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The hosted /v1/models list is a static catalog, not per-account availability:
// on 2026-10-31 it listed 85 IDs for one key, and a one-token chat probe of each
// returned 404 "function ... not found" for 60 of them; retired IDs answer 410.
// Model discovery therefore validates every candidate with the chat endpoint the
// executor uses and advertises only models the gateway does not definitively
// reject. Unknown outcomes (timeouts, 429, 5xx, transport errors) keep the model
// so a transient upstream problem never empties the catalog.
//
// Model serving goes through kit ModelCatalog (v0.5.0): every ModelsForAuth
// call attempts one fresh fetch, any failure falls back to that auth's durable
// last-good list and then to the static list, and only an already-cancelled
// context (or a failure with nothing to fall back to) surfaces an error. The
// kit performs a single fetch per call — it includes no bounded retry, so a
// failure followed by recovery is observed only when a later call fetches a
// healthy catalog again; Timeline-B recovery is never guaranteed.
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
	inner *kit.ModelCatalog
	cache *verdictCache
	quota *quotaProvider
	// probes is shared by every discovery so overlapping ModelsForAuth calls
	// stay within probeConcurrency in total.
	probes chan struct{}
}

// Catalog serving is delegated to a kit ModelCatalog: the catalog value
// carries the plugin's probe state (verdict cache, quota recorder, shared
// probe semaphore) in its Fetch/Validate hooks, and the kit owns the fresh
// fetch, last-good store and namespace pipeline. A zero-value catalog uses
// a process-wide default so the legacy (catalog{}) call shape keeps working.
var defaultCatalog = newCatalog(nil)

func newCatalog(q *quotaProvider) catalog {
	c := catalog{cache: newVerdictCache(), quota: q, probes: make(chan struct{}, probeConcurrency)}
	c.inner = kit.NewModelCatalog(c.modelCatalogConfig(nil))
	return c
}

// modelCatalogConfig builds the production kit configuration for this catalog
// value; fixture tests pass overrides so the Overrides plumbing is proven
// without shipping any.
func (c catalog) modelCatalogConfig(overrides map[string]kit.ModelOverride) kit.ModelCatalogConfig {
	return kit.ModelCatalogConfig{
		Namespace: modelNamespace,
		Static:    staticModels(),
		Fetch:     c.fetchModels,
		Validate:  c.validateModels,
		Overrides: overrides,
	}
}

func (c catalog) innerOrDefault() *kit.ModelCatalog {
	if c.inner != nil {
		return c.inner
	}
	return defaultCatalog.inner
}

// The kit echoes the plugin's display-name metadata as Provider, but the
// host keys registered models by that field: the provider id, not the display
// name, must be stamped back onto every response.
func (c catalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	resp, err := c.innerOrDefault().RegisterModels(ctx, req)
	resp.Provider = provider
	return resp, err
}
func (c catalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.innerOrDefault().StaticModels(ctx, req)
	resp.Provider = provider
	return resp, err
}
func (c catalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.innerOrDefault().ModelsForAuth(ctx, req)
	resp.Provider = provider
	return resp, err
}

// Fallback IDs are published chat models, not an entitlement/free-tier claim.
// Wire/API references are recorded in docs/SOURCES.md. Static is trusted as
// configured: like RegisterModels before the kit migration, the fallback
// path serves it without probe validation.
//
// staticModels is the kit Static list: upstream (pre-namespace) form, which
// the catalog pipeline serves prefixed.
func staticModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{{
		ID:          "stepfun-ai/step-5-preview",
		Object:      "model",
		OwnedBy:     provider,
		DisplayName: "stepfun-ai/step-5-preview",
	}}
}

// fallbackModels is the served (post-namespace) form of the static list,
// kept for callers that already hold served ids.
func fallbackModels() []pluginapi.ModelInfo {
	ids := []string{"stepfun-ai/step-5-preview"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, modelInfo(id))
	}
	return out
}
func modelInfo(id string) pluginapi.ModelInfo {
	served := serveID(id)
	return pluginapi.ModelInfo{ID: served, Object: "model", OwnedBy: provider, DisplayName: id}
}

// fetchModels is the kit Fetch hook: a single GET of the hosted catalog.
// The kit turns any error into a last-good/static fallback, so failures are
// reported, never masked here; Fetch itself never substitutes the fallback.
func (c catalog) fetchModels(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	key, err := authKey(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, err
	}
	if req.HTTPClient == nil {
		return nil, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	return fetchCatalog(ctx, req.HTTPClient, key)
}

// fetchCatalog returns the upstream catalog in upstream (pre-namespace)
// form. Credential/rate statuses map to classified errors; every other
// failure (transport, malformed or oversized body, empty catalog) is a
// fetch error. The kit owns the post-return parse-size bound via
// kit.MaxModelsBody, checked here because custom fetches must enforce it.
func fetchCatalog(ctx context.Context, client pluginapi.HostHTTPClient, key string) ([]pluginapi.ModelInfo, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: selectedBase(upstreamBase) + "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 401, 402, 403, 429:
			return nil, classifyResponse(resp)
		}
		return nil, rpcError("models_fetch_failed", "model discovery failed", resp.StatusCode)
	}
	if len(resp.Body) > kit.MaxModelsBody {
		return nil, rpcError("models_fetch_failed", "model discovery response exceeds the size limit", 502)
	}
	var wire struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(resp.Body, &wire) != nil {
		return nil, rpcError("models_fetch_failed", "model discovery returned a malformed catalog", 502)
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
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	if len(out) == 0 {
		return nil, rpcError("models_fetch_failed", "model discovery returned no usable models", 502)
	}
	return out, nil
}

// validateModels is the kit Validate hook: it probes every candidate against
// its own upstream id and drops only the definitively rejected ones, in
// catalog order. Unproven candidates are kept, never dropped — including
// when the validation budget or kit fetch deadline expires first (timeout
// is an unknown outcome, like 429/5xx). An explicit caller cancellation
// returns an error instead so the kit falls back; a catalog whose every
// candidate was definitively rejected is the same kind of failure, never an
// empty catalog to cache as last-good.
func (c catalog) validateModels(ctx context.Context, req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	key, err := authKey(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, err
	}
	if req.HTTPClient == nil {
		return nil, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	budget, cancel := context.WithTimeout(ctx, discoveryBudget)
	defer cancel()
	cache := c.cache
	tag := cache.tag(key)
	alive := make([]bool, len(models))
	sem := c.probes
	if sem == nil {
		sem = make(chan struct{}, probeConcurrency)
	}
	var wg sync.WaitGroup
	for i, m := range models {
		// Each candidate is probed against its own upstream id: two
		// upstream ids can share one served id, and inverting the served
		// id would probe both against the same first-registered upstream
		// id, hiding a usable model behind a rejected namesake.
		if v, ok := cache.get(tag, m.ID); ok {
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
			usable, definitive := probeModel(budget, req.HTTPClient, key, m.ID)
			alive[i] = usable
			if definitive {
				cache.put(tag, m.ID, usable, verdictTTL)
			} else if budget.Err() == nil {
				cache.put(tag, m.ID, true, inconclusiveTTL)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		return nil, err
	}
	// Colliding upstream ids share one served id, which can be advertised
	// only once. Keep the first surviving candidate in catalog order and
	// point dispatch at it, so the advertised id routes to a probed-usable
	// upstream id rather than to whichever namesake registered first.
	out := make([]pluginapi.ModelInfo, 0, len(models))
	emitted := make(map[string]bool)
	for i, m := range models {
		if !alive[i] {
			continue
		}
		served := m.ID
		if !strings.HasPrefix(served, modelPrefix) {
			served = modelPrefix + served
		}
		if emitted[served] {
			continue
		}
		emitted[served] = true
		assignMapping(m.ID, served)
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, rpcError("models_fetch_failed", "model discovery returned no usable models", 502)
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

func rpcError(code, message string, status int) error {
	return &pluginabi.Error{Code: code, Message: message, HTTPStatus: status}
}
