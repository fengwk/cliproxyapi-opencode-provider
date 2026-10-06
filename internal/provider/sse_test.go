package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// runChatStream drives a same-format chat passthrough stream and waits for the
// downstream close.
func runChatStream(t *testing.T, chunks ...[]byte) *fakeHost {
	t.Helper()
	host := newFakeHost()
	host.queueStream(chunks...)
	manager := configuredManager(t, host, "")
	req := baseExecRequest()
	req.Stream = true
	rawResp, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, "down-stream", "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	decodeResult(t, rawResp, nil)
	if !waitForClose(t, host, 3*time.Second) {
		t.Fatalf("downstream stream was not closed")
	}
	return host
}

func lastCloseError(host *fakeHost) string {
	closes := host.closes()
	if len(closes) == 0 {
		return "<no close>"
	}
	return closes[len(closes)-1].Error
}

// TestStreamJoinsMultilineDataEvents verifies consecutive data lines of one
// event are joined at the blank-line delimiter and dispatched exactly once.
func TestStreamJoinsMultilineDataEvents(t *testing.T) {
	raw := "data: {\"id\":\"1\",\ndata: \"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\n"
	host := runChatStream(t, splitBytes([]byte(raw), 3)...)
	if err := lastCloseError(host); err != "" {
		t.Fatalf("close error = %q, want success", err)
	}
	emitted := host.emitted("down-stream")
	if len(emitted) != 1 {
		t.Fatalf("expected exactly one event, got %d: %q", len(emitted), emitted)
	}
	if !json.Valid(emitted[0]) {
		t.Fatalf("joined payload is not valid json: %q", emitted[0])
	}
	if !strings.Contains(string(emitted[0]), `"content":"Hi"`) || !strings.Contains(string(emitted[0]), `"finish_reason":"stop"`) {
		t.Fatalf("joined payload lost fields: %q", emitted[0])
	}
}

// TestStreamCRLFAndComments verifies CRLF termination and comment lines are
// tolerated.
func TestStreamCRLFAndComments(t *testing.T) {
	raw := ": ping\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\r\n\r\n"
	host := runChatStream(t, splitBytes([]byte(raw), 2)...)
	if err := lastCloseError(host); err != "" {
		t.Fatalf("close error = %q, want success", err)
	}
	emitted := joinedEmitted(host, "down-stream")
	if !strings.Contains(emitted, "ok") {
		t.Fatalf("content missing: %q", emitted)
	}
	if strings.Contains(emitted, "ping") {
		t.Fatalf("comment must be dropped: %q", emitted)
	}
}

// TestStreamUTF8Split verifies multibyte payloads survive arbitrary byte splits.
func TestStreamUTF8Split(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"stop\"}]}\n\n"
	host := runChatStream(t, splitBytes([]byte(raw), 1)...)
	if err := lastCloseError(host); err != "" {
		t.Fatalf("close error = %q, want success", err)
	}
	if !strings.Contains(joinedEmitted(host, "down-stream"), "你好") {
		t.Fatalf("utf8 content missing: %q", joinedEmitted(host, "down-stream"))
	}
}

// TestStreamEarlyEOFWithoutTerminal verifies a clean EOF before any completion
// signal closes with an error instead of a fake success.
func TestStreamEarlyEOFWithoutTerminal(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	host := runChatStream(t, splitBytes([]byte(raw), 4)...)
	if err := lastCloseError(host); err == "" {
		t.Fatalf("early EOF must close with an error")
	}
	if !strings.Contains(joinedEmitted(host, "down-stream"), "a") {
		t.Fatalf("partial content should still be emitted")
	}
}

// TestStreamMalformedFrame verifies malformed data terminates with an error and
// is not emitted downstream.
func TestStreamMalformedFrame(t *testing.T) {
	host := runChatStream(t, []byte("data: {not json}\n\n"))
	if err := lastCloseError(host); err == "" {
		t.Fatalf("malformed frame must close with an error")
	}
	if len(host.emitted("down-stream")) != 0 {
		t.Fatalf("malformed frame must not be emitted")
	}
}

// TestStreamTruncatedEvent verifies an undispatched data buffer at EOF is an
// error.
func TestStreamTruncatedEvent(t *testing.T) {
	host := runChatStream(t, []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}"))
	if err := lastCloseError(host); err == "" {
		t.Fatalf("truncated event must close with an error")
	}
}

// TestStreamNativeErrorTerminates verifies a native error event closes the
// stream with a fixed, sanitized message.
func TestStreamNativeErrorTerminates(t *testing.T) {
	host := runChatStream(t, []byte("data: {\"error\":{\"message\":\"boom sk-fake\"}}\n\n"))
	errMsg := lastCloseError(host)
	if errMsg == "" {
		t.Fatalf("native error must close with an error")
	}
	if strings.Contains(errMsg, "boom") || strings.Contains(errMsg, "sk-fake") {
		t.Fatalf("host error text leaked: %q", errMsg)
	}
}
