package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// TestHostHTTPErrorNotSurfaced verifies a raw host callback error that embeds a
// credential never reaches the plugin envelope.
func TestHostHTTPErrorNotSurfaced(t *testing.T) {
	host := newFakeHost()
	host.httpErrors["https://opencode.ai/zen/go/v1/chat/completions"] = errors.New("dial failed: sk-fake-secret")
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, baseExecRequest(), ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.Code != "host_call_failed" {
		t.Fatalf("code = %q, want host_call_failed", envErr.Code)
	}
	if strings.Contains(envErr.Message, "sk-fake") || strings.Contains(envErr.Message, "dial failed") {
		t.Fatalf("host error text leaked: %q", envErr.Message)
	}
}

// TestHostAuthListErrorNotSurfaced verifies a host error envelope containing a
// credential does not leak through the management list route.
func TestHostAuthListErrorNotSurfaced(t *testing.T) {
	host := newFakeHost()
	host.forcedErrors[pluginabi.MethodHostAuthList] = errors.New("boom sk-fake-secret")
	manager := newTestManager(host)
	resp := callManagement(t, manager, http.MethodGet, keysPath, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body := string(resp.Body)
	if strings.Contains(body, "sk-fake") || strings.Contains(body, "boom") {
		t.Fatalf("host error text leaked into management body: %q", body)
	}
}

// TestHostErrorEnvelopeNotSurfaced verifies the envelope error message from the
// host is replaced with a fixed message even when it is present.
func TestHostErrorEnvelopeNotSurfaced(t *testing.T) {
	bridge := NewBridge(func(method string, payload []byte) ([]byte, error) {
		return errResult("auth_denied", "denied for sk-fake-secret", http.StatusForbidden)
	})
	_, err := bridge.AuthList()
	if err == nil {
		t.Fatalf("expected a host error")
	}
	var hostErr *HostError
	if !errors.As(err, &hostErr) {
		t.Fatalf("error type = %T, want *HostError", err)
	}
	if hostErr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", hostErr.HTTPStatus)
	}
	if strings.Contains(hostErr.Error(), "sk-fake") || strings.Contains(hostErr.Message, "sk-fake") {
		t.Fatalf("host error text leaked: %q", hostErr.Error())
	}
}
