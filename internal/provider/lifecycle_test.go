package provider

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

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
