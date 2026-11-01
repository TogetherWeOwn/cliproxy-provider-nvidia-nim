package main

import (
	"context"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/nvidia-nim-id"
	testSubmitPath   = "/v0/management/plugin-login/nvidia-nim"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

func startTestLogin(t *testing.T) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	return startTestLoginTTL(t, 0)
}

func startTestLoginTTL(t *testing.T, sessionTTL time.Duration) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	ctx := context.Background()
	p, err := newAuthProviderWithLoginTTL(sessionTTL)
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
	// Kit v0.4.0+ returns a root-relative login URL: the panel resolves it
	// against its own origin, since the host's loopback callback base cannot
	// be loaded by a remote operator's browser.
	if !strings.HasPrefix(start.URL, testResourceBase+"/login?state=") {
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

// TestLoginAbandonedSubmitReleasesOwnership mirrors the kit regression for
// abandoned API-key logins (kit #13): the login-validation parse is
// provisional, so submitting a key and never polling must hold filename
// ownership only until the login session expires. After that, parsing the
// same key under a different filename succeeds instead of duplicate_auth.
func TestLoginAbandonedSubmitReleasesOwnership(t *testing.T) {
	ctx := context.Background()
	// The kit guard's clock is not injectable from outside the kit, so use a
	// short real session TTL and retry the upload parse until the
	// reservation lapses instead of sleeping a fixed duration.
	p, start := startTestLoginTTL(t, time.Second)
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"nvapi-examplekey"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	// Abandon the login: never poll it.
	upload := func() error {
		_, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "nvidia-nim",
			FileName: "upload.json",
			RawJSON:  []byte(`{"type":"nvidia-nim","api_key":"nvapi-examplekey"}`)})
		return err
	}
	// The in-flight login still blocks a different filename.
	if err := upload(); err == nil || kit.MapError(err).Code != "duplicate_auth" {
		t.Fatalf("upload during login = %v, want duplicate_auth", err)
	}
	// Once the abandoned session has expired the same upload owns the key.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := upload()
		if err == nil {
			break
		}
		if kit.MapError(err).Code != "duplicate_auth" {
			t.Fatalf("upload after abandon = %v, want success or duplicate_auth while reserved", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned login still owns the key after its session expired")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Submit-time duplicate feedback still works against the committed owner.
	second, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "nvidia-nim",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/nvidia-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err = p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + second.State + `","api_key":"nvapi-examplekey"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if submit.StatusCode != 400 || !strings.Contains(string(submit.Body), "duplicate_auth") {
		t.Fatalf("duplicate submit %+v", submit)
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
