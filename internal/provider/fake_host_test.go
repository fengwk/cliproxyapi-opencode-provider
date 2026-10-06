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

// fakeHost is a behaviorally faithful stand-in for the host callback ABI used
// by unit tests. It mirrors the real envelope shapes ({ok,result,error}) and the
// host's lowercase/uppercase JSON conventions so the tests exercise the actual
// wire contract.
type fakeHost struct {
	mu sync.Mutex

	calls []string

	authFiles []pluginapi.HostAuthFileEntry
	saved     []savedAuth
	saveFail  map[string]bool

	httpResponses map[string]pluginapi.HTTPResponse
	httpErrors    map[string]error
	httpRequests  []HTTPDoRequest

	nextStream   int
	readChans    map[string]chan []byte
	readStop     map[string]chan struct{}
	stopClosed   map[string]bool
	streamQueue  []string
	emitPayloads map[string][][]byte
	closed       []string
	closeDetail  []streamClose
	closeCh      chan string

	forcedErrors map[string]error
}

// streamClose records a downstream stream close and any terminal error text.
type streamClose struct {
	StreamID string
	Error    string
}

type savedAuth struct {
	Name string
	JSON json.RawMessage
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		saveFail:      map[string]bool{},
		httpResponses: map[string]pluginapi.HTTPResponse{},
		httpErrors:    map[string]error{},
		readChans:     map[string]chan []byte{},
		readStop:      map[string]chan struct{}{},
		stopClosed:    map[string]bool{},
		emitPayloads:  map[string][][]byte{},
		closeCh:       make(chan string, 32),
		forcedErrors:  map[string]error{},
	}
}

// queueStream registers an upstream stream that yields payloads then closes and
// makes it the next stream returned by host.http.do_stream.
func (f *fakeHost) queueStream(payloads ...[]byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextStream++
	id := fmt.Sprintf("up-%d", f.nextStream)
	ch := make(chan []byte, len(payloads)+1)
	for _, payload := range payloads {
		ch <- payload
	}
	close(ch)
	f.readChans[id] = ch
	f.readStop[id] = make(chan struct{})
	f.streamQueue = append(f.streamQueue, id)
	return id
}

// openStream registers an upstream stream that stays open until the returned
// channel is closed by the test.
func (f *fakeHost) openStream() (string, chan []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextStream++
	id := fmt.Sprintf("up-%d", f.nextStream)
	ch := make(chan []byte, 64)
	f.readChans[id] = ch
	f.readStop[id] = make(chan struct{})
	f.streamQueue = append(f.streamQueue, id)
	return id, ch
}

func (f *fakeHost) emitted(streamID string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emitPayloads[streamID]
}

// closes returns every observed downstream close with its terminal error text.
func (f *fakeHost) closes() []streamClose {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]streamClose(nil), f.closeDetail...)
}

func (f *fakeHost) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHost) savedAuths() []savedAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]savedAuth(nil), f.saved...)
}

// addAuthFile mirrors a successful host.auth.save in later host.auth.list
// responses so tests can exercise the import check-and-save against a host that
// reflects persisted files.
func (f *fakeHost) addAuthFile(entry pluginapi.HostAuthFileEntry) {
	f.mu.Lock()
	f.authFiles = append(f.authFiles, entry)
	f.mu.Unlock()
}

func (f *fakeHost) requests() []HTTPDoRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HTTPDoRequest(nil), f.httpRequests...)
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	forced := f.forcedErrors[method]
	f.mu.Unlock()
	if forced != nil {
		return nil, forced
	}

	switch method {
	case pluginabi.MethodHostHTTPDo:
		var req HTTPDoRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad http.do request")
		}
		f.mu.Lock()
		f.httpRequests = append(f.httpRequests, req)
		f.mu.Unlock()
		if err := f.httpErrors[req.URL]; err != nil {
			return nil, err
		}
		resp, ok := f.httpResponses[req.URL]
		if !ok {
			resp = pluginapi.HTTPResponse{StatusCode: http.StatusOK}
		}
		return okResult(resp)
	case pluginabi.MethodHostHTTPDoStream:
		var req HTTPDoRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad do_stream request")
		}
		f.mu.Lock()
		f.httpRequests = append(f.httpRequests, req)
		f.mu.Unlock()
		status := http.StatusOK
		if resp, ok := f.httpResponses[req.URL]; ok {
			status = resp.StatusCode
		}
		f.mu.Lock()
		streamID := ""
		if len(f.streamQueue) > 0 {
			streamID = f.streamQueue[0]
			f.streamQueue = f.streamQueue[1:]
		}
		f.mu.Unlock()
		if streamID == "" {
			streamID = f.newEmptyStream()
		}
		return okResult(map[string]any{"status_code": status, "stream_id": streamID})
	case pluginabi.MethodHostHTTPStreamRead:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad stream_read request")
		}
		f.mu.Lock()
		ch, ok := f.readChans[req.StreamID]
		stop := f.readStop[req.StreamID]
		f.mu.Unlock()
		if !ok {
			return okResult(map[string]any{"done": true})
		}
		if stop == nil {
			value, open := <-ch
			if !open {
				return okResult(map[string]any{"done": true})
			}
			return okResult(map[string]any{"payload": value})
		}
		select {
		case value, open := <-ch:
			if !open {
				return okResult(map[string]any{"done": true})
			}
			return okResult(map[string]any{"payload": value})
		case <-stop:
			return okResult(map[string]any{"done": true})
		}
	case pluginabi.MethodHostHTTPStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad upstream stream close request")
		}
		f.closeStop(req.StreamID)
		return okResult(struct{}{})
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad emit request")
		}
		f.mu.Lock()
		f.emitPayloads[req.StreamID] = append(f.emitPayloads[req.StreamID], req.Payload)
		f.mu.Unlock()
		return okResult(struct{}{})
	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad stream close request")
		}
		f.mu.Lock()
		f.closed = append(f.closed, req.StreamID)
		f.closeDetail = append(f.closeDetail, streamClose{StreamID: req.StreamID, Error: req.Error})
		f.mu.Unlock()
		select {
		case f.closeCh <- req.StreamID:
		default:
		}
		return okResult(struct{}{})
	case pluginabi.MethodHostLog:
		return okResult(struct{}{})
	case pluginabi.MethodHostAuthList:
		f.mu.Lock()
		files := append([]pluginapi.HostAuthFileEntry(nil), f.authFiles...)
		f.mu.Unlock()
		return okResult(map[string]any{"files": files})
	case pluginabi.MethodHostAuthSave:
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("fake: bad auth save request")
		}
		f.mu.Lock()
		fail := f.saveFail[req.Name]
		if !fail {
			f.saved = append(f.saved, savedAuth{Name: req.Name, JSON: req.JSON})
		}
		f.mu.Unlock()
		if fail {
			return nil, fmt.Errorf("fake: auth save rejected")
		}
		return okResult(map[string]any{"name": req.Name, "path": "/tmp/" + req.Name})
	default:
		return nil, fmt.Errorf("fake: unsupported host method %s", method)
	}
}

func (f *fakeHost) newEmptyStream() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextStream++
	id := fmt.Sprintf("up-%d", f.nextStream)
	ch := make(chan []byte, 1)
	close(ch)
	f.readChans[id] = ch
	f.readStop[id] = make(chan struct{})
	return id
}

// closeStop unblocks a pending host.http.stream_read, mirroring the real host
// behaviour when an upstream stream is closed.
func (f *fakeHost) closeStop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopClosed[id] {
		return
	}
	if stop, ok := f.readStop[id]; ok {
		f.stopClosed[id] = true
		close(stop)
	}
}

func okResult(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errResult(code, message string, status int) ([]byte, error) {
	result, err := json.Marshal(pluginabi.Envelope{OK: false, Error: pluginabi.NewError(code, message, status)})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func newTestManager(host *fakeHost) *Manager {
	return NewManager(NewBridge(host.call))
}

// registrationFor builds a registration payload with the given YAML body.
func registrationFor(t *testing.T, body string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"schema_version": 6, "config_yaml": []byte(body)})
	if err != nil {
		t.Fatalf("encode registration request: %v", err)
	}
	return payload
}

// decodeResult unmarshals an RPC envelope and fails when it reports an error.
func decodeResult(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got error: %+v", env.Error)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			t.Fatalf("decode result: %v (%s)", err, env.Result)
		}
	}
}

// decodeError unmarshals an RPC envelope and fails when it reports success.
func decodeError(t *testing.T, raw []byte) *pluginabi.Error {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	if env.OK {
		t.Fatalf("expected error envelope, got ok")
	}
	if env.Error == nil {
		t.Fatalf("expected error payload")
	}
	return env.Error
}

// waitForClose waits until the host observes a downstream stream close.
func waitForClose(t *testing.T, host *fakeHost, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-host.closeCh:
		return true
	case <-time.After(timeout):
		return false
	}
}
