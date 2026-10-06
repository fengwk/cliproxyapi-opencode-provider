package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func splitBytes(raw []byte, size int) [][]byte {
	var out [][]byte
	for len(raw) > 0 {
		n := size
		if n > len(raw) {
			n = len(raw)
		}
		out = append(out, raw[:n])
		raw = raw[n:]
	}
	return out
}

func streamExecBytes(t *testing.T, req pluginapi.ExecutorRequest, streamID, hostCallbackID string) []byte {
	t.Helper()
	payload, err := json.Marshal(rpcExecutorRequest{ExecutorRequest: req, StreamID: streamID, HostCallbackID: hostCallbackID})
	if err != nil {
		t.Fatalf("encode executor stream request: %v", err)
	}
	return payload
}

func joinedEmitted(host *fakeHost, streamID string) string {
	var builder strings.Builder
	for _, payload := range host.emitted(streamID) {
		builder.Write(payload)
	}
	return builder.String()
}

// runTranslatedStream drives one cross-protocol streaming request and returns
// everything the plugin emitted downstream.
func runTranslatedStream(t *testing.T, payload, model, sse string, chunkSize int) string {
	t.Helper()
	host := newFakeHost()
	host.queueStream(splitBytes([]byte(sse), chunkSize)...)
	manager := configuredManager(t, host, "")
	req := baseExecRequest()
	req.Stream = true
	req.Model = "opencode-go/" + model
	req.Payload = []byte(payload)
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	decodeResult(t, rawResp, nil)
	if !waitForClose(t, host, 2*time.Second) {
		t.Fatalf("downstream stream was not closed")
	}
	emitted := joinedEmitted(host, "down-stream")
	// Every emitted chunk for the chat target must be a standalone JSON object.
	for _, chunk := range host.emitted("down-stream") {
		var obj map[string]any
		if err := json.Unmarshal(chunk, &obj); err != nil {
			t.Fatalf("emitted chunk is not bare json: %q", chunk)
		}
	}
	return emitted
}

// TestStreamPassthroughOpenAI verifies same-format streaming strips SSE framing
// so the host handler can re-frame bare JSON chunks, and that [DONE] is dropped.
func TestStreamPassthroughOpenAI(t *testing.T) {
	host := newFakeHost()
	raw := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	host.queueStream(splitBytes([]byte(raw), 5)...)
	manager := configuredManager(t, host, "")

	req := baseExecRequest()
	req.Stream = true
	req.Payload = []byte(`{"model":"opencode-go/glm-5","stream":true}`)
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	decodeResult(t, rawResp, nil)

	if !waitForClose(t, host, 2*time.Second) {
		t.Fatalf("downstream stream was not closed")
	}
	emitted := joinedEmitted(host, "down-stream")
	if !strings.Contains(emitted, `"id":"1"`) {
		t.Fatalf("chunk not forwarded: %q", emitted)
	}
	if strings.Contains(emitted, "[DONE]") {
		t.Fatalf("[DONE] must be dropped for the chat handler: %q", emitted)
	}
	if strings.Contains(emitted, "data:") {
		t.Fatalf("SSE framing must be stripped: %q", emitted)
	}
}

// TestStreamTranslatedSplitFrames verifies cross-protocol streaming translation
// through the SDK works when upstream reads split SSE delimiters and lines, and
// that usage is preserved.
func TestStreamTranslatedSplitFrames(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"minimax-m2.5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	emitted := runTranslatedStream(t,
		`{"model":"opencode-go/minimax-m2.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"minimax-m2.5", sse, 3)
	if !strings.Contains(emitted, "Hello") {
		t.Fatalf("translated content missing: %q", emitted)
	}
	if !strings.Contains(emitted, `"finish_reason":"stop"`) {
		t.Fatalf("translated finish_reason missing: %q", emitted)
	}
	if !strings.Contains(emitted, `"total_tokens":8`) {
		t.Fatalf("translated usage missing: %q", emitted)
	}
}

// TestStreamTranslatedToolCalls verifies tool-call deltas survive streaming
// translation.
func TestStreamTranslatedToolCalls(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_t","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	emitted := runTranslatedStream(t,
		`{"model":"opencode-go/minimax-m2.5","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`,
		"minimax-m2.5", sse, 4)
	if !strings.Contains(emitted, `"tool_calls"`) || !strings.Contains(emitted, "get_weather") {
		t.Fatalf("tool call missing: %q", emitted)
	}
	if !strings.Contains(emitted, `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool finish reason missing: %q", emitted)
	}
}

// TestStreamTranslatedReasoning verifies reasoning deltas survive streaming
// translation.
func TestStreamTranslatedReasoning(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_r","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
	emitted := runTranslatedStream(t,
		`{"model":"opencode-go/minimax-m2.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"minimax-m2.5", sse, 4)
	if !strings.Contains(emitted, `"reasoning_content":"pondering"`) {
		t.Fatalf("reasoning content missing: %q", emitted)
	}
}

// TestStreamUpstreamStatusFailsBeforeOutput verifies a non-2xx upstream status
// is returned synchronously so the host can retry before any output.
func TestStreamUpstreamStatusFailsBeforeOutput(t *testing.T) {
	host := newFakeHost()
	url := "https://opencode.ai/zen/go/v1/chat/completions"
	host.httpResponses[url] = pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable}
	manager := configuredManager(t, host, "")

	req := baseExecRequest()
	req.Stream = true
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	envErr := decodeError(t, rawResp)
	if envErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", envErr.StatusCode())
	}
	if len(host.emitted("down-stream")) != 0 {
		t.Fatalf("no output may be emitted on upstream failure")
	}
}

// TestShutdownDrainsStreamPumps verifies shutdown releases blocked stream reads
// and waits for the pump goroutine before returning.
func TestShutdownDrainsStreamPumps(t *testing.T) {
	host := newFakeHost()
	_, ch := host.openStream()
	manager := configuredManager(t, host, "")

	req := baseExecRequest()
	req.Stream = true
	if _, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb")); err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		manager.shutdown()
		close(done)
	}()
	close(ch)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not drain the stream pump")
	}
}

// TestStreamChatUpstreamToResponsesFlushesDone verifies the native chat [DONE]
// sentinel is fed through the SDK translator so a Responses client receives the
// deferred terminal event and late usage instead of a synthesized success.
func TestStreamChatUpstreamToResponsesFlushesDone(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
		"data: [DONE]\n\n"
	host := newFakeHost()
	host.queueStream(splitBytes([]byte(sse), 5)...)
	manager := configuredManager(t, host, "")

	req := baseExecRequest()
	req.Model = "opencode-go/glm-5"
	req.Format = "openai-response"
	req.SourceFormat = "openai-response"
	req.Stream = true
	req.Payload = []byte(`{"model":"opencode-go/glm-5","stream":true,"input":[{"role":"user","content":"hi"}]}`)
	req.OriginalRequest = req.Payload
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	decodeResult(t, rawResp, nil)
	if !waitForClose(t, host, 3*time.Second) {
		t.Fatalf("downstream stream was not closed")
	}
	closes := host.closes()
	if len(closes) == 0 || closes[len(closes)-1].Error != "" {
		t.Fatalf("stream must close successfully, got %+v", closes)
	}
	emitted := joinedEmitted(host, "down-stream")
	if !strings.Contains(emitted, "response.completed") {
		t.Fatalf("terminal response.completed missing: %q", emitted)
	}
	if !strings.Contains(emitted, `"total_tokens":8`) {
		t.Fatalf("terminal usage missing: %q", emitted)
	}
}

// TestStreamNativeRawPassthroughCompleteEvents verifies native Claude and
// Responses passthrough emits exactly one whole event per chunk, byte-native,
// even when upstream reads split UTF-8 sequences and event boundaries.
func TestStreamNativeRawPassthroughCompleteEvents(t *testing.T) {
	const claudeSSE = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"minimax-m2.5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	const responsesSSE = "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"created_at\":1700000000,\"model\":\"gpt-5.6\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
	const claudeCRLFSSE = ": ping\r\n\r\n" +
		"event: message_start\r\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"minimax-m2.5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\r\n\r\n" +
		"event: message_stop\r\n" +
		"data: {\"type\":\"message_stop\"}\r\n\r\n"
	cases := []struct {
		name    string
		model   string
		format  string
		payload string
		sse     string
		events  int
	}{
		{
			name:    "claude",
			model:   "minimax-m2.5",
			format:  "claude",
			payload: `{"model":"opencode-go/minimax-m2.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			sse:     claudeSSE,
			events:  3,
		},
		{
			name:    "responses",
			model:   "gpt-5.6",
			format:  "openai-response",
			payload: `{"model":"opencode-go/gpt-5.6","stream":true,"input":[{"role":"user","content":"hi"}]}`,
			sse:     responsesSSE,
			events:  3,
		},
		{
			name:    "claude-crlf-heartbeat",
			model:   "minimax-m2.5",
			format:  "claude",
			payload: `{"model":"opencode-go/minimax-m2.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			sse:     claudeCRLFSSE,
			events:  3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			host.queueStream(splitBytes([]byte(tc.sse), 1)...)
			manager := configuredManager(t, host, "")

			req := baseExecRequest()
			req.Model = "opencode-go/" + tc.model
			req.Format = tc.format
			req.SourceFormat = tc.format
			req.Stream = true
			req.Payload = []byte(tc.payload)
			req.OriginalRequest = req.Payload
			rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
			if err != nil {
				t.Fatalf("execute stream: %v", err)
			}
			decodeResult(t, rawResp, nil)
			if !waitForClose(t, host, 3*time.Second) {
				t.Fatalf("downstream stream was not closed")
			}
			if err := lastCloseError(host); err != "" {
				t.Fatalf("close error = %q, want success", err)
			}
			chunks := host.emitted("down-stream")
			if len(chunks) != tc.events {
				t.Fatalf("expected %d complete events, got %d: %q", tc.events, len(chunks), chunks)
			}
			for i, chunk := range chunks {
				if !strings.HasSuffix(string(chunk), "\n\n") && !strings.HasSuffix(string(chunk), "\r\n\r\n") {
					t.Fatalf("chunk %d is a partial frame: %q", i, chunk)
				}
			}
			if joined := joinedEmitted(host, "down-stream"); joined != tc.sse {
				t.Fatalf("raw passthrough must be byte-native:\n got %q\nwant %q", joined, tc.sse)
			}
		})
	}
}
