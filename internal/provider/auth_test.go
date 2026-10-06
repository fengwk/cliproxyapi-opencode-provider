package provider

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParseAuthMapsPhysicalRecord(t *testing.T) {
	raw := []byte(`{"type":"opencode-go","api_key":"sk-secret","label":"team","disabled":false,"weight":3,"prefix":"pfx","proxy_url":"http://127.0.0.1:1"}`)
	resp, err := parseAuth(pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "opencode-go-abc.json", RawJSON: raw})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Handled {
		t.Fatalf("record was not handled")
	}
	if resp.Auth.ID != "opencode-go-abc.json" || resp.Auth.FileName != "opencode-go-abc.json" {
		t.Fatalf("id must mirror the host file name: %+v", resp.Auth)
	}
	if resp.Auth.Attributes["api_key"] != "sk-secret" {
		t.Fatalf("api key attribute missing")
	}
	if resp.Auth.Prefix != "pfx" || resp.Auth.ProxyURL != "http://127.0.0.1:1" {
		t.Fatalf("native fields not preserved: %+v", resp.Auth)
	}
	if _, leaked := resp.Auth.Metadata["api_key"]; leaked {
		t.Fatalf("metadata must not contain the api key")
	}
	if resp.Auth.Metadata["weight"].(float64) != 3 {
		t.Fatalf("weight metadata not preserved: %+v", resp.Auth.Metadata)
	}
}

func TestParseAuthIgnoresOtherProviders(t *testing.T) {
	resp, err := parseAuth(pluginapi.AuthParseRequest{Provider: "other", RawJSON: []byte(`{"type":"other","api_key":"x"}`)})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Handled {
		t.Fatalf("foreign provider must not be handled")
	}
	// Non-JSON files for a foreign provider are ignored.
	resp, err = parseAuth(pluginapi.AuthParseRequest{Provider: "other", RawJSON: []byte("not json")})
	if err != nil || resp.Handled {
		t.Fatalf("foreign non-json file must be ignored: %+v %v", resp, err)
	}
}

func TestParseAuthRejectsMissingKey(t *testing.T) {
	if _, err := parseAuth(pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "opencode-go-x.json", RawJSON: []byte(`{"type":"opencode-go"}`)}); err == nil {
		t.Fatalf("expected missing key error")
	}
}

// TestResolveKeyFallbackOrder verifies attributes win, then metadata (the
// host.auth.save initial state), then storage JSON.
func TestResolveKeyFallbackOrder(t *testing.T) {
	if key, err := resolveKey(map[string]string{"api_key": "attr"}, map[string]any{"api_key": "meta"}, []byte(`{"api_key":"store"}`)); err != nil || key != "attr" {
		t.Fatalf("attributes should win: %q %v", key, err)
	}
	if key, err := resolveKey(nil, map[string]any{"api_key": "meta"}, []byte(`{"api_key":"store"}`)); err != nil || key != "meta" {
		t.Fatalf("metadata fallback failed: %q %v", key, err)
	}
	if key, err := resolveKey(nil, nil, []byte(`{"api_key":"store"}`)); err != nil || key != "store" {
		t.Fatalf("storage fallback failed: %q %v", key, err)
	}
	if _, err := resolveKey(nil, map[string]any{"api_key": "  "}, nil); err == nil {
		t.Fatalf("expected missing key error")
	}
}

func TestAuthRefreshIsPassthrough(t *testing.T) {
	manager := newTestManager(newFakeHost())
	req := pluginapi.AuthRefreshRequest{
		AuthID:       "opencode-go-x.json",
		StorageJSON:  []byte(`{"type":"opencode-go","api_key":"sk"}`),
		Metadata:     map[string]any{"note": "keep"},
		Attributes:   map[string]string{"api_key": "sk"},
		AuthProvider: ProviderID,
	}
	payload, _ := json.Marshal(req)
	raw, err := manager.HandleCall(pluginabi.MethodAuthRefresh, payload)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var resp pluginapi.AuthRefreshResponse
	decodeResult(t, raw, &resp)
	if string(resp.Auth.StorageJSON) != string(req.StorageJSON) || resp.Auth.ID != req.AuthID {
		t.Fatalf("refresh must preserve data: %+v", resp.Auth)
	}
	if resp.Auth.Metadata["note"] != "keep" {
		t.Fatalf("refresh must preserve metadata")
	}
}

func TestLoginUnsupported(t *testing.T) {
	manager := newTestManager(newFakeHost())
	for _, method := range []string{pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll} {
		raw, err := manager.HandleCall(method, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if envErr := decodeError(t, raw); envErr.Code != "unsupported" {
			t.Fatalf("%s code = %q, want unsupported", method, envErr.Code)
		}
	}
}
