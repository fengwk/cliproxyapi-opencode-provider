//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// observed is the protocol-independent client view of one response.
type observed struct {
	Text      string
	Reasoning string
	ToolName  string
	ToolArgs  string
	Input     int
	Output    int
	Cached    int
	HasUsage  bool

	// Anthropic reports input_tokens excluding cache reads/creation, so the
	// Claude usage parts are accumulated and normalized into the total Input.
	claudeInput         int
	claudeCacheRead     int
	claudeCacheCreation int
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	Name string
	Data string
}

// parseSSE parses strict SSE frames that the host must emit: payloads arrive
// only through "data:" lines. Unknown field lines and comments are ignored per
// the SSE specification, so a bare JSON line (a malformed frame) contributes no
// data and cannot be mistaken for a valid event.
func parseSSE(raw []byte) []sseEvent {
	var events []sseEvent
	var current sseEvent
	flush := func() {
		if strings.TrimSpace(current.Data) != "" {
			events = append(events, current)
		}
		current = sseEvent{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			current.Name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(line[len("data:"):])
			if current.Data != "" {
				current.Data += "\n"
			}
			current.Data += payload
		default:
			// ignore id:, retry:, comments and unknown fields
		}
	}
	flush()
	return events
}

// TestSSEParserRejectsBareJSON locks in the strict framing contract: a payload
// that is not carried by a "data:" line is not a valid event and must not be
// observed as one, while comments and unknown fields stay ignored per spec.
func TestSSEParserRejectsBareJSON(t *testing.T) {
	if events := parseSSE([]byte("{\"id\":\"x\"}\r\n\r\n")); len(events) != 0 {
		t.Fatalf("bare JSON was accepted as %d SSE event(s): %#v", len(events), events)
	}
	framed := []byte(": heartbeat\r\nretry: 1000\r\ndata: {\"id\":\"x\"}\r\n\r\n")
	events := parseSSE(framed)
	if len(events) != 1 {
		t.Fatalf("valid framed event parsed as %d event(s): %#v", len(events), events)
	}
	if jsonObject(events[0].Data) == nil {
		t.Fatalf("framed event data is not JSON: %q", events[0].Data)
	}
}

// jsonObject decodes one SSE data payload as a JSON object.
func jsonObject(data string) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil
	}
	return obj
}

// --- chat (OpenAI) ---------------------------------------------------------

func observeChatNonStream(t *testing.T, raw []byte) observed {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("chat non-stream response is not JSON: %v\n%s", err, truncate(raw, 600))
	}
	obs := observed{}
	choice := firstObject(body, "choices")
	message, _ := choice["message"].(map[string]any)
	obs.Text, _ = message["content"].(string)
	obs.Reasoning = stringValue(message["reasoning_content"])
	if obs.Reasoning == "" {
		obs.Reasoning = stringValue(message["reasoning"])
	}
	tool := firstObject(message, "tool_calls")
	if fn, ok := tool["function"].(map[string]any); ok {
		obs.ToolName, _ = fn["name"].(string)
		obs.ToolArgs = normalizeArgs(fn["arguments"])
	}
	applyOpenAIUsage(&obs, body["usage"])
	return obs
}

func observeChatStream(t *testing.T, raw []byte) observed {
	t.Helper()
	obs := observed{}
	var text, reasoning strings.Builder
	argsByIndex := map[int]string{}
	nameByIndex := map[int]string{}
	for _, event := range parseSSE(raw) {
		if strings.TrimSpace(event.Data) == "[DONE]" {
			continue
		}
		obj := jsonObject(event.Data)
		if obj == nil {
			t.Errorf("chat stream frame is not valid JSON data %q", truncate([]byte(event.Data), 300))
			continue
		}
		if usage, ok := obj["usage"]; ok && usage != nil {
			applyOpenAIUsage(&obs, usage)
		}
		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		text.WriteString(stringValue(delta["content"]))
		if reasoningValue := stringValue(delta["reasoning_content"]); reasoningValue != "" {
			reasoning.WriteString(reasoningValue)
		} else {
			reasoning.WriteString(stringValue(delta["reasoning"]))
		}
		for _, item := range objectSlice(delta, "tool_calls") {
			index := intValue(item["index"])
			if fn, ok := item["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					nameByIndex[index] = name
				}
				argsByIndex[index] += stringValue(fn["arguments"])
			}
		}
	}
	obs.Text = text.String()
	obs.Reasoning = reasoning.String()
	obs.ToolName = firstNonEmpty(nameByIndex)
	obs.ToolArgs = normalizeArgs(firstNonEmpty(argsByIndex))
	return obs
}

// --- messages (Anthropic) --------------------------------------------------

func observeMessagesNonStream(t *testing.T, raw []byte) observed {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("messages non-stream response is not JSON: %v\n%s", err, truncate(raw, 600))
	}
	obs := observed{}
	for _, block := range objectSlice(body, "content") {
		switch stringValue(block["type"]) {
		case "text":
			obs.Text += stringValue(block["text"])
		case "thinking", "reasoning":
			obs.Reasoning += stringValue(block["thinking"]) + stringValue(block["reasoning"])
		case "tool_use":
			obs.ToolName, _ = block["name"].(string)
			obs.ToolArgs = normalizeArgs(block["input"])
		}
	}
	applyClaudeUsage(&obs, body["usage"])
	return obs
}

func observeMessagesStream(t *testing.T, raw []byte) observed {
	t.Helper()
	obs := observed{}
	toolArgsByIndex := map[int]string{}
	for _, event := range parseSSE(raw) {
		obj := jsonObject(event.Data)
		if obj == nil {
			if strings.TrimSpace(event.Data) != "" {
				t.Errorf("messages stream frame is not valid JSON data %q", truncate([]byte(event.Data), 300))
			}
			continue
		}
		switch stringValue(obj["type"]) {
		case "message_start":
			if message, ok := obj["message"].(map[string]any); ok {
				applyClaudeUsage(&obs, message["usage"])
			}
		case "content_block_start":
			if block, ok := obj["content_block"].(map[string]any); ok && stringValue(block["type"]) == "tool_use" {
				obs.ToolName, _ = block["name"].(string)
			}
		case "content_block_delta":
			delta, _ := obj["delta"].(map[string]any)
			switch stringValue(delta["type"]) {
			case "text_delta":
				obs.Text += stringValue(delta["text"])
			case "thinking_delta":
				obs.Reasoning += stringValue(delta["thinking"])
			case "input_json_delta":
				index := intValue(obj["index"])
				toolArgsByIndex[index] += stringValue(delta["partial_json"])
			}
		case "message_delta":
			applyClaudeUsage(&obs, obj["usage"])
		}
	}
	if obs.ToolArgs == "" {
		obs.ToolArgs = normalizeArgs(firstNonEmpty(toolArgsByIndex))
	}
	return obs
}

// --- responses (OpenAI Responses) ------------------------------------------

func observeResponsesNonStream(t *testing.T, raw []byte) observed {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("responses non-stream response is not JSON: %v\n%s", err, truncate(raw, 600))
	}
	obs := observed{}
	for _, item := range objectSlice(body, "output") {
		switch stringValue(item["type"]) {
		case "reasoning":
			for _, summary := range objectSlice(item, "summary") {
				obs.Reasoning += stringValue(summary["text"])
			}
		case "message":
			for _, content := range objectSlice(item, "content") {
				obs.Text += stringValue(content["text"])
			}
		case "function_call":
			obs.ToolName, _ = item["name"].(string)
			obs.ToolArgs = normalizeArgs(item["arguments"])
		}
	}
	applyResponsesUsage(&obs, body["usage"])
	return obs
}

func observeResponsesStream(t *testing.T, raw []byte) observed {
	t.Helper()
	obs := observed{}
	for _, event := range parseSSE(raw) {
		obj := jsonObject(event.Data)
		if obj == nil {
			if strings.TrimSpace(event.Data) != "" {
				t.Errorf("responses stream frame is not valid JSON data %q", truncate([]byte(event.Data), 300))
			}
			continue
		}
		switch stringValue(obj["type"]) {
		case "response.output_text.delta":
			obs.Text += stringValue(obj["delta"])
		case "response.reasoning_summary_text.delta":
			obs.Reasoning += stringValue(obj["delta"])
		case "response.function_call_arguments.delta":
			obs.ToolArgs += stringValue(obj["delta"])
		case "response.output_item.added", "response.output_item.done":
			if item, ok := obj["item"].(map[string]any); ok && stringValue(item["type"]) == "function_call" {
				if name, _ := item["name"].(string); name != "" {
					obs.ToolName = name
				}
			}
		case "response.completed":
			if response, ok := obj["response"].(map[string]any); ok {
				applyResponsesUsage(&obs, response["usage"])
			}
		}
	}
	obs.ToolArgs = normalizeArgs(obs.ToolArgs)
	return obs
}

// --- usage helpers ---------------------------------------------------------

func applyOpenAIUsage(obs *observed, usage any) {
	obj, ok := usage.(map[string]any)
	if !ok {
		return
	}
	obs.HasUsage = true
	obs.Input = intValue(obj["prompt_tokens"])
	obs.Output = intValue(obj["completion_tokens"])
	if details, ok := obj["prompt_tokens_details"].(map[string]any); ok {
		obs.Cached = intValue(details["cached_tokens"])
	}
}

func applyClaudeUsage(obs *observed, usage any) {
	obj, ok := usage.(map[string]any)
	if !ok {
		return
	}
	// Each field is applied only when the event actually carries it, so a later
	// message_delta that reports only output_tokens does not erase the cache
	// fields announced by message_start.
	if _, present := obj["input_tokens"]; present {
		obs.claudeInput = intValue(obj["input_tokens"])
	}
	if _, present := obj["cache_read_input_tokens"]; present {
		obs.claudeCacheRead = intValue(obj["cache_read_input_tokens"])
	}
	if _, present := obj["cache_creation_input_tokens"]; present {
		obs.claudeCacheCreation = intValue(obj["cache_creation_input_tokens"])
	}
	if _, present := obj["output_tokens"]; present {
		obs.Output = intValue(obj["output_tokens"])
	}
	// Anthropic input_tokens excludes cached tokens; expose the protocol-neutral
	// total input and the cache-read portion.
	obs.Input = obs.claudeInput + obs.claudeCacheRead + obs.claudeCacheCreation
	obs.Cached = obs.claudeCacheRead
	obs.HasUsage = true
}

func applyResponsesUsage(obs *observed, usage any) {
	obj, ok := usage.(map[string]any)
	if !ok {
		return
	}
	obs.HasUsage = true
	if _, present := obj["input_tokens"]; present {
		obs.Input = intValue(obj["input_tokens"])
	}
	obs.Output = intValue(obj["output_tokens"])
	if details, ok := obj["input_tokens_details"].(map[string]any); ok {
		obs.Cached = intValue(details["cached_tokens"])
	}
}

// --- generic JSON helpers --------------------------------------------------

func firstObject(parent map[string]any, key string) map[string]any {
	items, _ := parent[key].([]any)
	if len(items) == 0 {
		return map[string]any{}
	}
	obj, _ := items[0].(map[string]any)
	if obj == nil {
		return map[string]any{}
	}
	return obj
}

func objectSlice(parent map[string]any, key string) []map[string]any {
	items, _ := parent[key].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	}
	return 0
}

func firstNonEmpty(values map[int]string) string {
	for index := 0; ; index++ {
		if value, ok := values[index]; ok {
			return value
		}
		if index > 64 {
			return ""
		}
	}
}

// normalizeArgs canonicalizes tool arguments, which may arrive as a JSON string
// or an already-decoded object.
func normalizeArgs(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return ""
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return trimmed
		}
		return mustMarshalString(decoded)
	default:
		return mustMarshalString(typed)
	}
}

// assertObserved enforces the shared response contract for every matrix cell.
func assertObserved(t *testing.T, obs observed) {
	t.Helper()
	if strings.TrimSpace(obs.Text) != fixtureText {
		t.Errorf("client text = %q, want %q", obs.Text, fixtureText)
	}
	if obs.ToolName != fixtureToolName {
		t.Errorf("client tool name = %q, want %q", obs.ToolName, fixtureToolName)
	}
	wantArgs := mustMarshalString(map[string]any{"tz": "UTC"})
	if obs.ToolArgs != wantArgs {
		t.Errorf("client tool args = %q, want %q", obs.ToolArgs, wantArgs)
	}
	if !strings.Contains(obs.Reasoning, fixtureReasoning) {
		t.Errorf("client reasoning = %q, want to contain %q", obs.Reasoning, fixtureReasoning)
	}
	if !obs.HasUsage {
		t.Errorf("client response carried no usage")
	} else {
		if obs.Input != fixtureInputTokens {
			t.Errorf("usage input = %d, want %d", obs.Input, fixtureInputTokens)
		}
		if obs.Output != fixtureOutputTokens {
			t.Errorf("usage output = %d, want %d", obs.Output, fixtureOutputTokens)
		}
		if obs.Cached != fixtureCachedTokens {
			t.Errorf("usage cached = %d, want %d", obs.Cached, fixtureCachedTokens)
		}
	}
}

func describe(obs observed) string {
	return fmt.Sprintf("text=%q reasoning=%q tool=%s(%s) usage(in=%d,out=%d,cached=%d,has=%v)",
		obs.Text, obs.Reasoning, obs.ToolName, obs.ToolArgs, obs.Input, obs.Output, obs.Cached, obs.HasUsage)
}
