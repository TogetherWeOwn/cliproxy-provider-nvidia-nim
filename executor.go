package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type executor struct {
	kit.Passthrough
	quota *quotaProvider
}

func newExecutor(q *quotaProvider) executor {
	return executor{Passthrough: kit.Passthrough{Provider: provider, BuildRequest: buildRequest, PrepareHTTP: prepareHTTP, ClassifyError: classifyResponse}, quota: q}
}

// Count immediately before dispatch, NOT on completion or UsagePlugin events:
// failed attempts and streams count once each, independently of notifications.
// The host remains solely responsible for admission, selection and cooldown.
type observedClient struct {
	pluginapi.HostHTTPClient
	quota *quotaProvider
}

func (c observedClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	c.quota.record(strings.TrimPrefix(r.Headers.Get("Authorization"), "Bearer "))
	return c.HostHTTPClient.Do(ctx, r)
}
func (c observedClient) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	c.quota.record(strings.TrimPrefix(r.Headers.Get("Authorization"), "Bearer "))
	return c.HostHTTPClient.DoStream(ctx, r)
}
func (e executor) client(c pluginapi.HostHTTPClient) pluginapi.HostHTTPClient {
	if c == nil {
		return nil
	}
	return observedClient{HostHTTPClient: c, quota: e.quota}
}

// Execute serves non-streaming clients from an upstream STREAMING request.
// Hosted NIM routes stream=false through NVCF's queued invocation path: it holds
// the connection up to NVCF-POLL-SECONDS (default 300 s) and then answers 504
// (or 202 pending) with an EMPTY body, while the same model streams within
// seconds. Streaming upstream and aggregating the SSE into one chat.completion
// keeps non-streaming clients on the fast path and never relays an empty or
// non-JSON 2xx body. One upstream attempt is still counted exactly once.
func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Payload = requestStreamUsage(r.Payload)
	stream, err := e.ExecuteStream(streamCtx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	var payload []byte
	if isJSONResponse(stream.Headers) {
		// Some deployments ignore stream=true and answer one JSON document.
		payload, err = collectJSON(streamCtx, stream.Chunks)
	} else {
		payload, err = aggregateSSE(streamCtx, stream.Chunks)
	}
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	headers := stream.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	for _, h := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		headers.Del(h)
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
}

// requestStreamUsage asks for the usage chunk a non-streaming reply needs.
// Some hosted models (live: nvidia/nemotron-3.5-lightning-30b-a3b) omit usage
// from a stream unless stream_options.include_usage is set. Caller-supplied
// stream_options are kept; an explicit include_usage is never overridden. An
// invalid payload is left for buildRequest to reject.
func requestStreamUsage(payload []byte) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil || body == nil {
		return payload
	}
	options := map[string]json.RawMessage{}
	if raw, ok := body["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &options) != nil || options == nil {
			return payload
		}
	}
	if _, set := options["include_usage"]; set {
		return payload
	}
	options["include_usage"] = json.RawMessage("true")
	body["stream_options"], _ = json.Marshal(options)
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

func isJSONResponse(h http.Header) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(h.Get("Content-Type"), ";")[0]))
	return ct == "application/json"
}

// collectJSON reads a bounded single JSON completion. An empty, non-JSON or
// choice-less 2xx body is an invalid upstream reply, never a success.
func collectJSON(ctx context.Context, chunks <-chan pluginapi.ExecutorStreamChunk) ([]byte, error) {
	raw, err := io.ReadAll(&chunkReader{ctx: ctx, chunks: chunks})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, invalidSSE()
	}
	// A 2xx JSON pending/error object is not a completion.
	var object struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(raw, &object) != nil || len(object.Choices) == 0 {
		return nil, invalidSSE()
	}
	return raw, nil
}
func (e executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	r.HTTPClient = e.client(r.HTTPClient)
	return e.Passthrough.ExecuteStream(ctx, r)
}
func (e executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	r.HTTPClient = e.client(r.HTTPClient)
	return e.Passthrough.HttpRequest(ctx, r)
}

func buildRequest(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
	key, err := authKey(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &body) != nil || body == nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_request", Message: "payload must be a JSON object", HTTPStatus: 400}
	}
	var served string
	if json.Unmarshal(body["model"], &served) != nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected an nvidia/ model", HTTPStatus: 400}
	}
	// Invert the catalog mapping: third-party ids lose one prefix,
	// NVIDIA-owned "nvidia/…" ids (which already carry it) pass through.
	model, ok := upstreamID(served)
	if !ok {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected an nvidia/ model", HTTPStatus: 400}
	}
	body["model"], _ = json.Marshal(model)
	body["stream"], _ = json.Marshal(r.Stream)
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: selectedBase(upstreamBase) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, Body: raw}, nil
}
func prepareHTTP(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
	var mode struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(r.Body, &mode) != nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_request", Message: "invalid chat JSON", HTTPStatus: 400}
	}
	// Raw HTTP execution cannot inject destinations, authorization or extra headers.
	return buildRequest(ctx, pluginapi.ExecutorRequest{Stream: mode.Stream, Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata})
}
