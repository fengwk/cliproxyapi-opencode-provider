package provider

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

//go:embed models.json
var modelsSnapshot []byte

// family routes map a native model-id prefix to its upstream protocol.
// Order is irrelevant because the prefixes are mutually exclusive.
var familyRoutes = []struct {
	prefix string
	format translator.Format
}{
	{"minimax", translator.FormatClaude},
	{"qwen", translator.FormatClaude},
	{"gpt", translator.FormatOpenAIResponse},
	{"grok", translator.FormatOpenAIResponse},
	{"muse-spark", translator.FormatOpenAIResponse},
	{"glm", translator.FormatOpenAI},
	{"kimi", translator.FormatOpenAI},
	{"deepseek", translator.FormatOpenAI},
	{"longcat", translator.FormatOpenAI},
	{"mimo", translator.FormatOpenAI},
	{"hy", translator.FormatOpenAI},
	{"space-bunny", translator.FormatOpenAI},
}

// nativeModelID strips the fixed public namespace from a requested model id.
func nativeModelID(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, modelPrefix) {
		return strings.TrimPrefix(model, modelPrefix)
	}
	return model
}

// routeNativeModel resolves a native model id to an upstream protocol using the
// configured overrides first, then the static family table. Unknown families
// fail closed.
func routeNativeModel(cfg Config, native string) (translator.Format, bool) {
	if native == "" {
		return "", false
	}
	if format, ok := cfg.Routes[native]; ok {
		return format, true
	}
	lower := strings.ToLower(native)
	for _, entry := range familyRoutes {
		if strings.HasPrefix(lower, entry.prefix) {
			return entry.format, true
		}
	}
	return "", false
}

// snapshotEntries is the decoded models.json payload.
type snapshotEntries struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func loadSnapshot() ([]pluginapi.ModelInfo, error) {
	var decoded snapshotEntries
	if err := json.Unmarshal(modelsSnapshot, &decoded); err != nil {
		return nil, fmt.Errorf("decode embedded model snapshot")
	}
	models := make([]pluginapi.ModelInfo, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		models = append(models, modelInfo(id, item.Created, item.OwnedBy, false))
	}
	return models, nil
}

func modelInfo(native string, created int64, ownedBy string, userDefined bool) pluginapi.ModelInfo {
	if ownedBy == "" {
		ownedBy = "opencode"
	}
	return pluginapi.ModelInfo{
		ID:          modelPrefix + native,
		Object:      "model",
		Created:     created,
		OwnedBy:     ownedBy,
		Name:        native,
		DisplayName: native,
		UserDefined: userDefined,
	}
}

// buildModels merges the embedded snapshot with configured overrides/additions.
// Configured ids win over snapshot entries for the same native id and are
// emitted in sorted order for deterministic registration.
func buildModels(cfg Config) []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, 64)
	seen := make(map[string]struct{}, len(cfg.Routes))
	configured := make([]string, 0, len(cfg.Routes))
	for native := range cfg.Routes {
		configured = append(configured, native)
	}
	sort.Strings(configured)
	for _, native := range configured {
		models = append(models, modelInfo(native, 0, "", true))
		seen[native] = struct{}{}
	}
	snapshot, err := loadSnapshot()
	if err != nil {
		return models
	}
	for _, item := range snapshot {
		native := nativeModelID(item.ID)
		if _, ok := seen[native]; ok {
			continue
		}
		seen[native] = struct{}{}
		models = append(models, item)
	}
	return models
}

// discoverModels asks the upstream /models endpoint through the host HTTP
// callback using the supplied credential. It returns the merged static and
// discovered model list; discovery failures degrade to the static list.
func (m *Manager) discoverModels(cfg Config, key, callbackID string) []pluginapi.ModelInfo {
	models := buildModels(cfg)
	if strings.TrimSpace(key) == "" {
		return models
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	resp, err := m.bridge.HTTPDo(http.MethodGet, joinURL(cfg.BaseURL, "/models"), headers, nil, callbackID)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return models
	}
	var decoded snapshotEntries
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		return models
	}
	seen := make(map[string]struct{}, len(models))
	for _, item := range models {
		seen[nativeModelID(item.ID)] = struct{}{}
	}
	for _, item := range decoded.Data {
		native := strings.TrimSpace(item.ID)
		if native == "" {
			continue
		}
		if _, ok := seen[native]; ok {
			continue
		}
		seen[native] = struct{}{}
		models = append(models, modelInfo(native, item.Created, item.OwnedBy, false))
	}
	return models
}
