//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// clientProtocol describes one inbound client surface.
type clientProtocol struct {
	name string
	path string
	kind string // chat | messages | responses
}

var (
	clientChat      = clientProtocol{name: "chat", path: "/v1/chat/completions", kind: "chat"}
	clientMessages  = clientProtocol{name: "messages", path: "/v1/messages", kind: "messages"}
	clientResponses = clientProtocol{name: "responses", path: "/v1/responses", kind: "responses"}
)

// request builds a client request for the given native upstream model.
func (p clientProtocol) request(native, session, mode string, stream bool) ([]byte, map[string]string) {
	marker := markerFor(mode)
	headers := map[string]string{
		"Content-Type":       "application/json",
		"x-opencode-session": session,
	}
	var body map[string]any
	switch p.kind {
	case "chat":
		headers["Authorization"] = "Bearer " + clientKey
		body = map[string]any{
			"model":  publicModel(native),
			"stream": stream,
			"messages": []any{
				map[string]any{"role": "user", "content": "Use the get_time tool. " + marker},
			},
			"tools": []any{chatTool()},
		}
	case "messages":
		headers["x-api-key"] = clientKey
		headers["anthropic-version"] = "2023-06-01"
		body = map[string]any{
			"model":      publicModel(native),
			"max_tokens": 1024,
			"stream":     stream,
			"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "Use the get_time tool. " + marker},
				}},
			},
			"tools": []any{claudeTool()},
		}
	default:
		headers["Authorization"] = "Bearer " + clientKey
		body = map[string]any{
			"model":  publicModel(native),
			"stream": stream,
			"input": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "Use the get_time tool. " + marker},
				}},
			},
			"tools": []any{responsesTool()},
		}
	}
	return mustMarshal(body), headers
}

// observe decodes the client-visible response for this protocol.
func (p clientProtocol) observe(t *testing.T, raw []byte, stream bool) observed {
	t.Helper()
	if stream {
		switch p.kind {
		case "chat":
			return observeChatStream(t, raw)
		case "messages":
			return observeMessagesStream(t, raw)
		default:
			return observeResponsesStream(t, raw)
		}
	}
	switch p.kind {
	case "chat":
		return observeChatNonStream(t, raw)
	case "messages":
		return observeMessagesNonStream(t, raw)
	default:
		return observeResponsesNonStream(t, raw)
	}
}

func timeParams() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tz": map[string]any{"type": "string", "description": "IANA timezone"},
		},
		"required": []any{"tz"},
	}
}

func chatTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        fixtureToolName,
			"description": "Return the current time.",
			"parameters":  timeParams(),
		},
	}
}

func claudeTool() map[string]any {
	return map[string]any{
		"name":         fixtureToolName,
		"description":  "Return the current time.",
		"input_schema": timeParams(),
	}
}

func responsesTool() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        fixtureToolName,
		"description": "Return the current time.",
		"parameters":  timeParams(),
	}
}

// nativeUpstreams is the upstream protocol axis of the matrix.
var nativeUpstreams = []struct {
	name   string
	native string
	path   string
}{
	{name: "glm_chat", native: nativeGLM, path: "/v1/chat/completions"},
	{name: "minimax_claude", native: nativeMinimax, path: "/v1/messages"},
	{name: "gpt_responses", native: nativeGPT, path: "/v1/responses"},
}

// TestProtocolMatrix exercises every client protocol against every native
// upstream protocol, non-streaming and streaming.
func TestProtocolMatrix(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "matrix"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	clients := []clientProtocol{clientChat, clientMessages, clientResponses}
	for _, client := range clients {
		client := client
		for _, upstream := range nativeUpstreams {
			upstream := upstream
			for _, stream := range []bool{false, true} {
				stream := stream
				name := fmt.Sprintf("%s_client_%s_upstream/stream=%t", client.name, upstream.name, stream)
				t.Run(name, func(t *testing.T) {
					session := fmt.Sprintf("matrix-%s-%s-%t", client.name, upstream.name, stream)
					body, headers := client.request(upstream.native, session, "full", stream)
					before := len(h.mock.callsForPath(upstream.path))

					status, raw := h.doJSON(t, http.MethodPost, client.path, body, headers)
					if status != http.StatusOK {
						t.Fatalf("status %d body %s", status, truncate(raw, 600))
					}
					obs := client.observe(t, raw, stream)
					assertObserved(t, obs)

					calls := h.mock.callsForPath(upstream.path)[before:]
					if len(calls) == 0 {
						t.Fatalf("no upstream call reached %s", upstream.path)
					}
					if got := mockModelOf(calls[len(calls)-1]); got != upstream.native {
						t.Errorf("upstream model = %q, want %q", got, upstream.native)
					}
				})
			}
		}
	}
	h.mock.requireClean(t)
}

// TestStreamTruncationNoFakeSuccess proves a truncated upstream stream is never
// presented as a clean terminal success and the host stays healthy.
func TestStreamTruncationNoFakeSuccess(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "truncate"); status != http.StatusOK {
		t.Fatalf("import keys: status %d body %s", status, truncate(body, 400))
	}
	h.mock.reset()

	body, headers := clientChat.request(nativeGLM, "truncate-session-1", "truncate", true)
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, mergedHeaders(headers))
	if err != nil {
		// A transport-level truncation is an acceptable error outcome.
		t.Logf("truncated stream surfaced transport error: %v", err)
	} else if status != http.StatusOK {
		t.Logf("truncated stream surfaced status %d", status)
	} else if hasCleanTerminal(raw, clientChat.kind) {
		t.Errorf("truncated stream produced a clean terminal success: %s", truncate(raw, 600))
	} else {
		t.Logf("truncated stream ended without a terminal sentinel (body %d bytes)", len(raw))
	}

	// The host and mock must both remain healthy after the truncated stream.
	healthBody, healthHeaders := clientChat.request(nativeGLM, "truncate-session-2", "full", false)
	healthStatus, healthRaw := h.doJSON(t, http.MethodPost, clientChat.path, healthBody, mergedHeaders(healthHeaders))
	if healthStatus != http.StatusOK {
		t.Fatalf("host unhealthy after truncation: status %d body %s", healthStatus, truncate(healthRaw, 400))
	}
	obs := observeChatNonStream(t, healthRaw)
	if strings.TrimSpace(obs.Text) != fixtureText {
		t.Errorf("post-truncation text = %q, want %q", obs.Text, fixtureText)
	}
}

// hasCleanTerminal reports whether an SSE body carries a protocol terminal event.
func hasCleanTerminal(raw []byte, kind string) bool {
	text := string(raw)
	switch kind {
	case "chat":
		return strings.Contains(text, "[DONE]")
	case "messages":
		return strings.Contains(text, "message_stop")
	default:
		return strings.Contains(text, "response.completed")
	}
}

// mergedHeaders gives a copy of headers without mutating the caller's map.
func mergedHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value
	}
	return out
}

func mockModelOf(call mockCall) string {
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil {
		return ""
	}
	return stringValue(body["model"])
}

// ensure strconv stays referenced for deterministic subtest naming via sprintf.
var _ = strconv.Itoa
