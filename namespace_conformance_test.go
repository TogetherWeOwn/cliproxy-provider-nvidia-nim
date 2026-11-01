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
// once: static fallback, registrar, and a discovery refresh — including a
// first-party "nvidia/..." upstream id, which passes through undoubled.
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
		io.WriteString(w, `{"data":[{"id":"openai/gpt-oss-20b"},{"id":"meta/llama-3.1-8b-instruct"},{"id":"nvidia/llama-3.3-nemotron-super-49b-v1.5"}]}`)
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

// serveID prefixes once, except ids already carrying the namespace (including
// NVIDIA-owned ids); upstreamID inverts it, and doubled ids never round-trip.
func TestServeUpstreamIDRoundTrip(t *testing.T) {
	for upstream, served := range map[string]string{
		"meta/llama-3.1-8b-instruct":               "nvidia/meta/llama-3.1-8b-instruct",
		"stepfun-ai/step-5-preview":                "nvidia/stepfun-ai/step-5-preview",
		"nvidia/llama-3.3-nemotron-super-49b-v1.5": "nvidia/llama-3.3-nemotron-super-49b-v1.5",
	} {
		if got := serveID(upstream); got != served {
			t.Fatalf("serveID(%q) = %q, want %q", upstream, got, served)
		}
		if got, ok := upstreamID(served); !ok || got != upstream {
			t.Fatalf("upstreamID(%q) = %q, %v; want %q, true", served, got, ok, upstream)
		}
	}
	for _, bad := range []string{"", "nvidia/", "nvidia", "meta/llama", "nvidia/nvidia/llama-3.3-nemotron-super-49b-v1.5"} {
		if got, ok := upstreamID(bad); ok {
			t.Fatalf("upstreamID(%q) = %q, true; want rejected", bad, got)
		}
	}
}

// Regression (Devin Review on TWO-683): a slashless third-party upstream id
// is ambiguous under the serveID heuristic ("example-tiny" and
// "nvidia/example-tiny" both serve as "nvidia/example-tiny"), so inversion
// consults the registration ground truth first and the probe and dispatch
// send the exact upstream id.
func TestUpstreamIDPrefersRegisteredMapping(t *testing.T) {
	if got := serveID("example-tiny"); got != "nvidia/example-tiny" {
		t.Fatalf("serveID = %q, want %q", got, "nvidia/example-tiny")
	}
	if got, ok := upstreamID("nvidia/example-tiny"); !ok || got != "example-tiny" {
		t.Fatalf("upstreamID = %q, %v; want %q, true", got, ok, "example-tiny")
	}
}
