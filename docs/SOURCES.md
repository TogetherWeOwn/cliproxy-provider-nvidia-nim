# Wire contract, provenance and evidence limits

Official documentation was checked on 2026-10-31. Documentation requests are not
live inference/model/quota verification. Repository tests run exclusively against
loopback HTTP mocks and the original pinned host module. Rows marked **Live check
2026-10-31** record separate manual measurements against the hosted API with one
key (direct calls and a staging host); they are dated observations, not
guarantees, and no test depends on them.

## Official NVIDIA wire sources

| Detail implemented | Official source | Scope/evidence limit |
| --- | --- | --- |
| `POST https://integrate.api.nvidia.com/v1/chat/completions`, JSON request body, `model`, `messages`, `stream`, OpenAI-style response | [Step-5 Preview chat reference](https://docs.api.nvidia.com/nim/reference/stepfun-ai-step-5-preview-infer), [Llama 3.1 8B inference reference](https://docs.api.nvidia.com/nim/reference/meta-llama-3_1-8b-infer), [LLM API index](https://docs.api.nvidia.com/nim/reference/llm-apis) | Endpoint is fixed in production; payload options remain upstream-owned. Model-specific support is not universal. |
| Bearer authorization | Step-5 reference above (`BearerAuth`, HTTP scheme `bearer`); [NVIDIA NeMo Helix Guardrails troubleshooting](https://docs.nvidia.com/nemo-helix/documentation/reference/troubleshooting/guardrails/) | The latter shows `NVIDIA_API_KEY=<nvapi-...>` with `Authorization: Bearer ${NVIDIA_API_KEY}`. Key-shape validation is not entitlement verification. |
| API key acquisition, hosted prototyping flow | [NVIDIA API Catalog Quickstart](https://docs.api.nvidia.com/nim/docs/api-quickstart) | Obtain keys privately. The plugin never obtains or refreshes a key via a fabricated API. |
| Streaming SSE deltas and `data: [DONE]` | Step-5 and Llama inference references above | `stream=true` uses SSE; `ExecuteStream` relays deltas without interpretation. Non-streaming `Execute` aggregates the same SSE into one `chat.completion`. |
| Non-streaming invocations time out in the NVCF queue | Live check 2026-10-31 against `integrate.api.nvidia.com` | `stream:false` for `stepfun-ai/step-5-preview`: 504, empty body, `nvcf-status: errored` after 302 s (after 7 s with `NVCF-POLL-SECONDS: 5`); `stream:true` for the same request: 200 in 5 s. Basis for always streaming upstream. |
| Catalog is not account availability | Live check 2026-10-31 | `GET /v1/models` listed 85 IDs; a one-token chat probe returned 404 "Function ... not found" for 60; a retired former fallback answered 410 "end of life". Basis for probe validation in `ModelsForAuth`. |
| `GET https://integrate.api.nvidia.com/v1/models` | [NVIDIA NIM Operator: Managing NeMo Guardrails](https://docs.nvidia.com/nim-operator/latest/guardrail.html) | Explicitly links that hosted model-list endpoint. It does not document a numerical RPM entitlement or prove current per-key model access. |
| Expected OpenAI model-list JSON `data[].id` | [OpenAI models/list reference](https://platform.openai.com/docs/api-reference/models/list), NVIDIA's OpenAI-compatible chat references above | Compatibility expectation, **pending live NVIDIA catalog-schema verification**. NVIDIA's hosted list-endpoint link alone does not establish its current full response schema. Invalid/unusable catalogs fall back to static IDs. |
| Removed static `meta/llama-3.1-8b-instruct`, `nvidia/llama-3.3-nemotron-super-49b-v1.5` | Live check 2026-10-31 | The first answered 410 "reached its end of life on 2026-08-26"; the second is no longer in the hosted catalog. NVIDIA-owned `nvidia/...` upstream IDs already carry the namespace, so they are served undoubled; any other doubled id is still dropped by the kit. |
| Static `stepfun-ai/step-5-preview` | Step-5 inference reference above | Published ID, not a current free access/production-rights assertion. |
| 401 missing/invalid key, 402 insufficient API credits, 429 request rate limit exceeded, 400/422 validation | Step-5 inference reference above | Other 403/auth and 404/not-found handling follows HTTP semantics. Actual NVIDIA credit/trial message variants remain pending live verification. |
| Per-model/account-dependent request limits; no official universal free-tier RPM | [Official NVIDIA NIM FAQ](https://forums.developer.nvidia.com/t/nvidia-nim-faq/300317), [General FAQ](https://docs.api.nvidia.com/nim/docs/product) | The FAQ says the request rate varies by model and concurrent users and directs users to their signed-in account at build.nvidia.com. It gives no number. |
| Unofficial 40 RPM display default | Developer reports, e.g. the [NVIDIA Developer Forums rate-limit request thread](https://forums.developer.nvidia.com/t/request-for-nvidia-build-api-rate-limit-increase-40-rpm-200-rpm/378450) (users citing 40 RPM; no staff post in that thread gives a number) | **Not an NVIDIA wire detail or official allowance.** Used only when an auth file omits `requests_per_minute`, and labelled "Default capacity (unofficial)" on the card. Never gates inference. Override per auth file. |
| Rate-limit response headers | The inference/FAQ references above | No official remaining/capacity/reset header names were established. `NVCF-REQID`/`NVCF-STATUS` in older 202 responses are request/polling metadata, **not** quota headers. No guessed `X-RateLimit-*` parser is implemented. |
| Credit/request allowance API | The inference/FAQ sources above | 402 establishes credit exhaustion can occur, not a readable balance API. No documented hosted remaining-credit endpoint was found; no extra bucket is invented. |

HTTP status fallback and `Retry-After` parsing use the official
[HTTP Semantics RFC 9110, section 10.2.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-10.2.3),
not a claim that NVIDIA always emits that header. Category default cooldown
hints (60 seconds for rate, one hour for explicit exhaustion) are plugin policy
inherited from prior-art backoff, not provider reset times.

## Local quota behavior is deliberately local

The rolling 60-second observer, per-key digest identity, configured capacity,
clamped remaining fraction, next-aging-out timestamp and used/capacity summary
metrics are this plugin's display contract. They are **not NVIDIA wire fields**.
The original pinned `sdk/pluginapi/types.go: QuotaBucket` exposes window,
remainingFraction, resetTime and description, but no numeric used/capacity; those
are represented via `QuotaMetric` summaries instead.

No Scheduler, free-admission guard, durable lifetime allowance, sticky exhaustion,
credit projection, auth reset, preemptive account switch or payment action is
ported. Distinct keys can share a provider-level account limit; local cards
cannot observe other keys/processes/clients or silently establish independence.

## Source provenance

- Rolling-window expiration and next-expiry semantics are adapted from the
  MIT-licensed [subscription-pool NVIDIA module](https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/tree/ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b/internal/providers/nvidianim),
  commit `ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`, specifically
  `module.go: prune` and `status.go`. Credit/trial vs generic-rate handling is
  based on `usage.go`. The old admission Scheduler and durable reservation store
  are deliberately **not** reused; the new quota has no routing authority.
- Package layout, API-key duplicate-file ownership pattern, C-ABI factory,
  test-only loopback guard, real-host harness, release/registry/CI structure and
  public-file checks follow [cliproxy-provider-synthetic](https://github.com/TogetherWeOwn/cliproxy-provider-synthetic/tree/b5eacd4f1375fec10419cdcab6c68709444e7424),
  commit `b5eacd4f1375fec10419cdcab6c68709444e7424`.
- Shared C-ABI/RPC/auth/passthrough wiring uses the **published**
  [cliproxy-provider-kit at commit `21069bf`](https://github.com/TogetherWeOwn/cliproxy-provider-kit/tree/21069bfec8f10dac7130d8a1dd82501fec662e4d)
  (RPC schema 10).
  No module replacement or sibling checkout is required.
- The earlier unreleased NVIDIA dirty diff was consulted only as historical
  design context, not treated as published wire documentation or current quota
  entitlement. No host key watcher or legacy deployment automation is copied.
- The MIT license is preserved at the root and included in release archives.

## Native host contracts

Original [CLIProxyAPI v8.0.49](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.49):

- `sdk/pluginabi/types.go`: C-ABI/RPC envelopes, schema and retry hints.
- `sdk/pluginapi/types.go`: auth/model/executor/quota SDK contracts.
- `internal/registry/devin_models.go` and
  `internal/runtime/executor/helps/devin_models.go`: native fixed provider prefix
  registration/stripping pattern. NVIDIA-owned upstream model IDs still keep
  their own namespace after one leading plugin namespace is removed.
- `internal/pluginhost/adapters_executors.go`: native executor registration and
  ownership; compatibility entries can preempt plugin ownership.
- `internal/pluginhost/rpc_client.go`: status/retry propagation and the verified
  missing native transient-rate-limit marker/RPC Retryable propagation.
- `sdk/cliproxy/auth/conductor_cooldown.go`: host-owned rate/auth/model cooldowns.
- `internal/pluginhost/host.go: ApplyConfig`: native lifecycle/hot configuration
  reload. Migration does not restart containers or add a second control plane.
- `internal/pluginstore/registry.go` and `github.go`: schema-v1 GitHub-release
  installation, platform assets and checksums.

`integration/hostcompat/compat_test.go` uses the unmodified published host module
under a test-only import namespace to access the real internal loader. It
builds/loads the library, persists auth, registers prefixed models, drives normal
and streaming inference plus quota cards, and verifies native round-robin,
sibling failover and the pinned 429-marker limitation against httptest only.
