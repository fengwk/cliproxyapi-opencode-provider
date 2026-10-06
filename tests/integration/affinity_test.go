//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// upstreamKeyOf extracts the upstream credential a native call used.
func upstreamKeyOf(call mockCall) string {
	if key := bearerKey(call.Header.Get("Authorization")); key != "" {
		return key
	}
	return call.Header.Get("x-api-key")
}

// lastUpstreamCall returns the most recent call to a native path.
func lastUpstreamCall(h *harness, path string) (mockCall, bool) {
	calls := h.mock.callsForPath(path)
	if len(calls) == 0 {
		return mockCall{}, false
	}
	return calls[len(calls)-1], true
}

// sendChat sends one chat client request, optionally without an explicit
// session header, and returns status plus body.
func sendChat(t *testing.T, h *harness, native, session, mode string, stream bool, history []any) (int, []byte) {
	t.Helper()
	var body map[string]any
	if history != nil {
		body = map[string]any{
			"model":    publicModel(native),
			"stream":   stream,
			"messages": history,
			"tools":    []any{chatTool()},
		}
	} else {
		raw, _ := clientChat.request(native, session, mode, stream)
		_ = json.Unmarshal(raw, &body)
	}
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + clientKey,
	}
	if session != "" {
		headers["x-opencode-session"] = session
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, mustMarshal(body), headers)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	return status, raw
}

// TestSessionAffinityExplicit covers explicit session stickiness and isolation.
func TestSessionAffinityExplicit(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "affinity"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	// Turn 1 binds session S1 to a credential.
	if status, raw := sendChat(t, h, nativeGLM, "affinity-S1", "full", false, nil); status != http.StatusOK {
		t.Fatalf("turn1 status %d body %s", status, truncate(raw, 400))
	}
	first, ok := lastUpstreamCall(h, "/v1/chat/completions")
	if !ok {
		t.Fatal("no upstream call for turn1")
	}
	firstKey := upstreamKeyOf(first)
	firstScope := first.Header.Get("x-opencode-session")
	if firstKey == "" || firstScope == "" {
		t.Fatalf("turn1 missing key/scope: key=%q scope=%q", firstKey, firstScope)
	}

	// Turn 2 with the same explicit session must reuse key and upstream session.
	if status, raw := sendChat(t, h, nativeGLM, "affinity-S1", "full", false, nil); status != http.StatusOK {
		t.Fatalf("turn2 status %d body %s", status, truncate(raw, 400))
	}
	second, _ := lastUpstreamCall(h, "/v1/chat/completions")
	if got := upstreamKeyOf(second); got != firstKey {
		t.Errorf("turn2 credential = %q, want the bound %q", got, firstKey)
	}
	if got := second.Header.Get("x-opencode-session"); got != firstScope {
		t.Errorf("turn2 upstream session = %q, want %q", got, firstScope)
	}

	// A new explicit session must not inherit the previous upstream session.
	if status, raw := sendChat(t, h, nativeGLM, "affinity-S2", "full", false, nil); status != http.StatusOK {
		t.Fatalf("turn3 status %d body %s", status, truncate(raw, 400))
	}
	third, _ := lastUpstreamCall(h, "/v1/chat/completions")
	if got := third.Header.Get("x-opencode-session"); got == firstScope {
		t.Errorf("new session reused the previous upstream session %q", got)
	}
}

// TestSessionFallbackContent covers the no-explicit-header case where CPA owns
// the derived/LCP session identity: repeated identical history stays stable.
func TestSessionFallbackContent(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "fallback"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	history := []any{
		map[string]any{"role": "user", "content": "My name is Ada. " + markerFor("full")},
		map[string]any{"role": "assistant", "content": "Hi Ada."},
		map[string]any{"role": "user", "content": "What is my name?"},
	}

	if status, raw := sendChat(t, h, nativeGLM, "", "", false, history); status != http.StatusOK {
		t.Fatalf("first status %d body %s", status, truncate(raw, 400))
	}
	firstCalls := h.mock.callsForPath("/v1/chat/completions")
	if len(firstCalls) == 0 {
		t.Fatal("no upstream call for first fallback turn")
	}
	first := firstCalls[len(firstCalls)-1]
	firstScope := first.Header.Get("x-opencode-session")
	firstKey := upstreamKeyOf(first)

	if status, raw := sendChat(t, h, nativeGLM, "", "", false, history); status != http.StatusOK {
		t.Fatalf("second status %d body %s", status, truncate(raw, 400))
	}
	second := h.mock.callsForPath("/v1/chat/completions")
	last := second[len(second)-1]

	if got := last.Header.Get("x-opencode-session"); got != firstScope {
		t.Errorf("fallback session scope changed: %q -> %q", firstScope, got)
	}
	if got := upstreamKeyOf(last); got != firstKey {
		t.Errorf("fallback credential changed: %q -> %q", firstKey, got)
	}

	// The canonical conversation root must survive to the upstream body.
	var body map[string]any
	if err := json.Unmarshal(last.Body, &body); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	messages := bodyArray(body, "messages")
	if len(messages) != len(history) {
		t.Errorf("upstream messages length = %d, want %d", len(messages), len(history))
	}
}

// TestFailoverNonStream proves CPA switches credentials on a 429 within one
// client request and rebinds the session afterwards.
func TestFailoverNonStream(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "failover"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	const session = "failover-S1"
	// Bind the session to a credential first.
	if status, raw := sendChat(t, h, nativeGLM, session, "full", false, nil); status != http.StatusOK {
		t.Fatalf("bind status %d body %s", status, truncate(raw, 400))
	}
	bound, _ := lastUpstreamCall(h, "/v1/chat/completions")
	boundKey := upstreamKeyOf(bound)
	if boundKey == "" {
		t.Fatal("bind request used no credential")
	}
	h.mock.reset()

	// Force one 429; the client must still see a single successful response.
	status, raw := sendChat(t, h, nativeGLM, session, "fail_once", false, nil)
	if status != http.StatusOK {
		t.Fatalf("failover request status %d body %s", status, truncate(raw, 400))
	}
	assertObserved(t, observeChatNonStream(t, raw))

	calls := h.mock.callsForPath("/v1/chat/completions")
	if len(calls) < 2 {
		t.Fatalf("failover produced %d upstream attempts, want >= 2", len(calls))
	}
	failedKey := upstreamKeyOf(calls[0])
	retryKey := upstreamKeyOf(calls[1])
	if failedKey != boundKey {
		t.Errorf("first failover attempt used %q, want bound %q", failedKey, boundKey)
	}
	if retryKey == failedKey {
		t.Errorf("failover retried the same credential %q", retryKey)
	}
	failedScope := calls[0].Header.Get("x-opencode-session")
	retryScope := calls[1].Header.Get("x-opencode-session")
	if retryScope == failedScope {
		t.Errorf("failover kept the same upstream session %q", retryScope)
	}

	// The next turn on the same session stays on the new credential/session.
	if status, raw := sendChat(t, h, nativeGLM, session, "full", false, nil); status != http.StatusOK {
		t.Fatalf("post-failover status %d body %s", status, truncate(raw, 400))
	}
	after, _ := lastUpstreamCall(h, "/v1/chat/completions")
	if got := upstreamKeyOf(after); got != retryKey {
		t.Errorf("post-failover credential = %q, want %q", got, retryKey)
	}
	if got := after.Header.Get("x-opencode-session"); got != retryScope {
		t.Errorf("post-failover upstream session = %q, want %q", got, retryScope)
	}
}

// TestFailoverStreamOpening proves a 429 before any output is retried and the
// client still receives a complete stream.
func TestFailoverStreamOpening(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "failover-stream"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	const session = "failover-stream-S1"
	if status, raw := sendChat(t, h, nativeGLM, session, "full", true, nil); status != http.StatusOK {
		t.Fatalf("bind stream status %d body %s", status, truncate(raw, 400))
	}
	h.mock.reset()

	status, raw := sendChat(t, h, nativeGLM, session, "fail_once", true, nil)
	if status != http.StatusOK {
		t.Fatalf("stream failover status %d body %s", status, truncate(raw, 400))
	}
	obs := observeChatStream(t, raw)
	if obs.ToolName != fixtureToolName {
		t.Errorf("stream failover lost the tool call: %s", describe(obs))
	}

	calls := h.mock.callsForPath("/v1/chat/completions")
	if len(calls) < 2 {
		t.Fatalf("stream failover produced %d upstream attempts, want >= 2", len(calls))
	}
	if upstreamKeyOf(calls[0]) == upstreamKeyOf(calls[1]) {
		t.Errorf("stream failover retried the same credential %q", upstreamKeyOf(calls[1]))
	}
}

var _ = fmt.Sprintf
