package provider

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Native passthrough must reject malformed successful HTTP bodies too.
func TestNativeResponseRequiresJSONObject(t *testing.T) {
	for _, body := range []string{"", "not-json", "null", "[]", `"text"`} {
		t.Run(body, func(t *testing.T) {
			host := newFakeHost()
			host.httpResponses[DefaultBaseURL+"/chat/completions"] = pluginapi.HTTPResponse{
				StatusCode: http.StatusOK, Body: []byte(body),
			}
			raw, err := newTestManager(host).HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, baseExecRequest(), ""))
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeError(t, raw).StatusCode(); got != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got)
			}
		})
	}
}

// A Chat sentinel is not a Claude/Responses completion event.
func TestNativeSentinelCannotCompleteOtherProtocols(t *testing.T) {
	for _, wire := range []translator.Format{translator.FormatClaude, translator.FormatOpenAIResponse} {
		signal, malformed := classifyNativeEvent(wire, []byte("[DONE]"))
		if signal != signalNone || malformed {
			t.Fatalf("%s sentinel classified as completion: %v, malformed=%v", wire, signal, malformed)
		}
		host := newFakeHost()
		host.queueStream([]byte("data: [DONE]\n\n"))
		var terminalError string
		newTestManager(host).pumpRawPassthrough(wire, "up-1", func([]byte) bool { return true }, func(message string) {
			terminalError = message
		})
		if terminalError == "" {
			t.Fatalf("%s accepted a missing native terminal event", wire)
		}
	}
}

// Recover worker panics before closing the stream so the host survives and sees
// a generic failure rather than a successful EOF or leaked panic text.
func TestStreamWorkerPanicFailsClosed(t *testing.T) {
	host := newFakeHost()
	host.queueStream([]byte("data: [DONE]\n\n"))
	manager := NewManager(NewBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPStreamRead {
			panic("fake-secret-panic")
		}
		return host.call(method, payload)
	}))
	startStreamWithID(t, manager, "panic-stream")
	if !waitForClose(t, host, time.Second) {
		t.Fatal("panicking worker did not close its stream")
	}
	manager.shutdown()
	closes := host.closes()
	if len(closes) == 0 || closes[0].Error == "" || strings.Contains(closes[0].Error, "fake-secret") {
		t.Fatalf("panic must produce a sanitized failure: %+v", closes)
	}
}

// Retry hints must survive the RPC envelope, without a plugin retry loop.
func TestRetryableStatusSurvivesEnvelope(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		result := resultError(upstreamError(status))
		if result.Error.HTTPStatus != status || !result.Error.Retryable {
			t.Fatalf("retry hint lost for status %d: %+v", status, result.Error)
		}
	}
}
