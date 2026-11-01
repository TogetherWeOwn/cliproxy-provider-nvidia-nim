package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthIdentityDuplicatesAndRPMValidation(t *testing.T) {
	p := newAuthProvider()
	parse := func(file, raw string) (pluginapi.AuthParseResponse, error) {
		return p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: "nvidia-nim", FileName: file, RawJSON: []byte(raw)})
	}
	a, err := parse("first.json", `{"type":"nvidia-nim","api_key":"nvapi-example-key","requests_per_minute":12,"weight":2}`)
	if err != nil || !a.Handled || a.Auth.ID == "" {
		t.Fatalf("auth %+v %v", a, err)
	}
	b, err := parse("first.json", `{"type":"nvidia-nim","api_key":" nvapi-example-key "}`)
	if err != nil || b.Auth.ID != a.Auth.ID {
		t.Fatalf("reparse %+v %v", b, err)
	}
	if _, err = parse("duplicate.json", `{"type":"nvidia-nim","api_key":"nvapi-example-key"}`); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err = parse("first.json", `{"type":"nvidia-nim","api_key":"nvapi-replacement"}`); err != nil {
		t.Fatal(err)
	}
	if _, err = parse("second.json", `{"type":"nvidia-nim","api_key":"nvapi-example-key"}`); err != nil {
		t.Fatal("old ownership not released", err)
	}
	for _, raw := range []string{
		`{"type":"nvidia-nim","api_key":""}`,
		`{"type":"nvidia-nim","api_key":"not-a-nvidia-key"}`,
		`{"type":"nvidia-nim","api_key":"nvapi-bad\r\nheader"}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","id":"custom"}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","requests_per_minute":0}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","requests_per_minute":-1}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","requests_per_minute":1.5}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","requests_per_minute":"40"}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","requests_per_minute":100001}`,
		`{"type":"nvidia-nim","api_key":"nvapi-example","api_key":"nvapi-other"}`,
	} {
		if _, err = parse("invalid.json", raw); err == nil {
			t.Fatalf("accepted invalid auth %s", raw)
		}
	}
	other, err := parse("other.json", `{"type":"other","api_key":"example"}`)
	if err != nil || other.Handled {
		t.Fatalf("foreign auth %+v %v", other, err)
	}
	if _, err := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("API key login offered")
	}
	if _, err := p.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{}); err == nil {
		t.Fatal("API key polling offered")
	}
	refreshed, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: a.Auth.ID, StorageJSON: a.Auth.StorageJSON, Metadata: a.Auth.Metadata})
	if err != nil || string(refreshed.Auth.StorageJSON) != string(a.Auth.StorageJSON) {
		t.Fatalf("refresh %+v %v", refreshed, err)
	}
}

func TestBuildRequestStripsExactlyOneNamespace(t *testing.T) {
	for _, stream := range []bool{false, true} {
		got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Stream: stream, Payload: []byte(`{"model":"nvidia/llama-3.3-nemotron-super-49b-v1.5","messages":[],"temperature":0.25,"custom":{"preserve":true},"stream":true}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if json.Unmarshal(got.Body, &body) != nil {
			t.Fatal("invalid body")
		}
		if got.URL != "https://integrate.api.nvidia.com/v1/chat/completions" || got.Method != "POST" || got.Headers.Get("Authorization") != "Bearer nvapi-example" {
			t.Fatalf("bad request %+v", got)
		}
		if body["model"] != "nvidia/llama-3.3-nemotron-super-49b-v1.5" || body["stream"] != stream || body["temperature"] != 0.25 || body["custom"].(map[string]any)["preserve"] != true {
			t.Fatalf("payload changed %s", got.Body)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"model":"meta/llama"}`, `{"model":"nvidia/"}`, `{"model":4}`} {
		if _, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(raw), StorageJSON: []byte(`{"api_key":"nvapi-example"}`)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPrepareHTTPIgnoresCallerDestinationsAndCredentials(t *testing.T) {
	got, err := prepareHTTP(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/private", Headers: http.Header{"Authorization": {"Bearer attacker"}}, Body: []byte(`{"model":"nvidia/meta/llama-3.1-8b-instruct","stream":true}`), StorageJSON: []byte(`{"api_key":"nvapi-example"}`)})
	if err != nil || got.Method != "POST" || got.URL != "https://integrate.api.nvidia.com/v1/chat/completions" || got.Headers.Get("Authorization") != "Bearer nvapi-example" {
		t.Fatalf("unsafe request %+v %v", got, err)
	}
	var body map[string]any
	if json.Unmarshal(got.Body, &body) != nil || body["stream"] != true || body["model"] != "meta/llama-3.1-8b-instruct" {
		t.Fatalf("bad raw request %s", got.Body)
	}
}

func TestStaticModelsAreNamespacedAndRegistrarMatches(t *testing.T) {
	c := newCatalog(nil)
	out, err := c.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || len(out.Models) == 0 {
		t.Fatalf("static models %+v %v", out, err)
	}
	reg, err := c.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || len(reg.Models) != len(out.Models) {
		t.Fatalf("registrar %+v %v", reg, err)
	}
	for _, m := range out.Models {
		if len(m.ID) <= 7 || m.ID[:7] != "nvidia/" {
			t.Fatalf("unprefixed model %q", m.ID)
		}
		// Fallbacks must not include IDs the hosted gateway reports as retired
		// (410 end of life) or undeployed (404) as of the last live check.
		for _, retired := range []string{"nvidia/meta/llama-3.1-8b-instruct", "nvidia/llama-3.3-nemotron-super-49b-v1.5"} {
			if m.ID == retired {
				t.Fatalf("fallback advertises retired model %q", m.ID)
			}
		}
	}
}
