package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The hosted catalog lists IDs the account cannot use. Live evidence (2026-10-31):
// GET /v1/models listed meta/llama-3.2-11b-vision-instruct and the chat endpoint
// answered 404 "Function ... not found"; retired models answer 410 "end of life".
// Only definitively rejected models may be dropped; inconclusive probes keep the
// model, order is preserved and definitive verdicts are cached per key.
func TestModelsForAuthAdvertisesOnlyModelsTheGatewayAccepts(t *testing.T) {
	var mu sync.Mutex
	probes := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"ok/alive"},{"id":"meta/llama-3.2-11b-vision-instruct"},{"id":"old/retired"},{"id":"paid/forbidden"},{"id":"nvidia/nemotron-parse"},{"id":"strict/validation"},{"id":"queued/pending"},{"id":"busy/rate-limited"},{"id":"flaky/server-error"},{"id":"slow/no-headers"},{"id":"nvidia/also-alive"}]}`)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.Unmarshal(raw, &body) != nil || body["stream"] != true || body["max_tokens"] != float64(1) || r.Header.Get("Authorization") != "Bearer nvapi-example" {
			t.Errorf("bad probe %s %s %v", r.Method, r.URL.Path, body)
		}
		id, _ := body["model"].(string)
		mu.Lock()
		probes[id]++
		mu.Unlock()
		switch id {
		case "meta/llama-3.2-11b-vision-instruct":
			w.WriteHeader(http.StatusNotFound)
		case "old/retired":
			w.WriteHeader(http.StatusGone)
		case "paid/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "nvidia/nemotron-parse": // live: document parser rejects a chat request with 400
			w.WriteHeader(http.StatusBadRequest)
		case "strict/validation":
			w.WriteHeader(http.StatusUnprocessableEntity)
		case "queued/pending": // NVCF 202: inconclusive, kept and re-probed later
			w.WriteHeader(http.StatusAccepted)
		case "busy/rate-limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "flaky/server-error":
			w.WriteHeader(http.StatusInternalServerError)
		case "slow/no-headers":
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"p\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done() // probe must cancel instead of reading the generation
		}
	}))
	defer server.Close()
	q := newQuotaProvider()
	c := newCatalog(q)
	req := pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}}
	want := "nvidia/ok/alive,nvidia/queued/pending,nvidia/busy/rate-limited,nvidia/flaky/server-error,nvidia/slow/no-headers,nvidia/also-alive"
	start := time.Now()
	out, err := c.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(out.Models); got != want {
		t.Fatalf("advertised %s\nwant       %s", got, want)
	}
	if elapsed := time.Since(start); elapsed > probeTimeout+3*time.Second {
		t.Fatalf("validation took %s; slow probes must be bounded", elapsed)
	}
	quota, err := q.FetchQuota(context.Background(), quotaRequest("nvapi-example", 100))
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != .89 {
		t.Fatalf("each probe is a real dispatch and must be counted: %+v %v", quota, err)
	}
	// A reload within the inconclusive TTL re-probes nothing.
	out, err = c.ModelsForAuth(context.Background(), req)
	if err != nil || ids(out.Models) != want {
		t.Fatalf("second discovery %v %v", ids(out.Models), err)
	}
	// After it, only inconclusive models are re-probed.
	later := time.Now().Add(inconclusiveTTL)
	c.cache.now = func() time.Time { return later }
	out, err = c.ModelsForAuth(context.Background(), req)
	if err != nil || ids(out.Models) != want {
		t.Fatalf("third discovery %v %v", ids(out.Models), err)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, wantProbes := range map[string]int{"ok/alive": 1, "meta/llama-3.2-11b-vision-instruct": 1, "old/retired": 1, "paid/forbidden": 1, "nvidia/nemotron-parse": 1, "strict/validation": 1, "queued/pending": 2, "nvidia/also-alive": 1, "busy/rate-limited": 2, "flaky/server-error": 2, "slow/no-headers": 2} {
		if probes[id] != wantProbes {
			t.Errorf("%s probed %d times, want %d", id, probes[id], wantProbes)
		}
	}
}

func TestVerdictCacheExpires(t *testing.T) {
	c := newVerdictCache()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.put(1, "m", false, verdictTTL)
	if usable, ok := c.get(1, "m"); !ok || usable {
		t.Fatal("fresh verdict missing")
	}
	now = now.Add(verdictTTL)
	if _, ok := c.get(1, "m"); ok {
		t.Fatal("expired verdict reused")
	}
	c.put(2, "other", true, verdictTTL) // writes prune expired entries
	if _, kept := c.m[1]; kept {
		t.Fatal("expired verdicts not pruned")
	}
	if c.tag("nvapi-a") == c.tag("nvapi-b") || c.tag("nvapi-a") != c.tag("nvapi-a") {
		t.Fatal("cache tag must separate keys and be stable")
	}
}

func TestModelsForAuthHonoursCancellationDuringValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"slow/a"},{"id":"slow/b"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body) // lets the server observe the client hanging up
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := newCatalog(nil).ModelsForAuth(ctx, pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}})
	if err == nil {
		t.Fatal("cancelled discovery returned a catalog")
	}
}

func ids(models []pluginapi.ModelInfo) string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return strings.Join(out, ",")
}

// Overlapping discoveries share one probe limit.
func TestConcurrentDiscoveriesShareTheProbeLimit(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var catalogIDs []string
	for i := 0; i < 3*probeConcurrency; i++ {
		catalogIDs = append(catalogIDs, `{"id":"org/m`+string(rune('a'+i))+`"}`)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[`+strings.Join(catalogIDs, ",")+`]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	c := newCatalog(nil)
	var wg sync.WaitGroup
	for _, key := range []string{"nvapi-one", "nvapi-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := c.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"` + key + `"}`), HTTPClient: mockHostClient{server}})
			if err != nil || len(out.Models) != len(catalogIDs) {
				t.Errorf("discovery %d %v", len(out.Models), err)
			}
		}()
	}
	wg.Wait()
	if peak > probeConcurrency {
		t.Fatalf("peak concurrent probes %d > %d", peak, probeConcurrency)
	}
}
