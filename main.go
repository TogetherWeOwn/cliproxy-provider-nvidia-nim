package main

import (
	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider     = "nvidia-nim"
	displayName  = "NVIDIA NIM"
	version      = "0.2.4"
	modelPrefix  = "nvidia/"
	upstreamBase = "https://integrate.api.nvidia.com"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	quota := newQuotaProvider()
	models := newCatalog(quota)
	// API-key only: no AuthProvider and no ManagementAPI login
	// routes, so the management UI shows no login card. Auth files load
	// through the host's generic file path.
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: displayName, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-nvidia-nim", Logo: "https://www.google.com/s2/favicons?domain=nvidia.com"},
		ModelNamespace: modelNamespace,
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			ModelRegistrar: models, ModelProvider: models,
			Executor: newExecutor(quota), ExecutorModelScope: pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"},
			QuotaProvider: quota,
		},
	}, host)
}
