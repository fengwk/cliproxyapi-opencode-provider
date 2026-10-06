//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authFileName mirrors the plugin's stable physical file name derivation.
func authFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return providerID + "-" + hex.EncodeToString(sum[:]) + ".json"
}

// TestUIResourcesPublic verifies the resource routes are public, secret-free and
// hardened, and that no query can trigger an import mutation.
func TestUIResourcesPublic(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		path        string
		contentType string
	}{
		{"/v0/resource/plugins/" + pluginID + "/ui", "text/html"},
		{"/v0/resource/plugins/" + pluginID + "/ui.js", "javascript"},
		{"/v0/resource/plugins/" + pluginID + "/ui.css", "text/css"},
	}
	for _, tc := range cases {
		status, header, body, err := h.rawRequestWithHeaders(http.MethodGet, tc.path, nil, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		if status != http.StatusOK {
			t.Fatalf("GET %s status %d body %s", tc.path, status, truncate(body, 300))
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned an empty body", tc.path)
		}
		if got := header.Get("Content-Type"); !strings.Contains(got, tc.contentType) {
			t.Errorf("GET %s Content-Type = %q, want to contain %q", tc.path, got, tc.contentType)
		}
		if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("GET %s CSP = %q, want self-only policy", tc.path, csp)
		}
		if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", tc.path, got)
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", tc.path, got)
		}
		for _, secret := range []string{keyAlpha, keyBeta, clientKey, mgmtKey} {
			if strings.Contains(string(body), secret) {
				t.Errorf("GET %s leaked secret %q", tc.path, secret)
			}
		}
	}

	// The UI entry point is reachable and carries the hardened headers.
	status, _, uiBody, err := h.rawRequestWithHeaders(http.MethodGet, "/v0/resource/plugins/"+pluginID+"/ui", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET ui: status %d err %v", status, err)
	}
	// The page shows the human-readable plugin name and keeps the technical ID
	// visible for support.
	uiHTML := string(uiBody)
	if !strings.Contains(uiHTML, pluginName) {
		t.Errorf("ui.html must display the plugin name %q", pluginName)
	}
	if !strings.Contains(uiHTML, pluginID) {
		t.Errorf("ui.html must preserve the technical ID %q", pluginID)
	}

	// A query string must not mutate plugin/host state.
	before := countAuthFiles(t, h.authDir)
	if status, _, err = h.rawRequest(http.MethodGet, "/v0/resource/plugins/"+pluginID+"/ui?action=import&keys="+keyAlpha, nil, nil); err != nil {
		t.Fatalf("GET ui with query: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET ui with query status %d", status)
	}
	if after := countAuthFiles(t, h.authDir); after != before {
		t.Errorf("resource query mutated auth files: %d -> %d", before, after)
	}

	// Non-GET resource requests must not be routed.
	status, _, err = h.rawRequest(http.MethodPost, "/v0/resource/plugins/"+pluginID+"/ui", []byte(`{"keys":["`+keyAlpha+`"]}`), nil)
	if err != nil {
		t.Fatalf("POST resource: %v", err)
	}
	if status == http.StatusOK {
		t.Errorf("POST resource route unexpectedly succeeded")
	}
	if after := countAuthFiles(t, h.authDir); after != before {
		t.Errorf("POST resource mutated auth files: %d -> %d", before, after)
	}
}

// TestKeyImportListDelete covers the authenticated plugin key API and host-side
// credential persistence.
func TestKeyImportListDelete(t *testing.T) {
	h := newHarness(t)

	// Exactly one unauthenticated probe to avoid the wrong-attempt ban.
	status, _, err := h.rawRequest(http.MethodGet, "/v0/management/plugins/"+pluginID+"/keys", nil, nil)
	if err != nil {
		t.Fatalf("unauthorized list: %v", err)
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("unauthorized list status = %d, want 401 or 403", status)
	}

	// Import two keys; the response must be immediate metadata-only saves.
	status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "primary")
	if status != http.StatusOK {
		t.Fatalf("import status %d body %s", status, truncate(body, 400))
	}
	assertCounts(t, body, 2, 0, 0)

	// Duplicate re-import is skipped, never failed.
	status, body = h.importKeys(t, []string{keyAlpha, keyBeta}, "primary")
	if status != http.StatusOK {
		t.Fatalf("re-import status %d body %s", status, truncate(body, 400))
	}
	assertCounts(t, body, 0, 2, 0)

	// Files are persisted under the temp auth dir with safe permissions.
	files := authFilesOnDisk(t, h.authDir)
	if len(files) != 2 {
		t.Fatalf("auth dir has %d files, want 2: %v", len(files), files)
	}
	for _, key := range []string{keyAlpha, keyBeta} {
		name := authFileName(key)
		path := filepath.Join(h.authDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("expected auth file %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("auth file %s mode = %o, want 600", name, perm)
		}
		assertAuthFileContent(t, path, key)
	}

	// The list route exposes only sanitized metadata.
	status, body = h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list status %d body %s", status, truncate(body, 400))
	}
	assertListSanitized(t, body, h.authDir)

	// Deleting through the native v8 management credential route removes it.
	target := authFileName(keyAlpha)
	status, body, err = h.rawRequest(http.MethodDelete, "/v8/management/credentials?name="+target, nil, managementHeaders())
	if err != nil {
		t.Fatalf("delete credential: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("delete status %d body %s", status, truncate(body, 400))
	}
	if _, err := os.Stat(filepath.Join(h.authDir, target)); !os.IsNotExist(err) {
		t.Errorf("deleted auth file still present (err=%v)", err)
	}
	remaining := authFilesOnDisk(t, h.authDir)
	if len(remaining) != 1 {
		t.Fatalf("after delete auth dir has %d files, want 1: %v", len(remaining), remaining)
	}
	status, body = h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list after delete status %d", status)
	}
	assertListSanitized(t, body, h.authDir)
}

// TestCredentialsPersistAcrossRestart proves CPA owns credential persistence and
// that a restarted host reproduces the deterministic upstream session.
func TestCredentialsPersistAcrossRestart(t *testing.T) {
	mock := newMockOpenCode(t)
	t.Cleanup(mock.Close)
	dir := t.TempDir()

	h1, err := startHost(t, mock, dir)
	if err != nil {
		t.Fatalf("first host: %v", err)
	}
	if status, body := h1.importKeys(t, []string{keyAlpha, keyBeta}, "persist"); status != http.StatusOK {
		h1.Stop()
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	if status, raw := sendChat(t, h1, nativeGLM, "persist-S1", "full", false, nil); status != http.StatusOK {
		h1.Stop()
		t.Fatalf("pre-restart request status %d body %s", status, truncate(raw, 400))
	}
	before, _ := lastUpstreamCall(h1, "/v1/chat/completions")
	beforeKey := upstreamKeyOf(before)
	beforeScope := before.Header.Get("x-opencode-session")
	h1.Stop()
	mock.reset()

	h2, err := startHost(t, mock, dir)
	if err != nil {
		t.Fatalf("second host: %v", err)
	}
	t.Cleanup(h2.Stop)

	// Credentials persist on disk, but model/credential registration is again
	// asynchronous after restart.
	h2.waitForCatalog(t, 10*time.Second, nativeGLM, nativeMinimax, nativeGPT)
	h2.waitForServing(t, nativeGLM, 10*time.Second)

	status, body := h2.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list after restart status %d body %s", status, truncate(body, 400))
	}
	var listed struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("list after restart not JSON: %v", err)
	}
	if len(listed.Files) != 2 {
		t.Fatalf("after restart saw %d credentials, want 2", len(listed.Files))
	}

	if status, raw := sendChat(t, h2, nativeGLM, "persist-S1", "full", false, nil); status != http.StatusOK {
		t.Fatalf("post-restart request status %d body %s", status, truncate(raw, 400))
	}
	after, _ := lastUpstreamCall(h2, "/v1/chat/completions")
	afterKey := upstreamKeyOf(after)
	afterScope := after.Header.Get("x-opencode-session")
	// The upstream session scope is a pure function of auth id + canonical
	// session. Restart drops the in-memory affinity binding, so round-robin may
	// select a different credential; determinism is only required when the same
	// credential is reselected.
	if afterKey == beforeKey {
		if afterScope != beforeScope {
			t.Errorf("same credential produced a different deterministic session: %q -> %q", beforeScope, afterScope)
		}
	} else {
		t.Logf("restart reselected credential %q -> %q; auth-scoped sessions cannot be compared", beforeKey, afterKey)
	}
}

// --- assertions ------------------------------------------------------------

func assertCounts(t *testing.T, body []byte, imported, skipped, failed int) {
	t.Helper()
	var result map[string]int
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("import response not JSON: %v\n%s", err, truncate(body, 300))
	}
	if result["imported"] != imported || result["skipped"] != skipped || result["failed"] != failed {
		t.Errorf("import counts = %v, want imported=%d skipped=%d failed=%d", result, imported, skipped, failed)
	}
}

func assertListSanitized(t *testing.T, body []byte, authDir string) {
	t.Helper()
	text := string(body)
	for _, secret := range []string{keyAlpha, keyBeta, clientKey, mgmtKey} {
		if strings.Contains(text, secret) {
			t.Errorf("list response leaked credential %q", secret)
		}
	}
	for _, forbidden := range []string{"api_key", "apiKey", authDir, "raw_json", "rawJSON"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("list response contains forbidden marker %q: %s", forbidden, truncate(body, 400))
		}
	}
	var listed struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("list response not JSON: %v", err)
	}
	allowed := map[string]struct{}{
		"name": {}, "auth_index": {}, "label": {}, "status": {}, "disabled": {},
		"unavailable": {}, "success": {}, "failed": {},
	}
	for _, entry := range listed.Files {
		name, _ := entry["name"].(string)
		if !strings.HasPrefix(name, providerID+"-") || !strings.HasSuffix(name, ".json") {
			t.Errorf("list entry name %q is not the stable opencode-go file name", name)
		}
		// The quota UI needs a bounded, non-secret lookup token, not auth data.
		index, ok := entry["auth_index"].(string)
		if !ok || index == "" || len(index) > 200 || strings.ContainsAny(index, " \t\r\n\x00") {
			t.Errorf("list entry has no safe quota lookup index")
		}
		for field := range entry {
			if _, ok := allowed[field]; !ok {
				t.Errorf("list entry exposes unexpected field %q: %v", field, entry)
			}
		}
	}
}

func assertAuthFileContent(t *testing.T, path, key string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file %s: %v", path, err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("auth file %s not JSON: %v", path, err)
	}
	if got, _ := record["type"].(string); got != providerID {
		t.Errorf("auth file type = %q, want %q", got, providerID)
	}
	if got, _ := record["api_key"].(string); got != key {
		t.Errorf("auth file api_key mismatch")
	}
	for _, forbidden := range []string{"name", "path"} {
		if _, ok := record[forbidden]; ok {
			t.Errorf("auth file exposes arbitrary field %q: %v", forbidden, record)
		}
	}
}

func authFilesOnDisk(t *testing.T, authDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

func countAuthFiles(t *testing.T, authDir string) int {
	t.Helper()
	return len(authFilesOnDisk(t, authDir))
}
