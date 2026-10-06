package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	quotaResetRolling = "2026-10-07T00:00:00Z"
	quotaResetWeekly  = "2026-10-12T00:00:00Z"
	quotaResetMonthly = "2026-11-01T00:00:00Z"
)

// quotaWindowPayload builds one official usage window for fake upstream bodies.
func quotaWindowPayload(status string, percent float64, resetsAt string) map[string]any {
	return map[string]any{"status": status, "percent": percent, "resetsAt": resetsAt}
}

// validQuotaUsage is a complete, valid three-window usage object.
func validQuotaUsage() map[string]any {
	return map[string]any{
		"rolling": quotaWindowPayload("ok", 20, quotaResetRolling),
		"weekly":  quotaWindowPayload("ok", 40, quotaResetWeekly),
		"monthly": quotaWindowPayload("ok", 60, quotaResetMonthly),
	}
}

func quotaBody(t *testing.T, usage any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"usage": usage})
	if err != nil {
		t.Fatalf("encode quota body: %v", err)
	}
	return raw
}

func quotaFetchBytes(t *testing.T, req rpcQuotaRequest) []byte {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode quota request: %v", err)
	}
	return raw
}

// TestQuotaCapabilityRegisteredAndDescribed verifies the host can discover the
// quota provider on both register and reconfigure and that the advertised
// contract is exactly the single opencode-go provider with reset unsupported.
func TestQuotaCapabilityRegisteredAndDescribed(t *testing.T) {
	manager := newTestManager(newFakeHost())
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		raw, err := manager.HandleCall(method, registrationFor(t, "base-url: https://opencode.ai/zen/go/v1\n"))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var reg registration
		decodeResult(t, raw, &reg)
		if !reg.Capabilities.QuotaProvider {
			t.Fatalf("%s did not advertise quota_provider", method)
		}
	}

	raw, _ := manager.HandleCall(pluginabi.MethodQuotaIdentifier, nil)
	var identifier struct {
		Identifier string `json:"identifier"`
	}
	decodeResult(t, raw, &identifier)
	if identifier.Identifier != ProviderID {
		t.Fatalf("quota identifier = %q, want %q", identifier.Identifier, ProviderID)
	}

	raw, _ = manager.HandleCall(pluginabi.MethodQuotaDescribe, []byte(`{"plugin":{}}`))
	var desc pluginapi.QuotaDescribeResponse
	decodeResult(t, raw, &desc)
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != ProviderID {
		t.Fatalf("supported providers = %v, want [%s]", desc.SupportedProviders, ProviderID)
	}
	if desc.DisplayName != quotaDisplayName {
		t.Fatalf("display name = %q, want %q", desc.DisplayName, quotaDisplayName)
	}
	if desc.SupportsReset {
		t.Fatalf("reset must not be advertised")
	}
}

// TestQuotaFetchJoinsBasePathAndUsesSelectedKey verifies the single read-only
// GET targets the configured base path with the selected credential, the
// plugin-owned headers, and the exact host callback id.
func TestQuotaFetchJoinsBasePathAndUsesSelectedKey(t *testing.T) {
	const usageURL = "https://proxy.example/zen/go/v1/usage"
	host := newFakeHost()
	host.httpResponses[usageURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: quotaBody(t, validQuotaUsage())}
	manager := configuredManager(t, host, "base-url: https://proxy.example/zen/go/v1\n")

	raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, rpcQuotaRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{
			AuthIndex:  "idx-1",
			AuthID:     "auth-1",
			Provider:   ProviderID,
			Attributes: map[string]string{"api_key": "sk-fake-selected"},
		},
		HostCallbackID: "cb-9",
	}))
	if err != nil {
		t.Fatalf("quota fetch: %v", err)
	}
	var resp pluginapi.QuotaFetchResponse
	decodeResult(t, raw, &resp)

	requests := host.requests()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(requests))
	}
	got := requests[0]
	if got.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", got.Method)
	}
	if got.URL != usageURL {
		t.Fatalf("url = %q, want %q", got.URL, usageURL)
	}
	if got.HostCallbackID != "cb-9" {
		t.Fatalf("callback id = %q, want cb-9", got.HostCallbackID)
	}
	if got.Headers.Get("Authorization") != "Bearer sk-fake-selected" {
		t.Fatalf("authorization = %q", got.Headers.Get("Authorization"))
	}
	if got.Headers.Get("Accept") != "application/json" {
		t.Fatalf("accept = %q, want application/json", got.Headers.Get("Accept"))
	}
	if got.Headers.Get("User-Agent") != PluginID+"/"+Version {
		t.Fatalf("user-agent = %q", got.Headers.Get("User-Agent"))
	}
	if len(got.Body) != 0 {
		t.Fatalf("usage GET must not carry a body")
	}

	if resp.Subscription == nil || resp.Subscription.Plan != quotaDisplayName {
		t.Fatalf("subscription = %+v, want plan %q", resp.Subscription, quotaDisplayName)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].DisplayName != quotaDisplayName {
		t.Fatalf("groups = %+v, want one %q group", resp.Groups, quotaDisplayName)
	}
	assertBuckets(t, resp.Groups[0].Buckets, [][2]any{
		{"rolling", 0.8},
		{"weekly", 0.6},
		{"monthly", 0.4},
	}, []string{quotaResetRolling, quotaResetWeekly, quotaResetMonthly})
}

// TestQuotaFetchKeyFallbacks verifies the documented credential lookup order and
// that a missing key fails safely without any upstream call.
func TestQuotaFetchKeyFallbacks(t *testing.T) {
	const usageURL = "https://opencode.ai/zen/go/v1/usage"
	cases := []struct {
		name string
		req  rpcQuotaRequest
		want string
	}{
		{
			name: "attributes win",
			req: rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
				Provider:   ProviderID,
				Attributes: map[string]string{"api_key": "sk-fake-attr"},
				Metadata:   map[string]any{"api_key": "sk-fake-meta"},
			}},
			want: "sk-fake-attr",
		},
		{
			name: "metadata fallback",
			req: rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
				Provider: ProviderID,
				Metadata: map[string]any{"api_key": "sk-fake-meta"},
			}},
			want: "sk-fake-meta",
		},
		{
			name: "storage fallback",
			req: rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
				Provider:    ProviderID,
				StorageJSON: []byte(`{"api_key":"sk-fake-store"}`),
			}},
			want: "sk-fake-store",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			host.httpResponses[usageURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: quotaBody(t, validQuotaUsage())}
			manager := configuredManager(t, host, "")
			raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, tc.req))
			if err != nil {
				t.Fatalf("quota fetch: %v", err)
			}
			decodeResult(t, raw, nil)
			requests := host.requests()
			if len(requests) != 1 || requests[0].Headers.Get("Authorization") != "Bearer "+tc.want {
				t.Fatalf("authorization not derived from fallback: %+v", requests)
			}
		})
	}

	host := newFakeHost()
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, rpcQuotaRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID},
	}))
	if err != nil {
		t.Fatalf("quota fetch: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.Code != "auth_unavailable" || envErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("missing key error = %+v, want auth_unavailable 401", envErr)
	}
	if len(host.callLog()) != 0 || len(host.requests()) != 0 {
		t.Fatalf("missing key must not reach the host: %v", host.callLog())
	}
}

// TestQuotaFetchPercentageMapping covers the 0/100 boundaries, the rate-limited
// override, and exact reset timestamp preservation.
func TestQuotaFetchPercentageMapping(t *testing.T) {
	const usageURL = "https://opencode.ai/zen/go/v1/usage"
	usage := map[string]any{
		"rolling": quotaWindowPayload("ok", 0, quotaResetRolling),
		"weekly":  quotaWindowPayload("ok", 100, quotaResetWeekly),
		"monthly": quotaWindowPayload("rate-limited", 10, quotaResetMonthly),
	}
	host := newFakeHost()
	host.httpResponses[usageURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: quotaBody(t, usage)}
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, rpcQuotaRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-fake"}},
	}))
	if err != nil {
		t.Fatalf("quota fetch: %v", err)
	}
	var resp pluginapi.QuotaFetchResponse
	decodeResult(t, raw, &resp)
	assertBuckets(t, resp.Groups[0].Buckets, [][2]any{
		{"rolling", 1.0},
		{"weekly", 0.0},
		{"monthly", 0.0},
	}, []string{quotaResetRolling, quotaResetWeekly, quotaResetMonthly})
}

// TestQuotaFetchRejectsInvalidPayloads verifies every malformed upstream shape
// becomes a sanitized 502 that never echoes the payload.
func TestQuotaFetchRejectsInvalidPayloads(t *testing.T) {
	const usageURL = "https://opencode.ai/zen/go/v1/usage"
	validWindow := func() map[string]any { return quotaWindowPayload("ok", 10, quotaResetRolling) }
	complete := func(rolling, weekly, monthly any) map[string]any {
		return map[string]any{"rolling": rolling, "weekly": weekly, "monthly": monthly}
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"not json", []byte("sk-fake-secret not-json")},
		{"empty object", []byte(`{}`)},
		{"null usage", []byte(`{"usage":null}`)},
		{"missing rolling", quotaBody(t, map[string]any{"weekly": validWindow(), "monthly": validWindow()})},
		{"null window", quotaBody(t, complete(nil, validWindow(), validWindow()))},
		{"window is string", quotaBody(t, complete("ok", validWindow(), validWindow()))},
		{"missing percent", quotaBody(t, complete(map[string]any{"status": "ok", "resetsAt": quotaResetRolling}, validWindow(), validWindow()))},
		{"null percent", quotaBody(t, complete(map[string]any{"status": "ok", "percent": nil, "resetsAt": quotaResetRolling}, validWindow(), validWindow()))},
		{"percent 101", quotaBody(t, complete(quotaWindowPayload("ok", 101, quotaResetRolling), validWindow(), validWindow()))},
		{"percent negative", quotaBody(t, complete(quotaWindowPayload("ok", -1, quotaResetRolling), validWindow(), validWindow()))},
		{"unknown status", quotaBody(t, complete(quotaWindowPayload("limited", 10, quotaResetRolling), validWindow(), validWindow()))},
		{"missing reset", quotaBody(t, complete(map[string]any{"status": "ok", "percent": 10}, validWindow(), validWindow()))},
		{"invalid reset", quotaBody(t, complete(quotaWindowPayload("ok", 10, "not-a-time"), validWindow(), validWindow()))},
		{"secret in body", quotaBody(t, complete(quotaWindowPayload("ok", 10, "sk-fake-secret"), validWindow(), validWindow()))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			host.httpResponses[usageURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: tc.body}
			manager := configuredManager(t, host, "")
			raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, rpcQuotaRequest{
				QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-fake"}},
			}))
			if err != nil {
				t.Fatalf("quota fetch: %v", err)
			}
			envErr := decodeError(t, raw)
			if envErr.Code != "invalid_upstream" || envErr.StatusCode() != http.StatusBadGateway {
				t.Fatalf("error = %+v, want invalid_upstream 502", envErr)
			}
			if envErr.Message != "upstream quota response is invalid" {
				t.Fatalf("message = %q, want fixed text", envErr.Message)
			}
			if strings.Contains(envErr.Message, "sk-fake") {
				t.Fatalf("upstream material leaked: %q", envErr.Message)
			}
		})
	}
}

// TestQuotaFetchForeignProviderRejected verifies an explicitly non-OpenCode Go
// provider is refused before any key resolution or upstream call.
func TestQuotaFetchForeignProviderRejected(t *testing.T) {
	for _, attributes := range []map[string]string{nil, {"api_key": "sk-fake-secret"}} {
		host := newFakeHost()
		manager := configuredManager(t, host, "")
		raw, err := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, rpcQuotaRequest{
			QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "other-provider", Attributes: attributes},
		}))
		if err != nil {
			t.Fatalf("quota fetch: %v", err)
		}
		envErr := decodeError(t, raw)
		if envErr.Code != "unsupported_provider" || envErr.StatusCode() != http.StatusBadRequest {
			t.Fatalf("error = %+v, want unsupported_provider 400", envErr)
		}
		if strings.Contains(envErr.Message, "other-provider") || strings.Contains(envErr.Message, "sk-fake") {
			t.Fatalf("foreign provider material leaked: %q", envErr.Message)
		}
		if len(host.callLog()) != 0 {
			t.Fatalf("foreign provider must not reach the host: %v", host.callLog())
		}
	}
}

// TestQuotaFetchUpstreamFailuresSanitized verifies host and upstream failures
// surface fixed messages and never leak credentials or raw bodies.
func TestQuotaFetchUpstreamFailuresSanitized(t *testing.T) {
	const usageURL = "https://opencode.ai/zen/go/v1/usage"
	request := rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-fake-secret"}}}

	t.Run("host transport error", func(t *testing.T) {
		host := newFakeHost()
		host.httpErrors[usageURL] = errors.New("dial failed sk-fake-secret")
		manager := configuredManager(t, host, "")
		raw, _ := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, request))
		envErr := decodeError(t, raw)
		if envErr.Code != "host_call_failed" || strings.Contains(envErr.Message, "sk-fake") {
			t.Fatalf("error = %+v", envErr)
		}
	})

	cases := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"bad gateway", http.StatusBadGateway, true},
		{"unauthorized", http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			host.httpResponses[usageURL] = pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte("sk-fake-secret")}
			manager := configuredManager(t, host, "")
			raw, _ := manager.HandleCall(pluginabi.MethodQuotaFetch, quotaFetchBytes(t, request))
			envErr := decodeError(t, raw)
			if envErr.Code != "upstream_error" || envErr.StatusCode() != tc.status {
				t.Fatalf("error = %+v, want upstream_error %d", envErr, tc.status)
			}
			if envErr.Retryable != tc.retryable {
				t.Fatalf("retryable = %v, want %v", envErr.Retryable, tc.retryable)
			}
			if strings.Contains(envErr.Message, "sk-fake") {
				t.Fatalf("upstream body leaked: %q", envErr.Message)
			}
		})
	}
}

// TestQuotaResetUnsupported verifies reset is refused with 501 without any
// upstream request, credential save, or routing-state change.
func TestQuotaResetUnsupported(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodQuotaReset, quotaFetchBytes(t, rpcQuotaRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-fake-secret"}},
	}))
	if err != nil {
		t.Fatalf("quota reset: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.Code != "unsupported" || envErr.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("error = %+v, want unsupported 501", envErr)
	}
	if len(host.callLog()) != 0 || len(host.savedAuths()) != 0 {
		t.Fatalf("reset must not touch the host: %v", host.callLog())
	}
}

// assertBuckets checks ordered windows, remaining fractions, and reset times.
func assertBuckets(t *testing.T, buckets []pluginapi.QuotaBucket, want [][2]any, resets []string) {
	t.Helper()
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %d, want %d", len(buckets), len(want))
	}
	for i, expected := range want {
		if buckets[i].Window != expected[0].(string) {
			t.Fatalf("bucket %d window = %q, want %q", i, buckets[i].Window, expected[0])
		}
		if diff := buckets[i].RemainingFraction - expected[1].(float64); diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("bucket %d remaining = %v, want %v", i, buckets[i].RemainingFraction, expected[1])
		}
		if buckets[i].ResetTime != resets[i] {
			t.Fatalf("bucket %d reset = %q, want %q", i, buckets[i].ResetTime, resets[i])
		}
	}
}
