package provider

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// RawCaller issues one host callback RPC and returns the raw envelope bytes.
// It is satisfied by the C ABI shim in main.go and by fakes in tests.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostError is a decoded host callback failure. Host-provided text is never
// retained: only the plugin-owned method name, a fixed message, and the HTTP
// status survive, so host errors that echo credentials cannot leak outward.
type HostError struct {
	Method     string
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

func (e *HostError) Error() string {
	if e == nil {
		return ""
	}
	if e.Method == "" {
		return "host callback failed"
	}
	return fmt.Sprintf("host callback %s failed", e.Method)
}

// StatusCode exposes an HTTP status associated with the host failure.
func (e *HostError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// Bridge wraps the host callback ABI with typed helpers. A single Bridge is
// safe for concurrent use.
type Bridge struct {
	call RawCaller
}

// NewBridge returns a bridge over the supplied raw caller.
func NewBridge(call RawCaller) *Bridge {
	return &Bridge{call: call}
}

// invoke marshals request, performs the callback and decodes the envelope into
// out. A host-reported failure is returned as *HostError.
func (b *Bridge) invoke(method string, request any, out any) error {
	if b == nil || b.call == nil {
		return fmt.Errorf("host bridge is unavailable")
	}
	var payload []byte
	if request != nil {
		encoded, errMarshal := json.Marshal(request)
		if errMarshal != nil {
			return fmt.Errorf("encode %s request", method)
		}
		payload = encoded
	}
	raw, errCall := b.call(method, payload)
	if errCall != nil {
		return &HostError{Method: method, Code: "host_call_failed", Message: "host callback failed"}
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return fmt.Errorf("decode %s response", method)
	}
	if !env.OK {
		hostErr := &HostError{Method: method, Code: "host_call_failed", Message: "host callback failed"}
		if env.Error != nil {
			hostErr.Retryable = env.Error.Retryable
			hostErr.HTTPStatus = env.Error.HTTPStatus
		}
		return hostErr
	}
	if out != nil && len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
			return fmt.Errorf("decode %s result", method)
		}
	}
	return nil
}

// HTTPDoRequest is the wire shape for host.http.do / host.http.do_stream.
type HTTPDoRequest struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

// HTTPStreamStart is the decoded host.http.do_stream result.
type HTTPStreamStart struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

// HTTPStreamRead is the decoded host.http.stream_read result.
type HTTPStreamRead struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// HTTPDo performs a non-streaming upstream HTTP request through the host.
func (b *Bridge) HTTPDo(method, url string, headers http.Header, body []byte, callbackID string) (pluginapi.HTTPResponse, error) {
	req := HTTPDoRequest{Method: method, URL: url, Headers: headers, Body: body, HostCallbackID: callbackID}
	var resp pluginapi.HTTPResponse
	err := b.invoke(pluginabi.MethodHostHTTPDo, req, &resp)
	return resp, err
}

// HTTPDoStream opens a streaming upstream HTTP request through the host.
func (b *Bridge) HTTPDoStream(method, url string, headers http.Header, body []byte, callbackID string) (HTTPStreamStart, error) {
	req := HTTPDoRequest{Method: method, URL: url, Headers: headers, Body: body, HostCallbackID: callbackID}
	var resp HTTPStreamStart
	err := b.invoke(pluginabi.MethodHostHTTPDoStream, req, &resp)
	return resp, err
}

// HTTPStreamRead reads the next chunk from an upstream HTTP stream.
func (b *Bridge) HTTPStreamRead(streamID string) (HTTPStreamRead, error) {
	var resp HTTPStreamRead
	err := b.invoke(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID}, &resp)
	return resp, err
}

// HTTPStreamClose closes an upstream HTTP stream.
func (b *Bridge) HTTPStreamClose(streamID string) {
	if streamID == "" {
		return
	}
	_ = b.invoke(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID}, nil)
}

// StreamEmit forwards one downstream stream chunk.
func (b *Bridge) StreamEmit(streamID string, payload []byte) error {
	return b.invoke(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": streamID, "payload": payload}, nil)
}

// StreamClose closes the downstream stream, optionally with a terminal error.
func (b *Bridge) StreamClose(streamID, errMessage string) {
	if streamID == "" {
		return
	}
	req := map[string]string{"stream_id": streamID}
	if errMessage != "" {
		req["error"] = errMessage
	}
	_ = b.invoke(pluginabi.MethodHostStreamClose, req, nil)
}

// Log forwards a structured log line to the host. Key material must never be
// placed in message or fields.
func (b *Bridge) Log(level, message string, fields map[string]any) {
	req := map[string]any{"level": level, "message": message}
	if len(fields) > 0 {
		req["fields"] = fields
	}
	_ = b.invoke(pluginabi.MethodHostLog, req, nil)
}

// AuthList returns every credential known to the host.
func (b *Bridge) AuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := b.invoke(pluginabi.MethodHostAuthList, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// AuthSave persists one physical auth file. jsonBody must be a raw JSON object.
func (b *Bridge) AuthSave(name string, jsonBody json.RawMessage) error {
	req := struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}{Name: name, JSON: jsonBody}
	return b.invoke(pluginabi.MethodHostAuthSave, req, nil)
}
