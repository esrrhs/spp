package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// msgChannel is a close-safe buffered FIFO for sonny ProxyFrame traffic.
// Close signals via done (waking blocked writers) and does not close the data
// channel, so send and close never race. Receivers must also select on Done().
type msgChannel struct {
	ch     chan interface{}
	done   chan struct{}
	closed uint32
}

func newMsgChannel(n int) *msgChannel {
	if n < 1 {
		n = 1
	}
	return &msgChannel{
		ch:   make(chan interface{}, n),
		done: make(chan struct{}),
	}
}

func (c *msgChannel) Close() {
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		close(c.done)
	}
}

func (c *msgChannel) Done() <-chan struct{} {
	return c.done
}

func (c *msgChannel) Write(v interface{}) {
	if atomic.LoadUint32(&c.closed) != 0 {
		return
	}
	select {
	case c.ch <- v:
	case <-c.done:
	}
}

func (c *msgChannel) WriteTimeout(v interface{}, timeoutms int) bool {
	if timeoutms <= 0 {
		timeoutms = 1
	}
	if atomic.LoadUint32(&c.closed) != 0 {
		return true
	}
	select {
	case c.ch <- v:
		return true
	case <-c.done:
		return true
	case <-time.After(time.Duration(timeoutms) * time.Millisecond):
		return false
	}
}

func (c *msgChannel) Ch() <-chan interface{} {
	return c.ch
}

// TryReadWait pops without blocking: ok=false when empty.
func (c *msgChannel) TryReadWait() (v interface{}, ok bool) {
	select {
	case v = <-c.ch:
		return v, true
	default:
		return nil, false
	}
}

// ReadWait pops the next frame from the channel, prioritizing buffered frames
// even after Close/Done has been signaled so no trailing bytes are dropped.
func (c *msgChannel) ReadWait() (v interface{}, ok bool) {
	select {
	case v = <-c.ch:
		return v, true
	default:
		select {
		case v = <-c.ch:
			return v, true
		case <-c.done:
			select {
			case v = <-c.ch:
				return v, true
			default:
				return nil, false
			}
		}
	}
}

// framePrio is send/recv priority on the single main-channel queue.
// Lower value = higher priority (control jumps ahead of data).
type framePrio int

const (
	prioControl framePrio = iota // OPEN/CLOSE/LOGIN/...
	prioInter                    // initial interactive DATA
	prioBulk                     // bulk DATA
	prioCount
)

// prioQueue is a single bounded queue with priority pop (插队).
// One shared capacity matches the single underlying main pipe (rudp/ricmp/...).
type prioQueue struct {
	mu sync.Mutex
	// popCond wakes the consumer; pushCond[p] wakes one blocked pusher of
	// priority p. Separate conds let PopWait hand each freed slot to exactly
	// one waiter per priority instead of Broadcast, which used to wake every
	// blocked pusher on every pop — a thundering herd on the client, where
	// every sonny's copySonnyRecv pushes into the one shared main-channel
	// queue and stays blocked whenever it fills up under backpressure.
	popCond  *sync.Cond
	pushCond [prioCount]*sync.Cond
	cap      int
	size     int
	closed   bool
	done     chan struct{}
	q        [prioCount][]interface{}
	// head is the consume position per priority. Popping used to reslice
	// (q[i] = q[i][1:]), which leaves the backing array's capacity behind at
	// the new start: the very next append then reallocates and copies the
	// whole queue. Advancing head instead keeps the array and makes the
	// steady state allocation-free. When head grows large or the queue drains,
	// it rewinds/compacts so the backing array never grows unbounded.
	head [prioCount]int
}

func newPrioQueue(n int) *prioQueue {
	if n < 1 {
		n = 1
	}
	pq := &prioQueue{
		cap:  n,
		done: make(chan struct{}),
	}
	pq.popCond = sync.NewCond(&pq.mu)
	for i := range pq.pushCond {
		pq.pushCond[i] = sync.NewCond(&pq.mu)
	}
	return pq
}

// broadcastAllLocked wakes every waiter. All conds share q.mu, so callers
// must hold it.
func (q *prioQueue) broadcastAllLocked() {
	q.popCond.Broadcast()
	for i := range q.pushCond {
		q.pushCond[i].Broadcast()
	}
}

func (q *prioQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.done)
	q.broadcastAllLocked()
}

func (q *prioQueue) Done() <-chan struct{} {
	return q.done
}

func (q *prioQueue) Wake() {
	q.mu.Lock()
	q.broadcastAllLocked()
	q.mu.Unlock()
}

func (q *prioQueue) limitForPrio(p framePrio) int {
	if q.cap <= 1 {
		return q.cap
	}
	switch p {
	case prioControl:
		return q.cap + 256
	case prioInter:
		return q.cap + 64
	default:
		return q.cap
	}
}

func (q *prioQueue) compactLocked(p framePrio) {
	if q.head[p] == 0 {
		return
	}
	n := copy(q.q[p], q.q[p][q.head[p]:])
	for j := n; j < len(q.q[p]); j++ {
		q.q[p][j] = nil
	}
	q.q[p] = q.q[p][:n]
	q.head[p] = 0
}

func (q *prioQueue) pushItemLocked(v interface{}, p framePrio) {
	if len(q.q[p]) == cap(q.q[p]) && q.head[p] > 0 {
		q.compactLocked(p)
	}
	q.q[p] = append(q.q[p], v)
	q.size++
	q.popCond.Signal()
}

func (q *prioQueue) Push(v interface{}, p framePrio) {
	if p < 0 || p >= prioCount {
		p = prioBulk
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && q.size >= q.limitForPrio(p) {
		q.pushCond[p].Wait()
	}
	if q.closed {
		return
	}
	q.pushItemLocked(v, p)
}

func (q *prioQueue) PushTimeout(v interface{}, p framePrio, timeoutms int) bool {
	if timeoutms <= 0 {
		timeoutms = 1
	}
	if p < 0 || p >= prioCount {
		p = prioBulk
	}
	deadline := time.Now().Add(time.Duration(timeoutms) * time.Millisecond)
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && q.size >= q.limitForPrio(p) {
		if time.Now().After(deadline) {
			return false
		}
		// Wake periodically to re-check deadline.
		timeout := time.Until(deadline)
		timer := time.AfterFunc(timeout, func() { q.broadcastAll() })
		q.pushCond[p].Wait()
		timer.Stop()
	}
	if q.closed {
		return true
	}
	q.pushItemLocked(v, p)
	return true
}

// broadcastAll wakes every waiter without holding the lock.
func (q *prioQueue) broadcastAll() {
	q.mu.Lock()
	q.broadcastAllLocked()
	q.mu.Unlock()
}

// popOneLocked removes and returns the highest-priority frame. Caller holds
// q.mu and has verified q.size > 0.
func (q *prioQueue) popOneLocked() interface{} {
	for i := framePrio(0); i < prioCount; i++ {
		if q.head[i] >= len(q.q[i]) {
			continue
		}
		v := q.q[i][q.head[i]]
		q.q[i][q.head[i]] = nil // drop the reference so the frame can be collected
		q.head[i]++
		if q.head[i] == len(q.q[i]) {
			// Drained: rewind so the backing array keeps its capacity.
			q.q[i] = q.q[i][:0]
			q.head[i] = 0
		} else if q.head[i] >= 64 && q.head[i] >= len(q.q[i])/2 {
			// Compact partially drained queue to prevent unbounded backing array growth
			// during continuous streaming where the queue never fully drains.
			q.compactLocked(i)
		}
		q.size--
		// Hand the freed capacity to at most one blocked pusher per priority:
		// Signal is a no-op when nobody of that priority waits, so this stays
		// cheap in the steady state and never wakes the whole herd.
		for p := framePrio(0); p < prioCount; p++ {
			if q.size < q.limitForPrio(p) {
				q.pushCond[p].Signal()
			}
		}
		return v
	}
	return nil
}

// PopWait pops the highest-priority frame.
// ok=true with non-nil v on success; closed=true if queue was closed empty;
// both false means timeout with empty queue.
func (q *prioQueue) PopWait(timeout time.Duration) (v interface{}, closed bool, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if timeout <= 0 {
		for q.size == 0 && !q.closed {
			q.popCond.Wait()
		}
	} else {
		deadline := time.Now().Add(timeout)
		var timer *time.Timer
		for q.size == 0 && !q.closed {
			left := time.Until(deadline)
			if left <= 0 {
				break
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(left, func() { q.popCond.Broadcast() })
			q.popCond.Wait()
		}
		if timer != nil {
			timer.Stop()
		}
	}

	if q.size == 0 {
		return nil, q.closed, false
	}
	return q.popOneLocked(), false, true
}

// TryPop pops without blocking: ok=false when the queue is empty. Used by the
// send loop to top up a batch of frames for one Write without stalling on the
// first empty moment.
func (q *prioQueue) TryPop() (v interface{}, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == 0 {
		return nil, false
	}
	return q.popOneLocked(), true
}
