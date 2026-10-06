//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file locks the official-model catalog contract: the plugin owns no
// embedded/static model list. Exactly the models returned by the single
// authenticated upstream GET /models are published as "opencode-go/<native>",
// a successful empty list clears the credential's models, and a failed
// discovery never fabricates a fallback catalog. It is black-box: only the CPA
// HTTP surface and the fake httptest upstream are used.

// upstreamModelsBody renders an official /models payload for the given native
// ids. Ids are emitted verbatim so a test can assert the published prefix is
// applied exactly once by the plugin.
func upstreamModelsBody(ids ...string) []byte {
	models := make([]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{"id": id, "object": "model", "created": 1791258569, "owned_by": "opencode"})
	}
	return mustMarshal(map[string]any{"object": "list", "data": models})
}

// emptyCatalogBody is a valid official response with no models.
func emptyCatalogBody() []byte {
	return []byte(`{"object":"list","data":[]}`)
}

// catalogIDs reads the client-visible model catalog.
func (h *harness) catalogIDs(t *testing.T) []string {
	t.Helper()
	status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models status %d body %s", status, truncate(body, 300))
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("model catalog is not JSON: %v body %s", err, truncate(body, 300))
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, entry := range decoded.Data {
		ids = append(ids, entry.ID)
	}
	return ids
}

// assertCatalogExactly fails unless the catalog is exactly want: a subset, a
// stale superset, or any duplicate is a contract violation.
func assertCatalogExactly(t *testing.T, got []string, want ...string) {
	t.Helper()
	wantSet := make(map[string]struct{}, len(want))
	for _, id := range want {
		wantSet[id] = struct{}{}
	}
	gotSet := make(map[string]struct{}, len(got))
	for _, id := range got {
		if _, dup := gotSet[id]; dup {
			t.Errorf("catalog lists model %q more than once", id)
		}
		gotSet[id] = struct{}{}
	}
	for id := range gotSet {
		if _, ok := wantSet[id]; !ok {
			t.Errorf("catalog published unexpected model %q (want exactly %v)", id, want)
		}
	}
	for id := range wantSet {
		if _, ok := gotSet[id]; !ok {
			t.Errorf("catalog is missing model %q (got %v)", id, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("catalog size = %d, want %d (got %v, want %v)", len(got), len(want), got, want)
	}
}

// sameStringSet reports whether got and want contain the same ids (as a set).
func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]struct{}, len(want))
	for _, id := range want {
		set[id] = struct{}{}
	}
	for _, id := range got {
		if _, ok := set[id]; !ok {
			return false
		}
		delete(set, id)
	}
	return len(set) == 0
}

// bumpCredentialLabel rewrites one persisted credential's label and renames it
// into place so the host's auth-directory watcher re-reads the record and
// re-runs per-auth model discovery without ever observing a partial file.
func bumpCredentialLabel(t *testing.T, h *harness, key, label string) {
	t.Helper()
	path := filepath.Join(h.authDir, authFileName(key))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file %s: %v", path, err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("auth file %s is not JSON: %v", path, err)
	}
	record["label"] = label
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	tmp := filepath.Join(filepath.Dir(h.authDir), filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		t.Fatalf("write auth file temp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename auth file into place: %v", err)
	}
}

// waitForProbeIncrease waits until the upstream GET /v1/models probe count
// exceeds baseline, proving the trigger actually caused a rediscovery.
func waitForProbeIncrease(t *testing.T, mock *mockOpenCode, baseline int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(mock.callsForPath("/v1/models")) > baseline {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("upstream GET /v1/models did not run after trigger (baseline %d)", baseline)
}

// waitForCatalogExactly waits for a rediscovery probe and then for the catalog
// to become exactly want. Requiring the probe first prevents a stale catalog
// from satisfying the assertion before the trigger took effect.
func waitForCatalogExactly(t *testing.T, h *harness, mock *mockOpenCode, baseline int, want ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []string
	for time.Now().Before(deadline) {
		if len(mock.callsForPath("/v1/models")) > baseline {
			last = h.catalogIDs(t)
			if sameStringSet(last, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("catalog did not become exactly %v after rediscovery (last %v, probes %d, baseline %d)",
		want, last, len(mock.callsForPath("/v1/models")), baseline)
}

// waitForCatalogContaining waits for a catalog entry whose id contains needle
// and returns the full published id.
func waitForCatalogContaining(t *testing.T, h *harness, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = h.catalogIDs(t)
		for _, id := range last {
			if strings.Contains(id, needle) {
				return id
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("catalog never exposed a model containing %q within %s (last %v)", needle, timeout, last)
	return ""
}

// sendChatWithModel sends one chat request for an explicit, already-resolved
// model id (used for alias ids that are not derived from publicModel).
func sendChatWithModel(t *testing.T, h *harness, model, session, mode string) (int, []byte) {
	t.Helper()
	body := mustMarshal(map[string]any{
		"model":  model,
		"stream": false,
		"messages": []any{
			map[string]any{"role": "user", "content": "Use the get_time tool. " + markerFor(mode)},
		},
		"tools": []any{chatTool()},
	})
	headers := map[string]string{
		"Content-Type":       "application/json",
		"Authorization":      "Bearer " + clientKey,
		"x-opencode-session": session,
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, headers)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	return status, raw
}

// assertUpstreamModel decodes a recorded native chat call and asserts the
// upstream model is exactly the native id: neither the public prefix nor an
// alias may leak upstream.
func assertUpstreamModel(t *testing.T, call mockCall, want string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(call.Body, &decoded); err != nil {
		t.Fatalf("recorded upstream chat body is not JSON: %v", err)
	}
	got, _ := decoded["model"].(string)
	if got != want {
		t.Fatalf("upstream chat model = %q, want native %q (no alias or public prefix may leak)", got, want)
	}
}

// TestCatalogBeforeCredentialsIsEmpty proves the plugin publishes no static or
// embedded snapshot and performs no upstream discovery without a credential.
func TestCatalogBeforeCredentialsIsEmpty(t *testing.T) {
	h := newHarness(t)

	ids := h.catalogIDs(t)
	if len(ids) != 0 {
		t.Fatalf("catalog published %d model(s) before any credential: %v", len(ids), ids)
	}
	if probes := len(h.mock.callsForPath("/v1/models")); probes != 0 {
		t.Fatalf("upstream GET /v1/models ran %d time(s) before any credential", probes)
	}
	h.mock.requireClean(t)
}

// TestCatalogMatchesUpstreamExactly proves the published catalog is exactly the
// official upstream list with the "opencode-go/" prefix applied once, with no
// embedded-snapshot extras merged in.
func TestCatalogMatchesUpstreamExactly(t *testing.T) {
	h := newHarness(t)

	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "catalog"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	if probes := len(h.mock.callsForPath("/v1/models")); probes == 0 {
		t.Fatalf("no upstream GET /v1/models after import")
	}
	assertCatalogExactly(t, h.catalogIDs(t),
		publicModel(nativeGLM), publicModel(nativeMinimax), publicModel(nativeGPT))
	h.mock.requireClean(t)
}

// TestCatalogRediscoveryAndEmptyRemoval drives per-auth rediscovery through the
// auth-directory watcher: a changed upstream list replaces the catalog, and a
// successful empty list clears the credential's models entirely.
func TestCatalogRediscoveryAndEmptyRemoval(t *testing.T) {
	h := newHarness(t)
	mock := h.mock

	// A single credential so the whole catalog is governed by one auth.
	if status, body := h.importKeys(t, []string{keyAlpha}, "rediscovery"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	assertCatalogExactly(t, h.catalogIDs(t),
		publicModel(nativeGLM), publicModel(nativeMinimax), publicModel(nativeGPT))

	// An unknown family must still be published, without stale or snapshot extras.
	onlyOne := "opencode-go-only-one"
	mock.setModelsResponse(http.StatusOK, upstreamModelsBody(onlyOne))
	baseline := len(mock.callsForPath("/v1/models"))
	bumpCredentialLabel(t, h, keyAlpha, "rediscovery-1")
	waitForCatalogExactly(t, h, mock, baseline, publicModel(onlyOne))

	// A successful empty catalog clears the credential's models.
	mock.setModelsResponse(http.StatusOK, emptyCatalogBody())
	baseline = len(mock.callsForPath("/v1/models"))
	bumpCredentialLabel(t, h, keyAlpha, "rediscovery-2")
	waitForCatalogExactly(t, h, mock, baseline)
	mock.requireClean(t)
}

// TestCatalogDiscoveryFailurePublishesNothing proves a failed initial discovery
// (HTTP error or malformed body) cannot fabricate a snapshot catalog: the
// credential never becomes a source of invented model ids.
func TestCatalogDiscoveryFailurePublishesNothing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
	}{
		{"unauthorized", http.StatusUnauthorized, []byte(`{"error":{"message":"nope"}}`)},
		{"unavailable", http.StatusServiceUnavailable, []byte(`{"error":{"message":"down"}}`)},
		{"malformed", http.StatusOK, []byte(`{"data":{}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			mock := h.mock
			mock.setModelsResponse(tc.status, tc.body)

			// Import without a catalog wait: discovery is expected to fail.
			payload, err := json.Marshal(map[string]any{"keys": []string{keyBeta}, "label": "discovery-failure"})
			if err != nil {
				t.Fatalf("marshal import: %v", err)
			}
			status, body := h.doJSON(t, http.MethodPost, "/v0/management/plugins/"+pluginID+"/keys", payload, managementHeaders())
			if status != http.StatusOK {
				t.Fatalf("import status %d body %s", status, truncate(body, 400))
			}

			// Wait until the host actually ran the failing discovery before
			// asserting the catalog, so an in-flight attempt cannot race it.
			waitForProbeIncrease(t, mock, 0, 10*time.Second)

			// The catalog must stay empty for a settled window: no invented ids.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if ids := h.catalogIDs(t); len(ids) != 0 {
					t.Fatalf("failed %s discovery published %d invented model(s): %v", tc.name, len(ids), ids)
				}
				time.Sleep(50 * time.Millisecond)
			}
			assertCatalogExactly(t, h.catalogIDs(t))
			mock.requireClean(t)
		})
	}
}

// TestCatalogFailedRefreshRetainsHostCatalog verifies a discovery error does
// not clear the last registered catalog in the supported CPA hosts.
func TestCatalogFailedRefreshRetainsHostCatalog(t *testing.T) {
	h := newHarness(t)
	mock := h.mock
	if status, body := h.importKeys(t, []string{keyAlpha}, "refresh-error"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	want := []string{publicModel(nativeGLM), publicModel(nativeMinimax), publicModel(nativeGPT)}
	assertCatalogExactly(t, h.catalogIDs(t), want...)

	mock.setModelsResponse(http.StatusServiceUnavailable, []byte(`{"error":{"message":"down"}}`))
	baseline := len(mock.callsForPath("/v1/models"))
	bumpCredentialLabel(t, h, keyAlpha, "refresh-error-1")
	waitForProbeIncrease(t, mock, baseline, 10*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		assertCatalogExactly(t, h.catalogIDs(t), want...)
		time.Sleep(50 * time.Millisecond)
	}
	if status, raw := sendChatWithModel(t, h, publicModel(nativeGLM), "refresh-S1", "full"); status != http.StatusOK {
		t.Fatalf("inference after failed discovery: status %d body %s", status, truncate(raw, 400))
	}
	call, ok := lastUpstreamCall(h, "/v1/chat/completions")
	if !ok {
		t.Fatal("inference after failed discovery did not reach upstream")
	}
	assertUpstreamModel(t, call, nativeGLM)

	mock.setModelsResponse(http.StatusOK, upstreamModelsBody(nativeGLM))
	baseline = len(mock.callsForPath("/v1/models"))
	bumpCredentialLabel(t, h, keyAlpha, "refresh-error-2")
	waitForCatalogExactly(t, h, mock, baseline, publicModel(nativeGLM))
	mock.requireClean(t)
}

// TestModelAliasRoutesToNativeUpstream proves a CPA model alias exposes a client
// name that routes to the real native model: the recorded upstream request body
// carries the native id only, and a host rediscovery of a reduced catalog does
// not break the alias.
func TestModelAliasRoutesToNativeUpstream(t *testing.T) {
	h := newHarness(t)
	mock := h.mock

	if status, body := h.importKeys(t, []string{keyAlpha}, "alias"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}

	alias := "mock-glm-alias"
	patch := mustMarshal(map[string]any{
		"channel": providerID,
		"aliases": []any{
			map[string]any{"name": publicModel(nativeGLM), "alias": alias, "fork": true},
		},
	})
	status, body := h.doJSON(t, http.MethodPatch, "/v0/management/oauth-model-alias", patch, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("alias patch status %d body %s", status, truncate(body, 400))
	}
	aliasID := waitForCatalogContaining(t, h, alias, 10*time.Second)
	t.Logf("alias %q published as %q", alias, aliasID)

	// A call through the alias must reach the upstream as the native model only.
	mock.reset()
	if status, raw := sendChatWithModel(t, h, aliasID, "alias-S1", "full"); status != http.StatusOK {
		t.Fatalf("alias chat status %d body %s", status, truncate(raw, 400))
	}
	call, ok := lastUpstreamCall(h, "/v1/chat/completions")
	if !ok {
		t.Fatalf("alias chat reached no native upstream request")
	}
	assertUpstreamModel(t, call, nativeGLM)

	// An exact smaller catalog proves registration completed for the only auth.
	mock.setModelsResponse(http.StatusOK, upstreamModelsBody(nativeGLM))
	baseline := len(mock.callsForPath("/v1/models"))
	bumpCredentialLabel(t, h, keyAlpha, "alias-refresh")
	waitForCatalogExactly(t, h, mock, baseline, publicModel(nativeGLM), aliasID)

	mock.reset()
	if status, raw := sendChatWithModel(t, h, aliasID, "alias-S2", "full"); status != http.StatusOK {
		t.Fatalf("post-rediscovery alias chat status %d body %s", status, truncate(raw, 400))
	}
	call, ok = lastUpstreamCall(h, "/v1/chat/completions")
	if !ok {
		t.Fatalf("post-rediscovery alias chat reached no native upstream request")
	}
	assertUpstreamModel(t, call, nativeGLM)
	mock.requireClean(t)
}
