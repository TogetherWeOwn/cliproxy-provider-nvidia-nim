package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Substitute only transport: no test can connect to NVIDIA. Streaming uses
// actual chunk boundaries and cancellation from a loopback httptest server.
type mockHostClient struct{ server *httptest.Server }

func (c mockHostClient) request(ctx context.Context, r pluginapi.HTTPRequest) (*http.Response, error) {
	if !strings.HasPrefix(r.URL, "https://integrate.api.nvidia.com/") {
		return nil, fmt.Errorf("unexpected upstream")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, c.server.URL+strings.TrimPrefix(r.URL, "https://integrate.api.nvidia.com"), bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Headers.Clone()
	return c.server.Client().Do(req)
}
func (c mockHostClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
}
func (c mockHostClient) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	out := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		for {
			buf := make([]byte, 7)
			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case out <- pluginapi.HTTPStreamChunk{Payload: buf[:n]}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case out <- pluginapi.HTTPStreamChunk{Err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Chunks: out}, nil
}

func TestExecutorCountsEachDispatchedRequestAndIgnoresInvalidLocalCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer nvapi-example" {
			t.Error("incorrect inference wire request")
		}
		var body map[string]any
		// Non-streaming Execute must still stream upstream (see executor.go).
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "nvidia/llama-3.3-nemotron-super-49b-v1.5" || body["stream"] != true {
			t.Error("model namespace or mode not rewritten")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	q := newQuotaProvider()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	e := newExecutor(q)
	req := pluginapi.ExecutorRequest{AuthID: "host-id", Payload: []byte(`{"model":"nvidia/llama-3.3-nemotron-super-49b-v1.5","stream":true}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}}
	// Going over configured capacity never gates native selection/execution.
	for i := 0; i < 5; i++ {
		out, err := e.Execute(context.Background(), req)
		if err != nil || !strings.Contains(string(out.Payload), `"content":"hello"`) {
			t.Fatalf("execute %s %v", out.Payload, err)
		}
	}
	requireQuota(t, q, quotaRequest("nvapi-example", 4), 0, "2030-01-01T00:01:00Z")
	if _, err := e.CountTokens(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Payload = []byte(`{"model":"not-prefixed"}`)
	if _, err := e.Execute(context.Background(), req); err == nil {
		t.Fatal("invalid model accepted")
	}
	req.Payload = []byte(`{"model":"nvidia/meta/llama-3.1-8b-instruct"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Execute(ctx, req); err == nil {
		t.Fatal("ignored cancellation")
	}
	req.HTTPClient = nil
	if _, err := e.Execute(context.Background(), req); err == nil {
		t.Fatal("missing host transport accepted")
	}
	requireQuota(t, q, quotaRequest("nvapi-example", 4), 0, "2030-01-01T00:01:00Z")
}

func TestStreamingIsIncrementalAndOneAttempt(t *testing.T) {
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream"] != true || body["model"] != "meta/llama-3.1-8b-instruct" {
			t.Error("bad streaming body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := newQuotaProvider()
	e := newExecutor(q)
	stream, err := e.ExecuteStream(ctx, pluginapi.ExecutorRequest{Payload: []byte(`{"model":"nvidia/meta/llama-3.1-8b-instruct","stream":false}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}})
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	select {
	case c := <-stream.Chunks:
		if c.Err != nil || len(c.Payload) == 0 {
			t.Fatalf("first chunk %+v", c)
		}
		all.Write(c.Payload)
		close(gate)
	case <-ctx.Done():
		t.Fatal("stream buffered until completion")
	}
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		all.Write(c.Payload)
	}
	// The kit re-frames to one data event per chunk and drops the SSE
	// [DONE] marker (the host appends its own terminator): the stream ends
	// by channel close. No request_path metadata means framed chunks.
	if want := `data: {"choices":[{"delta":{"content":"hello"}}]}`; all.String() != want {
		t.Fatalf("stream %q want %q", all.String(), want)
	}
	out, err := q.FetchQuota(ctx, quotaRequest("nvapi-example", 4))
	if err != nil || out.Groups[0].Buckets[0].RemainingFraction != .75 {
		t.Fatalf("stream count %+v %v", out, err)
	}
}

func TestExecutorErrorClassificationCountsFailedAttempts(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"sensitive-upstream-body"}`)
			}))
			defer server.Close()
			q := newQuotaProvider()
			e := newExecutor(q)
			r := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"nvidia/meta/llama-3.1-8b-instruct"}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}}
			_, err := e.Execute(context.Background(), r)
			var a *pluginabi.Error
			if !errors.As(err, &a) || a.HTTPStatus != status || strings.Contains(a.Message, "sensitive") {
				t.Fatalf("nonstream error %v", err)
			}
			_, err = e.ExecuteStream(context.Background(), r)
			var b *pluginabi.Error
			if !errors.As(err, &b) || *a != *b {
				t.Fatalf("inconsistent stream error %v vs %v", a, err)
			}
			_, err = e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/ignored", Body: r.Payload, StorageJSON: r.StorageJSON, HTTPClient: r.HTTPClient})
			if !errors.As(err, &b) || *a != *b {
				t.Fatalf("inconsistent raw HTTP error %v", err)
			}
			out, err := q.FetchQuota(context.Background(), quotaRequest("nvapi-example", 4))
			if err != nil || out.Groups[0].Buckets[0].RemainingFraction != .25 {
				t.Fatalf("failed attempts %+v %v", out, err)
			}
		})
	}
}

func TestDiscoveryPrefixesDeduplicatesAndKeepsUpstreamNvidiaNamespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" { // validation probe: every model usable
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer nvapi-example" {
			t.Error("bad discovery wire request")
		}
		io.WriteString(w, `{"data":[{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/llama-3.3-nemotron-super-49b-v1.5"},{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/nv-embedqa-e5-v5"},{"id":"nvidia/nv-rerankqa-mistral-4b-v3"},{"id":""}]}`)
	}))
	defer server.Close()
	out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 2 {
		t.Fatalf("discovery %+v %v", out, err)
	}
	if out.Models[0].ID != "nvidia/meta/llama-3.1-8b-instruct" || out.Models[1].ID != "nvidia/llama-3.3-nemotron-super-49b-v1.5" {
		t.Fatalf("namespaces %+v", out.Models)
	}
}

func TestDiscoveryFallbackForUnavailableOrInvalidCatalogButNotAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		wantErr bool
	}{
		{503, `{}`, false}, {404, `{}`, false}, {200, `not-json`, false}, {200, `{"data":[]}`, false}, {200, `{"data":null}`, false}, {401, `{}`, true}, {403, `{}`, true}, {429, `{}`, true},
	} {
		t.Run(fmt.Sprint(tc.status)+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/chat/completions" { // fallback IDs validate as usable
					io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}})
			if (err != nil) != tc.wantErr || !tc.wantErr && len(out.Models) == 0 {
				t.Fatalf("fallback %+v %v", out, err)
			}
		})
	}
}
