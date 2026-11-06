// Package nativearchive copies native diagnostic text; it carries no operational events or audit records.
package nativearchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const Capacity = 64
const MaxLineBytes = 8192
const MaxPathBytes = 4096

type copyLine struct {
	path, line string
	rotate     bool
}

// Status separates diagnostic-copy outcomes from radio or codec operation.
type Status struct {
	Accepted, Written, DroppedFull, DroppedBusy, DroppedOversize, DroppedClosed, WriteErrors uint64
	Queued, Capacity                                                                         int
	Closing, DrainComplete                                                                   bool
	LastWriteError, LastDrainError                                                           string
}

// Writer owns one bounded diagnostic-copy queue and its file operations.
type Writer struct {
	mu                                                             sync.Mutex
	queue                                                          chan copyLine
	done                                                           chan struct{}
	closed                                                         bool
	accepted, written, full, busy, oversized, stopped, writeErrors atomic.Uint64
	lastWriteError, lastDrainError                                 atomic.Value
}

func New() *Writer {
	w := &Writer{queue: make(chan copyLine, Capacity), done: make(chan struct{})}
	w.lastWriteError.Store("")
	w.lastDrainError.Store("")
	go w.run()
	return w
}

// Redact preserves the existing native-detail policy and masks the caller's known credential.
func Redact(line, credential string) string {
	lower := strings.ToLower(line)
	for _, word := range []string{"password", "secret", "unique id", "udid", "auth token", "authentication token"} {
		if strings.Contains(lower, word) {
			return "[Private native detail omitted]"
		}
	}
	if credential != "" && strings.Contains(line, credential) {
		return "[Private native detail omitted]"
	}
	return line
}

// Copy accepts immutable, already-redacted text and the exact path of its originating run.
// Admission never waits for a file operation. Rejected copies affect diagnostic counters only.
func (w *Writer) Copy(path, line string, rotate bool) {
	if len(line) >= MaxLineBytes || len(path) > MaxPathBytes || path == "" {
		w.oversized.Add(1)
		return
	}
	if !w.mu.TryLock() {
		w.busy.Add(1)
		return
	}
	defer w.mu.Unlock()
	if w.closed {
		w.stopped.Add(1)
		return
	}
	select {
	case w.queue <- copyLine{path, line, rotate}:
		w.accepted.Add(1)
	default:
		w.full.Add(1)
	}
}

// Stop closes admission. Only the writer closes files; an in-flight syscall is never detached or cancelled.
func (w *Writer) Stop() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
}

func (w *Writer) Done() <-chan struct{} { return w.done }

func (w *Writer) Close(ctx context.Context) error {
	w.Stop()
	select {
	case <-w.done:
		return nil
	default:
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.lastDrainError.Store(ctx.Err().Error())
		return fmt.Errorf("native diagnostic archive did not drain: %w", ctx.Err())
	}
}

func (w *Writer) Status() Status {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	drained := false
	select {
	case <-w.done:
		drained = true
	default:
	}
	return Status{w.accepted.Load(), w.written.Load(), w.full.Load(), w.busy.Load(), w.oversized.Load(),
		w.stopped.Load(), w.writeErrors.Load(), len(w.queue), Capacity, closed, drained,
		w.lastWriteError.Load().(string), w.lastDrainError.Load().(string)}
}

func (w *Writer) run() {
	defer close(w.done)
	var lastPath string
	var lastRotate bool
	var reported uint64
	report := func(path string, rotate bool) {
		full, busy, oversized, stopped := w.full.Load(), w.busy.Load(), w.oversized.Load(), w.stopped.Load()
		total := full + busy + oversized + stopped
		if total == reported || path == "" {
			return
		}
		marker := fmt.Sprintf("DIAGNOSTIC COPY LOSS: %d aggregate records omitted (full=%d busy=%d oversize=%d closed=%d)", total, full, busy, oversized, stopped)
		if err := writeCopy(path, marker, rotate); err != nil {
			w.recordError(err)
		} else {
			reported = total
		}
	}
	for record := range w.queue {
		report(record.path, record.rotate)
		if err := writeCopy(record.path, record.line, record.rotate); err != nil {
			w.recordError(err)
		} else {
			w.written.Add(1)
		}
		lastPath, lastRotate = record.path, record.rotate
	}
	// Best effort for the final known archive path. Later closed-admission losses can be counters only.
	report(lastPath, lastRotate)
}

func (w *Writer) recordError(err error) {
	w.writeErrors.Add(1)
	w.lastWriteError.Store(err.Error())
}

func writeCopy(path, line string, rotate bool) error {
	if rotate {
		info, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Size() > 2<<20 {
			if err = os.Rename(path, path+".previous"); err != nil {
				return err
			}
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, writeErr := file.WriteString(line + "\n")
	if writeErr == nil && n != len(line)+1 {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Close())
}
