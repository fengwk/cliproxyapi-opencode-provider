package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// sessionScopeVector recomputes the documented scope hash independently so the
// implementation is pinned to the agreed construction.
func sessionScopeVector(t *testing.T, authID, session string) string {
	t.Helper()
	payload, err := json.Marshal([]string{"opencode-go-session-v1", authID, session})
	if err != nil {
		t.Fatalf("vector encode: %v", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func configuredManager(t *testing.T, host *fakeHost, yaml string) *Manager {
	t.Helper()
	manager := newTestManager(host)
	raw, err := manager.HandleCall(pluginabi.MethodPluginRegister, registrationFor(t, yaml))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	decodeResult(t, raw, nil)
	return manager
}

func execBytes(t *testing.T, req pluginapi.ExecutorRequest, hostCallbackID string) []byte {
	t.Helper()
	payload, err := json.Marshal(rpcExecutorRequest{ExecutorRequest: req, HostCallbackID: hostCallbackID})
	if err != nil {
		t.Fatalf("encode executor request: %v", err)
	}
	return payload
}

func baseExecRequest() pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{
		AuthID:          "auth-1",
		AuthProvider:    ProviderID,
		Model:           "opencode-go/glm-5",
		Format:          "openai",
		SourceFormat:    "openai",
		Payload:         []byte(`{"model":"opencode-go/glm-5","messages":[{"role":"user","content":"hi"}]}`),
		OriginalRequest: []byte(`{"model":"opencode-go/glm-5","messages":[{"role":"user","content":"hi"}]}`),
		AuthAttributes:  map[string]string{"api_key": "sk-secret"},
		Metadata:        map[string]any{"canonical_session_id": "sess-1"},
	}
}

// TestExecuteSameFormatPassthroughAndHeaders verifies request headers, the
// deterministic session scope, prompt_cache_key consistency and header hygiene.
func TestExecuteSameFormatPassthroughAndHeaders(t *testing.T) {
	host := newFakeHost()
	url := "https://opencode.ai/zen/go/v1/chat/completions"
	host.httpResponses[url] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Length": {"7"}, "Content-Type": {"application/json"}, "X-Request-Id": {"r1"}},
		Body:       []byte(`{"id":"x"}`),
	}
	manager := configuredManager(t, host, "")
	req := baseExecRequest()
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, "cb-1"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp pluginapi.ExecutorResponse
	decodeResult(t, raw, &resp)
	if string(resp.Payload) != `{"id":"x"}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if _, leaked := resp.Headers["Content-Length"]; leaked {
		t.Fatalf("content-length must be dropped")
	}
	if resp.Headers.Get("X-Request-Id") != "r1" {
		t.Fatalf("safe headers should be forwarded: %+v", resp.Headers)
	}

	requests := host.requests()
	if len(requests) != 1 {
		t.Fatalf("expected one upstream request, got %d", len(requests))
	}
	out := requests[0]
	if out.URL != url {
		t.Fatalf("url = %s", out.URL)
	}
	if out.Headers.Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("missing bearer auth")
	}
	if out.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("missing content-type")
	}
	if out.Headers.Get("User-Agent") != PluginID+"/"+Version {
		t.Fatalf("user-agent = %q", out.Headers.Get("User-Agent"))
	}
	expectedScope, _ := sessionScope(req.AuthID, "sess-1")
	if out.Headers.Get("x-opencode-session") != expectedScope {
		t.Fatalf("session scope mismatch")
	}
	var body map[string]any
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatalf("outbound body is not json: %v", err)
	}
	if body["prompt_cache_key"] != expectedScope {
		t.Fatalf("prompt_cache_key = %v, want %s", body["prompt_cache_key"], expectedScope)
	}
	if body["model"] != "glm-5" {
		t.Fatalf("same-format upstream model must be the stripped native id, got %v", body["model"])
	}
}

// TestExecuteCrossProtocolTranslation translates an OpenAI request to the
// Claude endpoint and back using the CPA SDK translators.
func TestExecuteCrossProtocolTranslation(t *testing.T) {
	host := newFakeHost()
	url := "https://opencode.ai/zen/go/v1/messages"
	host.httpResponses[url] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"minimax-m2.5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`),
	}
	manager := configuredManager(t, host, "")
	req := baseExecRequest()
	req.Model = "opencode-go/minimax-m2.5"
	req.Payload = []byte(`{"model":"opencode-go/minimax-m2.5","messages":[{"role":"user","content":"hi"}]}`)
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp pluginapi.ExecutorResponse
	decodeResult(t, raw, &resp)
	if !strings.Contains(string(resp.Payload), `"choices"`) {
		t.Fatalf("expected openai chat completion, got %s", resp.Payload)
	}
	if !strings.Contains(string(resp.Payload), "hello") {
		t.Fatalf("translated content missing: %s", resp.Payload)
	}
	requests := host.requests()
	if got := requests[0].Headers.Get("x-api-key"); got != "sk-secret" {
		t.Fatalf("claude endpoint must use x-api-key, got %q", got)
	}
	if got := requests[0].Headers.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	var outBody map[string]any
	if err := json.Unmarshal(requests[0].Body, &outBody); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}
	if outBody["model"] != "minimax-m2.5" {
		t.Fatalf("cross-format upstream model must be stripped, got %v", outBody["model"])
	}
}

// TestExecuteRejections covers fail-closed routing and session/auth validation.
func TestExecuteRejections(t *testing.T) {
	manager := configuredManager(t, newFakeHost(), "")

	cases := []struct {
		name   string
		mutate func(*pluginapi.ExecutorRequest)
		status int
		code   string
	}{
		{"unknown-model", func(r *pluginapi.ExecutorRequest) { r.Model = "opencode-go/zzz-unknown" }, http.StatusNotFound, "unsupported_model"},
		{"unknown-source-format", func(r *pluginapi.ExecutorRequest) { r.Format = "bogus-format"; r.SourceFormat = "bogus-format" }, http.StatusBadRequest, "unsupported_protocol"},
		{"missing-session", func(r *pluginapi.ExecutorRequest) { r.Metadata = nil }, http.StatusBadRequest, "session_missing"},
		{"missing-auth-id", func(r *pluginapi.ExecutorRequest) { r.AuthID = "" }, http.StatusBadRequest, "session_missing"},
		{"missing-key", func(r *pluginapi.ExecutorRequest) { r.AuthAttributes = nil }, http.StatusUnauthorized, "auth_unavailable"},
		{"disabled", func(r *pluginapi.ExecutorRequest) { r.AuthMetadata = map[string]any{"disabled": true} }, http.StatusForbidden, "auth_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseExecRequest()
			tc.mutate(&req)
			raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, ""))
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			envErr := decodeError(t, raw)
			if envErr.StatusCode() != tc.status {
				t.Fatalf("status = %d, want %d", envErr.StatusCode(), tc.status)
			}
			if envErr.Code != tc.code {
				t.Fatalf("code = %q, want %q", envErr.Code, tc.code)
			}
			if strings.Contains(envErr.Message, "sk-") {
				t.Fatalf("error message leaked key material: %q", envErr.Message)
			}
		})
	}
}

// TestExecuteUpstreamErrorStatus verifies a non-2xx upstream status is surfaced
// with HTTPStatus so the host can retry before producing output.
func TestExecuteUpstreamErrorStatus(t *testing.T) {
	host := newFakeHost()
	url := "https://opencode.ai/zen/go/v1/chat/completions"
	host.httpResponses[url] = pluginapi.HTTPResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"slow down sk-secret"}`)}
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, baseExecRequest(), ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", envErr.StatusCode())
	}
	if strings.Contains(envErr.Message, "sk-") {
		t.Fatalf("upstream error body must not be echoed: %q", envErr.Message)
	}
}

// TestSessionScopeIsolation verifies the scope is deterministic per auth and
// canonical session, and differs across auths.
func TestSessionScopeIsolation(t *testing.T) {
	first, err := sessionScope("auth-1", "sess-1")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	again, _ := sessionScope("auth-1", "sess-1")
	if first != again {
		t.Fatalf("scope must be deterministic")
	}
	other, _ := sessionScope("auth-2", "sess-1")
	if other == first {
		t.Fatalf("scope must depend on the selected auth")
	}
	if _, err := sessionScope("", "sess-1"); err == nil {
		t.Fatalf("empty auth id must fail")
	}
	if _, err := sessionScope("auth-1", ""); err == nil {
		t.Fatalf("empty session must fail")
	}

	// The scope must match the documented hash construction.
	expected := sessionScopeVector(t, "auth-1", "sess-1")
	if first != expected {
		t.Fatalf("scope = %s, want %s", first, expected)
	}
}

func TestCountTokensAndHTTPRequestUnsupported(t *testing.T) {
	manager := configuredManager(t, newFakeHost(), "")
	for _, method := range []string{pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest} {
		raw, err := manager.HandleCall(method, execBytes(t, baseExecRequest(), ""))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		envErr := decodeError(t, raw)
		if envErr.Code != "unsupported" || envErr.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: %+v", method, envErr)
		}
	}
}
