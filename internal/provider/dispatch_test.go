package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRegisterNegotiatesSchema conservatively caps the schema at the host's
// offered version and advertises exactly the implemented capabilities.
func TestRegisterNegotiatesSchema(t *testing.T) {
	manager := newTestManager(newFakeHost())
	raw, err := manager.HandleCall(pluginabi.MethodPluginRegister, registrationFor(t, "base-url: https://opencode.ai/zen/go/v1\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registration
	decodeResult(t, raw, &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if !reg.Capabilities.Executor || !reg.Capabilities.ModelProvider || !reg.Capabilities.AuthProvider ||
		!reg.Capabilities.RequestInterceptor || !reg.Capabilities.ManagementAPI || !reg.Capabilities.QuotaProvider {
		t.Fatalf("missing required capability: %+v", reg.Capabilities)
	}
	if len(reg.Capabilities.ExecutorInputFormats) != 3 || len(reg.Capabilities.ExecutorOutputFormats) != 3 {
		t.Fatalf("executor formats not fully declared: %+v", reg.Capabilities)
	}
	if reg.Metadata.GitHubRepository == "" || reg.Metadata.Name != PluginName {
		t.Fatalf("invalid metadata: %+v", reg.Metadata)
	}
	if reg.Metadata.Version != Version {
		t.Fatalf("metadata version = %q, want %q", reg.Metadata.Version, Version)
	}
	// The display name must be distinct from, and must not degrade, the stable
	// plugin/provider identifiers that routing and file names depend on.
	if PluginName == PluginID || PluginName == ProviderID {
		t.Fatalf("display name collides with a technical ID: %q", PluginName)
	}
	if PluginID != "cliproxyapi-opencode-provider" || ProviderID != "opencode-go" {
		t.Fatalf("stable IDs changed: plugin=%q provider=%q", PluginID, ProviderID)
	}

	// A lower host schema must not be exceeded.
	low, err := json.Marshal(map[string]any{"schema_version": 3, "config_yaml": []byte("{}")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	raw, _ = manager.HandleCall(pluginabi.MethodPluginRegister, low)
	var regLow registration
	decodeResult(t, raw, &regLow)
	if regLow.SchemaVersion != 3 {
		t.Fatalf("negotiated schema = %d, want 3", regLow.SchemaVersion)
	}
}

// TestRegisterRejectsUnsafeBaseURL verifies config validation at registration.
func TestRegisterRejectsUnsafeBaseURL(t *testing.T) {
	manager := newTestManager(newFakeHost())
	cases := []string{
		"base-url: http://evil.example/v1\n",
		"base-url: ftp://example.com/v1\n",
		"base-url: https://user:pass@example.com/v1\n",
		"base-url: https://example.com/v1?x=1\n",
		"models:\n  - id: m\n    protocol: bogus\n",
	}
	for _, body := range cases {
		raw, err := manager.HandleCall(pluginabi.MethodPluginRegister, registrationFor(t, body))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := decodeError(t, raw); err.StatusCode() != http.StatusBadRequest {
			t.Fatalf("body %q: got status %d, want 400", body, err.StatusCode())
		}
	}
	// Loopback http is explicitly allowed for local mocks.
	raw, _ := manager.HandleCall(pluginabi.MethodPluginRegister, registrationFor(t, "base-url: http://127.0.0.1:8080/v1\n"))
	decodeResult(t, raw, nil)
}

func TestUnknownMethod(t *testing.T) {
	manager := newTestManager(newFakeHost())
	raw, err := manager.HandleCall("does.not.exist", nil)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if envErr := decodeError(t, raw); envErr.Code != "unknown_method" {
		t.Fatalf("code = %q, want unknown_method", envErr.Code)
	}
}

// TestStaticModelsCoverFamilies verifies the embedded snapshot is published
// under the fixed public prefix with deterministic, non-nil ids.
func TestStaticModelsCoverFamilies(t *testing.T) {
	manager := newTestManager(newFakeHost())
	raw, err := manager.HandleCall(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static: %v", err)
	}
	var resp pluginapi.ModelResponse
	decodeResult(t, raw, &resp)
	if resp.Provider != ProviderID {
		t.Fatalf("provider = %q, want %q", resp.Provider, ProviderID)
	}
	if len(resp.Models) == 0 {
		t.Fatalf("no models in snapshot")
	}
	found := false
	for _, model := range resp.Models {
		if !strings.HasPrefix(model.ID, modelPrefix) {
			t.Fatalf("model %q missing public prefix", model.ID)
		}
		if model.Name == "" {
			t.Fatalf("model %q missing native name", model.ID)
		}
		if strings.Contains(model.ID, "glm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot missing expected glm family model")
	}
}

// TestInterceptAdaptsExplicitSession verifies the before-auth interceptor only
// adapts x-opencode-session into X-Session-Affinity for OpenCode models.
func TestInterceptAdaptsExplicitSession(t *testing.T) {
	manager := newTestManager(newFakeHost())
	headers := http.Header{}
	headers.Set("x-opencode-session", "sess-123")
	payload, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		Model:   "opencode-go/glm-5",
		Headers: headers,
		Body:    []byte(`{"model":"opencode-go/glm-5"}`),
	})
	raw, err := manager.HandleCall(pluginabi.MethodRequestInterceptBefore, payload)
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	var resp pluginapi.RequestInterceptResponse
	decodeResult(t, raw, &resp)
	if got := headerValue(resp.Headers, "X-Session-Affinity"); got != "sess-123" {
		t.Fatalf("affinity = %q, want sess-123", got)
	}
	if len(resp.Body) != 0 {
		t.Fatalf("interceptor must not rewrite the body")
	}
	if resp.Terminate {
		t.Fatalf("interceptor must not terminate")
	}

	// A non-OpenCode model is left untouched.
	payload, _ = json.Marshal(pluginapi.RequestInterceptRequest{Model: "other/model", Headers: headers})
	raw, _ = manager.HandleCall(pluginabi.MethodRequestInterceptBefore, payload)
	var untouched pluginapi.RequestInterceptResponse
	decodeResult(t, raw, &untouched)
	if len(untouched.Headers) != 0 {
		t.Fatalf("non-OpenCode model must not be adapted: %+v", untouched.Headers)
	}

	// An existing affinity header is preserved.
	headers.Set("X-Session-Affinity", "explicit")
	payload, _ = json.Marshal(pluginapi.RequestInterceptRequest{Model: "opencode-go/glm-5", Headers: headers})
	raw, _ = manager.HandleCall(pluginabi.MethodRequestInterceptBefore, payload)
	var preserved pluginapi.RequestInterceptResponse
	decodeResult(t, raw, &preserved)
	if len(preserved.Headers) != 0 {
		t.Fatalf("existing affinity must be preserved, got %+v", preserved.Headers)
	}

	// No explicit session: the interceptor must not synthesize affinity.
	payload, _ = json.Marshal(pluginapi.RequestInterceptRequest{Model: "opencode-go/glm-5", Headers: http.Header{}})
	raw, _ = manager.HandleCall(pluginabi.MethodRequestInterceptBefore, payload)
	var synthetic pluginapi.RequestInterceptResponse
	decodeResult(t, raw, &synthetic)
	if len(synthetic.Headers) != 0 {
		t.Fatalf("no header must be synthesized without an explicit session: %+v", synthetic.Headers)
	}
	if headerValue(synthetic.Headers, "X-Session-Affinity") != "" {
		t.Fatalf("interceptor must not fabricate org/session affinity")
	}
}
