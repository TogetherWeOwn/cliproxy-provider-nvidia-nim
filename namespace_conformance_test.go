package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every id this plugin can emit carries the fixed "nvidia/" namespace exactly
// once: static fallback, registrar, and a discovery refresh. Upstream ids that
// already carry the namespace would double it, so they never reach the
// conformance set: the kit drops doubled ids at the registration boundary.
func TestModelNamespaceConformance(t *testing.T) {
	ctx := context.Background()
	var all []pluginapi.ModelInfo
	all = append(all, fallbackModels()...)

	c := newCatalog(newQuotaProvider())
	reg, err := c.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, reg.Models...)

	static, err := c.StaticModels(ctx, pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, static.Models...)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"openai/gpt-oss-20b"},{"id":"meta/llama-3.1-8b-instruct"}]}`)
	}))
	defer server.Close()
	discovered, err := discover(ctx, mockHostClient{server}, "nvapi-example")
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, discovered...)

	if len(all) == 0 {
		t.Fatal("no models to check")
	}
	kit.AssertModelNamespace(t, "nvidia", all)
}
