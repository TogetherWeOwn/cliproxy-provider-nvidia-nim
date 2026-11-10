package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
//
// Bucket identity is an HMAC-SHA256 of the credential under a per-process
// random key (generated once at construction, never persisted). A plain
// SHA-256 of the key would be a weak unsalted hash of a secret (CodeQL
// go/weak-sensitive-data-hashing); the keyed HMAC preserves per-key
// isolation without exposing a brute-forceable digest. On entropy failure
// the provider is fail-closed (macValid false): record is a no-op and
// FetchQuota errors, never falling back to a zero key or a plain hash.
type quotaProvider struct {
	mu       sync.Mutex
	recent   map[[32]byte][]time.Time
	now      func() time.Time
	macKey   [32]byte
	macValid bool
}

func newQuotaProvider() *quotaProvider {
	q := &quotaProvider{recent: make(map[[32]byte][]time.Time), now: time.Now}
	if _, err := rand.Read(q.macKey[:]); err == nil {
		q.macValid = true
	}
	return q
}

// bucket derives the process-local bucket for a credential. The macKey is
// immutable after construction, so this is safe without holding mu.
func (q *quotaProvider) bucket(key string) ([32]byte, bool) {
	if q == nil || !q.macValid {
		return [32]byte{}, false
	}
	h := hmac.New(sha256.New, q.macKey[:])
	h.Write([]byte(key))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, true
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
	if key == "" {
		return
	}
	digest, ok := q.bucket(key)
	if !ok {
		return
	}
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
	digest, ok := q.bucket(key)
	if !ok {
		return pluginapi.QuotaFetchResponse{}, &pluginabi.Error{Code: "quota_unavailable", Message: "quota state is unavailable", HTTPStatus: 503}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(q.now())
	history := q.recent[digest]
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
