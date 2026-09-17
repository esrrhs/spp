package proxy

import (
	"sync"
	"time"
)

// msgChannel is a close-safe buffered channel for ProxyFrame.
// Sends hold RLock only for a non-blocking attempt so Close (Lock) cannot
// race with a send on the underlying channel.
type msgChannel struct {
	mu     sync.RWMutex
	ch     chan interface{}
	closed bool
}

func newMsgChannel(n int) *msgChannel {
	if n < 1 {
		n = 1
	}
	return &msgChannel{ch: make(chan interface{}, n)}
}

func (c *msgChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.ch)
}

func (c *msgChannel) Write(v interface{}) {
	for {
		c.mu.RLock()
		if c.closed {
			c.mu.RUnlock()
			return
		}
		select {
		case c.ch <- v:
			c.mu.RUnlock()
			return
		default:
			c.mu.RUnlock()
			time.Sleep(time.Millisecond)
		}
	}
}

func (c *msgChannel) WriteTimeout(v interface{}, timeoutms int) bool {
	if timeoutms <= 0 {
		timeoutms = 1
	}
	deadline := time.Now().Add(time.Duration(timeoutms) * time.Millisecond)
	for {
		c.mu.RLock()
		if c.closed {
			c.mu.RUnlock()
			return true
		}
		select {
		case c.ch <- v:
			c.mu.RUnlock()
			return true
		default:
			c.mu.RUnlock()
			if !time.Now().Before(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func (c *msgChannel) Ch() <-chan interface{} {
	return c.ch
}
