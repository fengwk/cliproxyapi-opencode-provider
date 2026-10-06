package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// requestFormat is the client protocol of the inbound request body.
func requestFormat(req rpcExecutorRequest) translator.Format {
	if strings.TrimSpace(req.SourceFormat) != "" {
		return translator.FromString(req.SourceFormat)
	}
	if strings.TrimSpace(req.Format) != "" {
		return translator.FromString(req.Format)
	}
	return translator.FormatOpenAI
}

// responseFormat is the protocol the executor must emit to the client.
func responseFormat(req rpcExecutorRequest) translator.Format {
	if strings.TrimSpace(req.Format) != "" {
		return translator.FromString(req.Format)
	}
	if strings.TrimSpace(req.SourceFormat) != "" {
		return translator.FromString(req.SourceFormat)
	}
	return translator.FormatOpenAI
}

// sdkFormatFor maps the upstream wire protocol onto the SDK conversion format.
// The OpenAI Responses wire protocol is converted through the SDK "codex"
// format, whose request and response transformers are registered for the
// ordinary Responses API on both sides.
func sdkFormatFor(wire translator.Format) translator.Format {
	if wire == translator.FormatOpenAIResponse {
		return translator.FormatCodex
	}
	return wire
}

// translationSupported reports whether the SDK can bridge client<->upstream.
// The request direction is validated against the client request format and the
// response direction against the client response format, so a mismatch between
// them cannot authorize a response conversion that has no translator. A native
// match on either side needs no translator.
func translationSupported(clientReq, clientResp, sdk, wire translator.Format, stream bool) bool {
	if clientReq != wire && !translator.HasRequestTransformer(clientReq, sdk) {
		return false
	}
	if clientResp == wire {
		return true
	}
	if stream {
		return translator.HasStreamResponseTransformer(clientResp, sdk)
	}
	return translator.HasNonStreamResponseTransformer(clientResp, sdk)
}

// prepared holds the resolved per-request execution plan.
type prepared struct {
	cfg         Config
	nativeModel string
	wire        translator.Format // upstream wire protocol (endpoint + headers)
	sdk         translator.Format // SDK conversion format (codex for Responses)
	endpoint    string
	clientReq   translator.Format
	clientResp  translator.Format
	key         string
	scope       string
	translated  []byte
	original    []byte
}

// prepare validates and resolves everything needed before any upstream call.
func (m *Manager) prepare(req rpcExecutorRequest, stream bool) (*prepared, error) {
	if m.stopped() {
		return nil, &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
	}
	cfg := m.config()
	native := nativeModelID(req.Model)
	wire, ok := routeNativeModel(cfg, native)
	if !ok {
		return nil, &ProviderError{
			Code:       "unsupported_model",
			Message:    "model is not routable by opencode-go",
			HTTPStatus: http.StatusNotFound,
		}
	}
	sdk := sdkFormatFor(wire)
	endpoint, ok := protocolEndpoint(wire)
	if !ok {
		return nil, &ProviderError{Code: "unsupported_protocol", Message: "unsupported upstream protocol", HTTPStatus: http.StatusBadRequest}
	}
	clientReq := requestFormat(req)
	clientResp := responseFormat(req)
	if !translationSupported(clientReq, clientResp, sdk, wire, stream) {
		return nil, &ProviderError{
			Code:       "unsupported_protocol",
			Message:    "no protocol translation is available between the client and upstream formats",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if authDisabled(req.AuthMetadata) {
		return nil, &ProviderError{Code: "auth_disabled", Message: "selected auth is disabled", HTTPStatus: http.StatusForbidden}
	}
	key, errKey := resolveKey(req.AuthAttributes, req.AuthMetadata, req.StorageJSON)
	if errKey != nil {
		return nil, &ProviderError{Code: "auth_unavailable", Message: errKey.Error(), HTTPStatus: http.StatusUnauthorized}
	}
	canonical := canonicalSessionID(req.Metadata)
	scope, errScope := sessionScope(req.AuthID, canonical)
	if errScope != nil {
		return nil, &ProviderError{Code: "session_missing", Message: errScope.Error(), HTTPStatus: http.StatusBadRequest}
	}
	original := req.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	translated, errBody := buildRequestBody(req, clientReq, wire, sdk, native, stream)
	if errBody != nil {
		return nil, errBody
	}
	translated = injectPromptCacheKey(translated, sdk, scope)
	if !json.Valid(translated) {
		return nil, &ProviderError{Code: "invalid_request", Message: "request body is not valid json", HTTPStatus: http.StatusBadRequest}
	}
	return &prepared{
		cfg:         cfg,
		nativeModel: native,
		wire:        wire,
		sdk:         sdk,
		endpoint:    endpoint,
		clientReq:   clientReq,
		clientResp:  clientResp,
		key:         key,
		scope:       scope,
		translated:  translated,
		original:    original,
	}, nil
}

// buildRequestBody converts the client request into the upstream protocol. The
// SDK translator is always invoked with the stripped native model so the
// upstream never sees the public "opencode-go/" prefix; when no transformer is
// registered the SDK fallback still rewrites the model field. A native request
// (client wire protocol equals the upstream wire protocol) targets the wire
// format directly so a Responses-to-Responses request stays byte-native.
func buildRequestBody(req rpcExecutorRequest, client, wire, sdk translator.Format, native string, stream bool) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if len(body) == 0 || !json.Valid(body) {
		return nil, &ProviderError{Code: "invalid_request", Message: "request body is not valid json", HTTPStatus: http.StatusBadRequest}
	}
	target := sdk
	if client == wire {
		target = wire
	}
	out := translator.TranslateRequest(client, target, native, body, stream)
	if out == nil {
		out = body
	}
	if client != wire && sdk == translator.FormatCodex {
		out = applyResponsesControls(out, body)
		out = enforceStream(out, stream)
	}
	if json.Valid(out) && gjson.GetBytes(out, "model").Exists() {
		if updated, err := sjson.SetBytes(out, "model", native); err == nil {
			out = updated
		}
	}
	return out, nil
}

// responsesControlCopy is the raw field mapping that restores the ordinary
// Responses generation controls deliberately omitted by the SDK's Codex request
// transformers. Source and destination names are identical.
var responsesControlCopy = []string{
	"temperature",
	"top_p",
	"store",
	"metadata",
	"parallel_tool_calls",
	"max_output_tokens",
}

// applyResponsesControls performs a small raw field copy from the client request
// into the converted ordinary Responses body. It never rewrites the protocol.
func applyResponsesControls(out, client []byte) []byte {
	if len(out) == 0 || len(client) == 0 {
		return out
	}
	for _, field := range responsesControlCopy {
		value := gjson.GetBytes(client, field)
		if !value.Exists() {
			continue
		}
		if updated, err := sjson.SetRawBytes(out, field, []byte(value.Raw)); err == nil {
			out = updated
		}
	}
	if !gjson.GetBytes(out, "max_output_tokens").Exists() {
		for _, field := range []string{"max_tokens", "max_completion_tokens"} {
			value := gjson.GetBytes(client, field)
			if !value.Exists() {
				continue
			}
			if updated, err := sjson.SetRawBytes(out, "max_output_tokens", []byte(value.Raw)); err == nil {
				out = updated
			}
			break
		}
	}
	return out
}

// enforceStream pins the upstream stream flag to the actual execution mode. The
// SDK's Codex request translators for some sources force stream=true; a
// non-streaming execution must never ask the upstream for SSE.
func enforceStream(body []byte, stream bool) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return body
	}
	updated, err := sjson.SetBytes(body, "stream", stream)
	if err != nil {
		return body
	}
	return updated
}

// injectPromptCacheKey sets the cache key to the session scope for Chat and
// Responses payloads without a float round-trip. Invalid bodies are untouched.
func injectPromptCacheKey(body []byte, sdk translator.Format, scope string) []byte {
	if len(body) == 0 || scope == "" || !json.Valid(body) {
		return body
	}
	switch sdk {
	case translator.FormatOpenAI, translator.FormatCodex, translator.FormatOpenAIResponse:
	default:
		return body
	}
	updated, err := sjson.SetBytes(body, "prompt_cache_key", scope)
	if err != nil {
		return body
	}
	return updated
}

// wrapCodexTerminal wraps an ordinary Responses JSON body into the minimal
// terminal event envelope expected by the SDK's Codex non-stream converters.
func wrapCodexTerminal(native []byte) []byte {
	if !json.Valid(native) {
		return native
	}
	eventType := "response.completed"
	if gjson.GetBytes(native, "status").String() == "incomplete" {
		eventType = "response.incomplete"
	}
	envelope, err := sjson.SetBytes([]byte(`{"type":"response.completed","response":{}}`), "type", eventType)
	if err != nil {
		return native
	}
	envelope, err = sjson.SetRawBytes(envelope, "response", native)
	if err != nil {
		return native
	}
	return envelope
}

// outboundHeaders builds a fresh upstream header set; frontend headers are never
// forwarded.
func outboundHeaders(wire translator.Format, key, scope string, stream bool) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	if wire == translator.FormatClaude {
		headers.Set("x-api-key", key)
		headers.Set("anthropic-version", "2023-06-01")
	} else {
		headers.Set("Authorization", "Bearer "+key)
	}
	headers.Set("x-opencode-session", scope)
	headers.Set("User-Agent", PluginID+"/"+Version)
	return headers
}

// execute handles a non-streaming execution request.
func (m *Manager) execute(req rpcExecutorRequest) (pluginapi.ExecutorResponse, error) {
	plan, err := m.prepare(req, false)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	resp, errDo := m.bridge.HTTPDo(http.MethodPost, joinURL(plan.cfg.BaseURL, plan.endpoint),
		outboundHeaders(plan.wire, plan.key, plan.scope, false), plan.translated, req.HostCallbackID)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, upstreamError(resp.StatusCode)
	}
	payload := resp.Body
	if plan.clientResp != plan.wire {
		converted := resp.Body
		if plan.sdk == translator.FormatCodex {
			converted = wrapCodexTerminal(resp.Body)
		}
		var param any
		payload = translator.TranslateNonStream(context.Background(), plan.sdk, plan.clientResp,
			req.Model, plan.original, plan.translated, converted, &param)
		if len(payload) == 0 || !json.Valid(payload) {
			return pluginapi.ExecutorResponse{}, &ProviderError{
				Code:       "upstream_error",
				Message:    "upstream response could not be translated",
				HTTPStatus: http.StatusBadGateway,
			}
		}
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: sanitizeResponseHeaders(resp.Headers)}, nil
}

// executeStream opens the upstream stream and starts the downstream pump. Any
// pre-output failure is returned synchronously so the host can fail over.
func (m *Manager) executeStream(req rpcExecutorRequest) error {
	if strings.TrimSpace(req.StreamID) == "" {
		return &ProviderError{Code: "invalid_request", Message: "downstream stream id is required", HTTPStatus: http.StatusBadRequest}
	}
	plan, err := m.prepare(req, true)
	if err != nil {
		return err
	}
	start, errOpen := m.bridge.HTTPDoStream(http.MethodPost, joinURL(plan.cfg.BaseURL, plan.endpoint),
		outboundHeaders(plan.wire, plan.key, plan.scope, true), plan.translated, req.HostCallbackID)
	if errOpen != nil {
		return errOpen
	}
	if start.StatusCode < 200 || start.StatusCode >= 300 {
		m.bridge.HTTPStreamClose(start.StreamID)
		return upstreamError(start.StatusCode)
	}
	if strings.TrimSpace(start.StreamID) == "" {
		return &ProviderError{Code: "upstream_error", Message: "upstream stream bridge unavailable", HTTPStatus: http.StatusBadGateway}
	}
	// Register the downstream/upstream pair before the worker starts so shutdown
	// can always unblock and wait for it.
	if !m.registerStream(req.StreamID, start.StreamID) {
		m.bridge.HTTPStreamClose(start.StreamID)
		return &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
	}
	go m.pumpStream(req, plan, start.StreamID)
	return nil
}

// pumpStream forwards the upstream stream to the downstream stream, translating
// frames when the formats differ. It always closes both streams and unregisters
// itself so shutdown can wait for completion.
func (m *Manager) pumpStream(req rpcExecutorRequest, plan *prepared, upstreamStreamID string) {
	defer m.finishStream(req.StreamID)
	defer m.bridge.HTTPStreamClose(upstreamStreamID)

	closed := false
	closeDown := func(errMessage string) {
		if closed {
			return
		}
		closed = true
		m.bridge.StreamClose(req.StreamID, errMessage)
	}
	defer closeDown("")

	emit := func(payload []byte) bool {
		if len(payload) == 0 {
			return true
		}
		if m.stopped() {
			return false
		}
		return m.bridge.StreamEmit(req.StreamID, payload) == nil
	}

	if plan.clientResp == plan.wire {
		if plan.clientResp == translator.FormatOpenAI {
			m.pumpChatPassthrough(plan.wire, upstreamStreamID, emit, closeDown)
		} else {
			m.pumpRawPassthrough(plan.wire, upstreamStreamID, emit, closeDown)
		}
		return
	}
	m.pumpTranslated(req, plan, upstreamStreamID, emit, closeDown)
}

// pumpChatPassthrough frames Chat SSE events, strips the sentinel the host
// handler re-appends, and emits one bare JSON object per event.
func (m *Manager) pumpChatPassthrough(wire translator.Format, upstreamStreamID string, emit func([]byte) bool, closeDown func(string)) {
	completed := false
	var framer sseEventFramer
	for {
		chunk, errRead := m.bridge.HTTPStreamRead(upstreamStreamID)
		if errRead != nil {
			closeDown("upstream stream read failed")
			return
		}
		if chunk.Error != "" {
			closeDown("upstream stream read failed")
			return
		}
		if len(chunk.Payload) > 0 {
			sawError := false
			malformed := false
			pushErr := framer.push(chunk.Payload, func(event sseEvent) bool {
				data := event.Data
				if len(data) == 0 {
					return true
				}
				signal, isMalformed := classifyNativeEvent(wire, data)
				switch signal {
				case signalError:
					if isMalformed {
						malformed = true
					} else {
						sawError = true
					}
					return false
				case signalDone:
					completed = true
					return true
				case signalTerminal:
					completed = true
				}
				return emit(data)
			})
			if malformed {
				closeDown("malformed upstream stream")
				return
			}
			if sawError {
				closeDown("upstream stream error")
				return
			}
			if pushErr != nil {
				if errors.Is(pushErr, errEmitStopped) {
					return
				}
				closeDown("malformed upstream stream")
				return
			}
		}
		if chunk.Done {
			break
		}
	}
	if errFinish := framer.finish(); errFinish != nil {
		closeDown("truncated upstream stream")
		return
	}
	if !completed {
		closeDown("upstream stream ended before completion")
	}
}

// pumpRawPassthrough forwards complete native Claude/Responses SSE events while
// parsing them to detect the native terminal signal and native errors. It emits
// exactly one whole event (including its blank-line terminator) per emit, so a
// read split inside a UTF-8 sequence or JSON payload can never leak a partial
// frame downstream.
func (m *Manager) pumpRawPassthrough(wire translator.Format, upstreamStreamID string, emit func([]byte) bool, closeDown func(string)) {
	completed := false
	var framer sseEventFramer
	for {
		chunk, errRead := m.bridge.HTTPStreamRead(upstreamStreamID)
		if errRead != nil {
			closeDown("upstream stream read failed")
			return
		}
		if chunk.Error != "" {
			closeDown("upstream stream read failed")
			return
		}
		if len(chunk.Payload) > 0 {
			sawError := false
			malformed := false
			pushErr := framer.push(chunk.Payload, func(event sseEvent) bool {
				signal, isMalformed := classifyNativeEvent(wire, event.Data)
				switch signal {
				case signalError:
					if isMalformed {
						malformed = true
					} else {
						sawError = true
					}
					return false
				case signalDone, signalTerminal:
					completed = true
				}
				return emit(event.Raw)
			})
			if malformed {
				closeDown("malformed upstream stream")
				return
			}
			if sawError {
				closeDown("upstream stream error")
				return
			}
			if pushErr != nil {
				if errors.Is(pushErr, errEmitStopped) {
					return
				}
				closeDown("malformed upstream stream")
				return
			}
		}
		if chunk.Done {
			break
		}
	}
	if errFinish := framer.finish(); errFinish != nil {
		closeDown("truncated upstream stream")
		return
	}
	if !completed {
		closeDown("upstream stream ended before completion")
	}
}

// pumpTranslated feeds one normalized event payload per native event to the SDK
// stream translator and emits the resulting client-format frames.
func (m *Manager) pumpTranslated(req rpcExecutorRequest, plan *prepared, upstreamStreamID string, emit func([]byte) bool, closeDown func(string)) {
	var param any
	ctx := context.Background()
	feed := func(data []byte) bool {
		line := make([]byte, 0, len(data)+len("data: "))
		line = append(line, "data: "...)
		line = append(line, data...)
		frames := translator.TranslateStream(ctx, plan.sdk, plan.clientResp, req.Model, plan.original, plan.translated, line, &param)
		for _, frame := range frames {
			if len(frame) == 0 {
				continue
			}
			if !emit(frame) {
				return false
			}
		}
		return true
	}
	completed := false
	var framer sseEventFramer
	for {
		chunk, errRead := m.bridge.HTTPStreamRead(upstreamStreamID)
		if errRead != nil {
			closeDown("upstream stream read failed")
			return
		}
		if chunk.Error != "" {
			closeDown("upstream stream read failed")
			return
		}
		if len(chunk.Payload) > 0 {
			sawError := false
			malformed := false
			pushErr := framer.push(chunk.Payload, func(event sseEvent) bool {
				data := event.Data
				if len(data) == 0 {
					return true
				}
				signal, isMalformed := classifyNativeEvent(plan.wire, data)
				switch signal {
				case signalError:
					if isMalformed {
						malformed = true
					} else {
						sawError = true
					}
					return false
				case signalDone:
					// Feed the real sentinel so the SDK can flush the terminal
					// event and late usage instead of synthesizing success.
					completed = true
					return feed(data)
				case signalTerminal:
					completed = true
				}
				return feed(data)
			})
			if malformed {
				closeDown("malformed upstream stream")
				return
			}
			if sawError {
				closeDown("upstream stream error")
				return
			}
			if pushErr != nil {
				if errors.Is(pushErr, errEmitStopped) {
					return
				}
				closeDown("malformed upstream stream")
				return
			}
		}
		if chunk.Done {
			break
		}
	}
	if errFinish := framer.finish(); errFinish != nil {
		closeDown("truncated upstream stream")
		return
	}
	if !completed {
		closeDown("upstream stream ended before completion")
	}
}

// streamSignal classifies one native SSE event.
type streamSignal int

const (
	signalNone streamSignal = iota
	signalTerminal
	signalDone
	signalError
)

// classifyNativeEvent inspects a joined SSE data payload and reports whether it
// is a native terminal signal, the chat sentinel, or a native error. The second
// result is true when the payload was not valid JSON at all, which is treated
// as a malformed event so junk never flows downstream as success.
func classifyNativeEvent(wire translator.Format, data []byte) (streamSignal, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return signalNone, false
	}
	if isSSEDone(trimmed) {
		return signalDone, false
	}
	if !json.Valid(trimmed) {
		return signalError, true
	}
	switch wire {
	case translator.FormatClaude:
		switch gjson.GetBytes(trimmed, "type").String() {
		case "message_stop":
			return signalTerminal, false
		case "error":
			return signalError, false
		}
	case translator.FormatOpenAIResponse:
		if gjson.GetBytes(trimmed, "error").Exists() {
			return signalError, false
		}
		switch gjson.GetBytes(trimmed, "type").String() {
		case "response.completed", "response.incomplete":
			return signalTerminal, false
		case "response.failed", "error":
			return signalError, false
		}
	default:
		if gjson.GetBytes(trimmed, "error").Exists() {
			return signalError, false
		}
		choices := gjson.GetBytes(trimmed, "choices")
		if choices.IsArray() {
			for _, choice := range choices.Array() {
				reason := choice.Get("finish_reason")
				if reason.Exists() && reason.Type != gjson.Null {
					return signalTerminal, false
				}
			}
		}
	}
	return signalNone, false
}

// upstreamError maps a non-2xx upstream status to a retryable-aware error.
func upstreamError(status int) error {
	retryable := status == http.StatusTooManyRequests || status >= 500
	return &ProviderError{
		Code:       "upstream_error",
		Message:    "upstream returned status " + http.StatusText(status),
		HTTPStatus: status,
		Retryable:  retryable,
	}
}

// sanitizeResponseHeaders drops hop-by-hop and length/encoding headers that
// become invalid after any protocol conversion.
func sanitizeResponseHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	drop := map[string]struct{}{
		"content-length":      {},
		"content-encoding":    {},
		"transfer-encoding":   {},
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"upgrade":             {},
	}
	out := make(http.Header, len(headers))
	for key, values := range headers {
		if _, skip := drop[strings.ToLower(key)]; skip {
			continue
		}
		out[key] = values
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
