//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// mockCall is one recorded upstream request.
type mockCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// mockOpenCode is a local stand-in for the OpenCode Go upstream. It validates
// the native request contract and serves deterministic fixtures.
type mockOpenCode struct {
	server *httptest.Server

	mu         sync.Mutex
	calls      []mockCall
	violations []string
	failedOnce map[string]bool
}

var (
	sessionHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mockModePattern   = regexp.MustCompile(`\[\[mock_mode=([a-z0-9_]+)\]\]`)
)

func newMockOpenCode(t *testing.T) *mockOpenCode {
	t.Helper()
	m := &mockOpenCode{failedOnce: map[string]bool{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *mockOpenCode) baseURL() string { return m.server.URL }
func (m *mockOpenCode) Close()          { m.server.Close() }

func (m *mockOpenCode) violationf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

// requireClean fails the test if the plugin sent any contract-violating request.
func (m *mockOpenCode) requireClean(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.violations {
		t.Errorf("upstream contract violation: %s", v)
	}
}

// snapshot returns a copy of every recorded call.
func (m *mockOpenCode) snapshot() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockCall(nil), m.calls...)
}

// callsForPath returns recorded calls to a specific native path.
func (m *mockOpenCode) callsForPath(path string) []mockCall {
	var out []mockCall
	for _, c := range m.snapshot() {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// reset clears recorded calls and violations between test phases.
func (m *mockOpenCode) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
	m.violations = nil
	m.failedOnce = map[string]bool{}
}

func (m *mockOpenCode) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := mockCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		m.handleModels(w, call)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		m.handleNative(w, call, "chat")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		m.handleNative(w, call, "claude")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		m.handleNative(w, call, "responses")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleModels serves the discovered catalog. Discovery uses the selected key.
func (m *mockOpenCode) handleModels(w http.ResponseWriter, call mockCall) {
	key := bearerKey(call.Header.Get("Authorization"))
	if key == "" {
		m.violationf("GET /v1/models missing Authorization header")
	} else if !isUpstreamKey(key) {
		m.violationf("GET /v1/models used unexpected credential %q", key)
	}
	data := map[string]any{"object": "list"}
	models := make([]any, 0, 3)
	for _, native := range []string{nativeGLM, nativeMinimax, nativeGPT} {
		models = append(models, map[string]any{"id": native, "object": "model", "created": 1791258569, "owned_by": "opencode"})
	}
	data["data"] = models
	writeJSON(w, http.StatusOK, data)
}

// handleNative validates and answers one native protocol request.
func (m *mockOpenCode) handleNative(w http.ResponseWriter, call mockCall, kind string) {
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil {
		m.violationf("%s request body is not JSON: %v", kind, err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	stream := bodyBool(body, "stream")
	session := call.Header.Get("x-opencode-session")

	m.validateNative(call, body, kind, session)

	mode := detectMockMode(call.Body)
	key := nativeKey(kind, call.Header)

	if mode == "fail_once" {
		failID := "fail_once|" + failOnceKey(call.Body)
		m.mu.Lock()
		already := m.failedOnce[failID]
		if !already {
			m.failedOnce[failID] = true
		}
		m.mu.Unlock()
		_ = key
		if !already {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{"message": "forced failover", "type": "rate_limit_error", "code": "rate_limit_exceeded"},
			})
			return
		}
	}

	if mode == "truncate" && stream {
		writeTruncatedSSE(w, chatSSEFixture(mockNativeModel(body)))
		return
	}

	if stream {
		var text string
		switch kind {
		case "chat":
			text = chatSSEFixture(nativeModelOr(body, nativeGLM))
		case "claude":
			text = claudeSSEFixture(nativeModelOr(body, nativeMinimax))
		default:
			text = responsesSSEFixture(nativeModelOr(body, nativeGPT))
		}
		writeChunkedSSE(w, text)
		return
	}

	switch kind {
	case "chat":
		writeJSON(w, http.StatusOK, json.RawMessage(chatNonStreamFixture(nativeModelOr(body, nativeGLM))))
	case "claude":
		writeJSON(w, http.StatusOK, json.RawMessage(claudeNonStreamFixture(nativeModelOr(body, nativeMinimax))))
	default:
		writeJSON(w, http.StatusOK, json.RawMessage(responsesNonStreamFixture(nativeModelOr(body, nativeGPT))))
	}
}

// validateNative enforces the upstream request contract.
func (m *mockOpenCode) validateNative(call mockCall, body map[string]any, kind, session string) {
	model, _ := body["model"].(string)
	if strings.HasPrefix(model, providerID+"/") {
		m.violationf("%s leaked public model prefix upstream: %q", kind, model)
	}
	if model == "" {
		m.violationf("%s request carried no model", kind)
	}
	if strings.Contains(model, providerID) {
		m.violationf("%s model %q still references the provider prefix", kind, model)
	}

	if !sessionHexPattern.MatchString(session) {
		m.violationf("%s x-opencode-session %q is not 64 lowercase hex", kind, session)
	}

	ua := call.Header.Get("User-Agent")
	switch {
	case ua == "":
		m.violationf("%s request sent no User-Agent", kind)
	case strings.HasPrefix(ua, "Go-http-client"):
		m.violationf("%s request sent a generic User-Agent %q", kind, ua)
	case !strings.Contains(ua, pluginID):
		m.violationf("%s User-Agent %q does not identify the plugin", kind, ua)
	}

	switch kind {
	case "chat":
		if len(bodyArray(body, "messages")) == 0 {
			m.violationf("chat request has no messages array")
		}
		requireBearerKey(m, kind, call.Header)
		if cache, _ := body["prompt_cache_key"].(string); cache != session {
			m.violationf("chat prompt_cache_key %q does not match x-opencode-session %q", cache, session)
		}
	case "claude":
		if len(bodyArray(body, "messages")) == 0 {
			m.violationf("claude request has no messages array")
		}
		if _, ok := body["max_tokens"]; !ok {
			m.violationf("claude request missing max_tokens")
		}
		key := call.Header.Get("x-api-key")
		if !isUpstreamKey(key) {
			m.violationf("claude request used unexpected x-api-key %q", key)
		}
		if call.Header.Get("Authorization") != "" {
			m.violationf("claude request should authenticate with x-api-key, not Authorization")
		}
		if version := call.Header.Get("anthropic-version"); version != "2023-06-01" {
			m.violationf("claude request anthropic-version %q", version)
		}
	case "responses":
		if len(bodyArray(body, "input")) == 0 {
			m.violationf("responses request has no input")
		}
		requireBearerKey(m, kind, call.Header)
		if cache, _ := body["prompt_cache_key"].(string); cache != session {
			m.violationf("responses prompt_cache_key %q does not match x-opencode-session %q", cache, session)
		}
		m.validateFlattenedTools(call.Body)
	}
}

// validateFlattenedTools asserts Responses tools use the flattened shape.
func (m *mockOpenCode) validateFlattenedTools(raw []byte) {
	var decoded struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return
	}
	for i, tool := range decoded.Tools {
		if _, nested := tool["function"]; nested {
			m.violationf("responses tool %d is not flattened (nested function object present)", i)
		}
		if typ, _ := tool["type"].(string); typ != "function" {
			m.violationf("responses tool %d has type %q, want function", i, typ)
		}
		if name, _ := tool["name"].(string); name == "" {
			m.violationf("responses tool %d has no flattened name", i)
		}
	}
}

func requireBearerKey(m *mockOpenCode, kind string, header http.Header) {
	key := bearerKey(header.Get("Authorization"))
	if !isUpstreamKey(key) {
		m.violationf("%s request used unexpected Authorization credential %q", kind, key)
	}
}

// --- helpers ---------------------------------------------------------------

func bodyArray(body map[string]any, key string) []any {
	value, _ := body[key].([]any)
	return value
}

func bodyBool(body map[string]any, key string) bool {
	value, _ := body[key].(bool)
	return value
}

func detectMockMode(body []byte) string {
	match := mockModePattern.FindSubmatch(body)
	if len(match) == 2 {
		return string(match[1])
	}
	return "full"
}

// failOnceKey identifies one logical client request across credential failover.
// The plugin scopes x-opencode-session to the selected auth, so a retried
// attempt carries a different upstream session and prompt_cache_key; those
// transient fields are excluded so "fail_once" still means exactly one upstream
// attempt rather than one per credential.
func failOnceKey(body []byte) string {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return string(body)
	}
	delete(decoded, "prompt_cache_key")
	return mustMarshalString(decoded)
}

func nativeKey(kind string, header http.Header) string {
	if kind == "claude" {
		return header.Get("x-api-key")
	}
	return bearerKey(header.Get("Authorization"))
}

func bearerKey(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[len("bearer "):])
	}
	return value
}

func isUpstreamKey(key string) bool { return key == keyAlpha || key == keyBeta }

func mockNativeModel(body map[string]any) string {
	model, _ := body["model"].(string)
	if model == "" {
		return nativeGLM
	}
	return model
}

func nativeModelOr(body map[string]any, fallback string) string {
	if model, _ := body["model"].(string); model != "" {
		return model
	}
	return fallback
}

func short(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeChunkedSSE streams an SSE body three bytes at a time so the plugin line
// framer must reassemble split lines (including multi-byte UTF-8).
func writeChunkedSSE(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for i := 0; i < len(text); i += 3 {
		end := i + 3
		if end > len(text) {
			end = len(text)
		}
		_, _ = w.Write([]byte(text[i:end]))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// writeTruncatedSSE hijacks the connection, writes a chunked prefix and drops
// the socket without the terminating chunk so the reader observes a hard error.
func writeTruncatedSSE(w http.ResponseWriter, text string) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	if len(text) > 0 {
		_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(text), text)
	}
	_ = buf.Flush()
}
