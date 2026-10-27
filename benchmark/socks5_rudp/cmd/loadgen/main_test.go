package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startFakeSocks5 starts a minimal socks5 CONNECT proxy that relays to
// backend. behavior flips the error branches of the handshake:
// ok (relay), reject (auth refused), connfail (CONNECT refused), rst (reset
// right after the success reply), drop (close before the CONNECT reply).
func startFakeSocks5(t *testing.T, backend, behavior string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				hello := make([]byte, 3)
				if _, err := io.ReadFull(conn, hello); err != nil {
					return
				}
				if behavior == "reject" {
					conn.Write([]byte{0x05, 0x02})
					return
				}
				if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				if behavior == "drop" {
					return
				}
				hdr := make([]byte, 4)
				if _, err := io.ReadFull(conn, hdr); err != nil {
					return
				}
				if hdr[3] != 0x01 { // IPv4 only, mirroring socks5Dial
					return
				}
				ip := make([]byte, 4)
				if _, err := io.ReadFull(conn, ip); err != nil {
					return
				}
				var pb [2]byte
				if _, err := io.ReadFull(conn, pb[:]); err != nil {
					return
				}
				target := fmt.Sprintf("%d.%d.%d.%d:%d", ip[0], ip[1], ip[2], ip[3], binary.BigEndian.Uint16(pb[:]))
				up, err := net.DialTimeout("tcp", target, 3*time.Second)
				if err != nil || behavior == "connfail" {
					conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				if behavior == "rst" {
					// Reset so the client's next write errors out.
					if tc, ok := conn.(*net.TCPConn); ok {
						tc.SetLinger(0)
					}
					return
				}
				done := make(chan struct{}, 2)
				go func() { io.Copy(conn, up); done <- struct{}{} }()
				go func() { io.Copy(up, conn); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	addr := ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	return addr
}

// tinyBackend is a minimal echo/source/sink/page server for loadgen runs.
func tinyBackend(t *testing.T, mode string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				switch mode {
				case "echo":
					io.Copy(conn, conn)
				case "source":
					buf := make([]byte, 4096)
					for {
						if _, err := conn.Write(buf); err != nil {
							return
						}
					}
				case "page":
					var hdr [4]byte
					if _, err := io.ReadFull(conn, hdr[:]); err != nil {
						return
					}
					req := make([]byte, binary.BigEndian.Uint32(hdr[:]))
					if _, err := io.ReadFull(conn, req); err != nil {
						return
					}
					body := make([]byte, 256)
					for i := range body {
						body[i] = byte(i) + 0x5A
					}
					binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
					if _, err := conn.Write(hdr[:]); err != nil {
						return
					}
					sum := sha256.Sum256(body)
					if _, err := conn.Write(sum[:]); err != nil {
						return
					}
					conn.Write(body)
				default: // sink
					buf := make([]byte, 4096)
					for {
						if _, err := conn.Read(buf); err != nil {
							return
						}
					}
				}
			}(c)
		}
	}()
	addr := ln.Addr().String()
	t.Cleanup(func() { ln.Close(); <-done })
	return addr
}

func TestSocks5Dial_Success(t *testing.T) {
	backend := tinyBackend(t, "echo")
	proxy := startFakeSocks5(t, backend, "ok")

	c, err := socks5Dial(proxy, backend, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("socks5-dial-echo")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch %q", got)
	}
}

func TestSocks5Dial_Errors(t *testing.T) {
	backend := tinyBackend(t, "echo")

	if _, err := socks5Dial("127.0.0.1:1", backend, 500*time.Millisecond); err == nil {
		t.Fatal("expected dial error for dead proxy")
	}

	reject := startFakeSocks5(t, backend, "reject")
	if _, err := socks5Dial(reject, backend, 3*time.Second); err == nil || !strings.Contains(err.Error(), "socks5 auth rejected") {
		t.Fatalf("expected auth reject, got %v", err)
	}

	fail := startFakeSocks5(t, backend, "connfail")
	if _, err := socks5Dial(fail, backend, 3*time.Second); err == nil || !strings.Contains(err.Error(), "socks5 connect failed") {
		t.Fatalf("expected connect failure, got %v", err)
	}

	ok := startFakeSocks5(t, backend, "ok")
	if _, err := socks5Dial(ok, "not-an-ip-target:80", 3*time.Second); err == nil {
		t.Fatal("expected non-ipv4 target error")
	}
	if _, err := socks5Dial(ok, "bad-addr", 3*time.Second); err == nil {
		t.Fatal("expected SplitHostPort error")
	}
}

func TestFillPatternLoadgen(t *testing.T) {
	b := make([]byte, 257)
	fillPattern(b, 0x5A)
	for i := range b {
		if b[i] != byte(i)+0x5A {
			t.Fatalf("fillPattern mismatch at %d", i)
		}
	}
}

// TestRunLoadgen_Modes drives one short run per mode against a matching
// backend via the fake socks5 proxy; this is the statement-level coverage for
// the workload loops (short/page/upload/download/bidi).
func TestRunLoadgen_Modes(t *testing.T) {
	backendModes := map[string]string{
		"short":    "echo",
		"page":     "page",
		"upload":   "sink",
		"download": "source",
		"bidi":     "echo",
	}
	for mode, backendMode := range backendModes {
		t.Run(mode, func(t *testing.T) {
			backend := tinyBackend(t, backendMode)
			proxy := startFakeSocks5(t, backend, "ok")
			runLoadgen(loadgenCfg{
				proxy:    proxy,
				target:   backend,
				conns:    1,
				duration: 600 * time.Millisecond,
				chunk:    1024,
				reqSize:  64,
				pageSize: 256,
				mode:     mode,
			}, make(chan struct{}))
		})
	}
}

// Page-size mismatch must count as bad instead of wedging the loop.
func TestRunLoadgen_PageMismatch(t *testing.T) {
	backend := tinyBackend(t, "page")
	proxy := startFakeSocks5(t, backend, "ok")
	runLoadgen(loadgenCfg{
		proxy:    proxy,
		target:   backend,
		conns:    1,
		duration: 600 * time.Millisecond,
		chunk:    1024,
		reqSize:  64,
		pageSize: 128, // backend serves 256
		mode:     "page",
	}, make(chan struct{}))
}

// Dead proxy: every op errs but the loop keeps retrying until duration ends.
func TestRunLoadgen_ProxyDown(t *testing.T) {
	runLoadgen(loadgenCfg{
		proxy:    "127.0.0.1:1",
		target:   "127.0.0.1:1",
		conns:    2,
		duration: 400 * time.Millisecond,
		chunk:    1024,
		reqSize:  64,
		pageSize: 256,
		mode:     "short",
	}, make(chan struct{}))
}

// Write errors after the proxy resets the session must not wedge the loop.
func TestRunLoadgen_ProxyResets(t *testing.T) {
	for _, mode := range []string{"short", "page"} {
		t.Run(mode, func(t *testing.T) {
			backend := tinyBackend(t, "echo")
			if mode == "page" {
				backend = tinyBackend(t, "page")
			}
			proxy := startFakeSocks5(t, backend, "rst")
			runLoadgen(loadgenCfg{
				proxy:    proxy,
				target:   backend,
				conns:    1,
				duration: 600 * time.Millisecond,
				chunk:    1024,
				reqSize:  64,
				pageSize: 256,
				mode:     mode,
			}, make(chan struct{}))
		})
	}
}

// Proxy closing before the CONNECT reply exercises the handshake-read error.
func TestRunLoadgen_ProxyDropsHandshake(t *testing.T) {
	backend := tinyBackend(t, "echo")
	proxy := startFakeSocks5(t, backend, "drop")
	runLoadgen(loadgenCfg{
		proxy:    proxy,
		target:   backend,
		conns:    1,
		duration: 400 * time.Millisecond,
		chunk:    1024,
		reqSize:  64,
		pageSize: 256,
		mode:     "short",
	}, make(chan struct{}))
}

// A run longer than the 2s progress-ticker period covers the printer branch.
func TestRunLoadgen_ProgressTicker(t *testing.T) {
	backend := tinyBackend(t, "echo")
	proxy := startFakeSocks5(t, backend, "ok")
	runLoadgen(loadgenCfg{
		proxy:    proxy,
		target:   backend,
		conns:    1,
		duration: 2400 * time.Millisecond,
		chunk:    1024,
		reqSize:  64,
		pageSize: 256,
		mode:     "short",
	}, make(chan struct{}))
}

// Short mode clamps oversized chunks to 4KiB.
func TestRunLoadgen_ShortClamp(t *testing.T) {
	backend := tinyBackend(t, "echo")
	proxy := startFakeSocks5(t, backend, "ok")
	runLoadgen(loadgenCfg{
		proxy:    proxy,
		target:   backend,
		conns:    1,
		duration: 600 * time.Millisecond,
		chunk:    32 * 1024, // > 8KiB triggers the clamp branch
		reqSize:  64,
		pageSize: 256,
		mode:     "short",
	}, make(chan struct{}))
}
