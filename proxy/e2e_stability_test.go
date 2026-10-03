package proxy

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// isResourceExhaustedErr reports a client-side dial failure caused by the
// local host running out of ephemeral ports / socket resources. On macOS
// loopback, thousands of short-lived conns back-to-back trigger
// EADDRNOTAVAIL ("can't assign requested address") because active-connect
// sockets cannot reuse peers in TIME_WAIT. This is an environment limit,
// not a proxy integrity failure, so it must never fail the test outright.
func isResourceExhaustedErr(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EADDRNOTAVAIL, syscall.EADDRINUSE,
			syscall.EAGAIN, syscall.EMFILE, syscall.ENFILE:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't assign requested address") ||
		strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "too many open files")
}

// dialResourceRetry dials addr, backing off when the local socket/ephemeral
// pool is exhausted. exhausted=true means every attempt failed solely for
// resource reasons (the test should skip, not fail). Any other dial error is
// returned verbatim.
func dialResourceRetry(addr string, timeout time.Duration) (conn net.Conn, exhausted bool, err error) {
	deadline := time.Now().Add(timeout)
	var last error
	for attempt := 0; ; attempt++ {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr == nil {
			return c, false, nil
		}
		last = derr
		if !isResourceExhaustedErr(derr) {
			return nil, false, last
		}
		if time.Now().After(deadline) {
			return nil, true, nil
		}
		// Linear 50ms..1s backoff; pausing all churners lets TIME_WAIT recycle.
		wait := time.Duration(50*(attempt+1)) * time.Millisecond
		if wait > time.Second {
			wait = time.Second
		}
		time.Sleep(wait)
	}
}

func TestE2E_UnderlayMatrix_ForwardEcho(t *testing.T) {
	for _, proto := range matrixUnderlaysCore {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			h := startForwardProxy(t, proto, "matrix-echo-secret-"+proto+"-ok")
			defer h.Close()

			conn := h.Dial(5 * time.Second)
			defer conn.Close()

			msg := []byte("hello underlay " + proto)
			// 20s deadline: under -race or noisy CI runners the QUIC
			// handshake/first-data nudge can starve for several seconds; a
			// tiny echo must still fail fast on a genuinely broken path.
			echoRoundTrip(t, conn, msg, 20*time.Second)
		})
	}
}

// Mixed large/small chunks on one stream: catches recv-side priority reordering
// (small DATA jumping ahead of bulk of the same sonny → sendToSonny index error).
func TestE2E_MixedChunkIntegrity(t *testing.T) {
	for _, proto := range []string{"tcp", "rudp"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			h := startForwardProxy(t, proto, "mixed-chunk-secret-"+proto+"-ok")
			defer h.Close()

			// Background bulk to build main-channel queue pressure.
			stop := make(chan struct{})
			var bgErr atomic.Value
			go func() {
				conn, err := net.DialTimeout("tcp", h.clientAddr, 3*time.Second)
				if err != nil {
					bgErr.Store(err)
					return
				}
				defer conn.Close()
				chunk := make([]byte, 32*1024)
				fillPattern(chunk, 7)
				sink := make([]byte, 32*1024)
				go func() {
					for {
						_, err := conn.Read(sink)
						if err != nil {
							return
						}
					}
				}()
				for {
					select {
					case <-stop:
						return
					default:
						if _, err := conn.Write(chunk); err != nil {
							return
						}
						time.Sleep(2 * time.Millisecond)
					}
				}
			}()
			defer close(stop)
			time.Sleep(200 * time.Millisecond)

			conn, dialExhausted, dialErr := dialResourceRetry(h.clientAddr, 30*time.Second)
			if dialErr != nil {
				if dialExhausted {
					t.Skip("local resource exhaustion before mixed-chunk dial")
				}
				t.Fatalf("mixed-chunk dial: %v", dialErr)
			}
			defer conn.Close()

			// Alternating sizes force ≤4096 trailing/interleaved frames after
			// MAX_CHUNK_SIZE framing on the proxy path.
			var payload []byte
			for i := 0; i < 8; i++ {
				large := make([]byte, 64*1024+512) // → ~64KB chunk + small remainder
				fillPattern(large, byte(i+1))
				payload = append(payload, large...)
				small := make([]byte, 1024)
				fillPattern(small, byte(100+i))
				payload = append(payload, small...)
			}

			echoAndHash(t, conn, payload, 60*time.Second)

			if v := bgErr.Load(); v != nil {
				t.Logf("background dial note: %v", v)
			}
		})
	}
}

// Short-connection churn with byte-exact verify — CI gate for "stable proxy".
func TestE2E_ShortConnChurnIntegrity(t *testing.T) {
	h := startForwardProxy(t, "tcp", "short-churn-secret-ok-123")
	defer h.Close()

	const (
		workers  = 32
		duration = 8 * time.Second
		msgSize  = 4096
	)

	deadline := time.Now().Add(duration)
	var okN, failN, exhaustedN atomic.Int64
	var wg sync.WaitGroup
	errCh := make(chan string, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := make([]byte, msgSize)
			fillPattern(payload, byte(id))
			want := sha256.Sum256(payload)
			for time.Now().Before(deadline) {
				conn, exhausted, err := dialResourceRetry(h.clientAddr, 30*time.Second)
				if err != nil {
					if exhausted {
						exhaustedN.Add(1)
						return // host cannot assess integrity right now; gate below
					}
					failN.Add(1)
					select {
					case errCh <- fmt.Sprintf("dial: %v", err):
					default:
					}
					time.Sleep(20 * time.Millisecond)
					continue
				}
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, werr := conn.Write(payload)
				got := make([]byte, msgSize)
				_, rerr := io.ReadFull(conn, got)
				conn.Close()
				if werr != nil || rerr != nil || sha256.Sum256(got) != want {
					failN.Add(1)
					select {
					case errCh <- fmt.Sprintf("w=%d write=%v read=%v", id, werr, rerr):
					default:
					}
					continue
				}
				okN.Add(1)
			}
		}(w)
	}
	wg.Wait()

	ok, fail, exhausted := okN.Load(), failN.Load(), exhaustedN.Load()
	t.Logf("short churn: ok=%d fail=%d resourceExhausted=%d", ok, fail, exhausted)
	if exhausted > 0 && ok < 50 {
		t.Skipf("local ephemeral port/socket exhaustion prevented the churn run: ok=%d exhausted=%d", ok, exhausted)
	}
	if ok < 50 {
		t.Fatalf("too few successful short conns: ok=%d", ok)
	}
	// Resource-exhaustion dials are environmental; only real dial/integrity
	// failures count toward the 5% gate.
	if fail*20 > ok { // >5% failure
		msg := "unknown"
		select {
		case msg = <-errCh:
		default:
		}
		t.Fatalf("short conn failure rate too high: ok=%d fail=%d sample=%s", ok, fail, msg)
	}
}

// Concurrent bulk + interactive on same tunnel; assert interactive echoes are intact
// (primary correctness gate under load — not a latency SLO).
func TestE2E_ConcurrentBulkAndInteractiveIntegrity(t *testing.T) {
	h := startForwardProxy(t, "tcp", "concurrent-integrity-secret")
	defer h.Close()

	stop := make(chan struct{})
	go func() {
		conn, err := net.DialTimeout("tcp", h.clientAddr, 3*time.Second)
		if err != nil {
			return
		}
		defer conn.Close()
		chunk := make([]byte, 16*1024)
		fillPattern(chunk, 9)
		sink := make([]byte, 16*1024)
		go func() {
			for {
				if _, err := conn.Read(sink); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := conn.Write(chunk); err != nil {
					return
				}
				// Yield so interactive OPEN/DATA can progress on the shared pipe.
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer close(stop)
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 8; i++ {
		conn, exhausted, err := dialResourceRetry(h.clientAddr, 30*time.Second)
		if err != nil {
			if exhausted {
				t.Skipf("local resource exhaustion before interactive dial %d", i)
			}
			t.Fatalf("interactive dial %d: %v", i, err)
		}
		msg := []byte(fmt.Sprintf("interactive-page-%d-payload", i))
		echoRoundTrip(t, conn, msg, 45*time.Second)
		conn.Close()
	}
}
