package main

import (
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Classification derives from internal/providers/nvidianim/usage.go in
// TogetherWeOwn/cpa-plugin-subscription-pool at
// ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b (MIT), but no sticky scheduler state
// or free-admission policy is ported. NVIDIA's chat reference documents 402 as
// insufficient credits and 429 as request rate limit exceeded. No raw body or
// credentials are copied into a public RPC error.
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	delay := kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now)
	e := &pluginabi.Error{Code: "upstream_error", Message: "NVIDIA NIM request failed", HTTPStatus: resp.StatusCode, RetryAfterSeconds: delay}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Code, e.Message, e.RetryAfterSeconds = "invalid_credentials", "NVIDIA NIM rejected the credential or its access", 0
	case http.StatusPaymentRequired:
		e.Code, e.Message, e.RetryAfterSeconds = "quota_exhausted", "NVIDIA NIM reported insufficient API credits", 3600
	case http.StatusNotFound:
		e.Code, e.Message, e.RetryAfterSeconds = "upstream_not_found", "NVIDIA NIM endpoint or model was not found", 0
	case http.StatusTooManyRequests:
		e.Code, e.Message, e.Retryable, e.RetryAfterSeconds = "rate_limited", "NVIDIA NIM request rate limit reached", true, 60
		body := strings.ToLower(string(resp.Body[:min(len(resp.Body), 16384)]))
		// Generic 'quota per minute' language is NOT evidence of credit exhaustion.
		for _, marker := range []string{"insufficient_credits", "insufficient credits", "credits exhausted", "credit exhausted", "insufficient_quota", "trial expired", "trial exhausted"} {
			if strings.Contains(body, marker) {
				e.Code, e.Message, e.Retryable, e.RetryAfterSeconds = "quota_exhausted", "NVIDIA NIM reported exhausted credits or trial access", false, 3600
				break
			}
		}
		if delay > 0 {
			e.RetryAfterSeconds = delay
		}
	default:
		e.Retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
	}
	return e
}
func classifyResponse(resp pluginapi.HTTPResponse) error { return classifyFailure(resp, time.Now()) }
