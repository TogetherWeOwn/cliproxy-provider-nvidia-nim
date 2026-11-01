package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ownership is by filename, through the kit's KeyOwnership guard. Only
// digests are retained; neither duplicate-file errors nor RPM state retain keys.
// A plugin disable/enable clears filename ownership after renaming auth files.
// Host parses commit ownership; login-validation parses are provisional and
// only reserve the digest until the login session expires, so an abandoned
// login releases the key instead of holding it until plugin restart.
type authProvider struct {
	kit.APIKeyAuth
	guard kit.KeyOwnership
}

func newAuthProvider() (*authProvider, error) {
	return newAuthProviderWithLoginTTL(0)
}

// newAuthProviderWithLoginTTL overrides the login session TTL; zero keeps the
// kit default. It exists so tests can let a session lapse quickly.
func newAuthProviderWithLoginTTL(sessionTTL time.Duration) (*authProvider, error) {
	p := &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(fields map[string]any) error {
		if _, exists := fields["id"]; exists {
			return invalidAuth("auth identity is derived from the API key")
		}
		key, _ := fields["api_key"].(string)
		if err := validateKey(key); err != nil {
			return err
		}
		_, _, err := rpmCapacity(fields)
		return err
	}}}
	// No extra fields: the page prompts for the API key only, so the v0.4.0
	// Secret field flag and FilenameFields (identities spanning extra fields)
	// have nothing to attach to. Parse is the full ParseAuth pipeline
	// (nvapi- check, RPM validation, duplicate-key guard).
	login, err := kit.NewAPIKeyLogin(kit.APIKeyLoginConfig{Provider: provider, Parse: p.ParseAuth, SessionTTL: sessionTTL})
	if err != nil {
		return nil, err
	}
	p.APIKeyAuth.Login = login
	return p, nil
}

func invalidAuth(message string) *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_auth", Message: message, HTTPStatus: 400}
}

func validateKey(key string) error {
	if !strings.HasPrefix(key, "nvapi-") || len(key) <= len("nvapi-") || strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return invalidAuth("api_key must be an nvapi- key without embedded whitespace")
	}
	return nil
}

func authKey(storage []byte, metadata map[string]any) (string, error) {
	key, err := kit.APIKey(storage, metadata)
	if err != nil {
		return "", err
	}
	if err = validateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// defaultRequestsPerMinute is the build.nvidia.com free-tier rate shown to
// developer accounts and reported by users (40 RPM). NVIDIA's official FAQ
// publishes no number: it says the rate varies per model and concurrent load and
// points to the signed-in account page. It is therefore only a display default,
// labelled as such on the card; requests_per_minute overrides it per auth file.
const defaultRequestsPerMinute = 40

// rpmCapacity returns the per-auth capacity and whether it was configured.
func rpmCapacity(fields map[string]any) (int, bool, error) {
	value, ok := fields["requests_per_minute"]
	if !ok {
		return defaultRequestsPerMinute, false, nil
	}
	var number float64
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false, invalidAuth("requests_per_minute must be an integer between 1 and 100000")
		}
		number = float64(n)
	case float64:
		number = v
	case int:
		number = float64(v)
	default:
		return 0, false, invalidAuth("requests_per_minute must be an integer between 1 and 100000")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 1 || number > 100000 || number != math.Trunc(number) {
		return 0, false, invalidAuth("requests_per_minute must be an integer between 1 and 100000")
	}
	return int(number), true, nil
}

func (p *authProvider) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	out, err := p.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := authKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := sha256.Sum256([]byte(key))
	if err := p.guard.Claim(ctx, r.FileName, digest); err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	return out, nil
}
