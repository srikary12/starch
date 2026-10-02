package win32

import "sync"

// The UI thread's work queue, kept in an untagged file so it is tested on the
// machine this is written on.
//
// It is small, and it is the part of the message loop that can deadlock: work
// running on the UI thread routinely posts more work, and draining under the
// lock would have the thread wait on itself. That is a bug with no symptom
// until someone opens an overlay from a tray menu on a real Windows desktop, at
// which point it is a hang with no message. So it lives here, with a test.

type workQueue struct {
	mu      sync.Mutex
	pending []func()
}

// add queues fn. Safe from any goroutine.
func (q *workQueue) add(fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, fn)
}

// drain runs everything queued, in order, until nothing is left.
//
// The lock is released around each call, so work that queues more work is run
// in this same pass rather than deadlocking or waiting for another message.
func (q *workQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()

		fn()
	}
}

// waiting reports how much work is queued.
func (q *workQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
