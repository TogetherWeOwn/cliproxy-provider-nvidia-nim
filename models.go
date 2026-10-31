package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type catalog struct{}

// Fallback IDs are published chat models, not an entitlement/free-tier claim.
// Wire/API references are recorded in docs/SOURCES.md.
func fallbackModels() []pluginapi.ModelInfo {
	ids := []string{"meta/llama-3.1-8b-instruct", "nvidia/llama-3.3-nemotron-super-49b-v1.5", "stepfun-ai/step-5-preview"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}
func (catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) ModelsForAuth(ctx context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	key, err := authKey(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	if r.HTTPClient == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	fallback := pluginapi.ModelResponse{Provider: provider, Models: fallbackModels()}
	resp, err := r.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: selectedBase(upstreamBase) + "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if ctx.Err() != nil {
		return pluginapi.ModelResponse{}, ctx.Err()
	}
	if err != nil {
		return fallback, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 401, 402, 403, 429:
			return pluginapi.ModelResponse{}, classifyResponse(resp)
		}
		return fallback, nil
	}
	var wire struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if len(resp.Body) > 1<<20 || json.Unmarshal(resp.Body, &wire) != nil {
		return fallback, nil
	}
	out := pluginapi.ModelResponse{Provider: provider}
	seen := make(map[string]bool)
	for _, m := range wire.Data {
		id := strings.TrimSpace(m.ID)
		lower := strings.ToLower(id)
		// This executor implements chat, not embedding/reranking APIs. The catalog
		// does not prove chat/tool support or entitlement for every returned ID.
		if id == "" || strings.ContainsAny(id, " \r\n\t") || strings.Contains(lower, "embed") || strings.Contains(lower, "rerank") || seen[id] {
			continue
		}
		seen[id] = true
		out.Models = append(out.Models, pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	if len(out.Models) == 0 {
		return fallback, nil
	}
	return out, nil
}
