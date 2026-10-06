//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Fixture semantics shared by every native protocol. The client must observe
// exactly these values regardless of which translation path is taken.
const (
	fixtureText      = "hello"
	fixtureToolName  = "get_time"
	fixtureToolArgs  = `{"tz":"UTC"}`
	fixtureReasoning = "checking clock"

	fixtureInputTokens  = 5
	fixtureOutputTokens = 3
	fixtureCachedTokens = 2
)

// markerFor embeds the mock control marker in a client prompt. Translation
// preserves text content, so the mock can select behavior from the raw body.
func markerFor(mode string) string {
	if mode == "" {
		mode = "full"
	}
	return "[[mock_mode=" + mode + "]]"
}

// --- Native non-streaming fixtures -----------------------------------------

// chatNonStreamFixture is an OpenAI chat.completion with reasoning, text, a tool
// call and usage.
func chatNonStreamFixture(model string) []byte {
	return mustMarshal(map[string]any{
		"id":      "chatcmpl-\u27131",
		"object":  "chat.completion",
		"created": 1791258569,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           fixtureText,
					"reasoning_content": fixtureReasoning,
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      fixtureToolName,
								"arguments": fixtureToolArgs,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     fixtureInputTokens,
			"completion_tokens": fixtureOutputTokens,
			"total_tokens":      fixtureInputTokens + fixtureOutputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": fixtureCachedTokens,
			},
		},
	})
}

// claudeNonStreamFixture is an Anthropic message with thinking, text and a
// tool_use block plus usage.
func claudeNonStreamFixture(model string) []byte {
	return mustMarshal(map[string]any{
		"id":    "msg_\u27131",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []any{
			map[string]any{"type": "thinking", "thinking": fixtureReasoning, "signature": "fake-signature"},
			map[string]any{"type": "text", "text": fixtureText},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": fixtureToolName, "input": map[string]any{"tz": "UTC"}},
		},
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage": map[string]any{
			// Anthropic input_tokens excludes cached tokens, so the uncached part
			// is total minus cache_read; the shared total input stays 5.
			"input_tokens":            fixtureInputTokens - fixtureCachedTokens,
			"output_tokens":           fixtureOutputTokens,
			"cache_read_input_tokens": fixtureCachedTokens,
		},
	})
}

// responsesNonStreamFixture is an OpenAI Responses object with a reasoning
// summary, an output message and a function_call plus usage.
func responsesNonStreamFixture(model string) []byte {
	return mustMarshal(map[string]any{
		"id":     "resp_\u27131",
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": []any{
			map[string]any{
				"id":      "rs_1",
				"type":    "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": fixtureReasoning}},
			},
			map[string]any{
				"id":      "msg_1",
				"type":    "message",
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": fixtureText, "annotations": []any{}}},
			},
			map[string]any{
				"id":        "fc_1",
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      fixtureToolName,
				"arguments": fixtureToolArgs,
				"status":    "completed",
			},
		},
		"usage": map[string]any{
			"input_tokens":  fixtureInputTokens,
			"output_tokens": fixtureOutputTokens,
			"total_tokens":  fixtureInputTokens + fixtureOutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": fixtureCachedTokens,
			},
		},
	})
}

// --- Native streaming fixtures ---------------------------------------------

// chatSSEFixture returns OpenAI chat.completion.chunk events terminated by the
// [DONE] sentinel. Lines use CRLF to exercise the plugin line framer.
func chatSSEFixture(model string) string {
	chunk := func(delta map[string]any, finish any, usage map[string]any) string {
		payload := map[string]any{
			"id":      "chatcmpl-\u27131",
			"object":  "chat.completion.chunk",
			"created": 1791258569,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			payload["usage"] = usage
		}
		return openAISSE(payload)
	}
	var b strings.Builder
	b.WriteString(chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
	b.WriteString(chunk(map[string]any{"reasoning_content": fixtureReasoning}, nil, nil))
	b.WriteString(chunk(map[string]any{"content": fixtureText}, nil, nil))
	b.WriteString(chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": fixtureToolName, "arguments": ""},
	}}}, nil, nil))
	b.WriteString(chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index":    0,
		"function": map[string]any{"arguments": fixtureToolArgs},
	}}}, nil, nil))
	b.WriteString(chunk(map[string]any{}, "tool_calls", map[string]any{
		"prompt_tokens":     fixtureInputTokens,
		"completion_tokens": fixtureOutputTokens,
		"total_tokens":      fixtureInputTokens + fixtureOutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": fixtureCachedTokens,
		},
	}))
	b.WriteString("data: [DONE]\r\n\r\n")
	return b.String()
}

// claudeSSEFixture returns the Anthropic Messages streaming event sequence.
func claudeSSEFixture(model string) string {
	var b strings.Builder
	b.WriteString(namedSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_\u27131", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": fixtureInputTokens - fixtureCachedTokens, "output_tokens": 0, "cache_read_input_tokens": fixtureCachedTokens},
		},
	}))
	b.WriteString(namedSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	}))
	b.WriteString(namedSSE("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": fixtureReasoning},
	}))
	b.WriteString(namedSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
	b.WriteString(namedSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": 1,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	b.WriteString(namedSSE("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "text_delta", "text": fixtureText},
	}))
	b.WriteString(namedSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}))
	b.WriteString(namedSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": 2,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": fixtureToolName, "input": map[string]any{}},
	}))
	b.WriteString(namedSSE("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": fixtureToolArgs},
	}))
	b.WriteString(namedSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2}))
	b.WriteString(namedSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": fixtureOutputTokens},
	}))
	b.WriteString(namedSSE("message_stop", map[string]any{"type": "message_stop"}))
	return b.String()
}

// responsesSSEFixture returns the OpenAI Responses streaming event sequence.
func responsesSSEFixture(model string) string {
	reasoningItem := map[string]any{
		"id": "rs_1", "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": fixtureReasoning}},
	}
	messageItem := map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": fixtureText, "annotations": []any{}}},
	}
	functionItem := map[string]any{
		"id": "fc_1", "type": "function_call", "call_id": "call_1",
		"name": fixtureToolName, "arguments": fixtureToolArgs, "status": "completed",
	}
	var b strings.Builder
	b.WriteString(namedSSE("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": "resp_\u27131", "object": "response", "status": "in_progress", "model": model, "output": []any{}},
	}))
	b.WriteString(namedSSE("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}},
	}))
	b.WriteString(namedSSE("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": "rs_1",
		"output_index": 0, "summary_index": 0, "delta": fixtureReasoning,
	}))
	b.WriteString(namedSSE("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0, "item": reasoningItem,
	}))
	b.WriteString(namedSSE("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	}))
	b.WriteString(namedSSE("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": "msg_1",
		"output_index": 1, "content_index": 0, "delta": fixtureText,
	}))
	b.WriteString(namedSSE("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 1, "item": messageItem,
	}))
	b.WriteString(namedSSE("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 2,
		"item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": fixtureToolName, "arguments": "", "status": "in_progress"},
	}))
	b.WriteString(namedSSE("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "fc_1",
		"output_index": 2, "delta": fixtureToolArgs,
	}))
	b.WriteString(namedSSE("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 2, "item": functionItem,
	}))
	b.WriteString(namedSSE("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_\u27131", "object": "response", "status": "completed", "model": model,
			"output": []any{reasoningItem, messageItem, functionItem},
			"usage": map[string]any{
				"input_tokens":  fixtureInputTokens,
				"output_tokens": fixtureOutputTokens,
				"total_tokens":  fixtureInputTokens + fixtureOutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": fixtureCachedTokens,
				},
			},
		},
	}))
	return b.String()
}

// truncateSSE cuts a fixture mid-stream so the upstream closes without a
// terminal event, exercising the no-fake-success guarantee.
func truncateSSE(model string) string {
	full := chatSSEFixture(model)
	cut := strings.Index(full, fixtureText)
	if cut < 0 {
		return full
	}
	// Keep the role chunk and the beginning of the text delta, then stop.
	end := cut + len(fixtureText)/2
	return full[:end]
}

// openAISSE renders one data-only SSE frame.
func openAISSE(payload map[string]any) string {
	return "data: " + mustMarshalString(payload) + "\r\n\r\n"
}

// namedSSE renders one event+data SSE frame.
func namedSSE(event string, payload map[string]any) string {
	return "event: " + event + "\r\ndata: " + mustMarshalString(payload) + "\r\n\r\n"
}

func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("fixture marshal: %v", err))
	}
	return data
}

func mustMarshalString(value any) string { return string(mustMarshal(value)) }
