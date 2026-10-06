package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// activeCount reports how many downstream streams are currently registered.
func (m *Manager) activeCount() int {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	return len(m.active)
}

// callResult carries a raw RPC envelope out of a lifecycle call run off the test
// goroutine so the test goroutine can decode it.
type callResult struct {
	raw []byte
	err error
}

// lifecycleCall invokes a register/reconfigure/quiesce/shutdown method with the
// given config yaml and returns the raw envelope.
func lifecycleCall(t *testing.T, manager *Manager, method, yaml string) []byte {
	t.Helper()
	raw, err := manager.HandleCall(method, registrationFor(t, yaml))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return raw
}

// registerManager drives the initial plugin.register on a bare manager.
func registerManager(t *testing.T, manager *Manager) {
	t.Helper()
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginRegister, ""), nil)
}

// executeNonStreamRaw issues one non-streaming execution and returns the
// envelope without asserting success, so callers can inspect an error.
func executeNonStreamRaw(t *testing.T, manager *Manager) []byte {
	t.Helper()
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, baseExecRequest(), ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return raw
}

func startStreamWithID(t *testing.T, manager *Manager, downstreamID string) {
	t.Helper()
	req := baseExecRequest()
	req.Stream = true
	raw, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, downstreamID, "cb"))
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}
	decodeResult(t, raw, nil)
}

// TestShutdownUnblocksBlockedUpstreamRead verifies shutdown closes the upstream
// stream so a worker blocked in stream_read is released and awaited.
func TestShutdownUnblocksBlockedUpstreamRead(t *testing.T) {
	host := newFakeHost()
	host.openStream()
	manager := configuredManager(t, host, "")
	startStreamWithID(t, manager, "down-stream")
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		manager.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown blocked on a pending upstream read")
	}
}

// TestQuiesceRejectsNewExecutions verifies quiesce stops accepting work.
func TestQuiesceRejectsNewExecutions(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	raw, err := manager.HandleCall(pluginabi.MethodPluginQuiesce, nil)
	if err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	decodeResult(t, raw, nil)

	raw, err = manager.HandleCall(pluginabi.MethodExecutorExecute, execBytes(t, baseExecRequest(), ""))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	envErr := decodeError(t, raw)
	if envErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("execution after quiesce must be rejected, got %+v", envErr)
	}
}

// TestConcurrentStartAndShutdown exercises the registration/shutdown race under
// the race detector: no worker may be started after shutdown snapshots state.
func TestConcurrentStartAndShutdown(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := baseExecRequest()
			req.Stream = true
			id := fmt.Sprintf("down-%d", index)
			_, _ = manager.HandleCall(pluginabi.MethodExecutorExecuteStream, streamExecBytes(t, req, id, "cb"))
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		manager.shutdown()
	}()
	wg.Wait()
	if !manager.stopped() {
		t.Fatalf("manager must be stopping after shutdown")
	}
}

// TestNoCallbackAfterShutdown verifies shutdown returns only after every worker
// has stopped issuing host callbacks.
func TestNoCallbackAfterShutdown(t *testing.T) {
	host := newFakeHost()
	_, ch := host.openStream()
	manager := configuredManager(t, host, "")
	startStreamWithID(t, manager, "down-stream")
	ch <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	time.Sleep(50 * time.Millisecond)

	manager.shutdown()
	before := len(host.callLog())
	time.Sleep(100 * time.Millisecond)
	if after := len(host.callLog()); after != before {
		t.Fatalf("host callbacks continued after shutdown: %d -> %d", before, after)
	}
}

// TestQuiesceThenReconfigureRecoversInference pins the rollback sequence: a
// quiesce rejects new work, and a later valid reconfigure reopens the same
// manager for both non-streaming and streaming inference.
func TestQuiesceThenReconfigureRecoversInference(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[DefaultBaseURL+"/chat/completions"] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK, Body: []byte(`{"id":"x"}`),
	}
	manager := configuredManager(t, host, "")

	// Baseline: inference works before the rollback.
	decodeResult(t, executeNonStreamRaw(t, manager), nil)

	// Quiesce rejects new work.
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginQuiesce, ""), nil)
	if code := decodeError(t, executeNonStreamRaw(t, manager)).StatusCode(); code != http.StatusServiceUnavailable {
		t.Fatalf("quiesced non-stream status = %d, want 503", code)
	}

	// A valid reconfigure reopens the manager.
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, ""), nil)
	decodeResult(t, executeNonStreamRaw(t, manager), nil)

	host.queueStream([]byte("data: [DONE]\n\n"))
	startStreamWithID(t, manager, "down-recovered")
	if !waitForClose(t, host, time.Second) {
		t.Fatal("stream after reconfigure did not complete")
	}
	if errMsg := lastCloseError(host); errMsg != "" {
		t.Fatalf("recovered stream closed with error %q", errMsg)
	}
}

// TestInvalidReconfigureKeepsQuiesced verifies config validation runs before any
// lifecycle reset, so an invalid reconfigure cannot reactivate the manager.
func TestInvalidReconfigureKeepsQuiesced(t *testing.T) {
	host := newFakeHost()
	host.httpResponses[DefaultBaseURL+"/chat/completions"] = pluginapi.HTTPResponse{
		StatusCode: http.StatusOK, Body: []byte(`{"id":"x"}`),
	}
	manager := configuredManager(t, host, "")
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginQuiesce, ""), nil)

	bad := lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, "base-url: ftp://example.com\n")
	if code := decodeError(t, bad).StatusCode(); code != http.StatusBadRequest {
		t.Fatalf("invalid reconfigure status = %d, want 400", code)
	}
	if code := decodeError(t, executeNonStreamRaw(t, manager)).StatusCode(); code != http.StatusServiceUnavailable {
		t.Fatalf("invalid reconfigure reactivated the manager: status %d, want 503", code)
	}

	// A later valid reconfigure still recovers.
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, ""), nil)
	decodeResult(t, executeNonStreamRaw(t, manager), nil)
}

// TestReconfigureAfterShutdownIsUnavailable verifies shutdown is irreversible: a
// reconfigure cannot reopen a shut-down manager and reports a sanitized 503.
func TestReconfigureAfterShutdownIsUnavailable(t *testing.T) {
	host := newFakeHost()
	manager := configuredManager(t, host, "")
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginShutdown, ""), nil)

	envErr := decodeError(t, lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, ""))
	if envErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("reconfigure after shutdown status = %d, want 503", envErr.StatusCode())
	}
	if !manager.stopped() {
		t.Fatal("manager must stay stopped after shutdown")
	}
	if code := decodeError(t, executeNonStreamRaw(t, manager)).StatusCode(); code != http.StatusServiceUnavailable {
		t.Fatalf("execution after shutdown status = %d, want 503", code)
	}
}

// TestDelayedStreamOpenCannotRegisterAfterReopen covers the quiesce race where an
// upstream open prepared before a quiesce returns only after a reconfigure
// reopened the manager: the stale request must not start work under the new run.
func TestDelayedStreamOpenCannotRegisterAfterReopen(t *testing.T) {
	host := newFakeHost()
	host.openStream()
	openStarted := make(chan struct{})
	openRelease := make(chan struct{})
	manager := NewManager(NewBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			close(openStarted)
			<-openRelease
		}
		return host.call(method, payload)
	}))
	registerManager(t, manager)

	req := baseExecRequest()
	req.Stream = true
	payload := streamExecBytes(t, req, "down-delayed", "cb")
	resultCh := make(chan callResult, 1)
	go func() {
		raw, err := manager.HandleCall(pluginabi.MethodExecutorExecuteStream, payload)
		resultCh <- callResult{raw, err}
	}()

	// Hold the upstream open across a full quiesce/reconfigure cycle, then let it
	// return so the stale run channel is exposed.
	<-openStarted
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginQuiesce, ""), nil)
	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, ""), nil)
	close(openRelease)

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("execute stream rpc: %v", result.err)
	}
	if code := decodeError(t, result.raw).StatusCode(); code != http.StatusServiceUnavailable {
		t.Fatalf("delayed upstream open registered after reopen: status %d, want 503", code)
	}
}

// TestQuiesceReconfigureDrainsBlockedStreamBeforeReopen verifies the reopen waits
// for the old workers: a stream blocked in an upstream read is drained before the
// manager reactivates, so no stale worker survives the reopen.
func TestQuiesceReconfigureDrainsBlockedStreamBeforeReopen(t *testing.T) {
	host := newFakeHost()
	oldUp, _ := host.openStream()
	readGate := make(chan struct{})
	readBlocked := make(chan struct{})
	var once sync.Once
	manager := NewManager(NewBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPStreamRead {
			var req struct {
				StreamID string `json:"stream_id"`
			}
			_ = json.Unmarshal(payload, &req)
			if req.StreamID == oldUp {
				once.Do(func() { close(readBlocked) })
				<-readGate
			}
		}
		return host.call(method, payload)
	}))
	registerManager(t, manager)
	startStreamWithID(t, manager, "down-old")
	<-readBlocked // the worker is registered and blocked in an upstream read

	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginQuiesce, ""), nil)
	if got := manager.activeCount(); got != 1 {
		t.Fatalf("blocked worker must remain registered after quiesce, got %d", got)
	}

	reconfPayload := registrationFor(t, "")
	reconfCh := make(chan callResult, 1)
	go func() {
		raw, err := manager.HandleCall(pluginabi.MethodPluginReconfigure, reconfPayload)
		reconfCh <- callResult{raw, err}
	}()

	// Reconfigure must block on the withheld worker instead of reopening early.
	select {
	case res := <-reconfCh:
		t.Fatalf("reconfigure returned before the old worker drained: err=%v", res.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(readGate)
	res := <-reconfCh
	if res.err != nil {
		t.Fatalf("reconfigure rpc: %v", res.err)
	}
	decodeResult(t, res.raw, nil)
	if got := manager.activeCount(); got != 0 {
		t.Fatalf("old worker not drained before reopen: %d active", got)
	}
	closed := false
	for _, c := range host.closes() {
		if c.StreamID == "down-old" {
			closed = true
		}
	}
	if !closed {
		t.Fatal("old downstream stream was not closed")
	}

	// The manager reopened: a fresh stream registers on the new run and runs.
	host.queueStream([]byte("data: [DONE]\n\n"))
	startStreamWithID(t, manager, "down-new")
	if !waitForClose(t, host, time.Second) {
		t.Fatal("stream after reopen did not complete")
	}
}

// TestActiveReconfigureDoesNotInterruptStream verifies a reconfigure on a running
// manager only swaps the config snapshot and leaves the in-flight stream alone.
func TestActiveReconfigureDoesNotInterruptStream(t *testing.T) {
	host := newFakeHost()
	_, ch := host.openStream()
	manager := configuredManager(t, host, "")
	startStreamWithID(t, manager, "down-active")

	decodeResult(t, lifecycleCall(t, manager, pluginabi.MethodPluginReconfigure, ""), nil)
	if manager.stopped() {
		t.Fatal("active reconfigure must not stop the manager")
	}

	ch <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	ch <- []byte("data: [DONE]\n\n")
	close(ch)
	if !waitForClose(t, host, time.Second) {
		t.Fatal("stream was interrupted by an active reconfigure")
	}
	if errMsg := lastCloseError(host); errMsg != "" {
		t.Fatalf("stream closed with error %q", errMsg)
	}
	if got := len(host.emitted("down-active")); got != 1 {
		t.Fatalf("expected 1 emitted frame, got %d", got)
	}
}
