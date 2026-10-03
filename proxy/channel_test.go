package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrioQueueControlJumpsAhead(t *testing.T) {
	q := newPrioQueue(8)
	q.Push("bulk1", prioBulk)
	q.Push("bulk2", prioBulk)
	q.Push("inter", prioInter)
	q.Push("ctrl", prioControl)

	want := []string{"ctrl", "inter", "bulk1", "bulk2"}
	for i, w := range want {
		v, closed, ok := q.PopWait(time.Second)
		if !ok || closed {
			t.Fatalf("pop %d: ok=%v closed=%v", i, ok, closed)
		}
		if v.(string) != w {
			t.Fatalf("pop %d: got %v want %v", i, v, w)
		}
	}
}

func TestPrioQueueCloseUnblocksPush(t *testing.T) {
	q := newPrioQueue(1)
	q.Push("a", prioBulk)

	done := make(chan struct{})
	go func() {
		q.Push("b", prioControl) // blocks until Close or Pop
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	q.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Push not unblocked by Close")
	}
}

func TestNewQueues_MinSize(t *testing.T) {
	// Non-positive sizes must still yield usable 1-slot queues.
	mc := newMsgChannel(0)
	mc.Write("x")
	if v := <-mc.Ch(); v != "x" {
		t.Fatalf("msgChannel got %v", v)
	}

	pq := newPrioQueue(-3)
	pq.Push("y", prioBulk)
	v, closed, ok := pq.PopWait(time.Second)
	if !ok || closed || v != "y" {
		t.Fatalf("prioQueue got %v closed=%v ok=%v", v, closed, ok)
	}
}

func TestMsgChannel_FIFOAndClose(t *testing.T) {
	c := newMsgChannel(4)
	for i := 0; i < 4; i++ {
		c.Write(i)
	}
	for i := 0; i < 4; i++ {
		v := <-c.Ch()
		if v != i {
			t.Fatalf("FIFO broken at %d: got %v", i, v)
		}
	}

	// Close is idempotent and never panics under concurrent writers.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Write("late") // must not block/panic after close
			c.Close()
		}()
	}
	c.Close()
	wg.Wait()

	select {
	case <-c.Done():
	default:
		t.Fatal("done channel not closed")
	}
}

func TestMsgChannel_WriteTimeout(t *testing.T) {
	// Full queue: WriteTimeout reports false after the deadline.
	full := newMsgChannel(1)
	full.Write("a")
	if full.WriteTimeout("b", 20) {
		t.Fatal("WriteTimeout should return false on a full queue")
	}

	// Non-positive timeout clamps to ~1ms, still false when full.
	if full.WriteTimeout("b", 0) {
		t.Fatal("WriteTimeout(0) on full queue should be false")
	}

	// Drain one slot: next write succeeds.
	<-full.Ch()
	if !full.WriteTimeout("b", 1000) {
		t.Fatal("WriteTimeout should succeed after drain")
	}

	// Closed queue treats pending writers as released (true = don't kill conn).
	blocked := newMsgChannel(1)
	blocked.Write("a")
	done := make(chan bool, 1)
	go func() { done <- blocked.WriteTimeout("b", 30000) }()
	time.Sleep(20 * time.Millisecond)
	blocked.Close()
	select {
	case ret := <-done:
		if !ret {
			t.Fatal("WriteTimeout must return true when closed while blocked")
		}
	case <-time.After(time.Second):
		t.Fatal("WriteTimeout not unblocked by Close")
	}
}

func TestMsgChannel_WriteAfterClosedReturnsImmediately(t *testing.T) {
	c := newMsgChannel(1)
	c.Close()
	done := make(chan struct{})
	go func() { c.Write("x"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write on closed channel blocked")
	}
}

func TestPrioQueue_PopWaitEmptyAndClosed(t *testing.T) {
	q := newPrioQueue(4)

	// Timeout on empty queue: nothing, not closed, not ok.
	_, closed, ok := q.PopWait(10 * time.Millisecond)
	if ok || closed {
		t.Fatalf("empty pop: ok=%v closed=%v", ok, closed)
	}

	// Closed + empty: reports closed.
	q.Close()
	_, closed, ok = q.PopWait(time.Second)
	if ok || !closed {
		t.Fatalf("closed empty pop: ok=%v closed=%v", ok, closed)
	}
}

func TestPrioQueue_InvalidPrioCoercedToBulk(t *testing.T) {
	q := newPrioQueue(8)
	q.Push("x", framePrio(99))
	q.Push("y", -1)
	v, _, ok := q.PopWait(time.Second)
	if !ok || v != "x" {
		t.Fatalf("invalid-prio frame lost or reordered: %v", v)
	}
	v, _, _ = q.PopWait(time.Second)
	if v != "y" {
		t.Fatalf("second frame lost: %v", v)
	}
}

func TestPrioQueue_ControlBypassesBulkCap(t *testing.T) {
	// cap<=1 uses the cap itself for every prio; cap>1 grants headroom to
	// control/inter so control can never be stalled by a bulk backlog.
	q := newPrioQueue(1)
	q.Push("bulk", prioBulk)
	if q.PushTimeout("ctrl-over-cap1", prioControl, 20) {
		t.Fatal("cap=1 queue must not accept a second frame of any prio")
	}

	q = newPrioQueue(2)
	q.Push("b1", prioBulk)
	q.Push("b2", prioBulk)
	// Bulk is now at its limit, but control/inter have headroom.
	if !q.PushTimeout("ctrl", prioControl, 100) {
		t.Fatal("control frame should bypass the bulk cap")
	}
	if !q.PushTimeout("inter", prioInter, 100) {
		t.Fatal("interactive frame should get headroom over the bulk cap")
	}
	if q.PushTimeout("b3", prioBulk, 20) {
		t.Fatal("bulk frame must still respect its cap")
	}

	want := []string{"ctrl", "inter", "b1", "b2"}
	for _, w := range want {
		v, _, ok := q.PopWait(time.Second)
		if !ok || v != w {
			t.Fatalf("got %v want %s", v, w)
		}
	}
}

func TestPrioQueue_PushTimeoutClosed(t *testing.T) {
	q := newPrioQueue(1)
	q.Push("a", prioBulk)
	done := make(chan bool, 1)
	go func() { done <- q.PushTimeout("b", prioBulk, 30000) }()
	time.Sleep(20 * time.Millisecond)
	q.Close()
	select {
	case ret := <-done:
		if !ret {
			t.Fatal("PushTimeout should return true when queue is closed")
		}
	case <-time.After(time.Second):
		t.Fatal("PushTimeout not released by Close")
	}
}

func TestPrioQueue_CloseIdempotent(t *testing.T) {
	q := newPrioQueue(4)
	q.Close()
	q.Close() // must not panic on close(done) twice

	// Done is closed by Close.
	select {
	case <-q.Done():
	default:
		t.Fatal("prioQueue Done should be closed")
	}
}

func TestMsgChannel_WriteTimeoutOnClosedReturnsTrue(t *testing.T) {
	c := newMsgChannel(1)
	c.Close()
	if !c.WriteTimeout("x", 10) {
		t.Fatal("WriteTimeout on an already-closed channel must return true")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("msgChannel Done should be closed")
	}
}

func TestPrioQueue_PushTimeoutInvalidPrioAndBlockingPop(t *testing.T) {
	// Out-of-range priorities are coerced to bulk (covers PushTimeout clamp).
	q := newPrioQueue(8)
	if !q.PushTimeout("x", framePrio(42), 1000) {
		t.Fatal("PushTimeout should accept a frame")
	}
	v, _, ok := q.PopWait(time.Second)
	if !ok || v != "x" {
		t.Fatalf("invalid-prio PushTimeout frame lost: %v", v)
	}

	// PopWait with a zero timeout blocks on an empty queue until Push.
	q2 := newPrioQueue(8)
	got := make(chan interface{}, 1)
	go func() {
		v, _, _ := q2.PopWait(0)
		got <- v
	}()
	select {
	case <-got:
		t.Fatal("PopWait(0) should block on empty queue")
	case <-time.After(30 * time.Millisecond):
	}
	q2.Push("y", prioBulk)
	select {
	case v := <-got:
		if v != "y" {
			t.Fatalf("blocking pop got %v want y", v)
		}
	case <-time.After(time.Second):
		t.Fatal("PopWait(0) did not wake on Push")
	}

	// PopWait(0) also wakes when the queue is closed while empty.
	q3 := newPrioQueue(1)
	closed := make(chan bool, 1)
	go func() {
		_, isClosed, ok := q3.PopWait(0)
		closed <- isClosed && !ok
	}()
	time.Sleep(30 * time.Millisecond)
	q3.Close()
	select {
	case isClosed := <-closed:
		if !isClosed {
			t.Fatal("blocking PopWait should report closed")
		}
	case <-time.After(time.Second):
		t.Fatal("PopWait(0) not released by Close")
	}
}

func TestPrioQueue_ConcurrentStress(t *testing.T) {
	q := newPrioQueue(64)
	const producers = 8
	const perProducer = 500
	const total = producers * perProducer
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				q.Push(struct{ p, i int }{id, i}, framePrio(i%int(prioCount)))
			}
		}(p)
	}

	var consumed atomic.Int64
	stop := make(chan struct{})
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				v, closed, ok := q.PopWait(10 * time.Millisecond)
				if ok {
					_ = v
					consumed.Add(1)
					continue
				}
				if closed {
					return
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}

	// Drain until every item has been produced and consumed.
	deadline := time.Now().Add(10 * time.Second)
	for consumed.Load() < total && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	// Drain anything still buffered before closing the queue.
	for {
		v, _, ok := q.PopWait(20 * time.Millisecond)
		if !ok {
			break
		}
		_ = v
		consumed.Add(1)
	}
	q.Close()
	wg.Wait()
	if got := consumed.Load(); got < total {
		t.Fatalf("consumed %d want %d (lost frames under concurrency)", got, total)
	}
}
