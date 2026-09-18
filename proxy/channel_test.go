package proxy

import (
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
