package provider

import (
	"bytes"
	"testing"
)

// framedEvent captures one dispatched SSE event for framing-level assertions.
type framedEvent struct {
	Data []byte
	Raw  []byte
}

// collectFramer feeds raw in fixed-size chunks (1 = byte by byte) and returns
// every dispatched event plus the EOF result. It exercises the exact private
// framer contract the stream pumps rely on.
func collectFramer(t *testing.T, raw []byte, chunkSize int) ([]framedEvent, error) {
	t.Helper()
	var framer sseEventFramer
	var events []framedEvent
	emit := func(event sseEvent) bool {
		events = append(events, framedEvent{Data: append([]byte(nil), event.Data...), Raw: append([]byte(nil), event.Raw...)})
		return true
	}
	for _, chunk := range splitBytes(raw, chunkSize) {
		if err := framer.push(chunk, emit); err != nil {
			return events, err
		}
	}
	return events, framer.finish(emit)
}

// TestSSEFramerLineTerminators verifies LF, CRLF and lone CR all delimit
// events, including when every byte arrives in its own read.
func TestSSEFramerLineTerminators(t *testing.T) {
	cases := map[string]string{
		"lf":   "data: hello\n\ndata: world\n\n",
		"crlf": "data: hello\r\n\r\ndata: world\r\n\r\n",
		"cr":   "data: hello\r\rdata: world\r\r",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			for _, chunk := range []int{1, 2, len(raw)} {
				events, err := collectFramer(t, []byte(raw), chunk)
				if err != nil {
					t.Fatalf("chunk=%d: finish error = %v", chunk, err)
				}
				if len(events) != 2 {
					t.Fatalf("chunk=%d: events = %d, want 2", chunk, len(events))
				}
				if string(events[0].Data) != "hello" || string(events[1].Data) != "world" {
					t.Fatalf("chunk=%d: data = %q, %q", chunk, events[0].Data, events[1].Data)
				}
			}
		})
	}
}

// TestSSEFramerCRLFAcrossChunksKeepsRaw verifies a CRLF delimiter split between
// the CR and the LF is reassembled and the raw bytes stay byte-native.
func TestSSEFramerCRLFAcrossChunksKeepsRaw(t *testing.T) {
	raw := "data: hi\r\n\r\n"
	var framer sseEventFramer
	var events []framedEvent
	emit := func(event sseEvent) bool {
		events = append(events, framedEvent{Data: append([]byte(nil), event.Data...), Raw: append([]byte(nil), event.Raw...)})
		return true
	}
	if err := framer.push([]byte("data: hi\r"), emit); err != nil {
		t.Fatalf("push split CR: %v", err)
	}
	if err := framer.push([]byte("\n\r\n"), emit); err != nil {
		t.Fatalf("push LF: %v", err)
	}
	if err := framer.finish(emit); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if string(events[0].Data) != "hi" {
		t.Fatalf("data = %q, want hi", events[0].Data)
	}
	if string(events[0].Raw) != raw {
		t.Fatalf("raw = %q, want %q", events[0].Raw, raw)
	}
}

// TestSSEFramerTrailingLoneCRFlushes verifies a final lone CR is treated as a
// valid line terminator at EOF so the closing blank line still dispatches.
func TestSSEFramerTrailingLoneCRFlushes(t *testing.T) {
	raw := []byte("data: {\"a\":1}\r\r")
	for _, chunk := range []int{1, 2, len(raw)} {
		events, err := collectFramer(t, raw, chunk)
		if err != nil {
			t.Fatalf("chunk=%d: finish error = %v", chunk, err)
		}
		if len(events) != 1 || string(events[0].Data) != `{"a":1}` {
			t.Fatalf("chunk=%d: events = %+v", chunk, events)
		}
	}
}

// TestSSEFramerStripsOneLeadingBOM verifies a leading UTF-8 BOM is removed even
// when split across reads, exactly once and only at the stream start.
func TestSSEFramerStripsOneLeadingBOM(t *testing.T) {
	body := "data: hello\n\ndata: [DONE]\n\n"
	withBOM := append([]byte{0xef, 0xbb, 0xbf}, []byte(body)...)
	for _, chunk := range []int{1, 2, 3, len(withBOM)} {
		events, err := collectFramer(t, withBOM, chunk)
		if err != nil {
			t.Fatalf("chunk=%d: finish error = %v", chunk, err)
		}
		if len(events) != 2 || string(events[0].Data) != "hello" {
			t.Fatalf("chunk=%d: BOM must not discard the first frame: %+v", chunk, events)
		}
	}

	// A BOM inside a data payload is content, never stripped.
	body = "data: x\xef\xbb\xbfy\n\n"
	events, err := collectFramer(t, []byte(body), 1)
	if err != nil {
		t.Fatalf("interior BOM: %v", err)
	}
	if len(events) != 1 || string(events[0].Data) != "x\xef\xbb\xbfy" {
		t.Fatalf("interior BOM must be preserved: %+v", events)
	}
}

// TestSSEFramerTruncatedInputs verifies a partial line, a line without a
// closing blank line, and a lone trailing CR without a blank line are all
// truncated rather than silently completed.
func TestSSEFramerTruncatedInputs(t *testing.T) {
	for _, raw := range []string{
		"data: x",
		"data: x\n",
		"data: x\r",
		"data: x\r\n",
		": comment\n",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := collectFramer(t, []byte(raw), 1); err == nil {
				t.Fatalf("truncated input %q must fail", raw)
			}
		})
	}
}

// TestSSEFramerOversizeRejected verifies an unbounded single line is rejected
// at the configured event byte ceiling.
func TestSSEFramerOversizeRejected(t *testing.T) {
	var framer sseEventFramer
	huge := bytes.Repeat([]byte("a"), maxSSEEventBytes+1)
	if err := framer.push(huge, func(sseEvent) bool { return true }); err == nil {
		t.Fatal("oversize event must be rejected")
	}
}

// TestSSEFramerEmitStopped verifies a downstream stop is reported, including
// when the final deferred CR triggers the last dispatch.
func TestSSEFramerEmitStopped(t *testing.T) {
	stop := func(sseEvent) bool { return false }
	var framer sseEventFramer
	if err := framer.push([]byte("data: x\n\n"), stop); err != errEmitStopped {
		t.Fatalf("push stop error = %v, want errEmitStopped", err)
	}

	var trailing sseEventFramer
	if err := trailing.push([]byte("data: y\r\r"), func(sseEvent) bool { return true }); err != nil {
		t.Fatalf("push with deferred CR must not fail: %v", err)
	}
	if err := trailing.finish(stop); err != errEmitStopped {
		t.Fatalf("finish stop error = %v, want errEmitStopped", err)
	}
}
