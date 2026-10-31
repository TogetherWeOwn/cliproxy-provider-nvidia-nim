package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestFailureClassification(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name              string
		status            int
		body, retry, code string
		retryable         bool
		delay             int
	}{
		{"auth", 401, "sensitive-body", "", "invalid_credentials", false, 0},
		{"forbidden", 403, "sensitive-body", "", "invalid_credentials", false, 0},
		{"missing model", 404, "sensitive-body", "", "upstream_not_found", false, 0},
		{"credits", 402, "sensitive-body", "", "quota_exhausted", false, 3600},
		{"ordinary rate", 429, "rate limit exceeded", "", "rate_limited", true, 60},
		{"seconds", 429, "rate limit exceeded", "7", "rate_limited", true, 7},
		{"HTTP date", 429, "", "Tue, 01 Jan 2030 00:00:09 GMT", "rate_limited", true, 9},
		{"past date", 429, "", "Mon, 31 Dec 2029 23:59:59 GMT", "rate_limited", true, 60},
		{"invalid retry", 429, "", "invalid", "rate_limited", true, 60},
		{"credit exhaustion", 429, `{"error":{"code":"insufficient_credits","message":"API credits exhausted"}}`, "", "quota_exhausted", false, 3600},
		{"quota exhaustion", 429, `{"error":{"code":"insufficient_quota"}}`, "", "quota_exhausted", false, 3600},
		{"trial exhaustion", 429, "Trial expired", "", "quota_exhausted", false, 3600},
		{"not all quota wording is credits", 429, "Request quota per minute exceeded", "", "rate_limited", true, 60},
		{"server", 503, "sensitive-body", "12", "upstream_error", true, 12},
		{"timeout", 408, "sensitive-body", "", "upstream_error", true, 0},
		{"invalid request", 400, "sensitive-body", "", "upstream_error", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body), Headers: http.Header{"Retry-After": {tc.retry}}}, now)
			if e.Code != tc.code || e.HTTPStatus != tc.status || e.Retryable != tc.retryable || e.RetryAfterSeconds != tc.delay {
				t.Fatalf("error %+v", e)
			}
			if strings.Contains(e.Message, "sensitive-body") {
				t.Fatal("body leaked")
			}
		})
	}
}
