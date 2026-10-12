package provider

import (
	"bytes"
	"errors"
)

// maxSSEEventBytes bounds a single reassembled SSE event, including the raw
// bytes of every field and comment line, so a malformed or hostile upstream
// cannot exhaust plugin memory.
const maxSSEEventBytes = 8 << 20

// errEmitStopped reports that the downstream consumer asked to stop; it is not
// an upstream framing failure.
var errEmitStopped = errors.New("sse emit stopped")

// errSSEMalformed reports an oversized, malformed, or truncated event.
var errSSEMalformed = errors.New("malformed sse event")

// sseEvent is one complete Server-Sent Event.
type sseEvent struct {
	// Data is the SSE-spec data payload: consecutive data lines joined with "\n"
	// after stripping a single leading space. It is nil when the event carries no
	// data, e.g. a comment/heartbeat event.
	Data []byte
	// Raw is the exact event bytes, preserving every physical line and its
	// original delimiter, terminated by the blank line that ends the event. It is
	// a private copy so callers may retain it.
	Raw []byte
}

// sseEventFramer reassembles complete Server-Sent Events from arbitrarily split
// upstream reads. It tolerates LF, CRLF and lone CR termination, comment lines
// (": ..."), UTF-8 payloads split at any byte boundary, a single leading UTF-8
// BOM, and joins multiple consecutive data lines of one event with "\n" per the
// SSE specification so the semantic translator receives exactly one combined
// payload per event. Complete comment/heartbeat events are dispatched too, with
// a nil Data, so raw passthrough can forward them at event boundaries.
type sseEventFramer struct {
	buf     []byte
	data    [][]byte
	size    int
	raw     []byte
	any     bool
	started bool // the stream start has been inspected for a leading BOM
}

// push feeds raw upstream bytes. It invokes emit once per dispatched event. It
// returns errEmitStopped when emit returns false and errSSEMalformed on an
// oversized or malformed event.
func (f *sseEventFramer) push(chunk []byte, emit func(sseEvent) bool) error {
	if len(chunk) == 0 {
		return nil
	}
	f.buf = append(f.buf, chunk...)
	f.stripBOM()
	for {
		index, width := nextLineBreak(f.buf)
		if index < 0 {
			break
		}
		physical := f.buf[:index+width]
		f.buf = f.buf[index+width:]
		if err := f.line(physical, width, emit); err != nil {
			return err
		}
	}
	if len(f.buf) > maxSSEEventBytes {
		return errSSEMalformed
	}
	return nil
}

// finish reports whether the upstream ended at an event boundary. A trailing
// lone CR is a valid line terminator that push deferred in case the next read
// completed a CRLF, so it is flushed here; any other partial line or an
// undispatched event means the stream was truncated.
func (f *sseEventFramer) finish(emit func(sseEvent) bool) error {
	if len(f.buf) > 0 && f.buf[len(f.buf)-1] == '\r' {
		physical := f.buf
		f.buf = nil
		if err := f.line(physical, 1, emit); err != nil {
			return err
		}
	}
	if len(f.buf) > 0 || len(f.raw) > 0 {
		return errSSEMalformed
	}
	return nil
}

// nextLineBreak returns the index and byte width of the first line break in
// buf. A CR that is the final byte is deferred (returns -1) because the next
// read may complete a CRLF; finish flushes it at EOF.
func nextLineBreak(buf []byte) (int, int) {
	for i := 0; i < len(buf); i++ {
		switch buf[i] {
		case '\n':
			return i, 1
		case '\r':
			if i+1 == len(buf) {
				return -1, 0
			}
			if buf[i+1] == '\n' {
				return i, 2
			}
			return i, 1
		}
	}
	return -1, 0
}

// stripBOM removes exactly one leading UTF-8 BOM. It keeps the framer waiting
// while the buffered prefix could still be the start of a BOM, so a BOM split
// across reads or fed one byte at a time is still stripped once, and only at the
// stream start.
func (f *sseEventFramer) stripBOM() {
	if f.started || len(f.buf) == 0 {
		return
	}
	bom := []byte{0xef, 0xbb, 0xbf}
	n := len(f.buf)
	if n > len(bom) {
		n = len(bom)
	}
	if !bytes.Equal(f.buf[:n], bom[:n]) {
		f.started = true
		return
	}
	if n < len(bom) {
		return
	}
	f.buf = f.buf[len(bom):]
	f.started = true
}

// line consumes one physical line including its delimiter. The raw bytes are
// always retained; only "data" fields contribute to the joined payload.
func (f *sseEventFramer) line(physical []byte, width int, emit func(sseEvent) bool) error {
	f.raw = append(f.raw, physical...)
	if len(f.raw) > maxSSEEventBytes {
		return errSSEMalformed
	}
	content := physical[:len(physical)-width]
	if len(content) == 0 {
		return f.dispatch(emit)
	}
	f.any = true
	if content[0] == ':' {
		return nil
	}
	colon := bytes.IndexByte(content, ':')
	if colon < 0 || !bytes.Equal(content[:colon], []byte("data")) {
		return nil
	}
	value := content[colon+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	f.data = append(f.data, value)
	f.size += len(value)
	if f.size > maxSSEEventBytes {
		return errSSEMalformed
	}
	return nil
}

func (f *sseEventFramer) dispatch(emit func(sseEvent) bool) error {
	raw := append([]byte(nil), f.raw...)
	var joined []byte
	if len(f.data) > 0 {
		joined = bytes.Join(f.data, []byte("\n"))
	}
	stray := !f.any && len(f.data) == 0
	f.raw = f.raw[:0]
	f.data = f.data[:0]
	f.size = 0
	f.any = false
	if stray {
		// A blank line with no preceding field is not an event; drop it.
		return nil
	}
	if !emit(sseEvent{Data: joined, Raw: raw}) {
		return errEmitStopped
	}
	return nil
}

// isSSEDone reports whether a data payload is the terminal chat sentinel.
func isSSEDone(payload []byte) bool {
	return bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]"))
}
