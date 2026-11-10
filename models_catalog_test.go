package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Fixture tests for the kit ModelCatalog migration (v0.5.0), adapted to this
// plugin's fetch shape: a custom GET fetch with embed/rerank filtering, probe
// validation in the Validate hook, and the static fallback as the only
// configured list. No upstream NVIDIA endpoint is contacted.

type fixtureReply struct {
	status int
	body   string
}

type fixtureUpstream struct {
	mu            sync.Mutex
	defaultMode   fixtureReply
	byKey         map[string]fixtureReply
	blockModels   bool
	blockProbes   bool
	modelsStarted chan struct{}  // signalled when /v1/models starts blocking
	rejectProbe   map[string]int // probe model id → status (default 200)
}

func (f *fixtureUpstream) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultMode = fixtureReply{status, body}
}

func (f *fixtureUpstream) setKey(key string, reply fixtureReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byKey == nil {
		f.byKey = map[string]fixtureReply{}
	}
	f.byKey[key] = reply
}

func (f *fixtureUpstream) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/chat/completions" { // validation probe: usable
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		f.mu.Lock()
		status := f.rejectProbe[model]
		block := f.blockProbes
		f.mu.Unlock()
		if block {
			<-r.Context().Done()
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	f.mu.Lock()
	def, byKey, block, started := f.defaultMode, f.byKey, f.blockModels, f.modelsStarted
	f.mu.Unlock()
	if block {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return
	}
	reply := def
	if byKey != nil {
		if rk, ok := byKey[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
			reply = rk
		}
	}
	if reply.status != 200 {
		w.WriteHeader(reply.status)
	}
	io.WriteString(w, reply.body)
}

func fixtureServer(t *testing.T, f *fixtureUpstream) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(s.Close)
	return s
}

func catalogBody(ids ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%q}`, id)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

func fixtureCatalog(overrides map[string]kit.ModelOverride, ttl time.Duration) catalog {
	c := catalog{cache: newVerdictCache(), probes: make(chan struct{}, probeConcurrency)}
	cfg := c.modelCatalogConfig(overrides)
	if ttl > 0 {
		c.inner = kit.NewModelCatalogWithTTL(cfg, ttl)
	} else {
		c.inner = kit.NewModelCatalog(cfg)
	}
	return c
}

func fixtureRequest(server *httptest.Server, key string) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"` + key + `"}`), HTTPClient: mockHostClient{server}}
}

func fixtureServe(t *testing.T, c catalog, req pluginapi.AuthModelRequest) []pluginapi.ModelInfo {
	t.Helper()
	out, err := c.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal("ModelsForAuth:", err)
	}
	return out.Models
}

// Parse: the fetch keeps chat ids only, deduped and ordered, and overrides
// apply to both fresh and static models.
func TestCatalogFixtureParseFiltersCatalogAndAppliesOverrides(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("meta/llama-3.1-8b-instruct", "nvidia/nv-embedqa-e5-v5", "bad id", "", "meta/llama-3.1-8b-instruct", "org/rerank-x"))
	server := fixtureServer(t, up)
	c := fixtureCatalog(map[string]kit.ModelOverride{
		"meta/llama-3.1-8b-instruct": {DisplayName: "Llama 3.1 8B", ContextLength: 131072, MaxCompletionTokens: 8192},
		"stepfun-ai/step-5-preview":  {DisplayName: "Step 5 Preview"},
		"never/seen":                 {DisplayName: "Ignored"},
	}, 0)

	models := fixtureServe(t, c, fixtureRequest(server, "nvapi-example"))
	if len(models) != 1 || models[0].ID != "nvidia/meta/llama-3.1-8b-instruct" {
		t.Fatalf("parse served %+v, want only the usable chat id", models)
	}
	if m := models[0]; m.DisplayName != "Llama 3.1 8B" || m.ContextLength != 131072 || m.MaxCompletionTokens != 8192 {
		t.Fatalf("override not applied: %+v", m)
	}

	static, err := c.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || len(static.Models) != 1 {
		t.Fatalf("static %+v %v", static, err)
	}
	if m := static.Models[0]; m.ID != "nvidia/stepfun-ai/step-5-preview" || m.DisplayName != "Step 5 Preview" {
		t.Fatalf("override not applied to static: %+v", m)
	}
}

// Contributor rewrite: with the contributor entry present in the upstream
// catalog the plain id is dropped in its favor; without it the override's
// ContributorID is served as-is (an unknown upstream id, served once).
func TestCatalogFixtureContributorWithAndWithoutFixture(t *testing.T) {
	overrides := map[string]kit.ModelOverride{"zeta/main": {ContributorID: "zeta/main-contributor"}}

	t.Run("with fixture", func(t *testing.T) {
		up := &fixtureUpstream{}
		up.set(200, catalogBody("zeta/main", "zeta/main-contributor"))
		server := fixtureServer(t, up)
		models := fixtureServe(t, fixtureCatalog(overrides, 0), fixtureRequest(server, "nvapi-example"))
		if got := ids(models); got != "nvidia/zeta/main-contributor" {
			t.Fatalf("served %q, want the single contributor entry", got)
		}
	})

	t.Run("without fixture", func(t *testing.T) {
		up := &fixtureUpstream{}
		up.set(200, catalogBody("zeta/main"))
		server := fixtureServer(t, up)
		models := fixtureServe(t, fixtureCatalog(overrides, 0), fixtureRequest(server, "nvapi-example"))
		if got := ids(models); got != "nvidia/zeta/main-contributor" {
			t.Fatalf("served %q, want the rewritten contributor id", got)
		}
	})
}

// Executor mapping: a served id discovered through the catalog dispatches to
// exactly the upstream id that was probed, with one namespace stripped.
func TestCatalogFixtureExecutorMapping(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("meta/llama-3.1-8b-instruct"))
	server := fixtureServer(t, up)
	models := fixtureServe(t, fixtureCatalog(nil, 0), fixtureRequest(server, "nvapi-example"))
	if len(models) != 1 || models[0].ID != "nvidia/meta/llama-3.1-8b-instruct" {
		t.Fatalf("served %+v", models)
	}
	if got, ok := upstreamID(models[0].ID); !ok || got != "meta/llama-3.1-8b-instruct" {
		t.Fatalf("upstreamID(%q) = %q, %v; want the probed upstream id", models[0].ID, got, ok)
	}
	got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"` + models[0].ID + `"}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(got.Body, &body) != nil || body["model"] != "meta/llama-3.1-8b-instruct" {
		t.Fatalf("dispatched body %s, want exactly one namespace stripped", got.Body)
	}
}

// Timeline A: the catalog converges to a new upstream list on the very next
// call. TTL never gates a fetch, even a one-hour TTL.
func TestCatalogFixtureTimelineAFreshConverges(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("a/first"))
	server := fixtureServer(t, up)
	c := fixtureCatalog(nil, time.Hour)
	req := fixtureRequest(server, "nvapi-example")

	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("first convergence %q", got)
	}
	up.set(200, catalogBody("b/second"))
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/b/second" {
		t.Fatalf("second convergence %q, want the fresh list despite TTL", got)
	}
}

// Timeline B is DOCUMENTED, never claimed as a guarantee: kit v0.5.0 performs
// a single fetch per ModelsForAuth call with no bounded retry, so a failure
// right after a success only ever serves the fallback — recovery is observed
// solely when a later call's own fetch succeeds again.
func TestCatalogFixtureTimelineBFailureThenNoGuaranteedRecovery(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("a/first"))
	server := fixtureServer(t, up)
	c := fixtureCatalog(nil, 0)
	req := fixtureRequest(server, "nvapi-example")

	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("initial %q", got)
	}
	up.set(503, `{}`)
	// Failure after a success: last-good is served, without error.
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("after failure %q, want last-good fallback", got)
	}
	// Still failing: nothing self-heals between calls — the same fallback
	// repeats. There is no background retry to claim recovery from.
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("during outage %q, want last-good fallback", got)
	}
	up.set(200, catalogBody("b/second"))
	// Only a fresh successful invocation converges to the new catalog.
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/b/second" {
		t.Fatalf("after recovery %q, want the fresh list", got)
	}
}

// TTL freshness expiry never erases the durable last-good store.
func TestCatalogFixtureTTLExpiryKeepsLastGood(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("a/first"))
	server := fixtureServer(t, up)
	c := fixtureCatalog(nil, 10*time.Millisecond)
	req := fixtureRequest(server, "nvapi-example")

	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("initial %q", got)
	}
	time.Sleep(30 * time.Millisecond) // freshness marker long expired
	up.set(503, `{}`)
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/first" {
		t.Fatalf("after TTL expiry %q, want last-good", got)
	}
}

// Last-good is isolated per credential generation: a failing key cannot
// borrow another key's catalog, and a recovering key converges alone.
func TestCatalogFixtureCredentialGenerationIsolation(t *testing.T) {
	up := &fixtureUpstream{}
	up.setKey("nvapi-one", fixtureReply{200, catalogBody("a/model")})
	up.setKey("nvapi-two", fixtureReply{503, `{}`})
	server := fixtureServer(t, up)
	c := fixtureCatalog(nil, 0)
	reqOne := fixtureRequest(server, "nvapi-one")
	reqTwo := fixtureRequest(server, "nvapi-two")

	if got := ids(fixtureServe(t, c, reqOne)); got != "nvidia/a/model" {
		t.Fatalf("key one %q", got)
	}
	if got := ids(fixtureServe(t, c, reqTwo)); got != "nvidia/stepfun-ai/step-5-preview" {
		t.Fatalf("failing key two %q, want static (never key one's catalog)", got)
	}
	if got := ids(fixtureServe(t, c, reqOne)); got != "nvidia/a/model" {
		t.Fatalf("key one again %q", got)
	}
	up.setKey("nvapi-two", fixtureReply{200, catalogBody("b/model")})
	if got := ids(fixtureServe(t, c, reqTwo)); got != "nvidia/b/model" {
		t.Fatalf("recovered key two %q", got)
	}
	if got := ids(fixtureServe(t, c, reqOne)); got != "nvidia/a/model" {
		t.Fatalf("key one after key two recovered %q", got)
	}
}

// A catalog whose every candidate is definitively rejected is a failed
// fetch: the static list serves, the empty result is never cached as
// last-good, and a later healthy catalog still converges.
func TestCatalogFixtureAllRejectedUsesStatic(t *testing.T) {
	up := &fixtureUpstream{}
	up.set(200, catalogBody("old/retired", "paid/forbidden"))
	up.rejectProbe = map[string]int{"old/retired": 410, "paid/forbidden": 403}
	server := fixtureServer(t, up)
	c := fixtureCatalog(nil, 0)
	req := fixtureRequest(server, "nvapi-example")

	if got := ids(fixtureServe(t, c, req)); got != "nvidia/stepfun-ai/step-5-preview" {
		t.Fatalf("all-rejected served %q, want the static fallback", got)
	}
	// A subsequent fetch failure must still serve static: nothing usable was
	// ever cached as last-good.
	up.set(503, `{}`)
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/stepfun-ai/step-5-preview" {
		t.Fatalf("after failure %q, want the static fallback", got)
	}
	up.set(200, catalogBody("a/model"))
	if got := ids(fixtureServe(t, c, req)); got != "nvidia/a/model" {
		t.Fatalf("after recovery %q, want the fresh catalog", got)
	}
}

// Cancellation and enforceable limits: a pre-cancelled context errors; an
// explicit mid-flight cancellation and an oversize body fall back to the
// static list; a deadline keeps the unrejected candidates. Every case
// returns within its deadline.
func TestCatalogFixtureCancellationAndDeadline(t *testing.T) {
	t.Run("pre-cancelled errors", func(t *testing.T) {
		up := &fixtureUpstream{}
		up.set(200, catalogBody("a/model"))
		server := fixtureServer(t, up)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fixtureCatalog(nil, 0).ModelsForAuth(ctx, fixtureRequest(server, "nvapi-example")); err == nil {
			t.Fatal("pre-cancelled discovery returned a catalog")
		}
	})

	t.Run("mid-flight cancellation falls back", func(t *testing.T) {
		started := make(chan struct{})
		up := &fixtureUpstream{blockModels: true, modelsStarted: started}
		server := fixtureServer(t, up)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			cancel()
		}()
		start := time.Now()
		out, err := fixtureCatalog(nil, 0).ModelsForAuth(ctx, fixtureRequest(server, "nvapi-example"))
		if err != nil {
			t.Fatal("mid-flight cancellation must fall back, not error:", err)
		}
		if ids(out.Models) != "nvidia/stepfun-ai/step-5-preview" {
			t.Fatalf("served %q, want the static fallback", ids(out.Models))
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("cancellation took %s; the deadline must be enforceable", elapsed)
		}
	})

	t.Run("deadline keeps unrejected candidates", func(t *testing.T) {
		up := &fixtureUpstream{blockProbes: true}
		up.set(200, catalogBody("a/model"))
		server := fixtureServer(t, up)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		out, err := fixtureCatalog(nil, 0).ModelsForAuth(ctx, fixtureRequest(server, "nvapi-example"))
		if err != nil {
			t.Fatal("deadline must keep unrejected candidates, not error:", err)
		}
		if ids(out.Models) != "nvidia/a/model" {
			t.Fatalf("deadline served %q, want the unrejected candidate", ids(out.Models))
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("deadline took %s; the budget must be enforceable", elapsed)
		}
	})

	t.Run("oversize body falls back", func(t *testing.T) {
		up := &fixtureUpstream{}
		up.set(200, strings.Repeat("x", kit.MaxModelsBody+1))
		server := fixtureServer(t, up)
		if got := ids(fixtureServe(t, fixtureCatalog(nil, 0), fixtureRequest(server, "nvapi-example"))); got != "nvidia/stepfun-ai/step-5-preview" {
			t.Fatalf("oversize served %q, want the static fallback", got)
		}
	})
}
