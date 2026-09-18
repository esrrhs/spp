package proxy

import (
	"sync/atomic"
	"time"
)

// msgChannel is a close-safe buffered channel for ProxyFrame.
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
