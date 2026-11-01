package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func quotaRequest(key string, capacity int) pluginapi.QuotaFetchRequest {
	return pluginapi.QuotaFetchRequest{Provider: "nvidia-nim", AuthID: "arbitrary-host-id", StorageJSON: []byte(fmt.Sprintf(`{"api_key":%q,"requests_per_minute":%d}`, key, capacity))}
}

func requireQuota(t *testing.T, q *quotaProvider, r pluginapi.QuotaFetchRequest, used, capacity, fraction float64, reset string) {
	t.Helper()
	out, err := q.FetchQuota(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 1 {
		t.Fatalf("quota buckets %+v", out)
	}
	b := out.Groups[0].Buckets[0]
	if out.Groups[0].DisplayName != "Requests per minute (local)" ||
		b.Window != "per-minute" || b.Description != "" || b.RemainingFraction != fraction || b.ResetTime != reset {
		t.Fatalf("quota bucket %+v", out.Groups[0])
	}
	// A configured capacity needs no chips: it is already in the auth file.
	if len(out.Summary) != 0 {
		t.Fatalf("configured capacity shows chips %+v", out.Summary)
	}
	if v := kit.CheckCard(out); len(v) != 0 {
		t.Fatalf("card style violations: %v", v)
	}
}

func TestQuotaTrailingWindowBoundaryIsolationAndNoReset(t *testing.T) {
	q := newQuotaProvider()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	a := quotaRequest("nvapi-first", 4)
	requireQuota(t, q, a, 0, 4, 1, "")
	q.record("nvapi-first")
	now = now.Add(30 * time.Second)
	q.record("nvapi-first")
	q.record("nvapi-second")
	requireQuota(t, q, a, 2, 4, .5, "2030-01-01T00:01:00Z")
	// Renaming an auth record does not erase the same key's observed window.
	a.AuthID = "renamed"
	requireQuota(t, q, a, 2, 4, .5, "2030-01-01T00:01:00Z")
	requireQuota(t, q, quotaRequest("nvapi-second", 4), 1, 4, .75, "2030-01-01T00:01:30Z")
	now = now.Add(30 * time.Second)
	requireQuota(t, q, a, 1, 4, .75, "2030-01-01T00:01:30Z")
	now = now.Add(30 * time.Second)
	requireQuota(t, q, a, 0, 4, 1, "")
	if len(q.recent) != 0 {
		t.Fatal("expired key histories retained")
	}
	desc, err := q.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil || desc.SupportsReset || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != "nvidia-nim" {
		t.Fatalf("quota capabilities %+v %v", desc, err)
	}
	if _, err = q.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{}); err == nil {
		t.Fatal("local window could be manually replenished")
	}
}

func TestQuotaCapacityDefaultIsLabelledAndOverridable(t *testing.T) {
	q := newQuotaProvider()
	q.record("nvapi-example")
	out, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: "nvidia-nim", StorageJSON: []byte(`{"api_key":"nvapi-example"}`)})
	if err != nil || len(out.Groups) != 1 || out.Groups[0].Buckets[0].RemainingFraction != 1-1.0/40 {
		t.Fatalf("default capacity %+v %v", out, err)
	}
	// The unconfigured default is a guess, so it gets one flagging chip and
	// no sentence; a configured capacity shows no chips at all.
	if out.Groups[0].Buckets[0].Description != "" || len(out.Summary) != 1 ||
		out.Summary[0].Key != "requests_per_minute" || out.Summary[0].Value != 40 ||
		out.Summary[0].Label != "Default capacity (unofficial)" {
		t.Fatalf("default capacity not labelled as unofficial %+v", out)
	}
	configured := quotaRequest("nvapi-example", 4)
	out, err = q.FetchQuota(context.Background(), configured)
	if err != nil || out.Groups[0].Buckets[0].RemainingFraction != .75 ||
		out.Groups[0].Buckets[0].Description != "" || len(out.Summary) != 0 {
		t.Fatalf("configured capacity %+v %v", out, err)
	}
	for _, r := range []pluginapi.QuotaFetchRequest{
		{Provider: "other", StorageJSON: []byte(`{"api_key":"nvapi-example","requests_per_minute":4}`)},
		{Provider: "nvidia-nim", StorageJSON: []byte(`{"api_key":"nvapi-example","requests_per_minute":0}`)},
		{Provider: "nvidia-nim", StorageJSON: []byte(`{"api_key":"nvapi-example","requests_per_minute":2.5}`)},
		{Provider: "nvidia-nim", StorageJSON: []byte(`{"requests_per_minute":4}`)},
	} {
		if _, err = q.FetchQuota(context.Background(), r); err == nil {
			t.Fatalf("accepted invalid quota request %+v", r)
		}
	}
	r := quotaRequest("nvapi-example", 4)
	r.Metadata = map[string]any{"requests_per_minute": float64(2)}
	out, err = q.FetchQuota(context.Background(), r)
	if err != nil || out.Groups[0].Buckets[0].RemainingFraction != .5 {
		t.Fatalf("metadata precedence %+v %v", out, err)
	}
}

func TestQuotaConcurrentCountsDoNotGateOrUnderflow(t *testing.T) {
	q := newQuotaProvider()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			q.record("nvapi-example")
			_, err := q.FetchQuota(context.Background(), quotaRequest("nvapi-example", 4))
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	requireQuota(t, q, quotaRequest("nvapi-example", 4), 100, 4, 0, "2030-01-01T00:01:00Z")
}
