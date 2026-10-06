package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// familyRoutes map a native model-id prefix to its upstream protocol.
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

// modelInfo builds one published ModelInfo for a native upstream id. The public
// "opencode-go/" namespace is added exactly once and every official catalog
// entry stays non-user-defined.
func modelInfo(native string, created int64, ownedBy string) pluginapi.ModelInfo {
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
		UserDefined: false,
	}
}

// catalogPayload is the upstream GET /models body. Data is a pointer so a
// missing or null "data" field is distinguishable from an explicit empty list.
type catalogPayload struct {
	Data *[]catalogEntry `json:"data"`
}

type catalogEntry struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// catalogInvalid reports an unusable upstream catalog without echoing any of it.
func catalogInvalid() error {
	return &ProviderError{
		Code:       "invalid_upstream",
		Message:    "upstream model catalog is invalid",
		HTTPStatus: http.StatusBadGateway,
	}
}

// invalidNativeRune reports whitespace or control characters, which can never
// appear inside an official native model id.
func invalidNativeRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// decodeCatalog validates one official /models response: a top-level object with
// a data array (missing or null data is invalid, an empty array is valid).
// Native order, created and owned_by are preserved, the public prefix is applied
// exactly once, and duplicate native ids are collapsed. Any malformed entry
// rejects the whole catalog so partial or invented data is never published.
func decodeCatalog(body []byte) ([]pluginapi.ModelInfo, error) {
	var decoded catalogPayload
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Data == nil {
		return nil, catalogInvalid()
	}
	models := make([]pluginapi.ModelInfo, 0, len(*decoded.Data))
	seen := make(map[string]struct{}, len(*decoded.Data))
	for _, item := range *decoded.Data {
		native := nativeModelID(item.ID)
		if native == "" || strings.IndexFunc(native, invalidNativeRune) >= 0 {
			return nil, catalogInvalid()
		}
		if _, ok := seen[native]; ok {
			continue
		}
		seen[native] = struct{}{}
		models = append(models, modelInfo(native, item.Created, item.OwnedBy))
	}
	return models, nil
}

// discoverModels performs exactly one authenticated GET <base>/models through
// the host HTTP callback and returns the official catalog. There is no static
// snapshot, no last-good cache and no fallback: a missing credential is reported
// by the caller, and every transport, status or payload failure is surfaced.
func (m *Manager) discoverModels(cfg Config, key, callbackID string) ([]pluginapi.ModelInfo, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", PluginID+"/"+Version)
	resp, err := m.bridge.HTTPDo(http.MethodGet, joinURL(cfg.BaseURL, "/models"), headers, nil, callbackID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError(resp.StatusCode)
	}
	return decodeCatalog(resp.Body)
}
