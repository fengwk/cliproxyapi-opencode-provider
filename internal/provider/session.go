package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// sessionScheme domain-separates the OpenCode Go session scope hash.
const sessionScheme = "opencode-go-session-v1"

// sessionHeader is the explicit client session signal adapted before auth.
const sessionHeader = "x-opencode-session"

// canonicalSessionID reads CPA's canonical session identity from plugin
// metadata. It is the primary, required source: the plugin never reconstructs
// sessions or hashes request content.
func canonicalSessionID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata["canonical_session_id"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// sessionScope derives the deterministic upstream session scope from the
// selected auth id and CPA's canonical session id.
func sessionScope(authID, canonical string) (string, error) {
	if strings.TrimSpace(authID) == "" {
		return "", errMissingAuthID
	}
	if strings.TrimSpace(canonical) == "" {
		return "", errMissingSession
	}
	encoded, err := json.Marshal([]string{sessionScheme, authID, canonical})
	if err != nil {
		return "", errSessionScope
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// headerValue performs a case-insensitive header lookup tolerant of the
// non-canonical key casing that can survive the RPC boundary.
func headerValue(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

// isOpenCodeModel reports whether a model id belongs to this provider.
func isOpenCodeModel(model string) bool {
	model = strings.TrimSpace(model)
	return strings.HasPrefix(model, modelPrefix)
}

// interceptBeforeAuth adapts an explicit x-opencode-session header into
// X-Session-Affinity so CPA can bind session affinity. It does nothing when the
// request is not an OpenCode model, when no explicit session is present, or when
// an affinity header already exists. The request body is never rewritten.
func interceptBeforeAuth(req interceptRequest) map[string][]string {
	if !isOpenCodeModel(req.Model) && !isOpenCodeModel(req.RequestedModel) {
		return nil
	}
	session := strings.TrimSpace(headerValue(req.Headers, sessionHeader))
	if session == "" {
		return nil
	}
	if strings.TrimSpace(headerValue(req.Headers, "X-Session-Affinity")) != "" {
		return nil
	}
	return map[string][]string{"X-Session-Affinity": {session}}
}
