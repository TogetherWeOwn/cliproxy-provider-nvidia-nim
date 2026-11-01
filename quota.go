package main

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Rolling-window semantics are adapted from internal/providers/nvidianim/module.go
// (prune) and status.go in TogetherWeOwn/cpa-plugin-subscription-pool, MIT,
// commit ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b. Unlike that scheduler, this
// provider only observes dispatched inference attempts and NEVER admits, blocks,
// reserves, selects or cools down an auth based on this local count.
type quotaProvider struct {
	mu     sync.Mutex
	recent map[[32]byte][]time.Time
	now    func() time.Time
}

func newQuotaProvider() *quotaProvider {
	return &quotaProvider{recent: make(map[[32]byte][]time.Time), now: time.Now}
}
func (*quotaProvider) Identifier() string { return provider }
func (*quotaProvider) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{provider}, DisplayName: "NVIDIA NIM (local RPM observation)"}, nil
}

// All timestamps are recorded under the lock so pruning can rely on order.
// Empty histories are evicted. State is process-local and not a credit balance.
func (q *quotaProvider) prune(now time.Time) {
	cutoff := now.Add(-time.Minute)
	for digest, history := range q.recent {
		first := 0
		for first < len(history) && !history[first].After(cutoff) {
			first++
		}
		if first == len(history) {
			delete(q.recent, digest)
		} else if first > 0 {
			// Do not retain an expired burst's backing array for a small active tail.
			q.recent[digest] = append([]time.Time(nil), history[first:]...)
		}
	}
}
func (q *quotaProvider) record(key string) {
	if q == nil {
		return
	}
	digest := sha256.Sum256([]byte(key))
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	q.prune(now)
	q.recent[digest] = append(q.recent[digest], now)
}
func (q *quotaProvider) FetchQuota(ctx context.Context, r pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	if r.Provider != "" && r.Provider != provider {
		return pluginapi.QuotaFetchResponse{}, invalidAuth("quota provider mismatch")
	}
	key, err := authKey(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	fields, err := kit.AuthFields(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	capacity, _, err := rpmCapacity(fields)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(q.now())
	history := q.recent[sha256.Sum256([]byte(key))]
	used := len(history)
	fraction := max(0, 1-float64(used)/float64(capacity))
	bucket := pluginapi.QuotaBucket{Window: "Requests per minute", RemainingFraction: fraction}
	if used > 0 {
		bucket.ResetTime = history[0].Add(time.Minute).UTC().Format(time.RFC3339Nano)
	}
	return pluginapi.QuotaFetchResponse{
		Groups: []pluginapi.QuotaGroup{{DisplayName: "NVIDIA NIM", Buckets: []pluginapi.QuotaBucket{bucket}}},
	}, nil
}
func (*quotaProvider) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, &pluginabi.Error{Code: "reset_unsupported", Message: "NVIDIA NIM RPM observations cannot be reset manually", HTTPStatus: 400}
}
