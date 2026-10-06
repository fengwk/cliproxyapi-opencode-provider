package provider

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ProviderID is the provider key served by this plugin.
const ProviderID = "opencode-go"

// PluginID is the host-local plugin identifier and the resource route prefix.
const PluginID = "cliproxyapi-opencode-provider"

// Version is the plugin release version. It can be overridden at build time via
// -ldflags "-X github.com/fengwk/cliproxyapi-opencode-provider/internal/provider.Version=...".
var Version = "0.1.0"

// GitHubRepository satisfies the host registration metadata requirement.
const GitHubRepository = "https://github.com/fengwk/cliproxyapi-opencode-provider"

// modelPrefix is the fixed public model namespace published to clients.
const modelPrefix = ProviderID + "/"

// Metadata returns the registration metadata reported to the host.
func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             PluginID,
		Version:          Version,
		Author:           "fengwk",
		GitHubRepository: GitHubRepository,
		ConfigFields: []pluginapi.ConfigField{
			{
				Name:        "base-url",
				Type:        pluginapi.ConfigFieldTypeString,
				Description: "Upstream base URL (default: https://opencode.ai/zen/go/v1).",
			},
			{
				Name:        "models",
				Type:        pluginapi.ConfigFieldTypeArray,
				Description: "Optional deterministic model route overrides: [{id, protocol: openai|claude|openai-response}].",
			},
		},
	}
}

// registration is the plugin.register / plugin.reconfigure response payload.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	RequestInterceptor    bool     `json:"request_interceptor"`
	ManagementAPI         bool     `json:"management_api"`
	QuotaProvider         bool     `json:"quota_provider"`
}

// registrationResponse negotiates the schema conservatively: never claim a
// newer contract than the host offered, and never exceed the SDK schema.
func registrationResponse(hostSchema uint32) registration {
	schema := pluginabi.SchemaVersion
	if hostSchema != 0 && hostSchema < schema {
		schema = hostSchema
	}
	if schema == 0 {
		schema = 1
	}
	return registration{
		SchemaVersion: schema,
		Metadata:      Metadata(),
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{"openai", "claude", "openai-response"},
			ExecutorOutputFormats: []string{"openai", "claude", "openai-response"},
			RequestInterceptor:    true,
			ManagementAPI:         true,
			QuotaProvider:         true,
		},
	}
}
