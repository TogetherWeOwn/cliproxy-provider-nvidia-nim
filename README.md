# NVIDIA NIM provider for CLIProxyAPI

First-class NVIDIA API-key auth files, namespaced OpenAI-compatible chat models,
incremental streaming and **display-only local requests-per-minute quota cards**.
Uses [`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at release `v0.4.1`
and is tested against the original **CLIProxyAPI v8.0.49 C-ABI host** (ABI 1 /
RPC schema 10). Requires CLIProxyAPI v8.0.49 or newer: the kit answers schema 10
and refuses hosts older than schema 10. The plugin declares no multi-auth
providers, so the host treats it as a single-auth plugin.

Multiple distinct-key auth files use native round-robin/weighted/fill-first
routing and native cooldowns. There is **no Scheduler**, local admission gate,
quota-based account avoidance, purchase, paid fallback or automatic quota reset.
This is a provider plugin, not the older subscription-pool scheduling overlay.

**Pending live verification.** All verification here uses loopback mocks. No
NVIDIA inference, model, quota or analytics endpoint was contacted during tests.
See [migration](docs/MIGRATION.md) and the pinned-host caveat below before cutover.

## Auth files

The provider is API-key only: there is no login card in the management UI.
Upload one private JSON auth file per legitimate API key through the
management UI's auth-file upload (the same path built-in API-key providers
use), or place the file directly in the host's auth directory, mode `0600`.
The following is illustrative JSON; replace both placeholders locally.
`requests_per_minute` is optional (see below).
Never commit credential files or real account labels.

```json
{
  "type": "nvidia-nim",
  "api_key": "nvapi-<api-key>",
  "requests_per_minute": 40,
  "weight": 1,
  "priority": 0,
  "disabled": false
}
```

**Default capacity: 40 RPM, unofficial.** The 40 requests-per-minute figure is
the build.nvidia.com free-tier limit developers report seeing on their account
(see [docs/SOURCES.md](docs/SOURCES.md)). NVIDIA's
[official FAQ](https://forums.developer.nvidia.com/t/nvidia-nim-faq/300317)
publishes **no** fixed number: it says the rate varies per model and with
concurrent users, and points to the signed-in account page on build.nvidia.com.
So the plugin uses 40 only when `requests_per_minute` is omitted. The card
itself carries no capacity label — one `Requests per minute` bucket, no
description, no summary — because the bucket schema has no display name and
extra text would repeat the bar. Set `requests_per_minute` per auth
file to the limit your account page shows. The value must be an integer from
1 through 100000. It only affects the display; inference is never gated by it.
Native metadata takes precedence over persisted provider storage.

API keys must begin `nvapi-`; leading/trailing whitespace is trimmed. This only
describes shape, not entitlement. The auth ID is the file's relative path under
the auth directory; a user-supplied `id` field is stored but ignored. Every
file loads as its own auth, so use one file per key — the same key in two
files loads twice instead of being rejected. Renaming, editing or deleting a
file takes effect when the host rescans the auth directory; no plugin restart
is needed. Uploaded files are not validated against the shape above at upload
time (a non-`nvapi-` key loads, then fails at request time; an out-of-range
`requests_per_minute` loads, then fails the quota fetch at request time while
inference keeps serving, since inference is never gated by it); the
unit-tested ParseAuth in `auth.go` documents the intended shape.

Optional native fields include `prefix`, `proxy_url`, `weight`, `priority`
and `disabled`. The management UI derives the display label from the file
itself; a file-level `label` is ignored on the generic file path. Fixed model namespace `nvidia/`; auth `prefix` does
not replace it. Refresh preserves storage
without calling NVIDIA and does not renew or rotate keys.

## Models and inference

Every static and discovered model id carries the fixed **`nvidia/`**
namespace exactly once. NVIDIA-owned upstream ids already start with
`nvidia/`, so they pass through undoubled instead of being refused; any other
upstream id gains exactly one prefix, and dispatch inverts the mapping.

| Client ID | Upstream ID | Static fallback |
| --- | --- | --- |
| `nvidia/stepfun-ai/step-5-preview` | `stepfun-ai/step-5-preview` | Yes |
| `nvidia/<organization>/<model>` | `<organization>/<model>` | Discovered |
| `nvidia/llama-3.3-nemotron-super-49b-v1.5` | `nvidia/llama-3.3-nemotron-super-49b-v1.5` | Discovered |

Discovery uses `GET https://integrate.api.nvidia.com/v1/models` with Bearer auth.
That list is a static catalog, **not per-account availability**: in a live check
on 2026-10-31 it returned 85 IDs for one key, and 60 of them answered the chat
endpoint with 404 (no function deployed for the account); retired IDs answer
410 (end of life).
Discovery therefore deduplicates IDs, excludes explicit embedding/reranking IDs
and then **validates every candidate** with a one-token streaming chat probe
(`max_tokens: 1`, cancelled as soon as response headers arrive):

- 400 or 422 (rejects a minimal chat request, e.g. a document parser), 403,
  404 or 410: the model is not usable by this key and is not advertised.
- 2xx other than 202: usable. Any other outcome (202 pending, 429, 5xx,
  timeout, transport error) is inconclusive and the model is kept, so a
  transient upstream problem never empties the catalog.
- At most 8 probes run at once across all discoveries, each bounded to 5 s, the whole validation to
  30 s. Definitive verdicts are cached per key for 6 h and inconclusive
  ones for 15 min, so host client reloads do not re-probe the catalog.
- Each probe is a real dispatch and is counted on the local RPM card. A full
  first discovery can therefore use most of a 40 RPM minute.

The static registrar supplies the fallback model above. Discovery falls back to
it (and validates it) on transport/server/missing catalog errors,
malformed/oversized (>1 MiB) JSON, or no usable returned models. It propagates
401/402/403/429 and cancellation rather than hiding auth/quota failures behind a
healthy catalog. Discovery requires host transport.

Execution sends `POST https://integrate.api.nvidia.com/v1/chat/completions`,
`Authorization: Bearer <api-key>` and JSON via the **injected host HTTP client**;
no independent transport bypasses the host's proxy/logging policy. Host-final
payload fields are preserved except for the single prefix and the stream mode.
**Upstream always streams.** Hosted NIM routes `stream:false` through NVCF's
queued invocation path, which holds the connection up to `NVCF-POLL-SECONDS`
(300 s by default) and then answers 504 (or 202 pending) with an empty body; on
2026-10-31 `stepfun-ai/step-5-preview` non-streaming timed out with 504 after
302 s while the same request streamed in 5 s. `Execute` therefore sends
`stream:true` plus `stream_options.include_usage: true` (some hosted models omit
usage from a stream otherwise; caller-set `stream_options` are kept) and
aggregates the SSE (content, reasoning, tool-call fragments, usage) into one
`chat.completion` JSON document; an upstream that ignores the
flag and answers `application/json` is passed through after validation. An
empty, non-JSON or truncated 2xx reply is a retryable 502, never a successful
empty completion. `ExecuteStream` relays incrementally without buffering to
completion; the kit re-frames to one data event per chunk and drops the
upstream `[DONE]` marker (the host appends its own terminator). Raw HTTP calls are constrained to this same
endpoint and credentials; caller URLs/methods/headers cannot redirect the key.
Local token counting is the kit's explicitly estimated count, not a model
tokenizer or a billable NVIDIA operation.

## RPM quota semantics

The quota card is **local observation, not an authoritative NVIDIA balance**:

- One bucket, `window: Requests per minute`, no description and no summary
  metrics, per key; all models on that key share it.
- `used` counts executor dispatch attempts in the trailing `(now - 60s, now]`.
  Normal, streaming and raw-HTTP inference each count once before host dispatch.
  Failed upstream requests, retries performed as new executor calls and
  transport failures count conservatively. Each catalog validation probe is a
  real one-token chat dispatch and counts too; the `/v1/models` list request,
  invalid local payloads, absent host transport, already-cancelled requests and
  token counting do not. Completion/usage notifications cannot double-count
  streams.
- `capacity` is the auth file's `requests_per_minute`, or the unofficial
  40 RPM default when it is omitted. Used and capacity counters stay off the
  card because they would repeat the bar.
- `remainingFraction = max(0, 1 - used / capacity)`. The used count is not capped:
  a zero card **never blocks or reroutes inference**.
- `resetTime` is when the oldest still-counted attempt ages out, **not a full
  refill**. Empty windows have no reset timestamp. Fetching recomputes immediately
  without a 60-second response cache or any network call.
- Counts are isolated by a full SHA-256 key digest, not mutable host auth IDs;
  keys are not retained in quota state. Expired histories are pruned on access.
  Reconfiguration of the same plugin runtime preserves its window. A new plugin
  instance/process loses history and initially observes zero local requests;
  wait 60 seconds before treating it as a complete observation window.
- Other clients, other processes, host-internal transport retries, or other keys
  on the **same NVIDIA account** can consume a shared provider limit and are not
  visible here. Separate key cards do not imply separate upstream entitlement.
- No official hosted rate-limit remaining/capacity/reset response headers were
  documented in the consulted sources. The plugin does not guess `X-RateLimit-*`
  names. It honors standard `Retry-After` on actual failures, not as a balance.
- No officially documented hosted credit-balance API was found. There is no
  fabricated credit/monthly/daily bucket, token-to-credit conversion or reset API.

This is not a billing or entitlement guard. Retain provider-side controls and
verify the applicable NVIDIA API/model terms before any deployment.

## Failure classification and native cooldowns

Errors expose only fixed safe messages, never keys or upstream bodies.

| Upstream result | RPC classification | Retry hint |
| --- | --- | --- |
| 401 / 403 | `invalid_credentials`, non-retryable | None |
| 402 | `quota_exhausted`, non-retryable | One hour |
| 404 | `upstream_not_found`, non-retryable | None; not auth exhaustion |
| Ordinary 429 | `rate_limited`, retryable | Valid `Retry-After`, otherwise 60 seconds |
| 429 with explicit insufficient/exhausted credits, `insufficient_quota`, or expired/exhausted trial evidence | `quota_exhausted`, non-retryable | Valid `Retry-After`, otherwise one hour |
| 408 / 5xx | `upstream_error`, retryable | Valid `Retry-After`, if supplied |
| Other 4xx | `upstream_error`, non-retryable | Upstream timing if supplied |

Retry-After accepts integer seconds or an HTTP date. A missing/invalid/past/zero
value uses the category default. Explicit exhaustion wording is conservative
compatibility handling ported from prior art, not a claim that every NVIDIA
account emits that shape. Generic “request quota per minute exceeded” stays a
short rate limit. The plugin never latches local exhaustion or rewrites status
codes to influence routing.

### Verified v8.0.49 RPC limitation

`internal/pluginhost/rpc_client.go` preserves HTTP status and retry timing, but
not the RPC `retryable` flag or the native transient-rate-limit marker. The real
host-manager test therefore observes `ModelStates[model].Quota.Exceeded` even
for a rate-only 429. A seven-second `Retry-After` still produces a short native
cooldown and rotates to a healthy sibling. This is **not** credit-exhaustion
parity or a reason to add a Scheduler/status disguise. Native cooldown scope
and duration remain host policy: 401/402/403 currently use 30-minute model
cooldowns, and ordinary 404 can cool the model for 12 hours. An upstream RPC
contract update plus a new pinned-host test is needed to distinguish transient
markers end to end. No local core patch is supplied.

## Build, test and package

Requires Go **1.26.10**, CGO, a C compiler, `nm`, `objdump`, Python 3 and `zip`.
No `replace` directives, sibling dependencies, live configuration or provider
credentials are needed.

```sh
go vet ./...
go test -race -count=1 ./...
go -C integration/hostcompat vet ./...
go -C integration/hostcompat test -race -count=1 ./...
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o <output-dir>/nvidia-nim.so .
nm -D <output-dir>/nvidia-nim.so | grep cliproxy_plugin_init
bash scripts/package-release.sh <output-dir>
```

Packaging creates `nvidia-nim_0.2.4_linux_amd64.zip` containing `nvidia-nim.so`
and `LICENSE`, plus `checksums.txt`. It checks the C-ABI symbol and rejects
libraries importing GLIBC versions newer than **2.34** or private GLIBC symbols.
Publish both files together in the matching release; schema-v1 `registry.json`
is release metadata, not evidence that release assets have been published.

CI runs formatting, both modules' vet/race tests, real-loader compatibility,
release packaging/GLIBC checks, gitleaks and supplemental public-file hygiene.
The hostcompat-tagged binary accepts **loopback mock upstreams only** and is
never a production release artifact. Build/test/package scripts do not install,
reload or restart a service.

## Pending live verification

- Confirm each account's actual free-tier RPM on its build.nvidia.com account
  page and permitted use; set `requests_per_minute` if it differs from the
  unofficial 40 RPM default.
- On a separately authorized staging CLIProxy, verify key entitlement, current
  model catalog/response schema, and one small stream and non-stream request.
- Check actual failure status/body and any returned documented rate-limit
  headers without intentionally exhausting or load-testing an account.
- Confirm the local card matches observed dispatches, native routing and sibling
  isolation; explicitly accept or resolve the pinned-host rate-marker limitation.
- Publish reviewed production artifacts before enabling registry-based install;
  then follow native hot configuration reload for provider-scoped cutover.

## Sources

- [NVIDIA NIM FAQ: account/model-dependent limits](https://forums.developer.nvidia.com/t/nvidia-nim-faq/300317)
- [NVIDIA chat wire reference and 402/429 semantics](https://docs.api.nvidia.com/nim/reference/stepfun-ai-step-5-preview-infer)
- [NVIDIA-hosted model-list endpoint](https://docs.nvidia.com/nim-operator/latest/guardrail.html)
- [Complete wire/provenance source map](docs/SOURCES.md)
