package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestE2E_UnderlayMatrix_ForwardEcho(t *testing.T) {
	for _, proto := range matrixUnderlaysCore {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			h := startForwardProxy(t, proto, "matrix-echo-secret-"+proto+"-ok")
			defer h.Close()

			conn := h.Dial(5 * time.Second)
			defer conn.Close()

			msg := []byte("hello underlay " + proto)
			echoRoundTrip(t, conn, msg, 10*time.Second)
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

			conn := h.Dial(5 * time.Second)
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
	var okN, failN atomic.Int64
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
				conn, err := net.DialTimeout("tcp", h.clientAddr, 2*time.Second)
				if err != nil {
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

	ok, fail := okN.Load(), failN.Load()
	t.Logf("short churn: ok=%d fail=%d", ok, fail)
	if ok < 50 {
		t.Fatalf("too few successful short conns: ok=%d", ok)
	}
	// Allow a tiny dial race at shutdown, but integrity failures must be rare.
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
		conn, err := net.DialTimeout("tcp", h.clientAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("interactive dial %d: %v", i, err)
		}
		msg := []byte(fmt.Sprintf("interactive-page-%d-payload", i))
		echoRoundTrip(t, conn, msg, 45*time.Second)
		conn.Close()
	}
}
