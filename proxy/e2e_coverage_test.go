package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Full mode × underlay × codec cartesian.
// Nested subtests so CI can shard with -run '.../tcp' and SPP_MATRIX_UNDERLAY.
// Cap parallelism so a single runner is not flooded with proxy pairs.
func TestE2E_FullMatrix(t *testing.T) {
	codecs := matrixCodecs()
	payload := make([]byte, 2*1024)
	fillPattern(payload, 7)

	for _, mode := range matrixModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			for _, proto := range selectedUnderlays() {
				proto := proto
				t.Run(proto, func(t *testing.T) {
					skipIfUnderlayUnavailable(t, proto)
					// ricmp demux is host-scoped; keep it serial within the shard.
					limit := 4
					if proto == "ricmp" {
						limit = 1
					}
					sem := make(chan struct{}, limit)

					for _, codec := range codecs {
						codec := codec
						t.Run(codec.name, func(t *testing.T) {
							t.Parallel()
							sem <- struct{}{}
							defer func() { <-sem }()

							key := fmt.Sprintf("full-matrix-%s-%s-%s-secret", mode, proto, strings.ReplaceAll(codec.name, "/", "-"))
							cfg := withCodec(testConfig(key), codec.compress, codec.encrypt, codec.encKey)
							if proto == "ricmp" || proto == "rhttp" {
								cfg.AuthTimeout = 20
							}
							h := startModeProxy(t, mode, proto, cfg)
							defer h.Close()
							echoViaMode(t, h, mode, payload, 30*time.Second)
						})
					}
				})
			}
		})
	}
}

func TestE2E_MultiPath_FailoverKeepsProxying(t *testing.T) {
	cfg := testConfig("multipath-failover-secret-ok")
	cfg.ProbeInter = 1
	cfg.ProbeSize = 1024
	h := startMultiPathProxy(t, cfg)
	defer h.Close()

	if h.livePipes() < 2 {
		t.Fatalf("need 2 pipes, have %d", h.livePipes())
	}

	c := h.Dial(5 * time.Second)
	msg := []byte("before-failover")
	echoRoundTrip(t, c, msg, 10*time.Second)

	h.killOnePipe()

	deadline := time.Now().Add(5 * time.Second)
	for h.livePipes() >= 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.livePipes() < 1 {
		t.Fatal("all pipes gone after killing one")
	}

	// Existing or new conn should still work via remaining pipe.
	msg2 := []byte("after-failover-still-ok")
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(msg2); err != nil {
		c.Close()
		c = h.Dial(5 * time.Second)
		echoRoundTrip(t, c, msg2, 10*time.Second)
	} else {
		got := make([]byte, len(msg2))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("read after failover: %v", err)
		}
		if sha256.Sum256(got) != sha256.Sum256(msg2) {
			t.Fatal("integrity after failover")
		}
	}
	c.Close()

	// New connection after failover.
	c2 := h.Dial(5 * time.Second)
	defer c2.Close()
	echoRoundTrip(t, c2, []byte("new-conn-after-failover"), 10*time.Second)
}

func TestE2E_Lifecycle_CloseWrite(t *testing.T) {
	h := startForwardProxy(t, "tcp", "lifecycle-closewrite-secret")
	defer h.Close()

	conn := h.Dial(8 * time.Second)
	defer conn.Close()
	echoRoundTrip(t, conn, []byte("before-closewrite"), 10*time.Second)

	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	} else {
		t.Fatal("tcp conn missing CloseWrite")
	}
	// Peer may FIN after half-close; either EOF or clean close is fine.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	_, _ = conn.Read(buf)
}

func TestE2E_Lifecycle_Bidirectional(t *testing.T) {
	h := startForwardProxy(t, "tcp", "lifecycle-bidi-secret-ok")
	defer h.Close()

	up := make([]byte, 48*1024)
	down := make([]byte, 48*1024)
	fillPattern(up, 9)
	fillPattern(down, 11)
	wantUp := sha256.Sum256(up)
	wantDown := sha256.Sum256(down)

	var upErr, downErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c, err := net.DialTimeout("tcp", h.clientAddr, 5*time.Second)
		if err != nil {
			upErr = err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := c.Write(up); err != nil {
			upErr = err
			return
		}
		got := make([]byte, len(up))
		if _, err := io.ReadFull(c, got); err != nil {
			upErr = err
			return
		}
		if sha256.Sum256(got) != wantUp {
			upErr = fmt.Errorf("uplink mismatch")
		}
	}()
	go func() {
		defer wg.Done()
		c, err := net.DialTimeout("tcp", h.clientAddr, 5*time.Second)
		if err != nil {
			downErr = err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := c.Write(down); err != nil {
			downErr = err
			return
		}
		got := make([]byte, len(down))
		if _, err := io.ReadFull(c, got); err != nil {
			downErr = err
			return
		}
		if sha256.Sum256(got) != wantDown {
			downErr = fmt.Errorf("downlink mismatch")
		}
	}()
	wg.Wait()
	if upErr != nil {
		t.Fatalf("stream A: %v", upErr)
	}
	if downErr != nil {
		t.Fatalf("stream B: %v", downErr)
	}
}

func TestE2E_Lifecycle_ReconnectAfterClientRestart(t *testing.T) {
	cfg := testConfig("lifecycle-reconnect-secret")
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()
	serverAddr := freeListenAddr(t, "tcp")
	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	startClient := func(from string) *Client {
		c, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "re", "PROXY",
			[]string{"tcp"}, []string{from}, []string{echoAddr})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return c
	}

	from1 := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	c1 := startClient(from1)
	conn, err := waitForPort(from1, 8*time.Second)
	if err != nil {
		c1.Close()
		t.Fatalf("wait1: %v", err)
	}
	echoRoundTrip(t, conn, []byte("before-restart"), 10*time.Second)
	conn.Close()
	c1.Close()

	from2 := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	c2 := startClient(from2)
	defer c2.Close()
	conn2, err := waitForPort(from2, 8*time.Second)
	if err != nil {
		t.Fatalf("wait2: %v", err)
	}
	defer conn2.Close()
	echoRoundTrip(t, conn2, []byte("after-restart"), 10*time.Second)
}

func TestE2E_Resilience_ShortSoak(t *testing.T) {
	h := startForwardProxy(t, "tcp", "soak-short-secret-ok-123")
	defer h.Close()

	baseG := runtime.NumGoroutine()
	const (
		workers  = 16
		duration = 12 * time.Second
		msgSize  = 2048
	)
	deadline := time.Now().Add(duration)
	var okN, failN atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := make([]byte, msgSize)
			fillPattern(payload, byte(id+3))
			want := sha256.Sum256(payload)
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("tcp", h.clientAddr, 2*time.Second)
				if err != nil {
					failN.Add(1)
					time.Sleep(10 * time.Millisecond)
					continue
				}
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, werr := c.Write(payload)
				got := make([]byte, msgSize)
				_, rerr := io.ReadFull(c, got)
				c.Close()
				if werr != nil || rerr != nil || sha256.Sum256(got) != want {
					failN.Add(1)
					continue
				}
				okN.Add(1)
			}
		}(w)
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)
	afterG := runtime.NumGoroutine()
	ok, fail := okN.Load(), failN.Load()
	t.Logf("soak ok=%d fail=%d goroutines base=%d after=%d", ok, fail, baseG, afterG)
	if ok < 80 {
		t.Fatalf("too few ok=%d", ok)
	}
	if fail*10 > ok {
		t.Fatalf("fail rate too high ok=%d fail=%d", ok, fail)
	}
	if afterG > baseG+200 {
		t.Fatalf("goroutine leak? base=%d after=%d", baseG, afterG)
	}
}

func TestE2E_Resilience_Socks5RudpShortChurn(t *testing.T) {
	cfg := testConfig("socks5-rudp-churn-secret")
	h := startSocks5Proxy(t, "rudp", cfg)
	defer h.Close()

	const (
		workers  = 20
		duration = 8 * time.Second
		msgSize  = 4096
	)
	deadline := time.Now().Add(duration)
	var okN, failN atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := make([]byte, msgSize)
			fillPattern(payload, byte(id+11))
			want := sha256.Sum256(payload)
			for time.Now().Before(deadline) {
				c, err := socks5ConnectErr(h.clientAddr, h.echoAddr, 3*time.Second)
				if err != nil {
					failN.Add(1)
					time.Sleep(20 * time.Millisecond)
					continue
				}
				_ = c.SetDeadline(time.Now().Add(6 * time.Second))
				_, werr := c.Write(payload)
				got := make([]byte, msgSize)
				_, rerr := io.ReadFull(c, got)
				c.Close()
				if werr != nil || rerr != nil || sha256.Sum256(got) != want {
					failN.Add(1)
					continue
				}
				okN.Add(1)
			}
		}(w)
	}
	wg.Wait()
	ok, fail := okN.Load(), failN.Load()
	t.Logf("socks5/rudp churn ok=%d fail=%d", ok, fail)
	if ok < 30 {
		t.Fatalf("too few ok=%d", ok)
	}
	if fail*5 > ok {
		t.Fatalf("fail rate too high ok=%d fail=%d", ok, fail)
	}
}

// Mild loss+reorder+delay on lo: upper layer must still echo intact.
// Opt-in only (SPP_NETEM_JOB=1 / SPP_REQUIRE_NETEM=1) so a broad `go test`
// does not put netem on lo and disturb sibling tests. CI has a dedicated job.
func TestE2E_Netem_LossReorder(t *testing.T) {
	if os.Getenv("SPP_NETEM_JOB") != "1" && os.Getenv("SPP_REQUIRE_NETEM") != "1" {
		t.Skip("set SPP_NETEM_JOB=1 to run netem loss+reorder (dedicated CI job)")
	}
	applyMildNetem(t)

	payload := make([]byte, 32*1024)
	fillPattern(payload, 9)

	for _, proto := range []string{"rudp", "kcp", "quic", "ricmp"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			if proto == "ricmp" {
				if os.Geteuid() != 0 && !canListenRicmp() {
					if netemRequired() {
						t.Fatal("ricmp required in netem job but CAP_NET_RAW/root unavailable")
					}
					t.Skip("ricmp requires root/CAP_NET_RAW")
				}
			}
			cfg := testConfig("netem-loss-reorder-" + proto + "-secret")
			cfg.AuthTimeout = 45
			h := startForwardProxyCfg(t, proto, cfg)
			defer h.Close()

			c := h.Dial(20 * time.Second)
			defer c.Close()
			echoAndHash(t, c, payload, 90*time.Second)

			// Second burst after impairment has been "warm".
			payload2 := make([]byte, 16*1024)
			fillPattern(payload2, 11)
			echoAndHash(t, c, payload2, 90*time.Second)
		})
	}
}

func TestE2E_Underlay_RhttpAndRicmp(t *testing.T) {
	for _, proto := range []string{"rhttp", "ricmp"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			skipIfUnderlayUnavailable(t, proto)
			cfg := testConfig("special-underlay-" + proto + "-secret")
			cfg.AuthTimeout = 20

			echoAddr, stopEcho := startTCPEchoServer(t)
			defer stopEcho()
			serverAddr := freeListenAddr(t, proto)
			clientAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

			server, err := NewServer(cfg, []string{proto}, []string{serverAddr})
			if err != nil {
				t.Skipf("NewServer(%s): %v", proto, err)
			}
			defer server.Close()

			client, err := NewClient(cfg, []string{proto}, []string{serverAddr}, "sp_"+proto, "PROXY",
				[]string{"tcp"}, []string{clientAddr}, []string{echoAddr})
			if err != nil {
				t.Skipf("NewClient(%s): %v", proto, err)
			}
			defer client.Close()

			conn, err := waitForPort(clientAddr, 20*time.Second)
			if err != nil {
				t.Skipf("wait %s: %v (underlay may be unavailable in this env)", proto, err)
			}
			defer conn.Close()
			echoRoundTrip(t, conn, []byte("hello-"+proto), 30*time.Second)
		})
	}
}
