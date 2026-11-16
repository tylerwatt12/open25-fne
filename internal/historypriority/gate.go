package historypriority

import (
	"sync"
	"time"
)

// Turn owns only host scheduling, never history data or radio policy. Release
// follows the durable result: retain exclusivity while the storage gets a quiet
// interval, then give another waiting worker a chance before taking a new turn.
type Turn struct {
	release    func() error
	foreground bool
	once       sync.Once
	err        error
}

func (t *Turn) Release() error {
	if t == nil || t.release == nil {
		return nil
	}
	t.once.Do(func() {
		if t.foreground {
			t.err = t.release()
			return
		}
		time.Sleep(250 * time.Millisecond)
		t.err = t.release()
		// Polling flock is not FIFO. A previous owner yields beyond a polling
		// interval so it cannot immediately retake its own released turn.
		time.Sleep(50 * time.Millisecond)
	})
	return t.err
}
