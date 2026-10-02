package win32

import (
	"sync"
	"testing"
	"time"
)

func TestWorkRunsInOrder(t *testing.T) {
	var q workQueue
	var order []int
	for i := range 5 {
		q.add(func() { order = append(order, i) })
	}
	q.drain()

	for i, got := range order {
		if got != i {
			t.Fatalf("order = %v, want it in the order it was queued", order)
		}
	}
	if len(order) != 5 {
		t.Errorf("ran %d of 5", len(order))
	}
}

// The hazard this file exists for. A tray menu item opens an overlay; both
// touch windows, so both run on the UI thread, so the first posts the second.
// Draining under the lock would have the thread wait on itself, and the symptom
// would be a hang with no message on a machine this code was not written on.
func TestWorkThatQueuesMoreWorkDoesNotDeadlock(t *testing.T) {
	var q workQueue
	done := make(chan struct{})

	q.add(func() {
		q.add(func() {
			q.add(func() { close(done) })
		})
	})

	finished := make(chan struct{})
	go func() {
		q.drain()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("drain deadlocked on work that queued more work")
	}

	select {
	case <-done:
	default:
		t.Error("the nested work never ran, so it would have waited for another message")
	}
	if q.waiting() != 0 {
		t.Errorf("%d items left queued after drain", q.waiting())
	}
}

func TestDrainingAnEmptyQueueIsFine(t *testing.T) {
	var q workQueue
	q.drain()
	if q.waiting() != 0 {
		t.Errorf("waiting = %d", q.waiting())
	}
}

// Post is called from the hot key goroutine, the daemon supervisor and the
// stream, so adding has to be safe from all of them at once.
func TestAddingFromManyGoroutines(t *testing.T) {
	var q workQueue
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.add(func() {})
		}()
	}
	wg.Wait()

	if q.waiting() != 50 {
		t.Errorf("queued %d of 50", q.waiting())
	}
	q.drain()
	if q.waiting() != 0 {
		t.Errorf("%d left after drain", q.waiting())
	}
}
