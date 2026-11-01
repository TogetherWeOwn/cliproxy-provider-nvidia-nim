package main

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/nvidia-nim-id"
	testSubmitPath   = "/v0/management/plugin-login/nvidia-nim"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

func startTestLogin(t *testing.T) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	ctx := context.Background()
	p, err := newAuthProvider()
	if err != nil {
		t.Fatal(err)
	}
	if p.Login == nil {
		t.Fatal("API-key login not wired")
	}
	reg, err := p.Login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase})
	if err != nil {
		t.Fatal(err)
	}
	var sawSubmit, sawPage bool
	for _, r := range reg.Routes {
		if r.Method == "POST" && r.Path == "plugin-login/nvidia-nim" {
			sawSubmit = true
		}
	}
	for _, r := range reg.Resources {
		if r.Path == "login" {
			sawPage = true
		}
	}
	if !sawSubmit || !sawPage {
		t.Fatalf("login routes not registered: %+v", reg)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "nvidia-nim",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/nvidia-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	return p, start
}

// TestAPIKeyLoginWired drives the management-UI login flow end to end through
// the provider's own ParseAuth pipeline: page, key submit, poll.
func TestAPIKeyLoginWired(t *testing.T) {
	ctx := context.Background()
	p, start := startTestLogin(t)
	// Root-relative on purpose: an absolute loopback URL would hand a remote
	// operator's browser a link it cannot load; the panel opens the relative
	// URL against its own origin (the host, which serves the panel).
	if !strings.HasPrefix(start.URL, testResourceBase+"/login?state=") || strings.Contains(start.URL, "://") {
		t.Fatalf("unexpected login URL %q", start.URL)
	}
	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}
	page, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET",
		Path: testResourceBase + "/login", Query: map[string][]string{"state": {start.State}}})
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("login page %+v %v", page, err)
	}
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"nvapi-examplekey"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v %v", done, err)
	}
	if done.Auth.Provider != "nvidia-nim" {
		t.Fatalf("unexpected provider: %+v", done.Auth)
	}
	// A second poll must not redeliver credentials.
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("consumed login redelivered: %+v %v", again, err)
	}
}

// TestAPIKeyLoginRejectsBadKey submits a key without the nvapi- prefix and
// expects the provider's own validation to reject it without echoing it.
func TestAPIKeyLoginRejectsBadKey(t *testing.T) {
	p, start := startTestLogin(t)
	submit, err := p.Login.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"badkey-example"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if submit.StatusCode != 400 {
		t.Fatalf("bad key accepted: %+v", submit)
	}
	if strings.Contains(string(submit.Body), "badkey-example") {
		t.Fatalf("rejection echoed the key: %s", submit.Body)
	}
}
