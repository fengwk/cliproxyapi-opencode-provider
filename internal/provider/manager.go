package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Manager owns the immutable config snapshot and dispatches every RPC method.
// It is safe for concurrent use.
type Manager struct {
	bridge *Bridge

	mu  sync.RWMutex
	cfg Config

	// lifeMu guards the request lifecycle registry and the current run stop
	// channel. Registering a stream and closing or replacing the run channel
	// share the mutex so no worker is ever started against a closed run.
	lifeMu  sync.Mutex
	stop    chan struct{}     // closed to stop the current run; replaced to reopen
	closed  bool              // final shutdown; irreversible
	active  map[string]string // downstream stream id -> upstream stream id
	workers sync.WaitGroup

	// cycle serializes quiesce, reconfigure and shutdown so a WaitGroup drain
	// cannot overlap a reopen and concurrent lifecycle RPCs cannot interleave.
	cycle sync.Mutex

	// importMu serializes the key-import check-and-save so two concurrent
	// imports cannot both observe the same key as missing.
	importMu sync.Mutex
}

// NewManager returns a dispatcher whose host callbacks flow through bridge.
func NewManager(bridge *Bridge) *Manager {
	cfg, _ := parseConfig(nil)
	return &Manager{bridge: bridge, cfg: cfg, stop: make(chan struct{}), active: map[string]string{}}
}

func (m *Manager) config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) setConfig(cfg Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// stopped reports whether the plugin is quiesced or shut down.
func (m *Manager) stopped() bool {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	return m.stoppedLocked()
}

// stoppedLocked is stopped with lifeMu already held.
func (m *Manager) stoppedLocked() bool {
	if m.closed {
		return true
	}
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// beginRun captures the stop channel of the current run for a new execution. It
// returns false once the manager is quiesced or shut down. The captured channel
// is carried in the execution plan so a request prepared before a quiesce can
// never be resurrected by a later reopen.
func (m *Manager) beginRun() (chan struct{}, bool) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.stoppedLocked() {
		return nil, false
	}
	return m.stop, true
}

// registerStream records the downstream/upstream pair and reserves a worker
// slot before the pump goroutine starts. It accepts only the current open run
// channel, so a delayed upstream open prepared before a quiesce cannot start
// work after the manager reopened with a fresh run. It returns false otherwise.
func (m *Manager) registerStream(run chan struct{}, downstreamID, upstreamID string) bool {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.closed || m.stop != run {
		return false
	}
	select {
	case <-run:
		return false
	default:
	}
	m.workers.Add(1)
	if downstreamID != "" {
		m.active[downstreamID] = upstreamID
	}
	return true
}

// finishStream releases the worker slot. It runs after every host callback the
// worker can issue, so a completed shutdown guarantees no further callbacks.
func (m *Manager) finishStream(downstreamID string) {
	m.lifeMu.Lock()
	delete(m.active, downstreamID)
	m.lifeMu.Unlock()
	m.workers.Done()
}

// closeRunLocked closes the current run channel, rejecting new executions, and
// returns a snapshot of the active streams. It is idempotent and must be called
// with lifeMu held.
func (m *Manager) closeRunLocked() map[string]string {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	pairs := make(map[string]string, len(m.active))
	for downstream, upstream := range m.active {
		pairs[downstream] = upstream
	}
	return pairs
}

// closeStreams unblocks the upstream and downstream halves of a stream
// snapshot. It never runs while lifeMu is held.
func (m *Manager) closeStreams(pairs map[string]string) {
	for downstream, upstream := range pairs {
		if upstream != "" {
			m.bridge.HTTPStreamClose(upstream)
		}
		if downstream != "" {
			m.bridge.StreamClose(downstream, "")
		}
	}
}

// quiesce closes the current run channel, rejecting new work and unblocking the
// active streams so they can drain. It is reversible: a later successful
// reconfigure reopens the manager.
func (m *Manager) quiesce() {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	pairs := m.closeRunLocked()
	m.lifeMu.Unlock()
	m.closeStreams(pairs)
}

// shutdown quiesces background stream pumps and waits until every worker has
// finished. It never returns while a worker could still call back into the host.
// Shutdown is irreversible.
func (m *Manager) shutdown() {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	m.closed = true
	pairs := m.closeRunLocked()
	m.lifeMu.Unlock()
	m.closeStreams(pairs)
	m.workers.Wait()
}

// applyConfig resumes a quiesced manager only after old workers drain. Active
// streams keep their config snapshot; final shutdown cannot be reversed.
func (m *Manager) applyConfig(cfg Config) error {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	if m.closed {
		m.lifeMu.Unlock()
		return &ProviderError{Code: "unavailable", Message: "plugin is shut down", HTTPStatus: http.StatusServiceUnavailable}
	}
	if !m.stoppedLocked() {
		m.lifeMu.Unlock()
		m.setConfig(cfg)
		return nil
	}
	m.lifeMu.Unlock()
	// Quiesced: drain every old worker before reopening. New registrations stay
	// impossible while the run channel is closed, so no worker can be added
	// between the wait and the reopen.
	m.workers.Wait()
	m.setConfig(cfg)
	m.lifeMu.Lock()
	m.stop = make(chan struct{})
	m.lifeMu.Unlock()
	return nil
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error envelope so the host
// process is never taken down by this plugin.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = mustEnvelope(errorResult("plugin_error", "internal error handling "+method, 0))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce:
		m.quiesce()
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		m.shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodModelStatic:
		// The catalog has no static/embedded content: it is published only from a
		// successful per-auth upstream GET /models, so an explicit empty list is
		// returned here even when protocol overrides are configured.
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: []pluginapi.ModelInfo{}})
	case pluginabi.MethodModelForAuth:
		return m.handleModelsForAuth(request)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed auth parse request", 0))
		}
		parsed, errParse := parseAuth(req)
		if errParse != nil {
			return mustEnvelope(errorResult("auth_failure", errParse.Error(), 0))
		}
		return okEnvelope(parsed)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed auth refresh request", 0))
		}
		return okEnvelope(refreshAuth(req))
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return mustEnvelope(errorResult("unsupported", "opencode-go login is unsupported; import a manual api key", http.StatusBadRequest))
	case pluginabi.MethodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed executor request", 0))
		}
		result, errExecute := m.execute(req)
		if errExecute != nil {
			return mustEnvelope(resultError(errExecute))
		}
		return okEnvelope(result)
	case pluginabi.MethodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed executor stream request", 0))
		}
		if err := m.executeStream(req); err != nil {
			return mustEnvelope(resultError(err))
		}
		return okEnvelope(rpcExecutorStreamResponse{})
	case pluginabi.MethodExecutorCountTokens:
		return mustEnvelope(errorResult("unsupported", "executor.count_tokens has no OpenCode Go equivalent", http.StatusBadRequest))
	case pluginabi.MethodExecutorHTTPRequest:
		return mustEnvelope(errorResult("unsupported", "executor.http_request is not supported by opencode-go", http.StatusBadRequest))
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed interceptor request", 0))
		}
		headers := interceptBeforeAuth(interceptRequest{
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Headers:        req.Headers,
		})
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: headers})
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(quotaDescription())
	case pluginabi.MethodQuotaFetch:
		var req rpcQuotaRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed quota fetch request", 0))
		}
		result, errQuota := m.fetchQuota(req)
		if errQuota != nil {
			return mustEnvelope(resultError(errQuota))
		}
		return okEnvelope(result)
	case pluginabi.MethodQuotaReset:
		return mustEnvelope(errorResult("unsupported", "opencode-go quota reset is unsupported", http.StatusNotImplemented))
	default:
		return mustEnvelope(errorResult("unknown_method", "unknown method: "+method, 0))
	}
}

// rpcExecutorRequest mirrors the host executor wire shape.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse mirrors the host executor stream result shape.
type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// interceptRequest is the projection of pluginapi.RequestInterceptRequest used
// by the before-auth adapter.
type interceptRequest struct {
	Model          string
	RequestedModel string
	Headers        http.Header
}

func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed lifecycle request", 0))
		}
	}
	cfg, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return mustEnvelope(errorResult("invalid_config", err.Error(), http.StatusBadRequest))
	}
	if err := m.applyConfig(cfg); err != nil {
		return mustEnvelope(resultError(err))
	}
	return okEnvelope(registrationResponse(req.SchemaVersion))
}

func (m *Manager) handleModelsForAuth(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return mustEnvelope(errorResult("invalid_request", "malformed model discovery request", 0))
	}
	// A missing credential is an auth failure and must not trigger any network
	// call or fall back to a static list.
	key, errKey := resolveKey(req.Attributes, req.Metadata, req.StorageJSON)
	if errKey != nil {
		return mustEnvelope(resultError(&ProviderError{
			Code:       "auth_unavailable",
			Message:    errKey.Error(),
			HTTPStatus: http.StatusUnauthorized,
		}))
	}
	cfg := m.config()
	models, errDiscover := m.discoverModels(cfg, key, req.HostCallbackID)
	if errDiscover != nil && len(cfg.ManualModels) == 0 {
		return mustEnvelope(resultError(errDiscover))
	}
	models = addManualModels(models, cfg.ManualModels)
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models})
}

// ProviderError is a dispatch failure carrying an HTTP status and code for the
// host envelope. Messages never contain key material.
type ProviderError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// resultError maps a handler error to an envelope result. Host-provided text is
// never surfaced: only plugin-owned codes, generic messages, and the HTTP status
// are retained.
func resultError(err error) envelopeResult {
	if err == nil {
		return errorResult("plugin_error", "unknown error", 0)
	}
	if hostErr, ok := err.(*HostError); ok {
		result := errorResult("host_call_failed", "host callback failed", hostErr.HTTPStatus)
		result.Error.Retryable = hostErr.Retryable
		return result
	}
	if providerErr, ok := err.(*ProviderError); ok {
		result := errorResult(providerErr.Code, providerErr.Message, providerErr.HTTPStatus)
		result.Error.Retryable = providerErr.Retryable
		return result
	}
	return errorResult("plugin_error", "internal error", 0)
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

type envelopeResult struct {
	OK    bool           `json:"ok"`
	Error *envelopeError `json:"error,omitempty"`
}

func errorResult(code, message string, status int) envelopeResult {
	return envelopeResult{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}}
}

func mustEnvelope(result envelopeResult) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"internal error"}}`), nil
	}
	return raw, nil
}

// okEnvelope serializes a successful RPC envelope around a result value.
func okEnvelope(value any) ([]byte, error) {
	var result json.RawMessage
	if value != nil {
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode response")
		}
		result = encoded
	}
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}
