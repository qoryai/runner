// Package chunk cuts a stream of bytes into the log chunks of the contract: one line or
// Size bytes, whichever comes first.
package chunk

import "sync"

// Size is the longest chunk, in bytes. A line longer than it is cut at it.
const Size = 4096

// Writer is an io.Writer that hands complete chunks to emit as they form: at every
// newline, inclusive, and at Size bytes. What remains is held until the next write or
// [Writer.Flush]. It is safe for concurrent writes; chunks are emitted in write order.
type Writer struct {
	emit func([]byte)
	mu   sync.Mutex
	buf  []byte
}

// New returns a writer that hands each chunk to emit. The slice passed to emit is the
// callee's; the writer does not reuse it.
func New(emit func([]byte)) *Writer {
	return &Writer{emit: emit}
}

// Write cuts p, with what was held, into chunks. It never fails.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		n := -1
		for i, b := range w.buf {
			if b == '\n' || i+1 == Size {
				n = i + 1
				break
			}
		}
		if n < 0 {
			return len(p), nil
		}
		chunk := make([]byte, n)
		copy(chunk, w.buf[:n])
		w.buf = w.buf[n:]
		w.emit(chunk)
	}
}

// Flush emits what is held, if anything, as a final chunk.
func (w *Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	chunk := w.buf
	w.buf = nil
	w.emit(chunk)
}
