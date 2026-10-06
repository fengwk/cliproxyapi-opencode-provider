package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelsDiscoveryURL is the upstream catalog endpoint for the default base URL.
const modelsDiscoveryURL = "https://opencode.ai/zen/go/v1/models"

// modelDiscoveryPayload encodes a host model.for_auth call carrying the given
// credential material.
func modelDiscoveryPayload(t *testing.T, attributes map[string]string, metadata map[string]any, callbackID string) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id"`
	}{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:       "a",
			AuthProvider: ProviderID,
			Metadata:     metadata,
			Attributes:   attributes,
		},
		HostCallbackID: callbackID,
	})
	if err != nil {
		t.Fatalf("encode discovery request: %v", err)
	}
	return payload
}

// callModelDiscovery issues one model.for_auth RPC and returns the raw envelope.
func callModelDiscovery(t *testing.T, manager *Manager, payload []byte) []byte {
	t.Helper()
	raw, err := manager.HandleCall(pluginabi.MethodModelForAuth, payload)
	if err != nil {
		t.Fatalf("model.for_auth: %v", err)
	}
	return raw
}

// modelIDs projects the published ids for order-sensitive assertions.
func modelIDs(models []pluginapi.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestModelsForAuthPublishesOnlyOfficialCatalog proves the catalog mirrors the
// authenticated upstream response exactly: official order and metadata are
// preserved, every entry is non-user-defined, and a configured override never
// adds a phantom id to the published list.
func TestModelsForAuthPublishesOnlyOfficialCatalog(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body: []byte(`{"object":"list","data":[
			{"id":"glm-5.2","object":"model","created":111,"owned_by":"opencode"},
			{"id":"gpt-5.6-luna","object":"model","created":222,"owned_by":"vendored"}
		]}`),
	}
	manager := configuredManager(t, host, "models:\n  - id: phantom\n    protocol: claude\n")
	raw := callModelDiscovery(t, manager, modelDiscoveryPayload(t, map[string]string{"api_key": "sk-meta"}, nil, "cb"))
	var resp pluginapi.ModelResponse
	decodeResult(t, raw, &resp)

	if got, want := modelIDs(resp.Models), []string{modelPrefix + "glm-5.2", modelPrefix + "gpt-5.6-luna"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("published ids = %v, want %v", got, want)
	}
	first := resp.Models[0]
	if first.Object != "model" || first.Created != 111 || first.OwnedBy != "opencode" ||
		first.Name != "glm-5.2" || first.DisplayName != "glm-5.2" {
		t.Fatalf("official metadata not preserved: %+v", first)
	}
	if resp.Models[1].OwnedBy != "vendored" || resp.Models[1].Created != 222 {
		t.Fatalf("second official metadata not preserved: %+v", resp.Models[1])
	}
	for _, model := range resp.Models {
		if model.UserDefined {
			t.Fatalf("official model %q must not be user-defined", model.ID)
		}
		if model.ID == modelPrefix+"phantom" {
			t.Fatalf("configured override must not add a phantom model")
		}
	}

	requests := host.requests()
	if len(requests) != 1 {
		t.Fatalf("discovery requests = %d, want exactly 1", len(requests))
	}
	if requests[0].Method != http.MethodGet || requests[0].URL != modelsDiscoveryURL {
		t.Fatalf("unexpected discovery request: %+v", requests[0])
	}
	if got := requests[0].Headers.Get("Authorization"); got != "Bearer sk-meta" {
		t.Fatalf("discovery authorization = %q, want Bearer sk-meta", got)
	}
}

// TestModelsForAuthPublishesLatestResponse proves each successful discovery is
// authoritative: a shortened response removes retired models and an empty list
// clears the catalog, while still reporting success and preserving order.
func TestModelsForAuthPublishesLatestResponse(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	payload := modelDiscoveryPayload(t, map[string]string{"api_key": "sk-meta"}, nil, "cb")

	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"id":"glm-5.2"},{"id":"gpt-5.6-luna"}]}`),
	}
	var first pluginapi.ModelResponse
	decodeResult(t, callModelDiscovery(t, manager, payload), &first)
	if got, want := modelIDs(first.Models), []string{modelPrefix + "glm-5.2", modelPrefix + "gpt-5.6-luna"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial catalog = %v, want %v", got, want)
	}

	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"id":"glm-5.2"}]}`),
	}
	var second pluginapi.ModelResponse
	decodeResult(t, callModelDiscovery(t, manager, payload), &second)
	if got, want := modelIDs(second.Models), []string{modelPrefix + "glm-5.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retired model not removed: %v, want %v", got, want)
	}

	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[]}`),
	}
	var third pluginapi.ModelResponse
	decodeResult(t, callModelDiscovery(t, manager, payload), &third)
	if third.Models == nil || len(third.Models) != 0 {
		t.Fatalf("empty data must succeed with an empty catalog, got %#v", third.Models)
	}
}

// TestModelsForAuthDedupesAndPrefixesOnce proves duplicate native ids collapse
// and the public namespace is applied exactly once even for a prefixed upstream
// id, preserving the first occurrence order.
func TestModelsForAuthDedupesAndPrefixesOnce(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"id":"opencode-go/glm-5.2","created":7},{"id":"glm-5.2","created":9}]}`),
	}
	manager := configuredManager(t, host, "")
	var resp pluginapi.ModelResponse
	decodeResult(t, callModelDiscovery(t, manager, modelDiscoveryPayload(t, map[string]string{"api_key": "sk-meta"}, nil, "cb")), &resp)
	if got, want := modelIDs(resp.Models), []string{modelPrefix + "glm-5.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("published ids = %v, want %v", got, want)
	}
	if resp.Models[0].Created != 7 {
		t.Fatalf("first occurrence must win: %+v", resp.Models[0])
	}
}

// TestModelsForAuthWithoutKeySkipsNetwork proves a missing credential is an
// auth failure: no network call, no static fallback, and a 401 envelope.
func TestModelsForAuthWithoutKeySkipsNetwork(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	raw := callModelDiscovery(t, manager, modelDiscoveryPayload(t, nil, nil, "cb"))
	envErr := decodeError(t, raw)
	if envErr.Code != "auth_unavailable" || envErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("error = %+v, want auth_unavailable 401", envErr)
	}
	if len(host.requests()) != 0 {
		t.Fatalf("no network call should occur without a key")
	}
}

// TestModelsForAuthFailuresAreSanitized proves transport errors, non-2xx
// statuses and malformed payloads surface as sanitized error envelopes rather
// than a successful, empty or fallback catalog.
func TestModelsForAuthFailuresAreSanitized(t *testing.T) {
	const secret = "sk-leak-canary"
	ok := func(body string) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body)}
	}
	cases := []struct {
		name   string
		setup  func(host *fakeHost)
		code   string
		status int
	}{
		{"transport error", func(host *fakeHost) { host.httpErrors[modelsDiscoveryURL] = errors.New(secret) }, "host_call_failed", 0},
		{"upstream 503", func(host *fakeHost) {
			host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte(secret)}
		}, "upstream_error", http.StatusServiceUnavailable},
		{"upstream 401", func(host *fakeHost) {
			host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(secret)}
		}, "upstream_error", http.StatusUnauthorized},
		{"not json", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(secret) }, "invalid_upstream", http.StatusBadGateway},
		{"top-level array", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`[]`) }, "invalid_upstream", http.StatusBadGateway},
		{"missing data", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"object":"list"}`) }, "invalid_upstream", http.StatusBadGateway},
		{"null data", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"data":null}`) }, "invalid_upstream", http.StatusBadGateway},
		{"non-array data", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"data":"` + secret + `"}`) }, "invalid_upstream", http.StatusBadGateway},
		{"blank id", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"data":[{"id":"   "}]}`) }, "invalid_upstream", http.StatusBadGateway},
		{"internal whitespace", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"data":[{"id":"glm 5.2"}]}`) }, "invalid_upstream", http.StatusBadGateway},
		{"control character", func(host *fakeHost) { host.httpResponses[modelsDiscoveryURL] = ok(`{"data":[{"id":"glm\u00005.2"}]}`) }, "invalid_upstream", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			tc.setup(host)
			manager := configuredManager(t, host, "")
			raw := callModelDiscovery(t, manager, modelDiscoveryPayload(t, map[string]string{"api_key": secret}, nil, "cb"))
			envErr := decodeError(t, raw)
			if envErr.Code != tc.code || envErr.StatusCode() != tc.status {
				t.Fatalf("error = %+v, want %s %d", envErr, tc.code, tc.status)
			}
			if strings.Contains(envErr.Message, secret) {
				t.Fatalf("error leaked upstream material: %q", envErr.Message)
			}
			if tc.code == "invalid_upstream" && envErr.Message != "upstream model catalog is invalid" {
				t.Fatalf("malformed message = %q, want fixed text", envErr.Message)
			}
		})
	}
}

// TestConfiguredProtocolOverrideRoutesDiscoveredModel proves a configured
// override keeps its executor behavior for an officially discovered model: the
// request is translated to the overridden upstream protocol while the model
// remains published as an official (non-user-defined) entry.
func TestConfiguredProtocolOverrideRoutesDiscoveredModel(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"data":[{"id":"custom","created":5,"owned_by":"opencode"}]}`),
	}
	manager := configuredManager(t, host, "models:\n  - id: custom\n    protocol: claude\n")

	var catalog pluginapi.ModelResponse
	decodeResult(t, callModelDiscovery(t, manager, modelDiscoveryPayload(t, map[string]string{"api_key": "sk-meta"}, nil, "cb")), &catalog)
	if got, want := modelIDs(catalog.Models), []string{modelPrefix + "custom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("published ids = %v, want %v", got, want)
	}
	if catalog.Models[0].UserDefined {
		t.Fatalf("officially discovered model must not be user-defined")
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
