package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const responsesURL = "https://opencode.ai/zen/go/v1/responses"

// nativeResponsesJSON is a terminal ordinary Responses body carrying reasoning,
// text, a tool call and usage.
const nativeResponsesJSON = `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.6","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`

func executeNonStream(t *testing.T, host *fakeHost, req pluginapi.ExecutorRequest) pluginapi.ExecutorResponse {
	t.Helper()
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp pluginapi.ExecutorResponse
	decodeResult(t, raw, &resp)
	return resp
}

// TestOpenAIToResponsesRequestAndReply verifies the OpenAI chat request crosses
// the SDK codex conversion into an ordinary Responses body for /v1/responses and
// the terminal Responses reply converts back with text, reasoning, tools, usage,
// and preserved generation controls.
func TestOpenAIToResponsesRequestAndReply(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[responsesURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(nativeResponsesJSON)}
	req := baseExecRequest()
	req.Model = "opencode-go/gpt-5.6"
	req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"top_p":0.9,"max_tokens":123,"store":true,"metadata":{"trace":"t1"},"parallel_tool_calls":false}`)
	req.OriginalRequest = req.Payload
	resp := executeNonStream(t, host, req)

	requests := host.requests()
	if len(requests) != 1 || requests[0].URL != responsesURL {
		t.Fatalf("expected a /responses call, got %+v", requests)
	}
	if requests[0].Headers.Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("responses endpoint must use bearer auth")
	}
	var sent map[string]any
	if err := json.Unmarshal(requests[0].Body, &sent); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}
	if sent["model"] != "gpt-5.6" {
		t.Fatalf("upstream model = %v, want stripped native id", sent["model"])
	}
	if stream, _ := sent["stream"].(bool); stream {
		t.Fatalf("non-stream request must not force stream=true")
	}
	if sent["max_output_tokens"] != float64(123) {
		t.Fatalf("max_tokens must map to max_output_tokens, got %v", sent["max_output_tokens"])
	}
	if sent["temperature"] != 0.3 || sent["top_p"] != 0.9 {
		t.Fatalf("generation controls not preserved: %+v", sent)
	}
	if sent["store"] != true {
		t.Fatalf("store not preserved: %+v", sent)
	}
	if metadata, _ := sent["metadata"].(map[string]any); metadata["trace"] != "t1" {
		t.Fatalf("metadata not preserved: %+v", sent)
	}
	if sent["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls not preserved: %+v", sent)
	}

	payload := string(resp.Payload)
	for _, want := range []string{`"content":"hello"`, `"reasoning_content":"think"`, `"tool_calls"`, `"total_tokens":8`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("converted reply missing %s: %s", want, payload)
		}
	}
}

// TestClaudeToResponsesRequestAndReply verifies the Claude request crosses into
// /responses with the Claude max_tokens mapped and the reply converts back.
func TestClaudeToResponsesRequestAndReply(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[responsesURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(nativeResponsesJSON)}
	req := baseExecRequest()
	req.Model = "opencode-go/gpt-5.6"
	req.Format = "claude"
	req.SourceFormat = "claude"
	req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	req.OriginalRequest = req.Payload
	resp := executeNonStream(t, host, req)

	var sent map[string]any
	if err := json.Unmarshal(host.requests()[0].Body, &sent); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}
	if sent["model"] != "gpt-5.6" {
		t.Fatalf("upstream model = %v", sent["model"])
	}
	if sent["max_output_tokens"] != float64(64) {
		t.Fatalf("claude max_tokens must map to max_output_tokens, got %v", sent["max_output_tokens"])
	}
	payload := string(resp.Payload)
	if !strings.Contains(payload, `"type":"message"`) || !strings.Contains(payload, "hello") {
		t.Fatalf("claude reply not produced: %s", payload)
	}
}

// TestResponsesToResponsesNativePassthrough verifies a Responses client against a
// Responses upstream stays native with only the model stripped.
func TestResponsesToResponsesNativePassthrough(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[responsesURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(nativeResponsesJSON)}
	req := baseExecRequest()
	req.Model = "opencode-go/gpt-5.6"
	req.Format = "openai-response"
	req.SourceFormat = "openai-response"
	req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","input":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	req.OriginalRequest = req.Payload
	resp := executeNonStream(t, host, req)

	var sent map[string]any
	if err := json.Unmarshal(host.requests()[0].Body, &sent); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}
	if sent["model"] != "gpt-5.6" {
		t.Fatalf("upstream model = %v", sent["model"])
	}
	if _, ok := sent["input"]; !ok {
		t.Fatalf("native Responses body must be preserved: %+v", sent)
	}
	if string(resp.Payload) != nativeResponsesJSON {
		t.Fatalf("native reply must pass through byte-identical")
	}
}

// TestResponsesStreamToOpenAIAndClaude verifies a Responses upstream SSE stream
// translates to both chat and Claude clients.
func TestResponsesStreamToOpenAIAndClaude(t *testing.T) {
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"created_at\":1700000000,\"model\":\"gpt-5.6\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"think\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1700000000,\"model\":\"gpt-5.6\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"

	for _, client := range []string{"openai", "claude"} {
		t.Run(client, func(t *testing.T) {
			host := newFakeHost()
			host.queueStream(splitBytes([]byte(sse), 7)...)
			manager := configuredManager(t, host, "")
			req := baseExecRequest()
			req.Model = "opencode-go/gpt-5.6"
			req.Format = client
			req.SourceFormat = client
			req.Stream = true
			req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","stream":true,"messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
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
			if !strings.Contains(emitted, "Hel") || !strings.Contains(emitted, "lo") {
				t.Fatalf("translated text missing: %q", emitted)
			}
			if !strings.Contains(emitted, "think") {
				t.Fatalf("translated reasoning missing: %q", emitted)
			}
			var sent map[string]any
			if err := json.Unmarshal(host.requests()[0].Body, &sent); err != nil {
				t.Fatalf("upstream body is not json: %v", err)
			}
			if stream, _ := sent["stream"].(bool); !stream {
				t.Fatalf("stream request must set stream=true")
			}
			if sent["temperature"] != 0.2 {
				t.Fatalf("stream generation control not preserved: %+v", sent)
			}
		})
	}
}

// TestClaudeToResponsesRequestDoesNotForceStream pins the SDK's Claude->Codex
// translation to the actual execution mode. The SDK translator forces
// stream=true; buildRequestBody must override it for a non-streaming call so the
// upstream cannot answer HTTPDo with SSE, while the converted body keeps the
// native Responses fields.
func TestClaudeToResponsesRequestDoesNotForceStream(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[responsesURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(nativeResponsesJSON)}
	req := baseExecRequest()
	req.Model = "opencode-go/gpt-5.6"
	req.Format = "claude"
	req.SourceFormat = "claude"
	req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	req.OriginalRequest = req.Payload
	_ = executeNonStream(t, host, req)

	requests := host.requests()
	if len(requests) != 1 {
		t.Fatalf("expected one upstream request, got %d", len(requests))
	}
	var sent map[string]any
	if err := json.Unmarshal(requests[0].Body, &sent); err != nil {
		t.Fatalf("upstream body is not json: %v", err)
	}
	if stream, _ := sent["stream"].(bool); stream {
		t.Fatalf("non-streaming Claude->Responses must not force stream=true: %+v", sent)
	}
	if sent["model"] != "gpt-5.6" {
		t.Fatalf("upstream model = %v, want stripped native id", sent["model"])
	}
	if _, ok := sent["input"]; !ok {
		t.Fatalf("converted body must use native Responses input: %+v", sent)
	}
	if _, ok := sent["messages"]; ok {
		t.Fatalf("converted body must not leak chat messages: %+v", sent)
	}
}

// TestTranslationSupportedChecksResponseDirection pins the response capability
// check to the client response format. A request-side match alone must not
// authorize a response direction that has no translator.
func TestTranslationSupportedChecksResponseDirection(t *testing.T) {
	if !translationSupported(translator.FormatOpenAI, translator.FormatOpenAI, translator.FormatCodex, translator.FormatOpenAIResponse, false) {
		t.Fatalf("chat->responses translation must be supported")
	}
	if translationSupported(translator.FormatOpenAI, translator.Format("bogus-response"), translator.FormatCodex, translator.FormatOpenAIResponse, false) {
		t.Fatalf("the response direction must be validated with the client response format")
	}
}

// TestExecuteRejectsEmptyNonStreamTranslation verifies an upstream 200 whose
// body cannot be converted into the client protocol fails closed instead of
// being returned as a silent, empty success.
func TestExecuteRejectsEmptyNonStreamTranslation(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[responsesURL] = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte("not-json")}
	manager := configuredManager(t, host, "")
	req := baseExecRequest()
	req.Model = "opencode-go/gpt-5.6"
	req.Payload = []byte(`{"model":"opencode-go/gpt-5.6","messages":[{"role":"user","content":"hi"}]}`)
	req.OriginalRequest = req.Payload
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, req, ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.StatusCode() != http.StatusBadGateway {
		t.Fatalf("empty conversion must fail closed, got %+v", envErr)
	}
}
