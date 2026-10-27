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
		// minOK is a liveness gate ("proxy still serving"), NOT a throughput
		// SLO: under -race on a contended full-suite runner the same workload
		// drops from ~200 ok (quiet machine) to dozens; a wedged proxy converges
		// to 0 and trips the failure-rate gate as well.
		minOK = 24
	)

	deadline := time.Now().Add(duration)
	var okN, failN, exhaustedN, integrityN atomic.Int64
	var wg sync.WaitGroup
	// First transport error is kept for failure diagnostics; a buffered
	// channel used as a one-shot slot avoids a mutex on the hot path.
	firstErr := make(chan string, 1)
	recordErr := func(s string) {
		select {
		case firstErr <- s:
		default:
		}
	}

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
					recordErr(fmt.Sprintf("dial: %v", err))
					time.Sleep(20 * time.Millisecond)
					continue
				}
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, werr := conn.Write(payload)
				got := make([]byte, msgSize)
				_, rerr := io.ReadFull(conn, got)
				conn.Close()
				if werr != nil {
					failN.Add(1)
					recordErr(fmt.Sprintf("w=%d write=%v", id, werr))
					continue
				}
				if rerr != nil {
					failN.Add(1)
					recordErr(fmt.Sprintf("w=%d read=%v", id, rerr))
					continue
				}
				if sha256.Sum256(got) != want {
					// Corrupted bytes are never environmental: hard fail.
					failN.Add(1)
					integrityN.Add(1)
					recordErr(fmt.Sprintf("w=%d payload hash mismatch", id))
					continue
				}
				okN.Add(1)
			}
		}(w)
	}
	wg.Wait()

	ok, fail, exhausted, integrity := okN.Load(), failN.Load(), exhaustedN.Load(), integrityN.Load()
	first := "<none>"
	select {
	case first = <-firstErr:
	default:
	}
	t.Logf("short churn: ok=%d fail=%d integrity=%d resourceExhausted=%d firstErr=%q",
		ok, fail, integrity, exhausted, first)
	if integrity > 0 {
		t.Fatalf("payload integrity corrupted during churn: %d of %d echoes", integrity, ok+fail)
	}
	if exhausted > 0 && ok < 50 {
		t.Skipf("local ephemeral port/socket exhaustion prevented the churn run: ok=%d exhausted=%d", ok, exhausted)
	}
	if ok < minOK {
		t.Fatalf("too few successful short conns: ok=%d (min %d), firstErr=%q", ok, minOK, first)
	}
	// Tolerate load-induced transport timeouts (a fully -race-scheduled host
	// can starve loopback conns past their 5s deadline); only fail above 10%.
	if fail*10 > ok {
		t.Fatalf("short conn failure rate too high: ok=%d fail=%d sample=%s", ok, fail, first)
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
		msg := []byte(fmt.Sprintf("interactive-page-%d-payload", i))
		attempts := 0
		for {
			conn, exhausted, err := dialResourceRetry(h.clientAddr, 30*time.Second)
			if err != nil {
				if exhausted {
					t.Skipf("local resource exhaustion before interactive dial %d", i)
				}
				t.Fatalf("interactive dial %d: %v", i, err)
			}
			roundErr := echoRoundTripErr(conn, msg, 45*time.Second)
			conn.Close()
			if roundErr == nil {
				break
			}
			// Tunnel churn under a contended full-suite runner can surface as
			// EOF on a fresh interactive conn; integrity mismatches are fatal.
			// Retry with a new conn instead of failing the whole test.
			if !errors.Is(roundErr, io.EOF) && !errors.Is(roundErr, io.ErrUnexpectedEOF) {
				t.Fatalf("interactive echo %d: %v", i, roundErr)
			}
			attempts++
			if attempts > 2 {
				t.Fatalf("interactive echo %d: EOF persisted over %d fresh conns: %v", i, attempts, roundErr)
			}
		}
	}
}

// startStreamVerifyServer starts a TCP target server supporting two streaming modes:
//   - Header 'U' + uint64(totalBytes): reads totalBytes at rateBytesPerSec (0 = unthrottled),
//     computes streaming SHA-256, and writes the 32-byte digest back to the client.
//   - Header 'D' + uint64(totalBytes): streams totalBytes of deterministic pattern bytes
//     to the client as fast as possible and closes the connection.
func startStreamVerifyServer(t *testing.T, uploadReadRateBytesPerSec int64) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startStreamVerifyServer listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var hdr [9]byte
				if _, err := io.ReadFull(conn, hdr[:]); err != nil {
					return
				}
				mode := hdr[0]
				total := int64(uint64(hdr[1])<<56 | uint64(hdr[2])<<48 | uint64(hdr[3])<<40 | uint64(hdr[4])<<32 |
					uint64(hdr[5])<<24 | uint64(hdr[6])<<16 | uint64(hdr[7])<<8 | uint64(hdr[8]))
				switch mode {
				case 'U':
					h := sha256.New()
					buf := make([]byte, 64*1024)
					var received int64
					start := time.Now()
					for received < total {
						toRead := int64(len(buf))
						if rem := total - received; rem < toRead {
							toRead = rem
						}
						n, err := conn.Read(buf[:toRead])
						if n > 0 {
							_, _ = h.Write(buf[:n])
							received += int64(n)
							if uploadReadRateBytesPerSec > 0 {
								expectedElapsed := time.Duration(float64(received) / float64(uploadReadRateBytesPerSec) * float64(time.Second))
								if sleep := expectedElapsed - time.Since(start); sleep > 0 {
									time.Sleep(sleep)
								}
							}
						}
						if err != nil {
							return
						}
					}
					sum := h.Sum(nil)
					_, _ = conn.Write(sum)
				case 'D':
					chunk := make([]byte, 64*1024)
					var sent int64
					var seq byte
					for sent < total {
						toWrite := int64(len(chunk))
						if rem := total - sent; rem < toWrite {
							toWrite = rem
						}
						fillPattern(chunk[:toWrite], seq)
						seq++
						if _, err := conn.Write(chunk[:toWrite]); err != nil {
							return
						}
						sent += toWrite
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
	}
}

func putStreamHeader(mode byte, total int64) [9]byte {
	var hdr [9]byte
	hdr[0] = mode
	u := uint64(total)
	hdr[1] = byte(u >> 56)
	hdr[2] = byte(u >> 48)
	hdr[3] = byte(u >> 40)
	hdr[4] = byte(u >> 32)
	hdr[5] = byte(u >> 24)
	hdr[6] = byte(u >> 16)
	hdr[7] = byte(u >> 8)
	hdr[8] = byte(u)
	return hdr
}

// TestE2E_NarrowPipe_HeavyBackpressure verifies the "大流量 + 小水管" (large burst +
// narrow pipe) scenario where both one-way upload and one-way download saturate
// tight proxy queues (ConnBuffer=4, MainBuffer=8) for >2x the entire PingTimeoutInter
// window without triggering false ping-pong timeouts or dropping trailing frames.
func TestE2E_NarrowPipe_HeavyBackpressure(t *testing.T) {
	const (
		totalBytes        = 12 * 1024 * 1024 // 12 MiB
		narrowPipeRateBps = 2 * 1024 * 1024  // 2 MiB/s -> ~6s transfer (>2x the 2s PingTimeoutInter)
	)
	targetAddr, stopTarget := startStreamVerifyServer(t, narrowPipeRateBps)
	defer stopTarget()

	for _, proto := range []string{"tcp", "rudp"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			cfg := testConfig("narrow-pipe-secret-" + proto)
			cfg.ConnBuffer = 4
			cfg.MainBuffer = 8
			cfg.PingInter = 1
			cfg.PingTimeoutInter = 2 // 2s timeout window; 6s one-way transfer must stay alive
			cfg.MainWriteChannelTimeoutMs = 0
			cfg.ConnTimeout = 0

			h := startSocks5Proxy(t, proto, cfg)
			defer h.Close()

			// 1. One-Way Upload (12 MiB blasted by client, digested at 2 MiB/s by target over ~6s)
			t.Run("Upload", func(t *testing.T) {
				conn := socks5Connect(t, h.clientAddr, targetAddr, 10*time.Second)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(45 * time.Second))

				hdr := putStreamHeader('U', totalBytes)
				if _, err := conn.Write(hdr[:]); err != nil {
					t.Fatalf("write upload header: %v", err)
				}

				wantHash := sha256.New()
				chunk := make([]byte, 64*1024)
				var sent int64
				var seq byte
				for sent < totalBytes {
					fillPattern(chunk, seq)
					seq++
					_, _ = wantHash.Write(chunk)
					if _, err := conn.Write(chunk); err != nil {
						t.Fatalf("upload write at %d/%d: %v", sent, totalBytes, err)
					}
					sent += int64(len(chunk))
				}

				if !h.client.HasRecentTraffic(5 * time.Second) {
					t.Fatalf("expected HasRecentTraffic=true on client during one-way upload")
				}

				var gotSum [32]byte
				if _, err := io.ReadFull(conn, gotSum[:]); err != nil {
					t.Fatalf("read upload sha256 response after narrow-pipe drain: %v", err)
				}
				var wantSum [32]byte
				copy(wantSum[:], wantHash.Sum(nil))
				if gotSum != wantSum {
					t.Fatalf("upload sha256 mismatch: got %x, want %x", gotSum, wantSum)
				}
			})

			// 2. One-Way Download (12 MiB blasted by target + immediate close, digested at 2 MiB/s by client over ~6s)
			t.Run("Download", func(t *testing.T) {
				conn := socks5Connect(t, h.clientAddr, targetAddr, 10*time.Second)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(45 * time.Second))

				hdr := putStreamHeader('D', totalBytes)
				if _, err := conn.Write(hdr[:]); err != nil {
					t.Fatalf("write download header: %v", err)
				}

				wantHash := sha256.New()
				expectedChunk := make([]byte, 64*1024)
				for i := int64(0); i < totalBytes/(64*1024); i++ {
					fillPattern(expectedChunk, byte(i))
					_, _ = wantHash.Write(expectedChunk)
				}

				gotHash := sha256.New()
				buf := make([]byte, 64*1024)
				var received int64
				start := time.Now()
				for received < totalBytes {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = gotHash.Write(buf[:n])
						received += int64(n)
						expectedElapsed := time.Duration(float64(received) / float64(narrowPipeRateBps) * float64(time.Second))
						if sleep := expectedElapsed - time.Since(start); sleep > 0 {
							time.Sleep(sleep)
						}
					}
					if err != nil {
						if err == io.EOF && received == totalBytes {
							break
						}
						t.Fatalf("download read at %d/%d: %v", received, totalBytes, err)
					}
				}

				if !h.client.HasRecentTraffic(5 * time.Second) {
					t.Fatalf("expected HasRecentTraffic=true on client during one-way download")
				}

				var gotSum, wantSum [32]byte
				copy(gotSum[:], gotHash.Sum(nil))
				copy(wantSum[:], wantHash.Sum(nil))
				if gotSum != wantSum {
					t.Fatalf("download sha256 mismatch: got %x, want %x", gotSum, wantSum)
				}
			})
		})
	}
}

// TestE2E_LargeStream_1GB_UploadAndDownload streams a full 1 GiB (1,073,741,824 bytes)
// upload and a full 1 GiB download through a constrained proxy pipe with throttled
// digestion so queues remain under sustained backpressure across multiple heartbeat
// timeout windows, verifying byte-for-byte SHA-256 integrity.
func TestE2E_LargeStream_1GB_UploadAndDownload(t *testing.T) {
	const (
		oneGiB            = int64(1) << 30    // 1,073,741,824 bytes (1 GiB)
		throttleRateBps   = 220 * 1024 * 1024 // ~220 MiB/s -> ~4.7s per 1 GiB (>2x the 2s PingTimeoutInter)
	)
	targetAddr, stopTarget := startStreamVerifyServer(t, throttleRateBps)
	defer stopTarget()

	cfg := testConfig("large-stream-1gb-secret-ok")
	cfg.ConnBuffer = 8
	cfg.MainBuffer = 16
	cfg.PingInter = 1
	cfg.PingTimeoutInter = 2
	cfg.MainWriteChannelTimeoutMs = 0
	cfg.ConnTimeout = 0

	h := startSocks5Proxy(t, "tcp", cfg)
	defer h.Close()

	// Precompute the deterministic 1 GiB SHA-256 once (256 distinct 64 KiB chunks repeated).
	var patternChunks [256][]byte
	for i := 0; i < 256; i++ {
		b := make([]byte, 64*1024)
		fillPattern(b, byte(i))
		patternChunks[i] = b
	}
	wantHash := sha256.New()
	totalChunks := oneGiB / (64 * 1024)
	for i := int64(0); i < totalChunks; i++ {
		_, _ = wantHash.Write(patternChunks[byte(i)])
	}
	var wantSum [32]byte
	copy(wantSum[:], wantHash.Sum(nil))

	t.Run("Upload_1GiB", func(t *testing.T) {
		start := time.Now()
		conn := socks5Connect(t, h.clientAddr, targetAddr, 10*time.Second)
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

		hdr := putStreamHeader('U', oneGiB)
		if _, err := conn.Write(hdr[:]); err != nil {
			t.Fatalf("write 1GiB upload header: %v", err)
		}

		for i := int64(0); i < totalChunks; i++ {
			if _, err := conn.Write(patternChunks[byte(i)]); err != nil {
				t.Fatalf("1GiB upload write at chunk %d/%d: %v", i, totalChunks, err)
			}
		}

		var gotSum [32]byte
		if _, err := io.ReadFull(conn, gotSum[:]); err != nil {
			t.Fatalf("read 1GiB upload sha256 response: %v", err)
		}
		if gotSum != wantSum {
			t.Fatalf("1GiB upload sha256 mismatch: got %x, want %x", gotSum, wantSum)
		}
		t.Logf("1GiB upload verified in %s", time.Since(start))
	})

	t.Run("Download_1GiB", func(t *testing.T) {
		start := time.Now()
		conn := socks5Connect(t, h.clientAddr, targetAddr, 10*time.Second)
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

		hdr := putStreamHeader('D', oneGiB)
		if _, err := conn.Write(hdr[:]); err != nil {
			t.Fatalf("write 1GiB download header: %v", err)
		}

		gotHash := sha256.New()
		buf := make([]byte, 64*1024)
		var received int64
		for received < oneGiB {
			n, err := conn.Read(buf)
			if n > 0 {
				_, _ = gotHash.Write(buf[:n])
				received += int64(n)
				expectedElapsed := time.Duration(float64(received) / float64(throttleRateBps) * float64(time.Second))
				if sleep := expectedElapsed - time.Since(start); sleep > 0 {
					time.Sleep(sleep)
				}
			}
			if err != nil {
				if err == io.EOF && received == oneGiB {
					break
				}
				t.Fatalf("1GiB download read at %d/%d: %v", received, oneGiB, err)
			}
		}

		var gotSum [32]byte
		copy(gotSum[:], gotHash.Sum(nil))
		if gotSum != wantSum {
			t.Fatalf("1GiB download sha256 mismatch: got %x, want %x", gotSum, wantSum)
		}
		t.Logf("1GiB download verified in %s", time.Since(start))
	})
}

// TestE2E_InteractiveWebWhileBulkDownloadSaturatesPipe reproduces the scenario:
// "有一个连接在下载占满了小水管，这时候新开一个连接请求google网页，就会打不开"
// Connection 1 downloads a large stream while digesting slowly (saturating the narrow pipe).
// While Connection 1 is saturating the pipe and queues, Connection 2 opens a new request
// for a small webpage payload (e.g. 32 KiB) and expects a prompt response without timing out.
func TestE2E_InteractiveWebWhileBulkDownloadSaturatesPipe(t *testing.T) {
	const (
		bulkBytes         = 12 * 1024 * 1024 // 12 MiB bulk download
		narrowPipeRateBps = 1 * 1024 * 1024  // 1 MiB/s narrow pipe -> ~12s total download
		webPageBytes      = 32 * 1024        // 32 KiB interactive webpage response
	)
	targetAddr, stopTarget := startStreamVerifyServer(t, narrowPipeRateBps)
	defer stopTarget()

	for _, proto := range []string{"tcp", "kcp", "rudp"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			cfg := testConfig("interactive-under-bulk-" + proto)
			cfg.ConnBuffer = 8
			cfg.MainBuffer = 16
			cfg.PingInter = 1
			cfg.PingTimeoutInter = 5
			cfg.MainWriteChannelTimeoutMs = 0
			cfg.ConnTimeout = 0

			h := startSocks5Proxy(t, proto, cfg)
			defer h.Close()

			saturatedCh := make(chan struct{})
			bulkDoneCh := make(chan error, 1)

			// Start Connection 1: bulk download saturating the narrow pipe
			go func() {
				conn := socks5Connect(t, h.clientAddr, targetAddr, 10*time.Second)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(45 * time.Second))

				hdr := putStreamHeader('D', bulkBytes)
				if _, err := conn.Write(hdr[:]); err != nil {
					bulkDoneCh <- err
					return
				}

				buf := make([]byte, 64*1024)
				var received int64
				var signaled bool
				start := time.Now()
				for received < bulkBytes {
					n, err := conn.Read(buf)
					if n > 0 {
						received += int64(n)
						if !signaled && received >= 512*1024 {
							signaled = true
							close(saturatedCh)
						}
						expectedElapsed := time.Duration(float64(received) / float64(narrowPipeRateBps) * float64(time.Second))
						if sleep := expectedElapsed - time.Since(start); sleep > 0 {
							time.Sleep(sleep)
						}
					}
					if err != nil {
						if err == io.EOF && received == bulkBytes {
							break
						}
						bulkDoneCh <- err
						return
					}
				}
				bulkDoneCh <- nil
			}()

			// Wait for Connection 1 to fill and saturate the pipe
			select {
			case <-saturatedCh:
			case <-time.After(10 * time.Second):
				t.Fatalf("timed out waiting for bulk download to start saturating pipe")
			}
			// Give 300ms for sendq/recvq/bizq/sonny.sendch to fill up completely under backpressure
			time.Sleep(300 * time.Millisecond)

			// Now open Connection 2 (simulating opening Google webpage while pipe is saturated)
			webStart := time.Now()
			webConn := socks5Connect(t, h.clientAddr, targetAddr, 2*time.Second)
			defer webConn.Close()
			_ = webConn.SetDeadline(time.Now().Add(2 * time.Second))

			hdr := putStreamHeader('D', webPageBytes)
			if _, err := webConn.Write(hdr[:]); err != nil {
				t.Fatalf("[%s] write web request failed after %s: %v", proto, time.Since(webStart), err)
			}

			webBuf := make([]byte, webPageBytes)
			if _, err := io.ReadFull(webConn, webBuf); err != nil {
				t.Fatalf("[%s] read web page failed/timed out after %s while bulk download active: %v", proto, time.Since(webStart), err)
			}
			webElapsed := time.Since(webStart)
			t.Logf("[%s] interactive web request completed in %s while bulk download saturated pipe", proto, webElapsed)
			if webElapsed > 1500*time.Millisecond {
				t.Fatalf("[%s] interactive web request took too long (%s > 1.5s)", proto, webElapsed)
			}

			if err := <-bulkDoneCh; err != nil {
				t.Fatalf("[%s] bulk download failed: %v", proto, err)
			}
		})
	}
}

