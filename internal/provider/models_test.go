package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestModelsForAuthDiscovery verifies per-auth discovery runs through host
// callbacks with the credential and merges into the static snapshot.
func TestModelsForAuthDiscovery(t *testing.T) {
	host := newFakeHost()
	host.httpResponses["https://opencode.ai/zen/go/v1/models"] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"id":"brand-new-model"}]}`),
	}
	manager := configuredManager(t, host, "")
	payload, _ := json.Marshal(struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id"`
	}{AuthModelRequest: pluginapi.AuthModelRequest{
		AuthID:       "a",
		AuthProvider: ProviderID,
		Metadata:     map[string]any{"api_key": "sk-meta"},
		Attributes:   map[string]string{"api_key": "sk-meta"},
	}, HostCallbackID: "cb"})
	raw, err := manager.HandleCall(pluginabi.MethodModelForAuth, payload)
	if err != nil {
		t.Fatalf("models for auth: %v", err)
	}
	var resp pluginapi.ModelResponse
	decodeResult(t, raw, &resp)
	found := false
	for _, model := range resp.Models {
		if model.ID == modelPrefix+"brand-new-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovered model missing from response")
	}
	requests := host.requests()
	if len(requests) != 1 || requests[0].Headers.Get("Authorization") != "Bearer sk-meta" {
		t.Fatalf("discovery request not authenticated: %+v", requests)
	}
}

// TestModelsForAuthWithoutKeySkipsNetwork verifies discovery degrades to the
// static snapshot when no credential is available.
func TestModelsForAuthWithoutKeySkipsNetwork(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	payload, _ := json.Marshal(pluginapi.AuthModelRequest{AuthProvider: ProviderID, AuthID: "a"})
	raw, err := manager.HandleCall(pluginabi.MethodModelForAuth, payload)
	if err != nil {
		t.Fatalf("models for auth: %v", err)
	}
	var resp pluginapi.ModelResponse
	decodeResult(t, raw, &resp)
	if len(resp.Models) == 0 {
		t.Fatalf("static fallback returned no models")
	}
	if len(host.requests()) != 0 {
		t.Fatalf("no network call should occur without a key")
	}
}

// TestConfiguredModelOverride verifies a configured protocol overrides family
// routing and adds the model to the published list.
func TestConfiguredModelOverride(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "models:\n  - id: custom\n    protocol: claude\n")
	raw, err := manager.HandleCall(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static: %v", err)
	}
	var models pluginapi.ModelResponse
	decodeResult(t, raw, &models)
	found := false
	for _, model := range models.Models {
		if model.ID == modelPrefix+"custom" {
			found = true
			if !model.UserDefined {
				t.Fatalf("configured model must be flagged user-defined")
			}
		}
	}
	if !found {
		t.Fatalf("configured model not published")
	}

	host.httpResponses["https://opencode.ai/zen/go/v1/messages"] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`),
	}
	req := baseExecRequest()
	req.Model = "opencode-go/custom"
	req.Payload = []byte(`{"model":"opencode-go/custom","messages":[{"role":"user","content":"hi"}]}`)
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp pluginapi.ExecutorResponse
	decodeResult(t, rawResp, &resp)
	if !strings.Contains(string(resp.Payload), `"choices"`) {
		t.Fatalf("override did not route to claude: %s", resp.Payload)
	}
}
