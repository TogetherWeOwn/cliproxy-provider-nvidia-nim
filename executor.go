package main

import (
	"context"
	"encoding/json"
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
func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	r.Stream = false
	r.HTTPClient = e.client(r.HTTPClient)
	return e.Passthrough.Execute(ctx, r)
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
	var model string
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) || len(model) == len(modelPrefix) {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected an nvidia/ model", HTTPStatus: 400}
	}
	// Strip exactly ONE provider prefix. NVIDIA-owned upstream IDs themselves
	// begin nvidia/, so nvidia/nvidia/foo must become nvidia/foo, never foo.
	body["model"], _ = json.Marshal(strings.TrimPrefix(model, modelPrefix))
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
