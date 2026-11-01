package main

import (
	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider     = "nvidia-nim"
	version      = "0.1.4"
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
	models := newCatalog()
	return kit.New(kit.Definition{
		Metadata:  pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-nvidia-nim"},
		Configure: configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider: newAuthProvider(), ModelRegistrar: models, ModelProvider: models,
			Executor: newExecutor(quota), ExecutorModelScope: pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"},
			QuotaProvider: quota,
		},
	}, host)
}
