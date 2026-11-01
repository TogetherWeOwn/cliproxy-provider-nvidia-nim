//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type models struct{}

func (models) RegisterClient(string, string, []*registry.ModelInfo) {}
func (models) UnregisterClient(string)                              {}
func (models) GetModelProviders(string) []string                    { return nil }

// Adapted from the Synthetic provider's reviewed real-loader harness. This is
// the ORIGINAL pinned v8.0.49 host, not a stub loader or locally patched fork.
// Only the provider's upstream HTTP service is an httptest mock.
func TestRealHostLoader(t *testing.T) {
	var inference atomic.Int32
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nvapi-example-key" && r.Header.Get("Authorization") != "Bearer nvapi-second-key" {
			t.Error("incorrect authorization")
		}
		switch r.URL.Path {
		case "/v1/models":
			if r.Method != "GET" {
				t.Error("incorrect catalog method")
			}
			io.WriteString(w, `{"data":[{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/llama-3.3-nemotron-super-49b-v1.5"}]}`)
		case "/v1/chat/completions":
			inference.Add(1)
			if r.Method != "POST" {
				t.Error("incorrect chat method")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid inference JSON")
				w.WriteHeader(400)
				return
			}
			if body["model"] != "meta/llama-3.1-8b-instruct" && body["model"] != "nvidia/llama-3.3-nemotron-super-49b-v1.5" {
				t.Errorf("provider prefix was not stripped exactly once: %v", body["model"])
			}
			if status, ok := body["fixture_status"].(float64); ok && !(body["fixture_first_only"] == true && r.Header.Get("Authorization") == "Bearer nvapi-second-key") {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(int(status))
				io.WriteString(w, `{"error":"fixture failure"}`)
				return
			}
			if body["stream"] != true {
				// Non-streaming Execute and catalog probes must always stream upstream.
				t.Error("upstream received stream=false")
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if body["fixture_gate"] == true {
				io.WriteString(w, "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			io.WriteString(w, "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib := filepath.Join(arch, "nvidia-nim"+ext)
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", lib, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build %v\n%s", err, out)
	}
	if out, err := exec.Command("nm", "-D", lib).CombinedOutput(); err != nil || !strings.Contains(string(out), " T cliproxy_plugin_init") {
		t.Fatalf("entry point %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_base: "+server.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"nvidia-nim": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = logging.WithResponseHeadersHolder(ctx)
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("nvidia-nim") || !host.PluginRegistered("nvidia-nim") || !host.HasAuthProvider("nvidia-nim") || !host.HasQuotaProvider("nvidia-nim") {
		t.Fatalf("registration %+v", host.RegisteredPlugins())
	}
	info := host.RegisteredPlugins()
	if len(info) != 1 || !info[0].SupportsQuota || info[0].Metadata.Version != "0.1.4" {
		t.Fatalf("metadata %+v", info)
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "nvidia-nim", FileName: "example.json", RawJSON: []byte(`{"type":"nvidia-nim","api_key":"nvapi-example-key","requests_per_minute":4,"label":"Example"}`)})
	if err != nil || !handled || auth == nil {
		t.Fatalf("auth %v %v", handled, err)
	}
	if _, _, err = host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "nvidia-nim", FileName: "duplicate.json", RawJSON: []byte(`{"type":"nvidia-nim","api_key":"nvapi-example-key"}`)}); err == nil {
		t.Fatal("duplicate accepted")
	}
	file := filepath.Join(cfg.AuthDir, "example.json")
	if err = auth.Storage.SaveTokenToFile(file); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(saved, &fields) != nil || fields["type"] != "nvidia-nim" || fields["label"] != "Example" || fields["requests_per_minute"] != float64(4) {
		t.Fatal("auth storage merge failed")
	}
	quotaReq := pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: "nvidia-nim", StorageJSON: saved}
	quota, handled, err := host.FetchQuota(ctx, quotaReq)
	if err != nil || !handled || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].Window != "per-minute" || quota.Groups[0].Buckets[0].RemainingFraction != 1 || inference.Load() != 0 {
		t.Fatalf("initial local quota %+v %v", quota, err)
	}
	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	static := host.ModelsForProvider("nvidia-nim")
	if len(static) == 0 {
		t.Fatal("no static models")
	}
	for _, model := range static {
		if !strings.HasPrefix(model.ID, "nvidia/") {
			t.Fatalf("static namespace %q", model.ID)
		}
	}
	discovered := host.ModelsForAuth(ctx, auth)
	if !discovered.Handled || len(discovered.Models) != 2 || discovered.Models[1].ID != "nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5" {
		t.Fatalf("discovery %+v", discovered)
	}
	// Each discovered model is validated with one real one-token chat probe.
	// Probes dispatch upstream but do not count on the local RPM card.
	if inference.Load() != 2 {
		t.Fatalf("validation probes %d", inference.Load())
	}
	ex, ok := m["nvidia-nim"]
	if !ok {
		t.Fatal("executor not registered")
	}
	req := coreexec.Request{Model: "nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5", Payload: []byte(`{"model":"nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5","messages":[{"role":"user","content":"hello"}]}`), Format: translator.FormatOpenAI}
	opts := coreexec.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI}
	resp, err := ex.Execute(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(resp.Payload), `"content":"hello"`) || !strings.Contains(string(resp.Payload), `"object":"chat.completion"`) {
		t.Fatalf("execute %s %v", resp.Payload, err)
	}
	// 1 dispatch against a configured capacity of 4; the 2 validation
	// probes dispatch upstream but are excluded from the quota card.
	quota, _, err = host.FetchQuota(ctx, quotaReq)
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != .75 || quota.Groups[0].Buckets[0].ResetTime == "" {
		t.Fatalf("dispatch accounting %+v %v", quota, err)
	}
	req.Payload = []byte(`{"model":"nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5","stream":true,"fixture_gate":true}`)
	opts.Stream = true
	stream, err := ex.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var chunks strings.Builder
	select {
	case c := <-stream.Chunks:
		if c.Err != nil || len(c.Payload) == 0 {
			t.Fatalf("first chunk %+v", c)
		}
		chunks.Write(c.Payload)
		close(gate)
	case <-ctx.Done():
		t.Fatal("stream buffered until completion")
	}
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		chunks.Write(c.Payload)
	}
	if !strings.Contains(chunks.String(), "[DONE]") {
		t.Fatalf("stream %s", chunks.String())
	}
	// 2 counted dispatches (non-streaming execute + stream) of capacity 4;
	// validation probes are excluded from the card.
	quota, _, err = host.FetchQuota(ctx, quotaReq)
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != .5 {
		t.Fatalf("stream double-count %+v %v", quota, err)
	}
	opts.Stream = false
	for _, tc := range []struct {
		status int
		delay  time.Duration
	}{{401, 0}, {402, time.Hour}, {403, 0}, {404, 0}, {429, 7 * time.Second}, {503, 7 * time.Second}} {
		req.Payload, _ = json.Marshal(map[string]any{"model": "nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5", "fixture_status": tc.status})
		_, err = ex.Execute(ctx, auth, req, opts)
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != tc.status {
			t.Fatalf("status %d: %T %v", tc.status, err, err)
		}
		if tc.delay > 0 {
			retry, ok := err.(interface{ RetryAfter() *time.Duration })
			if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != tc.delay {
				t.Fatalf("retry %d: %v", tc.status, err)
			}
		}
	}
	t.Run("unconfigured capacity uses the labelled unofficial default", func(t *testing.T) {
		third, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "nvidia-nim", FileName: "default.json", RawJSON: []byte(`{"type":"nvidia-nim","api_key":"nvapi-default-key"}`)})
		if err != nil || !handled || third == nil {
			t.Fatalf("default auth %v", err)
		}
		before := inference.Load()
		card, handled, err := host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{AuthID: third.ID, Provider: "nvidia-nim", StorageJSON: []byte(`{"type":"nvidia-nim","api_key":"nvapi-default-key"}`)})
		if err != nil || !handled || len(card.Groups) != 1 || card.Groups[0].Buckets[0].RemainingFraction != 1 || !strings.Contains(card.Groups[0].Buckets[0].Description, "unofficial 40 RPM") || inference.Load() != before {
			t.Fatalf("default capacity card %+v %v", card, err)
		}
	})
	t.Run("native round-robin and per-auth rate cooldown", func(t *testing.T) {
		second, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "nvidia-nim", FileName: "second.json", RawJSON: []byte(`{"type":"nvidia-nim","api_key":"nvapi-second-key","requests_per_minute":4}`)})
		if err != nil || !handled || second == nil {
			t.Fatalf("second auth %v", err)
		}
		mgr := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
		mgr.SetRetryConfig(0, 0, 2)
		mgr.RegisterExecutor(ex)
		first := auth.Clone()
		first.ID = "first-auth"
		first.Status = coreauth.StatusActive
		second = second.Clone()
		second.ID = "second-auth"
		second.Status = coreauth.StatusActive
		model := "nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5"
		for _, a := range []*coreauth.Auth{first, second} {
			if _, err := mgr.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		}
		// Both direct and manager calls continue despite the first key's local RPM
		// card being at zero: no Scheduler or preemptive quota avoidance exists.
		call := coreexec.Request{Model: model, Payload: []byte(`{"model":"nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5","fixture_status":429,"fixture_first_only":true}`), Format: translator.FormatOpenAI}
		for i := 0; i < 2; i++ {
			if _, err := mgr.Execute(ctx, []string{"nvidia-nim"}, call, opts); err != nil {
				t.Fatalf("native sibling failover %v", err)
			}
		}
		failed, _ := mgr.GetByID(first.ID)
		healthy, _ := mgr.GetByID(second.ID)
		state := failed.ModelStates[model]
		if state == nil || !state.NextRetryAfter.After(time.Now()) || state.NextRetryAfter.After(time.Now().Add(15*time.Second)) {
			t.Fatalf("missing short failed-auth cooldown %+v", state)
		}
		if healthy.Unavailable || healthy.Quota.Exceeded {
			t.Fatalf("healthy sibling impaired %+v", healthy)
		}
		// v8.0.49 rpcError carries status/retry timing but drops Retryable and the
		// native transient-rate-limit marker. Characterize, do not patch/disguise.
		if !state.Quota.Exceeded {
			t.Fatal("pinned host changed its 429 quota marker; revisit documented limitation")
		}
		t.Log("v8.0.49 maps rate-only RPC 429 to model quota state, preserving short Retry-After")
	})
}

func TestRegistryUsesNativeStoreSchema(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := pluginstore.ParseRegistry(raw)
	if err != nil || reg.SchemaVersion != 1 || len(reg.Plugins) != 1 || reg.Plugins[0].ID != "nvidia-nim" || reg.Plugins[0].Version != "0.1.4" || pluginstore.PluginInstallType(reg.Plugins[0]) != "github-release" {
		t.Fatalf("registry %+v %v", reg, err)
	}
}
