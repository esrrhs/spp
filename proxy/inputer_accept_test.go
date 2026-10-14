package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

// socks5Greeting performs the RFC 1928 method negotiation and leaves the
// conn mid-handshake: the greeting reply has been consumed, the CONNECT /
// UDP ASSOCIATE request has not been sent yet. The server side is blocked
// in processSocks5Conn, which has reserved (but not registered) a sonny
// slot for it — exactly the in-flight state the accept-time reservation
// covers.
func socks5Greeting(t *testing.T, proxyAddr string) net.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial socks5 %s: %v", proxyAddr, err)
	}
	conn := raw.(*net.TCPConn)
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		t.Fatalf("socks5 greet write: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil || resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		t.Fatalf("socks5 greet resp: %v %v", resp, err)
	}
	return raw
}

// firstSocks5Inputer returns the single SOCKS5 Inputer of a running client.
func firstSocks5Inputer(t *testing.T, client *Client) *Inputer {
	t.Helper()
	client.connMu.Lock()
	sess := client.serverconn
	client.connMu.Unlock()
	if sess == nil {
		t.Fatal("client has no server session")
	}
	inputs, _ := sess.serviceSnapshot()
	if len(inputs) != 1 {
		t.Fatalf("want 1 socks5 inputer, got %d", len(inputs))
	}
	return inputs[0]
}

// goroutineStackContains reports whether any live goroutine's stack
// mentions frame. runtime.Stack with all=true dumps every goroutine, so a
// loop that is still running (even spinning in time.Sleep) is visible,
// unlike a plain goroutine count where one leftover goroutine hides in
// the noise.
func goroutineStackContains(frame string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), frame)
}

// TestInputer_CloseStopsAcceptLoop: Inputer.Close() closes the listen
// conn, but the accept loops used to watch only the root group — which
// stays alive across server-conn reconnects — so a closed Inputer left
// its loop retrying a permanently dead Accept (net.ErrClosed) at the
// backoff rate until the whole client shut down. The loop must exit
// promptly once Close() has run, while the root group is still alive.
func TestInputer_CloseStopsAcceptLoop(t *testing.T) {
	serverAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	socksAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

	serverCfg := testConfig("inputer-close-listen-secret")
	clientCfg := testConfig("inputer-close-listen-secret")

	server, err := NewServer(serverCfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(clientCfg, "tcp", serverAddr, "test_inputer_close_listen",
		"SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probe, err := waitForPort(socksAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("wait socks5 port: %v", err)
	}
	probe.Close()

	const frame = "listenSocks5"
	if !waitFor(t, 5*time.Second, func() bool { return goroutineStackContains(frame) }) {
		t.Fatal("accept loop goroutine not running before Close")
	}

	input := firstSocks5Inputer(t, client)
	input.Close()

	if !waitFor(t, 5*time.Second, func() bool { return !goroutineStackContains(frame) }) {
		t.Fatalf("accept loop still running after Inputer.Close (root group alive)")
	}
	t.Log("accept loop exited after Inputer.Close")
}

// TestSOCKS5_StalledHandshakeWatchdogClosesConn: a conn that connects and
// never sends a byte is not in Inputer.sonny yet and has no liveness guard
// of its own, so without the handshake watchdog it would pin a goroutine
// and an fd forever (slowloris / port scanner). The watchdog must close it —
// the peer observes EOF instead of a silent hold.
func TestSOCKS5_StalledHandshakeWatchdogClosesConn(t *testing.T) {
	const stalled = 2

	serverAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	socksAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

	serverCfg := testConfig("socks5-stalled-watchdog-secret")
	clientCfg := testConfig("socks5-stalled-watchdog-secret")
	clientCfg.AuthTimeout = 1

	server, err := NewServer(serverCfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(clientCfg, "tcp", serverAddr, "test_socks5_stalled_watchdog",
		"SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probe, err := waitForPort(socksAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("wait socks5 port: %v", err)
	}
	probe.Close()

	conns := make([]net.Conn, 0, stalled)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < stalled; i++ {
		raw, err := net.Dial("tcp", socksAddr)
		if err != nil {
			t.Fatalf("dial socks5 %d: %v", i, err)
		}
		conns = append(conns, raw)
	}

	// Watchdog (AuthTimeout=1s) closes each silent conn; the peer sees EOF.
	for i, c := range conns {
		tcp := c.(*net.TCPConn)
		_ = tcp.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1)
		if _, err := tcp.Read(buf); err == nil || !errors.Is(err, io.EOF) {
			t.Fatalf("silent conn %d not reaped by handshake watchdog: err=%v", i, err)
		}
	}
	t.Logf("%d silent conns reaped by watchdog", stalled)
}

// waitFor polls cond until it returns true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
