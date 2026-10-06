package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-opencode-provider/internal/web"
)

const (
	keysRoute = "/plugins/" + PluginID + "/keys"

	maxImportKeys    = 100
	maxImportBody    = 64 << 10
	maxKeyLength     = 4096
	maxLabelLength   = 200
	maxStatusLength  = 200
	maxNameLength    = 256
	authFileSuffix   = ".json"
	authResourcePath = "/v0/resource/plugins/" + PluginID
	managementPath   = "/v0/management"
)

// managementRegistration declares the plugin-owned Management API and resource
// routes. Resource routes are public and secret-free; the key routes are
// registered under the authenticated Management API prefix.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: keysRoute, Description: "List OpenCode Go credentials."},
			{Method: http.MethodPost, Path: keysRoute, Description: "Import OpenCode Go API keys."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: PluginName, Description: PluginName + " key management for the OpenCode Go upstream."},
			{Path: "/ui.js", Description: PluginName + " key management script."},
			{Path: "/ui.css", Description: PluginName + " key management styles."},
		},
	}
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return mustEnvelope(errorResult("invalid_request", "malformed management request", 0))
	}
	path := strings.TrimSpace(req.Path)
	switch {
	case req.Method == http.MethodGet && path == managementPath+keysRoute:
		return m.listKeys()
	case req.Method == http.MethodPost && path == managementPath+keysRoute:
		return m.importKeys(req.Body)
	case req.Method == http.MethodGet && path == authResourcePath+"/ui":
		return resourceResponse("ui.html")
	case req.Method == http.MethodGet && path == authResourcePath+"/ui.js":
		return resourceResponse("ui.js")
	case req.Method == http.MethodGet && path == authResourcePath+"/ui.css":
		return resourceResponse("ui.css")
	default:
		return managementJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func resourceResponse(name string) ([]byte, error) {
	body, contentType, ok := web.Asset(name)
	if !ok {
		return managementRaw(http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found"))
	}
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	// CPA embeds plugin pages in a same-origin iframe; other origins stay blocked.
	headers.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Cache-Control", "no-store")
	headers.Set("Referrer-Policy", "no-referrer")
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body})
}

func managementRaw(status int, contentType string, body []byte) ([]byte, error) {
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body})
}

func managementJSON(status int, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return managementRaw(http.StatusInternalServerError, "text/plain; charset=utf-8", []byte("internal error"))
	}
	return managementRaw(status, "application/json; charset=utf-8", body)
}

// keyFileEntry is one sanitized credential summary returned by the list route.
type keyFileEntry struct {
	Name        string `json:"name"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
}

func (m *Manager) listKeys() ([]byte, error) {
	entries, err := m.bridge.AuthList()
	if err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to list credentials"})
	}
	files := make([]keyFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != ProviderID && entry.Provider != ProviderID {
			continue
		}
		files = append(files, keyFileEntry{
			Name:        sanitizeString(entry.Name, maxNameLength),
			AuthIndex:   sanitizeString(entry.AuthIndex, maxNameLength),
			Label:       sanitizeString(entry.Label, maxLabelLength),
			Status:      sanitizeString(entry.Status, maxStatusLength),
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			Success:     entry.Success,
			Failed:      entry.Failed,
		})
	}
	return managementJSON(http.StatusOK, map[string]any{"files": files})
}

type importRequest struct {
	Keys  []string `json:"keys"`
	Label string   `json:"label"`
}

func (m *Manager) importKeys(body []byte) ([]byte, error) {
	if len(body) > maxImportBody {
		return managementJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	}
	var req importRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid json body"})
	}
	keys, errValidate := validateImportKeys(req.Keys)
	if errValidate != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": errValidate.Error()})
	}
	label := sanitizeString(req.Label, maxLabelLength)

	existing := map[string]struct{}{}
	if entries, errList := m.bridge.AuthList(); errList == nil {
		for _, entry := range entries {
			if name := strings.TrimSpace(entry.Name); name != "" {
				existing[name] = struct{}{}
			}
			if id := strings.TrimSpace(entry.ID); id != "" {
				existing[id] = struct{}{}
			}
		}
	}
	imported, skipped, failed := 0, 0, 0
	for _, key := range keys {
		name := keyFileName(key)
		if _, ok := existing[name]; ok {
			skipped++
			continue
		}
		payload := map[string]any{"type": ProviderID, "api_key": key}
		if label != "" {
			payload["label"] = label
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			failed++
			continue
		}
		if errSave := m.bridge.AuthSave(name, raw); errSave != nil {
			failed++
			continue
		}
		existing[name] = struct{}{}
		imported++
	}
	status := http.StatusOK
	if failed > 0 {
		status = http.StatusMultiStatus
	}
	return managementJSON(status, map[string]int{"imported": imported, "skipped": skipped, "failed": failed})
}

// validateImportKeys validates the whole input before any persistence and
// returns the deduplicated key list.
func validateImportKeys(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("keys must not be empty")
	}
	if len(raw) > maxImportKeys {
		return nil, fmt.Errorf("too many keys")
	}
	seen := make(map[string]struct{}, len(raw))
	keys := make([]string, 0, len(raw))
	for _, candidate := range raw {
		key := strings.TrimSpace(candidate)
		if key == "" {
			return nil, fmt.Errorf("keys must not contain empty values")
		}
		if len(key) > maxKeyLength {
			return nil, fmt.Errorf("a key exceeds the maximum length")
		}
		if strings.IndexFunc(key, isUnsafeKeyRune) >= 0 {
			return nil, fmt.Errorf("a key contains whitespace or control characters")
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func isUnsafeKeyRune(r rune) bool {
	return r <= ' ' || r == 0x7f || unicode.IsControl(r)
}

// keyFileName derives the stable physical auth file name from a key hash.
func keyFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return ProviderID + "-" + hex.EncodeToString(sum[:]) + authFileSuffix
}

// sanitizeString trims and bounds a host-provided display string.
func sanitizeString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}
