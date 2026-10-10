//go:build integration

// Black-box coverage for the native CPA quota-provider surface exposed by the
// OpenCode Go plugin. The suite drives real management HTTP routes on a real
// host binary; it never imports plugin internals or mirrors the plugin's own
// implementation.
package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// quotaUsagePath is the upstream endpoint the configured base URL (mockURL+"/v1")
// resolves to for a quota read.
const quotaUsagePath = "/v1/usage"

// Management POST routes that resolve a credential by auth_index and read the
// upstream usage endpoint. The v0 generic route and the v8 plugin-scoped route
// (used by the official management panel) must behave identically.
const (
	quotaGenericFetchPath = "/v0/management/quota/fetch"
	quotaV8PluginPath     = "/v8/management/plugins/" + pluginID + "/quota"
)

// quotaFetchRoutes are the management POST routes that select a credential by
// auth_index. The v0 generic route and the v8 plugin route (used by the official
// panel) must share authorization, validation and credential isolation behavior.
var quotaFetchRoutes = []struct {
	name string
	path string
}{
	{"v0 generic", quotaGenericFetchPath},
	{"v8 plugin", quotaV8PluginPath},
}

// Deterministic three-window fixture. Values are distinct per window so an
// incorrect mapping (wrong order, wrong percentage, unforced rate limit, or a
// dropped reset time) is observable.
const (
	quotaFixturePlan = "OpenCode Go"

	quotaWindowRolling = "rolling"
	quotaWindowWeekly  = "weekly"
	quotaWindowMonthly = "monthly"

	quotaRollingPercent int64 = 25
	quotaWeeklyPercent  int64 = 60
	quotaMonthlyPercent int64 = 30

	quotaRollingReset = "2026-10-06T12:00:00Z"
	quotaWeeklyReset  = "2026-10-13T00:00:00Z"
	quotaMonthlyReset = "2026-11-01T00:00:00Z"

	// quotaMalformedMarker is embedded in the invalid upstream payload so the
	// test can prove the host error never echoes raw upstream content.
	quotaMalformedMarker = "SECRET-UPSTREAM-GARBAGE"
)

// defaultQuotaUsageBody is the upstream /v1/usage fixture. rolling and weekly
// are healthy, monthly is rate-limited with a non-zero percent so the forced
// zero mapping is distinguishable from a plain percentage conversion.
func defaultQuotaUsageBody() []byte {
	return mustMarshal(map[string]any{
		"usage": map[string]any{
			quotaWindowRolling: map[string]any{"status": "ok", "percent": quotaRollingPercent, "resetsAt": quotaRollingReset},
			quotaWindowWeekly:  map[string]any{"status": "ok", "percent": quotaWeeklyPercent, "resetsAt": quotaWeeklyReset},
			quotaWindowMonthly: map[string]any{"status": "rate-limited", "percent": quotaMonthlyPercent, "resetsAt": quotaMonthlyReset},
		},
	})
}

// --- host response models --------------------------------------------------

type quotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
	Description       string  `json:"description"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaSubscription struct {
	Plan     string `json:"plan"`
	TierName string `json:"tierName"`
	TierID   string `json:"tierId"`
}

type quotaFetchResponse struct {
	Subscription *quotaSubscription `json:"subscription"`
	Summary      []map[string]any   `json:"summary"`
	Groups       []quotaGroup       `json:"groups"`
}

// credentialEntry is the sanitized host credential listing row.
type credentialEntry struct {
	Name          string `json:"name"`
	AuthIndex     string `json:"auth_index"`
	Type          string `json:"type"`
	Provider      string `json:"provider"`
	Status        string `json:"status"`
	Unavailable   bool   `json:"unavailable"`
	Disabled      bool   `json:"disabled"`
	SupportsQuota bool   `json:"supports_quota"`
	QuotaProvider string `json:"quota_provider"`
}

// v8PluginEntry is the subset of one official GET /v8/management/plugins row
// the management panel matches against to route a credential's quota read.
type v8PluginEntry struct {
	ID               string `json:"id"`
	Registered       bool   `json:"registered"`
	Enabled          bool   `json:"enabled"`
	EffectiveEnabled bool   `json:"effective_enabled"`
	SupportsQuota    bool   `json:"supports_quota"`
	QuotaProvider    string `json:"quota_provider"`
}

// --- helpers ---------------------------------------------------------------

// credentialEntries lists credentials through the native v8 management route.
func (h *harness) credentialEntries(t *testing.T) []credentialEntry {
	t.Helper()
	status, body := h.doJSON(t, http.MethodGet, "/v8/management/credentials", nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("GET /v8/management/credentials status %d body %s", status, truncate(body, 400))
	}
	var resp struct {
		Files []credentialEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("credentials listing not JSON: %v\n%s", err, truncate(body, 400))
	}
	return resp.Files
}

// authIndexForKey resolves the host auth_index for one imported fake key using
// the plugin's documented, deterministic file name.
func (h *harness) authIndexForKey(t *testing.T, key string) string {
	t.Helper()
	want := authFileName(key)
	for _, entry := range h.credentialEntries(t) {
		if entry.Name == want {
			if entry.AuthIndex == "" {
				t.Fatalf("credential %s has no auth_index", want)
			}
			return entry.AuthIndex
		}
	}
	t.Fatalf("credential %s not present in host listing", want)
	return ""
}

// decodeQuota asserts a 200 quota body and decodes it.
func decodeQuota(t *testing.T, status int, body []byte) quotaFetchResponse {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("quota fetch status %d body %s", status, truncate(body, 400))
	}
	var resp quotaFetchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("quota response not JSON: %v\n%s", err, truncate(body, 400))
	}
	return resp
}

// assertThreeWindows checks the normalized subscription, group and bucket
// mapping against the upstream fixture.
func assertThreeWindows(t *testing.T, resp quotaFetchResponse) {
	t.Helper()
	if resp.Subscription == nil {
		t.Fatalf("quota response has no subscription: %+v", resp)
	}
	if resp.Subscription.Plan != quotaFixturePlan {
		t.Errorf("subscription plan = %q, want %q", resp.Subscription.Plan, quotaFixturePlan)
	}
	if len(resp.Summary) != 0 {
		t.Errorf("quota response exposes amount summary metrics: %+v", resp.Summary)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("quota response has %d groups, want 1: %+v", len(resp.Groups), resp.Groups)
	}
	group := resp.Groups[0]
	if group.DisplayName != quotaFixturePlan {
		t.Errorf("group displayName = %q, want %q", group.DisplayName, quotaFixturePlan)
	}
	if len(group.Buckets) != 3 {
		t.Fatalf("group has %d buckets, want 3: %+v", len(group.Buckets), group.Buckets)
	}

	type want struct {
		window            string
		remainingFraction float64
		resetAt           string
	}
	wants := []want{
		// rate-limited forces a zero remaining fraction despite percent=30.
		{quotaWindowRolling, 1 - float64(quotaRollingPercent)/100, quotaRollingReset},
		{quotaWindowWeekly, 1 - float64(quotaWeeklyPercent)/100, quotaWeeklyReset},
		{quotaWindowMonthly, 0, quotaMonthlyReset},
	}
	for i, w := range wants {
		bucket := group.Buckets[i]
		if bucket.Window != w.window {
			t.Errorf("bucket %d window = %q, want %q", i, bucket.Window, w.window)
		}
		if diff := bucket.RemainingFraction - w.remainingFraction; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("bucket %q remainingFraction = %v, want %v", bucket.Window, bucket.RemainingFraction, w.remainingFraction)
		}
		gotReset, err := time.Parse(time.RFC3339, bucket.ResetTime)
		if err != nil {
			t.Errorf("bucket %q resetTime %q is not RFC3339: %v", bucket.Window, bucket.ResetTime, err)
			continue
		}
		wantReset, _ := time.Parse(time.RFC3339, w.resetAt)
		if !gotReset.Equal(wantReset) {
			t.Errorf("bucket %q resetTime = %s, want %s", bucket.Window, gotReset, wantReset)
		}
	}
}

// authIndexOf returns the bearer credential of a recorded upstream call.
func authIndexOf(call mockCall) string { return bearerKey(call.Header.Get("Authorization")) }

// --- tests -----------------------------------------------------------------

// TestQuotaProviderDiscovery proves the host discovers the quota provider,
// enforces management auth and marks credentials as quota-capable.
func TestQuotaProviderDiscovery(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota-discovery"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}

	// Exactly one unauthenticated probe to avoid the failed-attempt ban.
	status, _, err := h.rawRequest(http.MethodGet, "/v0/management/quota/providers", nil, nil)
	if err != nil {
		t.Fatalf("unauthorized quota providers: %v", err)
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("unauthorized quota providers status = %d, want 401 or 403", status)
	}

	status, body := h.doJSON(t, http.MethodGet, "/v0/management/quota/providers", nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("GET /v0/management/quota/providers status %d body %s", status, truncate(body, 400))
	}
	var providers struct {
		Providers []struct {
			PluginID           string   `json:"plugin_id"`
			Provider           string   `json:"provider"`
			DisplayName        string   `json:"display_name"`
			SupportedProviders []string `json:"supported_providers"`
			SupportsReset      bool     `json:"supports_reset"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &providers); err != nil {
		t.Fatalf("providers listing not JSON: %v\n%s", err, truncate(body, 400))
	}
	if len(providers.Providers) != 1 {
		t.Fatalf("discovered %d quota providers, want 1: %s", len(providers.Providers), truncate(body, 400))
	}
	got := providers.Providers[0]
	if got.PluginID != pluginID {
		t.Errorf("provider plugin_id = %q, want %q", got.PluginID, pluginID)
	}
	if got.Provider != providerID {
		t.Errorf("provider identifier = %q, want %q", got.Provider, providerID)
	}
	if got.DisplayName != quotaFixturePlan {
		t.Errorf("provider display_name = %q, want %q", got.DisplayName, quotaFixturePlan)
	}
	if len(got.SupportedProviders) != 1 || got.SupportedProviders[0] != providerID {
		t.Errorf("provider supported_providers = %v, want [%s]", got.SupportedProviders, providerID)
	}
	if got.SupportsReset {
		t.Errorf("provider advertises reset support, want read-only")
	}

	// Credential discovery marks both fake keys as quota-capable.
	entries := h.credentialEntries(t)
	if len(entries) != 2 {
		t.Fatalf("credentials listing has %d entries, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.Provider != providerID {
			t.Errorf("credential provider = %q, want %q", entry.Provider, providerID)
		}
		if !entry.SupportsQuota {
			t.Errorf("credential %s missing supports_quota", entry.Name)
		}
		if entry.QuotaProvider != providerID {
			t.Errorf("credential %s quota_provider = %q, want %q", entry.Name, entry.QuotaProvider, providerID)
		}
	}

	// The embedded UI receives the same safe host indexes, not upstream keys.
	status, body = h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("plugin key list status %d body %s", status, truncate(body, 400))
	}
	var listed struct {
		Files []credentialEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("plugin key list: %v", err)
	}
	if len(listed.Files) != len(entries) {
		t.Fatalf("plugin key list has %d rows, want %d", len(listed.Files), len(entries))
	}
	for _, row := range listed.Files {
		matched := false
		for _, entry := range entries {
			if row.Name == entry.Name && row.AuthIndex == entry.AuthIndex && row.AuthIndex != "" {
				matched = true
			}
		}
		if !matched {
			t.Errorf("UI quota lookup index does not match host credential: %+v", row)
		}
	}
	assertListSanitized(t, body, h.authDir)
}

// TestQuotaV8ManagementDiscovery proves the official management panel can select
// this plugin from the real v8 plugin listing: the row must carry the exact
// identity and quota metadata the panel's first-pass match consumes, and the
// listing must stay free of upstream key material. It asserts the row rather
// than mirroring the panel algorithm.
func TestQuotaV8ManagementDiscovery(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota-v8-discovery"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}

	status, body := h.doJSON(t, http.MethodGet, "/v8/management/plugins", nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("GET /v8/management/plugins status %d body %s", status, truncate(body, 400))
	}
	var listing struct {
		PluginsEnabled bool            `json:"plugins_enabled"`
		Plugins        []v8PluginEntry `json:"plugins"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("plugin listing not JSON: %v\n%s", err, truncate(body, 400))
	}
	if !listing.PluginsEnabled {
		t.Errorf("plugin listing plugins_enabled = false, want true")
	}

	var entry *v8PluginEntry
	for i := range listing.Plugins {
		if listing.Plugins[i].ID == pluginID {
			entry = &listing.Plugins[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("plugin %s missing from v8 listing: %s", pluginID, truncate(body, 400))
	}

	// The panel's first-pass match requires all four fields to agree.
	if !entry.Registered {
		t.Errorf("plugin %s registered = false, want true", pluginID)
	}
	if !entry.SupportsQuota {
		t.Errorf("plugin %s supports_quota = false, want true", pluginID)
	}
	if entry.QuotaProvider != providerID {
		t.Errorf("plugin %s quota_provider = %q, want %q", pluginID, entry.QuotaProvider, providerID)
	}
	if !entry.EffectiveEnabled {
		t.Errorf("plugin %s effective_enabled = false, want true", pluginID)
	}

	// The public plugin listing must never echo upstream key material.
	text := string(body)
	for _, secret := range []string{keyAlpha, keyBeta} {
		if strings.Contains(text, secret) {
			t.Errorf("v8 plugin listing leaked credential %q", secret)
		}
	}
}

// TestQuotaFetchThreeWindows proves the normalized three-window mapping and the
// upstream callback contract (GET base+/usage with the selected key, JSON Accept
// and plugin User-Agent).
func TestQuotaFetchThreeWindows(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-windows"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)
	h.mock.reset()

	status, body := h.doJSON(t, http.MethodPost, "/v0/management/quota/fetch",
		mustMarshal(map[string]any{"auth_index": index}), managementHeaders())
	resp := decodeQuota(t, status, body)
	assertThreeWindows(t, resp)

	calls := h.mock.callsForPath(quotaUsagePath)
	if len(calls) != 1 {
		t.Fatalf("upstream %s calls = %d, want 1", quotaUsagePath, len(calls))
	}
	if key := authIndexOf(calls[0]); key != keyAlpha {
		t.Errorf("upstream quota credential = %q, want %q", key, keyAlpha)
	}
	h.mock.requireClean(t)
}

// TestQuotaPluginSpecificRoutes proves the plugin-scoped read routes (v0 GET/POST
// and v8 GET/POST) return the same normalized quota and each performs a fresh
// read. The v8 POST carries only auth_index, exactly as the official panel sends
// it.
func TestQuotaPluginSpecificRoutes(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-routes"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)

	cases := []struct {
		name    string
		method  string
		path    string
		body    []byte
		wantGet int
	}{
		{"v0 GET", http.MethodGet, "/v0/management/plugins/" + pluginID + "/quota?auth_index=" + index, nil, 1},
		{"v0 POST", http.MethodPost, "/v0/management/plugins/" + pluginID + "/quota", mustMarshal(map[string]any{"auth_index": index}), 2},
		{"v8 GET", http.MethodGet, quotaV8PluginPath + "?auth_index=" + index, nil, 3},
		{"v8 POST", http.MethodPost, quotaV8PluginPath, mustMarshal(map[string]any{"auth_index": index}), 4},
	}
	for _, tc := range cases {
		status, body := h.doJSON(t, tc.method, tc.path, tc.body, managementHeaders())
		assertThreeWindows(t, decodeQuota(t, status, body))
		if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != tc.wantGet {
			t.Errorf("%s performed %d upstream %s calls, want %d", tc.name, calls, quotaUsagePath, tc.wantGet)
		}
	}
	h.mock.requireClean(t)
}

// TestQuotaRepeatedRefresh proves every management read triggers a new upstream
// GET rather than serving a cached snapshot.
func TestQuotaRepeatedRefresh(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-refresh"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)
	h.mock.reset()

	for i := 1; i <= 2; i++ {
		status, body := h.doJSON(t, http.MethodPost, "/v0/management/quota/fetch",
			mustMarshal(map[string]any{"auth_index": index}), managementHeaders())
		assertThreeWindows(t, decodeQuota(t, status, body))
		if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != i {
			t.Fatalf("after refresh %d upstream %s calls = %d, want %d", i, quotaUsagePath, calls, i)
		}
	}
	h.mock.requireClean(t)
}

// TestQuotaCredentialIsolation proves the requested auth_index selects the
// matching credential for the upstream read on every management quota route, so
// two keys do not leak into each other's quota.
func TestQuotaCredentialIsolation(t *testing.T) {
	for _, route := range quotaFetchRoutes {
		t.Run(route.name, func(t *testing.T) { testQuotaCredentialIsolation(t, route.path) })
	}
}

func testQuotaCredentialIsolation(t *testing.T, route string) {
	t.Helper()
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota-isolation"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	indexAlpha := h.authIndexForKey(t, keyAlpha)
	indexBeta := h.authIndexForKey(t, keyBeta)
	if indexAlpha == indexBeta {
		t.Fatalf("two keys resolved to the same auth_index %q", indexAlpha)
	}

	for _, tc := range []struct {
		name  string
		index string
		key   string
	}{
		{"alpha", indexAlpha, keyAlpha},
		{"beta", indexBeta, keyBeta},
	} {
		h.mock.reset()
		status, body := h.doJSON(t, http.MethodPost, route,
			mustMarshal(map[string]any{"auth_index": tc.index}), managementHeaders())
		assertThreeWindows(t, decodeQuota(t, status, body))
		call, ok := lastUpstreamCall(h, quotaUsagePath)
		if !ok {
			t.Fatalf("%s: no upstream %s call", tc.name, quotaUsagePath)
		}
		if key := authIndexOf(call); key != tc.key {
			t.Errorf("%s: upstream quota credential = %q, want %q", tc.name, key, tc.key)
		}
		h.mock.requireClean(t)
	}
}

// TestQuotaFetchValidation covers management authorization and request
// validation on every quota POST route: both the v0 generic route and the v8
// plugin route must reject unauthenticated, missing and unknown auth_index
// requests without ever reading upstream.
func TestQuotaFetchValidation(t *testing.T) {
	for _, route := range quotaFetchRoutes {
		t.Run(route.name, func(t *testing.T) { testQuotaFetchValidation(t, route.path) })
	}
}

func testQuotaFetchValidation(t *testing.T, route string) {
	t.Helper()
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-validation"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)

	// Exactly one unauthenticated probe per host to avoid the failed-attempt ban.
	status, _, err := h.rawRequest(http.MethodPost, route,
		mustMarshal(map[string]any{"auth_index": index}), nil)
	if err != nil {
		t.Fatalf("unauthorized %s: %v", route, err)
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("unauthorized %s status = %d, want 401 or 403", route, status)
	}

	// Missing auth_index is a client error, never a quota read.
	h.mock.reset()
	status, body := h.doJSON(t, http.MethodPost, route,
		mustMarshal(map[string]any{}), managementHeaders())
	if status != http.StatusBadRequest {
		t.Errorf("%s missing auth_index status = %d body %s, want 400", route, status, truncate(body, 300))
	}
	if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != 0 {
		t.Errorf("%s missing auth_index performed %d upstream quota calls, want 0", route, calls)
	}

	// Unknown auth_index is a not-found, never a quota read.
	status, body = h.doJSON(t, http.MethodPost, route,
		mustMarshal(map[string]any{"auth_index": "does-not-exist"}), managementHeaders())
	if status != http.StatusNotFound {
		t.Errorf("%s unknown auth_index status = %d body %s, want 404", route, status, truncate(body, 300))
	}
	if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != 0 {
		t.Errorf("%s unknown auth_index performed %d upstream quota calls, want 0", route, calls)
	}
	h.mock.requireClean(t)
}

// TestQuotaMalformedUpstreamIsSanitized proves an invalid upstream payload
// surfaces as a sanitized gateway error with no upstream body leakage.
func TestQuotaMalformedUpstreamIsSanitized(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-malformed"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)

	h.mock.resetQuotaResponse()
	h.mock.setQuotaResponse(http.StatusOK, "application/json",
		[]byte(`{"usage":{"rolling":[`+quotaMalformedMarker+`}}`))

	status, body := h.doJSON(t, http.MethodPost, "/v0/management/quota/fetch",
		mustMarshal(map[string]any{"auth_index": index}), managementHeaders())
	if status != http.StatusBadGateway {
		t.Fatalf("malformed upstream status = %d body %s, want 502", status, truncate(body, 400))
	}
	text := string(body)
	if strings.Contains(text, quotaMalformedMarker) {
		t.Errorf("gateway error leaked raw upstream payload: %s", truncate(body, 400))
	}
	for _, secret := range []string{keyAlpha, keyBeta} {
		if strings.Contains(text, secret) {
			t.Errorf("gateway error leaked credential %q: %s", secret, truncate(body, 300))
		}
	}
	h.mock.requireClean(t)
}

// TestQuotaUpstreamFailuresAreReadOnly verifies refresh failures do not alter
// routing state, echo raw upstream data, or cause plugin-side retries.
func TestQuotaUpstreamFailuresAreReadOnly(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-errors"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)
	before := h.credentialStatus(t, keyAlpha)
	for _, upstreamStatus := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		h.mock.reset()
		h.mock.setQuotaResponse(upstreamStatus, "application/json", []byte(keyAlpha+" "+quotaMalformedMarker))
		status, body := h.doJSON(t, http.MethodPost, "/v0/management/quota/fetch",
			mustMarshal(map[string]any{"auth_index": index}), managementHeaders())
		if status != http.StatusBadGateway {
			t.Fatalf("upstream %d: host status %d body %s, want 502", upstreamStatus, status, truncate(body, 400))
		}
		if strings.Contains(string(body), keyAlpha) || strings.Contains(string(body), quotaMalformedMarker) {
			t.Fatalf("upstream %d: quota failure leaked raw data", upstreamStatus)
		}
		if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != 1 {
			t.Fatalf("upstream %d: usage calls %d, want one without retry", upstreamStatus, calls)
		}
		if after := h.credentialStatus(t, keyAlpha); after != before {
			t.Fatalf("upstream %d: refresh mutated routing state: %+v -> %+v", upstreamStatus, before, after)
		}
	}
	h.mock.requireClean(t)
}

// TestQuotaResetReadOnly proves the advertised read-only provider never reads
// upstream on reset, never reports success and leaves routing state untouched.
func TestQuotaResetReadOnly(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "quota-reset"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 400))
	}
	index := h.authIndexForKey(t, keyAlpha)

	before := h.credentialStatus(t, keyAlpha)
	h.mock.reset()

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"generic reset", http.MethodPost, "/v0/management/quota/reset", mustMarshal(map[string]any{"auth_index": index})},
		{"plugin reset", http.MethodPost, "/v0/management/plugins/" + pluginID + "/quota/reset", mustMarshal(map[string]any{"auth_index": index})},
		{"plugin delete", http.MethodDelete, "/v0/management/plugins/" + pluginID + "/quota?auth_index=" + index, nil},
	}
	for _, tc := range cases {
		status, body := h.doJSON(t, tc.method, tc.path, tc.body, managementHeaders())
		if status == http.StatusOK {
			t.Errorf("%s unexpectedly succeeded: %s", tc.name, truncate(body, 300))
		}
		if status != http.StatusBadGateway {
			t.Errorf("%s status = %d body %s, want 502 read-only rejection", tc.name, status, truncate(body, 300))
		}
		if strings.Contains(string(body), `"success":true`) {
			t.Errorf("%s reported a successful reset: %s", tc.name, truncate(body, 300))
		}
		if calls := len(h.mock.callsForPath(quotaUsagePath)); calls != 0 {
			t.Errorf("%s performed %d upstream quota calls, want 0", tc.name, calls)
		}
	}

	after := h.credentialStatus(t, keyAlpha)
	if after != before {
		t.Errorf("reset attempt mutated routing state: %+v -> %+v", before, after)
	}
	h.mock.requireClean(t)
}

// credentialStatus captures the routing state fields of one credential.
type credentialStatus struct {
	Status      string
	Unavailable bool
	Disabled    bool
}

func (h *harness) credentialStatus(t *testing.T, key string) credentialStatus {
	t.Helper()
	want := authFileName(key)
	for _, entry := range h.credentialEntries(t) {
		if entry.Name == want {
			return credentialStatus{Status: entry.Status, Unavailable: entry.Unavailable, Disabled: entry.Disabled}
		}
	}
	t.Fatalf("credential %s not present in host listing", want)
	return credentialStatus{}
}
