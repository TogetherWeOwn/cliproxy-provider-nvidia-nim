package main

import (
	"strings"
	"sync"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelNamespace is the fixed vendor namespace for every served model id.
// NVIDIA-owned upstream ids already begin with it, so they pass through.
const modelNamespace = "nvidia"

// serveID maps an upstream model id to the id this plugin serves: ids already
// carrying the namespace exactly once (including NVIDIA-owned "nvidia/…"
// ids) pass through unchanged, anything else is prefixed once. The kit
// conformance check is the arbiter: whatever it accepts here is what the
// runtime serves.
func serveID(upstream string) string {
	if kit.CheckModelNamespace(modelNamespace, []pluginapi.ModelInfo{{ID: upstream}}) == nil {
		return rememberMapping(upstream, upstream)
	}
	return rememberMapping(upstream, modelPrefix+upstream)
}

// upstreamByServed records the exact upstream id behind every served id the
// plugin registers. The serveID/upstreamID pair is ambiguous for slashless
// upstream ids (upstream "foo" and upstream "nvidia/foo" both serve as
// "nvidia/foo"), so inversion consults this ground truth first and only
// falls back to the heuristic for ids the plugin never registered (which
// fail closed upstream). On a collision the first registration wins; the
// served id is ambiguous by construction and dispatch stays deterministic.
var upstreamByServed = struct {
	sync.RWMutex
	m map[string]string
}{m: make(map[string]string)}

func rememberMapping(upstream, served string) string {
	upstreamByServed.Lock()
	defer upstreamByServed.Unlock()
	if prev, ok := upstreamByServed.m[served]; !ok || prev == upstream {
		upstreamByServed.m[served] = upstream
	}
	return served
}

// assignMapping points a served id at its validated upstream id, replacing
// the registration first-wins entry. Validation keeps only one survivor per
// served id, so dispatch must follow the survivor, not whichever upstream
// id the catalog listed first.
func assignMapping(upstream, served string) {
	upstreamByServed.Lock()
	defer upstreamByServed.Unlock()
	upstreamByServed.m[served] = upstream
}

// upstreamID inverts serveID for dispatch. Discovery validation probes each
// candidate against its own upstream id (no inversion), and only dispatch
// needs the served→upstream mapping. A served id that only
// gained its namespace from the prefix maps back by trimming once; a served
// id that already carried it (NVIDIA-owned "nvidia/…" ids, which have no
// second segment after the prefix) maps to itself. Nested NVIDIA-owned ids
// ("nvidia/a/b" upstream) do not exist; if one ever appears the trim branch
// misroutes it to "a/b" and the upstream answers 404, fail-closed.
func upstreamID(served string) (string, bool) {
	upstreamByServed.RLock()
	upstream, ok := upstreamByServed.m[served]
	upstreamByServed.RUnlock()
	if ok {
		return upstream, true
	}
	rest, ok := strings.CutPrefix(served, modelPrefix)
	if !ok || rest == "" {
		return "", false
	}
	if strings.Contains(rest, "/") {
		if serveID(rest) != served {
			return "", false
		}
		return rest, true
	}
	if serveID(served) != served {
		return "", false
	}
	return served, true
}
