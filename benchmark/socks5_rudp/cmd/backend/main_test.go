package main

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// startBackend runs runBackend in the background and returns its address.
func startBackend(t *testing.T, mode string, bs, pageSize int) (string, func()) {
	t.Helper()
	stop := make(chan struct{})
	addrc := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		addr, err := runBackend("127.0.0.1:0", mode, bs, pageSize, stop, func(a net.Addr) { addrc <- a })
		if addr != nil {
			addrc <- addr
		}
		errc <- err
	}()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case err := <-errc:
		t.Fatalf("runBackend %s exited early: %v", mode, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("runBackend %s did not start", mode)
	}
	return addr.String(), func() {
		close(stop)
		if err := <-errc; err != nil {
			t.Errorf("runBackend %s returned error: %v", mode, err)
		}
	}
}

func TestRunBackend_Modes(t *testing.T) {
	t.Run("echo", func(t *testing.T) {
		addr, stop := startBackend(t, "echo", 4096, 4096)
		defer stop()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		msg := []byte("backend-echo-roundtrip")
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != string(msg) {
			t.Fatalf("echo mismatch %q", got)
		}
	})

	t.Run("source", func(t *testing.T) {
		addr, stop := startBackend(t, "source", 4096, 4096)
		defer stop()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		got := make([]byte, 128)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		want := make([]byte, 128)
		fillPattern(want, 0)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("source pattern mismatch at %d: %d != %d", i, got[i], want[i])
			}
		}
	})

	t.Run("sink", func(t *testing.T) {
		addr, stop := startBackend(t, "sink", 4096, 4096)
		defer stop()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 4096)
		fillPattern(payload, 7)
		if _, err := c.Write(payload); err != nil {
			t.Fatal(err)
		}
		c.Close()
	})

	t.Run("page", func(t *testing.T) {
		addr, stop := startBackend(t, "page", 4096, 4096)
		defer stop()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		req := make([]byte, 256)
		fillPattern(req, 0xA1)
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, uint32(len(req)))
		if _, err := c.Write(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(req); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(c, hdr); err != nil {
			t.Fatal(err)
		}
		bodyLen := binary.BigEndian.Uint32(hdr)
		if bodyLen != 4096 {
			t.Fatalf("page body len %d want 4096", bodyLen)
		}
		var sum [32]byte
		if _, err := io.ReadFull(c, sum[:]); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(c, body); err != nil {
			t.Fatal(err)
		}
		wantBody := make([]byte, 4096)
		fillPattern(wantBody, 0x5A)
		if sha256.Sum256(body) != sha256.Sum256(wantBody) {
			t.Fatal("page body sha mismatch")
		}
	})

	t.Run("page oversized request rejected", func(t *testing.T) {
		addr, stop := startBackend(t, "page", 4096, 4096)
		defer stop()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, 1<<21) // > 1MB limit
		if _, err := c.Write(hdr); err != nil {
			t.Fatal(err)
		}
		// The backend rejects without replying: read hits EOF after close.
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(c, hdr); err == nil {
			t.Fatal("expected EOF for oversized page request")
		}
	})
}

func TestRunBackend_ListenError(t *testing.T) {
	if _, err := runBackend("no-such-host-resolves-nowhere:1", "sink", 64, 64, make(chan struct{}), nil); err == nil {
		t.Fatal("expected listen error for invalid addr")
	}
}

func TestRunBackend_PageShortReadsAndTicker(t *testing.T) {
	addr, stop := startBackend(t, "page", 1024, 4096)
	defer stop()

	// Header shorter than 4 bytes: ReadFull fails and the handler returns.
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// Declared body longer than the bytes that actually arrive.
	c, err = net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, 64)
	if _, err := c.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// Keep the process alive across one stats-ticker interval.
	time.Sleep(2100 * time.Millisecond)
}

func TestFillPattern(t *testing.T) {
	b := make([]byte, 300)
	fillPattern(b, 3)
	for i := range b {
		if b[i] != byte(i)+3 {
			t.Fatalf("fillPattern mismatch at %d", i)
		}
	}
}
