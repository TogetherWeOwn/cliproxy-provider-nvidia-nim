package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func nonStreamRequest(server *httptest.Server) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Payload: []byte(`{"model":"nvidia/stepfun-ai/step-5-preview","stream":false,"messages":[{"role":"user","content":"hi"}]}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`), HTTPClient: mockHostClient{server}}
}

// Regression: hosted NIM holds stream=false invocations in NVCF's queue and then
// answers 504 with an empty body, while streaming the same model is fast. A
// non-streaming client must be served from an upstream stream, aggregated into
// one OpenAI chat.completion JSON document.
func TestNonStreamExecuteStreamsUpstreamAndAggregatesToJSON(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream"] != true || body["model"] != "stepfun-ai/step-5-preview" {
			t.Errorf("upstream must receive stream=true, got %v", body)
		}
		if body["stream"] == false {
			// Mimic NVCF: queued non-stream invocation times out with an empty body.
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"id":"c1","model":"stepfun-ai/step-5-preview","created":7,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "},"finish_reason":null}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"more"},"finish_reason":null}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		} {
			io.WriteString(w, "data: "+ev+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	q := newQuotaProvider()
	out, err := newExecutor(q).Execute(context.Background(), nonStreamRequest(server))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upstream attempts %d", calls)
	}
	if out.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", out.Headers.Get("Content-Type"))
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Payload, &got); err != nil {
		t.Fatalf("payload is not JSON: %q", out.Payload)
	}
	if got.Object != "chat.completion" || got.ID != "c1" || got.Model != "stepfun-ai/step-5-preview" || len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello" || got.Choices[0].Message.Reasoning != "think more" || got.Choices[0].Message.Role != "assistant" || got.Choices[0].Finish != "stop" || got.Usage.Total != 7 {
		t.Fatalf("aggregate %s", out.Payload)
	}
	quota, err := q.FetchQuota(context.Background(), quotaRequest("nvapi-example", 4))
	if err != nil || quota.Groups[0].Buckets[0].RemainingFraction != .75 {
		t.Fatalf("one attempt must be counted once: %+v %v", quota, err)
	}
}

func TestNonStreamExecuteAcceptsJSONWhenUpstreamIgnoresStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, `{"id":"j1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	out, err := newExecutor(newQuotaProvider()).Execute(context.Background(), nonStreamRequest(server))
	if err != nil || !strings.Contains(string(out.Payload), `"content":"ok"`) || out.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("json passthrough %s %v", out.Payload, err)
	}
}

// An empty, non-JSON or truncated 2xx body (including NVCF 202 "pending" with no
// body) must surface as a retryable 502, never as a successful empty completion.
func TestNonStreamExecuteRejectsEmptyOrIncompleteSuccessBodies(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		ct     string
		body   string
	}{
		"empty json 200":       {200, "application/json", ""},
		"non-json 200":         {200, "application/json", "<html>gateway</html>"},
		"202 pending":          {202, "application/json", ""},
		"json without choices": {200, "application/json", `{"status":"pending","id":"req-1"}`},
		"sse without done":     {200, "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"},
		"sse without choice":   {200, "text/event-stream", "data: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := newExecutor(newQuotaProvider()).Execute(context.Background(), nonStreamRequest(server))
			var e *pluginabi.Error
			if !errors.As(err, &e) || e.HTTPStatus != 502 || !e.Retryable {
				t.Fatalf("want retryable 502, got %v", err)
			}
		})
	}
}

func TestNonStreamExecuteClassifiesEmptyGatewayTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Nvcf-Status", "errored")
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()
	_, err := newExecutor(newQuotaProvider()).Execute(context.Background(), nonStreamRequest(server))
	var e *pluginabi.Error
	if !errors.As(err, &e) || e.HTTPStatus != 504 || !e.Retryable {
		t.Fatalf("504 classification %v", err)
	}
}

func TestNonStreamExecuteHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := newExecutor(newQuotaProvider()).Execute(ctx, nonStreamRequest(server))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation %v", err)
	}
}
