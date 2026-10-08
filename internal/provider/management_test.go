package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const keysPath = managementPath + keysRoute

func mgmtPayload(t *testing.T, method, path string, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(managementRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: method,
		Path:   path,
		Body:   body,
	}})
	if err != nil {
		t.Fatalf("encode management request: %v", err)
	}
	return raw
}

func callManagement(t *testing.T, manager *Manager, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := manager.HandleCall(pluginabi.MethodManagementHandle, mgmtPayload(t, method, path, body))
	if err != nil {
		t.Fatalf("management call: %v", err)
	}
	var resp pluginapi.ManagementResponse
	decodeResult(t, raw, &resp)
	return resp
}

func TestManagementRegistration(t *testing.T) {
	manager := newTestManager(newFakeHost())
	raw, err := manager.HandleCall(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg managementRegistrationResponse
	decodeResult(t, raw, &reg)
	foundKeys := false
	for _, route := range reg.Routes {
		if route.Path == keysRoute && route.Method == http.MethodGet {
			foundKeys = true
		}
	}
	if !foundKeys {
		t.Fatalf("GET keys route not registered: %+v", reg.Routes)
	}
	foundPost := false
	for _, route := range reg.Routes {
		if route.Path == keysRoute && route.Method == http.MethodPost {
			foundPost = true
		}
	}
	if !foundPost {
		t.Fatalf("POST keys route not registered: %+v", reg.Routes)
	}
	if len(reg.Resources) != 3 {
		t.Fatalf("expected 3 resource routes, got %+v", reg.Resources)
	}
}

// TestUIDisplayNameConsistency keeps the management menu and page header aligned
// with the shared plugin display name while preserving the technical ID and the
// upstream-facing OpenCode Go labels.
func TestUIDisplayNameConsistency(t *testing.T) {
	reg := managementRegistration()
	var ui *pluginapi.ResourceRoute
	for i := range reg.Resources {
		if reg.Resources[i].Path == "/ui" {
			ui = &reg.Resources[i]
		}
	}
	if ui == nil {
		t.Fatal("ui resource not registered")
	}
	if ui.Menu != PluginName {
		t.Fatalf("management menu = %q, want plugin display name %q", ui.Menu, PluginName)
	}
	if ui.Menu == PluginID {
		t.Fatalf("management menu must not be the technical ID %q", PluginID)
	}

	manager := newTestManager(newFakeHost())
	resp := callManagement(t, manager, http.MethodGet, authResourcePath+"/ui", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	html := string(resp.Body)
	if !strings.Contains(html, "<title>"+PluginName+" · 密钥与配额管理</title>") {
		t.Errorf("ui.html title must display %q", PluginName)
	}
	if !strings.Contains(html, "<h1>"+PluginName+"</h1>") {
		t.Errorf("ui.html header must display %q", PluginName)
	}
	// The technical ID stays visible as a subtitle and upstream import labels
	// keep naming the OpenCode Go upstream.
	if !strings.Contains(html, PluginID) {
		t.Errorf("ui.html must preserve the technical ID %q", PluginID)
	}
	if !strings.Contains(html, "导入 OpenCode Go 密钥") {
		t.Errorf("ui.html must keep the OpenCode Go upstream key import label")
	}
}

// TestListKeysFiltersAndSanitizes verifies only OpenCode credentials are listed
// and no secret material (key, raw json, path) leaks.
func TestListKeysFiltersAndSanitizes(t *testing.T) {
	host := newFakeHost()
	host.authFiles = []pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "idx-a", Name: "opencode-go-aaa.json", Provider: ProviderID, Type: ProviderID, Label: "team", Status: "active", Success: 3, Failed: 1},
		{ID: "b", Name: "other.json", Provider: "other", Type: "other", Label: "secret"},
	}
	manager := newTestManager(host)
	resp := callManagement(t, manager, http.MethodGet, keysPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var decoded struct {
		Files []keyFileEntry `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Files) != 1 || decoded.Files[0].Name != "opencode-go-aaa.json" {
		t.Fatalf("filtering failed: %+v", decoded.Files)
	}
	if decoded.Files[0].Success != 3 || decoded.Files[0].Failed != 1 {
		t.Fatalf("counts not surfaced: %+v", decoded.Files[0])
	}
	if decoded.Files[0].AuthIndex != "idx-a" {
		t.Fatalf("quota lookup index = %q, want idx-a", decoded.Files[0].AuthIndex)
	}
	if strings.Contains(string(resp.Body), "other.json") || strings.Contains(string(resp.Body), `"path"`) || strings.Contains(string(resp.Body), "api_key") {
		t.Fatalf("list response leaked data: %s", resp.Body)
	}
}

func TestImportKeysHappyPath(t *testing.T) {
	host := newFakeHost()
	manager := newTestManager(host)
	body := []byte(`{"keys":["sk-a","sk-b"],"label":"team"}`)
	resp := callManagement(t, manager, http.MethodPost, keysPath, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var counts map[string]int
	if err := json.Unmarshal(resp.Body, &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if counts["imported"] != 2 || counts["failed"] != 0 || counts["skipped"] != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	saved := host.savedAuths()
	if len(saved) != 2 {
		t.Fatalf("expected 2 saved auths, got %d", len(saved))
	}
	for _, entry := range saved {
		if !strings.HasPrefix(entry.Name, ProviderID+"-") || !strings.HasSuffix(entry.Name, ".json") {
			t.Fatalf("unexpected file name %q", entry.Name)
		}
		var obj map[string]any
		if err := json.Unmarshal(entry.JSON, &obj); err != nil {
			t.Fatalf("saved json is not an object: %v", err)
		}
		if obj["type"] != ProviderID || obj["label"] != "team" {
			t.Fatalf("saved json shape: %+v", obj)
		}
	}
	if strings.Contains(string(resp.Body), "sk-a") {
		t.Fatalf("response leaked a key")
	}
	// The derived name must be the full sha256 of the key.
	sum := sha256.Sum256([]byte("sk-a"))
	expected := ProviderID + "-" + hex.EncodeToString(sum[:]) + ".json"
	if saved[0].Name != expected {
		t.Fatalf("name = %s, want %s", saved[0].Name, expected)
	}
}

func TestImportKeysDedupeAndSkipExisting(t *testing.T) {
	host := newFakeHost()
	host.authFiles = []pluginapi.HostAuthFileEntry{{Name: keyFileName("sk-a"), Provider: ProviderID, Type: ProviderID}}
	manager := newTestManager(host)
	resp := callManagement(t, manager, http.MethodPost, keysPath, []byte(`{"keys":["sk-a","sk-a"]}`))
	var counts map[string]int
	if err := json.Unmarshal(resp.Body, &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if counts["skipped"] != 1 || counts["imported"] != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if len(host.savedAuths()) != 0 {
		t.Fatalf("existing auth must not be overwritten")
	}
}

func TestImportKeysPartialFailure(t *testing.T) {
	host := newFakeHost()
	host.saveFail[keyFileName("sk-b")] = true
	manager := newTestManager(host)
	resp := callManagement(t, manager, http.MethodPost, keysPath, []byte(`{"keys":["sk-a","sk-b"]}`))
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	var counts map[string]int
	_ = json.Unmarshal(resp.Body, &counts)
	if counts["imported"] != 1 || counts["failed"] != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	if strings.Contains(string(resp.Body), "sk-") {
		t.Fatalf("response leaked key or host error")
	}
}

func TestImportKeysValidation(t *testing.T) {
	manager := newTestManager(newFakeHost())
	cases := []struct {
		body   string
		status int
	}{
		{`{"keys":[]}`, http.StatusBadRequest},
		{`{"keys":["has space"]}`, http.StatusBadRequest},
		{`{"keys":["tab\tkey"]}`, http.StatusBadRequest},
		{`{"keys":["  "]}`, http.StatusBadRequest},
		{`{"keys":["` + strings.Repeat("k", maxKeyLength+1) + `"]}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := callManagement(t, manager, http.MethodPost, keysPath, []byte(tc.body))
		if resp.StatusCode != tc.status {
			t.Fatalf("body %q: status %d, want %d", tc.body, resp.StatusCode, tc.status)
		}
	}

	tooMany := make([]string, maxImportKeys+1)
	for i := range tooMany {
		tooMany[i] = "k" + strings.Repeat("x", i)
	}
	encoded, _ := json.Marshal(map[string]any{"keys": tooMany})
	if resp := callManagement(t, manager, http.MethodPost, keysPath, encoded); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("too many keys: status %d", resp.StatusCode)
	}

	oversized := make([]byte, maxImportBody+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if resp := callManagement(t, manager, http.MethodPost, keysPath, oversized); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d", resp.StatusCode)
	}
}

// TestResourceRoutesServeAssets verifies public resource routes return the
// embedded assets with restrictive security headers.
func TestResourceRoutesServeAssets(t *testing.T) {
	manager := newTestManager(newFakeHost())
	resp := callManagement(t, manager, http.MethodGet, authResourcePath+"/ui", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "<html") {
		t.Fatalf("ui.html not served: %s", resp.Body)
	}
	// Exact policies also block native form submissions if the UI script cannot run.
	wantCSP := "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
	for _, resource := range []string{"/ui", "/ui.js", "/ui.css"} {
		resp := callManagement(t, manager, http.MethodGet, authResourcePath+resource, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", resource, resp.StatusCode)
		}
		if got := resp.Headers.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s CSP = %q, want %q", resource, got, wantCSP)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"Cache-Control":          "no-store",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := resp.Headers.Get(header); got != want {
				t.Errorf("%s %s = %q, want %q", resource, header, got, want)
			}
		}
	}
	if resp := callManagement(t, manager, http.MethodGet, authResourcePath+"/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resource status = %d", resp.StatusCode)
	}
}

// TestImportKeysAuthListFailureNoWrites verifies a failed credential listing
// aborts the import with a sanitized 502 and performs no writes, so existing
// credentials can never be overwritten blind.
func TestImportKeysAuthListFailureNoWrites(t *testing.T) {
	host := newFakeHost()
	host.forcedErrors[pluginabi.MethodHostAuthList] = errors.New("host secret detail")
	manager := newTestManager(host)

	resp := callManagement(t, manager, http.MethodPost, keysPath, []byte(`{"keys":["sk-secret-value"]}`))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if saved := host.savedAuths(); len(saved) != 0 {
		t.Fatalf("a list failure must perform zero saves, got %d", len(saved))
	}
	if strings.Contains(string(resp.Body), "secret") || strings.Contains(string(resp.Body), "sk-") {
		t.Fatalf("list failure leaked host or key text: %s", resp.Body)
	}
}

// decodeImportCounts extracts the import result counts from a raw RPC envelope.
func decodeImportCounts(raw []byte) map[string]int {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil
	}
	var counts map[string]int
	if err := json.Unmarshal(resp.Body, &counts); err != nil {
		return nil
	}
	return counts
}

// TestImportKeysConcurrentDuplicateSavesOnce verifies the check-and-save is
// atomic: two importers submitting the same key start together, yet the host
// observes exactly one save and one skip, and the importer's metadata is kept.
func TestImportKeysConcurrentDuplicateSavesOnce(t *testing.T) {
	host := newFakeHost()
	// Wrap the callbacks so a successful save is reflected in later list
	// responses, mirroring the real host's persisted credential state.
	manager := NewManager(NewBridge(func(method string, payload []byte) ([]byte, error) {
		raw, err := host.call(method, payload)
		if err == nil && method == pluginabi.MethodHostAuthSave {
			var req struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(payload, &req) == nil {
				host.addAuthFile(pluginapi.HostAuthFileEntry{Name: req.Name, Provider: ProviderID, Type: ProviderID})
			}
		}
		return raw, err
	}))

	labels := []string{"first", "second"}
	payloads := make([][]byte, len(labels))
	for i, label := range labels {
		body, err := json.Marshal(importRequest{Keys: []string{"sk-dup"}, Label: label})
		if err != nil {
			t.Fatalf("encode import body: %v", err)
		}
		payloads[i] = mgmtPayload(t, http.MethodPost, keysPath, body)
	}

	start := make(chan struct{})
	counts := make([]map[string]int, len(labels))
	var wg sync.WaitGroup
	for i := range labels {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			raw, err := manager.HandleCall(pluginabi.MethodManagementHandle, payloads[i])
			if err != nil {
				return
			}
			counts[i] = decodeImportCounts(raw)
		}(i)
	}
	close(start)
	wg.Wait()

	total := func(field string) int {
		sum := 0
		for _, c := range counts {
			sum += c[field]
		}
		return sum
	}
	if total("imported") != 1 || total("skipped") != 1 {
		t.Fatalf("concurrent duplicate import counts = %+v", counts)
	}
	saved := host.savedAuths()
	if len(saved) != 1 {
		t.Fatalf("duplicate key must be saved exactly once, got %d", len(saved))
	}
	var savedObj map[string]any
	if err := json.Unmarshal(saved[0].JSON, &savedObj); err != nil {
		t.Fatalf("decode saved auth: %v", err)
	}
	winner := ""
	for i, c := range counts {
		if c["imported"] == 1 {
			winner = labels[i]
		}
	}
	if winner == "" || savedObj["label"] != winner {
		t.Fatalf("saved label %v, want importer label %q", savedObj["label"], winner)
	}
}
