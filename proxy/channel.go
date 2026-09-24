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
	mu     sync.Mutex
	cond   *sync.Cond
	cap    int
	size   int
	closed bool
	done   chan struct{}
	q      [prioCount][]interface{}
}

func newPrioQueue(n int) *prioQueue {
	if n < 1 {
		n = 1
	}
	pq := &prioQueue{
		cap:  n,
		done: make(chan struct{}),
	}
	pq.cond = sync.NewCond(&pq.mu)
	return pq
}

func (q *prioQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.done)
	q.cond.Broadcast()
}

func (q *prioQueue) Done() <-chan struct{} {
	return q.done
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

func (q *prioQueue) Push(v interface{}, p framePrio) {
	if p < 0 || p >= prioCount {
		p = prioBulk
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	limit := q.limitForPrio(p)
	for !q.closed && q.size >= limit {
		q.cond.Wait()
	}
	if q.closed {
		return
	}
	q.q[p] = append(q.q[p], v)
	q.size++
	q.cond.Signal()
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
	limit := q.limitForPrio(p)
	for !q.closed && q.size >= limit {
		if time.Now().After(deadline) {
			return false
		}
		// Wake periodically to re-check deadline.
		timeout := time.Until(deadline)
		timer := time.AfterFunc(timeout, func() { q.cond.Broadcast() })
		q.cond.Wait()
		timer.Stop()
	}
	if q.closed {
		return true
	}
	q.q[p] = append(q.q[p], v)
	q.size++
	q.cond.Signal()
	return true
}

// PopWait pops the highest-priority frame.
// ok=true with non-nil v on success; closed=true if queue was closed empty;
// both false means timeout with empty queue.
func (q *prioQueue) PopWait(timeout time.Duration) (v interface{}, closed bool, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if timeout <= 0 {
		for q.size == 0 && !q.closed {
			q.cond.Wait()
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
			timer = time.AfterFunc(left, func() { q.cond.Broadcast() })
			q.cond.Wait()
		}
		if timer != nil {
			timer.Stop()
		}
	}

	if q.size == 0 {
		return nil, q.closed, false
	}
	for i := framePrio(0); i < prioCount; i++ {
		if len(q.q[i]) == 0 {
			continue
		}
		v = q.q[i][0]
		q.q[i] = q.q[i][1:]
		q.size--
		q.cond.Broadcast()
		return v, false, true
	}
	return nil, q.closed, false
}
