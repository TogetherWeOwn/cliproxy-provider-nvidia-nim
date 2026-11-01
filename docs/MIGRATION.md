# Migration: staging first, native hot configuration reload

This is a provider-scoped operator runbook, not deployment automation. Repository
builds and tests never touch a running CLIProxy. All account checks below need
separate authorization and credentials. **Do not use a container restart for
configuration cutover.** Use the host's supported config watcher/hot reload and
native plugin store/lifecycle (`internal/pluginhost/host.go: ApplyConfig`).

## 1. Preserve rollback and validate the release

- Privately back up the current host configuration, auth files and plugin
  versions/settings, preserving mode/ownership. Never put them in this repo.
- Record existing NVIDIA compatibility entries, caller aliases and the legacy
  subscription-pool NVIDIA overlay, if any. Preserve unrelated providers.
- Verify the reviewed release archive against `checksums.txt`. It must contain
  the production `nvidia-nim.so`, not a `hostcompat` build. Run both modules'
  vet/race tests and the packaging script; require public GLIBC imports <=2.34.
- Check actual NVIDIA account/model RPM and applicable rights in the API
  Catalog. A key or catalog ID is not proof of free entitlement or permitted use.

## 2. Prove it on an isolated staging CLIProxy

Use a separately authorized instance with its own private auth directory,
plugin directory, access credentials and loopback listening port. No automatic
production traffic. Use the intended host version and native transport policy.

Do not keep an `openai-compatibility` entry named `nvidia-nim` alongside the new
plugin within the same instance. Native compatibility executors can preempt
plugin executor ownership and model claims are first-wins. Remove only the
NVIDIA block from the legacy scheduling overlay in this isolated staging
configuration; preserve every other provider block.

Install the reviewed release through the native plugin store using this repo's
registry. Illustrative staging configuration, with placeholder locations:

```yaml
auth-dir: <staging-auth-directory>
plugins:
  enabled: true
  dir: <staging-plugin-directory>
  store-sources:
    - https://raw.githubusercontent.com/TogetherWeOwn/cliproxy-provider-nvidia-nim/main/registry.json
  configs:
    nvidia-nim:
      enabled: true
```

The native store selects `<plugin-id>_<version>_linux_amd64.zip` plus
`checksums.txt` and installs into the platform-specific plugin layout. Wait for
native hot configuration reload, then verify active metadata, auth and quota
capabilities and actual executor ownership. A file on disk alone proves nothing.
Do not overwrite a mapped shared library in place; use the native store's
versioned install/replacement mechanism and supported hot reload.

## 3. Migrate auth files and model names

- One mode-0600 JSON file per distinct NVIDIA key using `type: nvidia-nim` and
  `api_key`. Set `requests_per_minute` to the limit the account page shows if it
  differs from the unofficial 40 RPM default. Different keys can share the same
  upstream limit.
- Preserve native priority, weight, disabled, label and proxy settings as needed.
  Do not assign a custom auth ID. Do not duplicate a key between files. For a
  rename, disable/re-enable this plugin through native hot reload to clear
  filename ownership after moving it; no host/container restart is necessary.
- The model namespace is always `nvidia/`; the host auth `prefix` is separate.
  Map downstream aliases explicitly. For example:

  | Upstream model | New client model |
  | --- | --- |
  | `meta/llama-3.1-8b-instruct` | `nvidia/meta/llama-3.1-8b-instruct` |
  | `nvidia/llama-3.3-nemotron-super-49b-v1.5` | `nvidia/llama-3.3-nemotron-super-49b-v1.5` (already namespaced: passes through undoubled) |
  | `stepfun-ai/step-5-preview` | `nvidia/stepfun-ai/step-5-preview` |

- Do not retain the old overlay's free-admission guards or scheduling settings
  as if this plugin implements them. This replacement is native routing plus a
  display-only RPM observer, not a billing guard or a preemptive account pacer.

## 4. Staging acceptance

1. Confirm parse, persistence, stable key fingerprint and duplicate-file rejection.
2. Confirm discovery registers fixed-prefixed IDs and strips exactly one leading
   prefix for upstream, including an NVIDIA-owned model's double namespace.
3. Check the card is explicitly local RPM. With capacity configured and after
   a full observation window, verify inference attempts in the trailing 60s,
   unclipped used count, clamped remaining fraction and next-aging-out timestamp.
   Omitted capacity must show the 40 RPM default labelled unofficial. Quota
   fetch must make no provider request and reset must be unsupported.
4. With authorization, make one small stream and one non-stream request on an
   entitled model. Verify incremental streaming, host proxy policy, response
   headers and local count. Do not intentionally exhaust or load-test an account.
5. Verify native routing over legitimate distinct keys and sibling isolation
   after errors. Compare mock error tests against actual observed failure
   status/headers, without deliberately provoking credential failures.
6. Explicitly accept or resolve the known v8.0.49 RPC transient-marker limitation
   before cutover. The host carries status and Retry-After but marks rate-only
   429 as model quota-exceeded and drops RPC Retryable. The mock host-manager
   test proves short cooldown/sibling failover, not full transient/exhaustion
   parity. An upstream RPC contract change/new pinned test is the exit; never a
   local core patch, status disguise or custom Scheduler.

## 5. Provider-scoped cutover

After separately authorized staging acceptance:

1. Keep the verified rollback snapshot and old plugin version available.
2. Remove only the old NVIDIA compatibility entry and NVIDIA overlay block from
   the target config; leaving them can invalidate plugin executor ownership.
3. Install/enable the reviewed production plugin and deduplicated auth files via
   native mechanisms. Let CLIProxy **hot-reload configuration**. Do not restart
   the container or change another provider to make this work.
4. Update downstream model aliases to the table above. Verify executor ownership,
   native auth/quota UI and low-volume authorized inference. Wait a full 60s
   after creating a new plugin instance before regarding local history as complete.
5. Retain the private snapshot until the agreed observation period passes.

## 6. Roll back through native hot reload

Stop sending new traffic to the new provider, restore prior caller aliases and
disable `configs.nvidia-nim` in native configuration. Restore only the old NVIDIA
compatibility entry and overlay block from the snapshot; leave all unrelated
provider settings unchanged. Restore/select a prior reviewed version via the
native store if needed, and wait for native hot reload. Verify old executor
ownership and inference/quota behavior before resuming traffic.

Logical disable/drain is not proof that the OS has physically unmapped a Go
C-shared image; do not overwrite mapped files or claim memory eviction. If the
native lifecycle cannot safely recover a particular bad binary, stop the cutover
and seek the host's documented recovery path separately. Routine migration and
rollback do not require a container restart or host automation.
