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
			io.WriteString(w, `{"data":[{"id":"ok/alive"},{"id":"meta/llama-3.2-11b-vision-instruct"},{"id":"old/retired"},{"id":"paid/forbidden"},{"id":"busy/rate-limited"},{"id":"flaky/server-error"},{"id":"slow/no-headers"},{"id":"nvidia/also-alive"}]}`)
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
	want := "nvidia/ok/alive,nvidia/busy/rate-limited,nvidia/flaky/server-error,nvidia/slow/no-headers,nvidia/nvidia/also-alive"
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
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != .92 {
		t.Fatalf("each probe is a real dispatch and must be counted: %+v %v", quota, err)
	}
	// Second discovery re-probes only inconclusive models.
	out, err = c.ModelsForAuth(context.Background(), req)
	if err != nil || ids(out.Models) != want {
		t.Fatalf("second discovery %v %v", ids(out.Models), err)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, wantProbes := range map[string]int{"ok/alive": 1, "meta/llama-3.2-11b-vision-instruct": 1, "old/retired": 1, "paid/forbidden": 1, "nvidia/also-alive": 1, "busy/rate-limited": 2, "flaky/server-error": 2, "slow/no-headers": 2} {
		if probes[id] != wantProbes {
			t.Errorf("%s probed %d times, want %d", id, probes[id], wantProbes)
		}
	}
}

func TestVerdictCacheExpires(t *testing.T) {
	c := newVerdictCache()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var digest [32]byte
	c.put(digest, "m", false)
	if usable, ok := c.get(digest, "m"); !ok || usable {
		t.Fatal("fresh verdict missing")
	}
	now = now.Add(verdictTTL)
	if _, ok := c.get(digest, "m"); ok {
		t.Fatal("expired verdict reused")
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
