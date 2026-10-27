package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
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

					for _, codec := range codecs {
						codec := codec
						t.Run(codec.name, func(t *testing.T) {
							t.Parallel()
							// Bound matrix-wide pair concurrency (see matrixParallel);
							// ricmp additionally takes a host-scoped serial slot.
							acquireMatrixSlot(proto)
							defer releaseMatrixSlot(proto)

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
		c, err := NewClient(cfg, "tcp", serverAddr, "re", "PROXY",
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

// TestE2E_Lifecycle_ReverseReclaimAfterServerSessionDrop reproduces the login
// startup-window teardown bug end to end: the server starts reverse inputers
// before publishing the session, so a conn group exiting while the session is
// absent from the clients map must still release the exposed port. Otherwise
// the client's reconnect — same fromaddr — fails iniService (address already
// in use) forever and the reverse service stays down until server restart.
func TestE2E_Lifecycle_ReverseReclaimAfterServerSessionDrop(t *testing.T) {
	cfg := testConfig("reverse-reclaim-secret")
	h := startReverseProxy(t, "tcp", cfg)
	defer h.Close()

	// Warm echo proves the first tunnel is fully established.
	conn := h.Dial(8 * time.Second)
	echoRoundTrip(t, conn, []byte("before-drop"), 10*time.Second)
	conn.Close()

	var serverSess *ClientConn
	h.server.clients.Range(func(_, v any) bool {
		serverSess = v.(*ClientConn)
		return false
	})
	if serverSess == nil {
		t.Fatal("server session not found")
	}
	h.client.connMu.Lock()
	oldClientSess := h.client.serverconn
	h.client.connMu.Unlock()

	// Reproduce the exact never-published state (conn died between iniService
	// and clients.Store), then force the conn group exit.
	h.server.clients.Delete(serverSess.clientID)
	atomic.AddInt32(&h.server.clientNum, -1)
	serverSess.setNeedClose()

	// The client must reconnect on its own and the server must rebind the SAME
	// exposed port via a fresh reverse inputer.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		h.client.connMu.Lock()
		cur := h.client.serverconn
		h.client.connMu.Unlock()
		if cur != nil && cur != oldClientSess && cur.isEstablished() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.client.connMu.Lock()
	newSess := h.client.serverconn
	h.client.connMu.Unlock()
	if newSess == nil || newSess == oldClientSess || !newSess.isEstablished() {
		t.Fatal("client did not establish a fresh session after server-side drop")
	}

	c2 := h.Dial(10 * time.Second)
	defer c2.Close()
	echoRoundTrip(t, c2, []byte("after-reclaim"), 15*time.Second)

	if n := h.server.clientSize(); n != 1 {
		t.Fatalf("server clientSize=%d want 1 (untracked teardown must not corrupt counter)", n)
	}
}

func TestE2E_Resilience_ShortSoak(t *testing.T) {
	h := startForwardProxy(t, "tcp", "soak-short-secret-ok-123")
	defer h.Close()

	baseG := runtime.NumGoroutine()
	const (
		workers  = 16
		duration = 12 * time.Second
		msgSize  = 2048
		// minOK is a liveness gate ("proxy not wedged"), NOT a throughput SLO.
		// Under -race on a contended 4-vCPU runner the same workload finishes
		// ~60 echoes; a quiet machine does hundreds. A dead pipe converges to 0
		// and trips the fail-rate gate as well.
		minOK = 24
	)
	deadline := time.Now().Add(duration)
	var okN, failN, exhaustedN, integrityN atomic.Int64
	// First non-integrity error is kept for failure diagnostics; a buffered
	// channel used as a one-shot slot avoids a mutex on the hot path.
	firstErr := make(chan string, 1)
	recordErr := func(s string) {
		select {
		case firstErr <- s:
		default:
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := make([]byte, msgSize)
			fillPattern(payload, byte(id+3))
			want := sha256.Sum256(payload)
			for time.Now().Before(deadline) {
				c, exhausted, err := dialResourceRetry(h.clientAddr, 30*time.Second)
				if err != nil {
					if exhausted {
						exhaustedN.Add(1)
						return // host cannot open sockets right now; gate below
					}
					failN.Add(1)
					recordErr("dial: " + err.Error())
					time.Sleep(10 * time.Millisecond)
					continue
				}
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, werr := c.Write(payload)
				got := make([]byte, msgSize)
				_, rerr := io.ReadFull(c, got)
				c.Close()
				if werr != nil {
					failN.Add(1)
					recordErr("write: " + werr.Error())
					continue
				}
				if rerr != nil {
					failN.Add(1)
					recordErr("read: " + rerr.Error())
					continue
				}
				if sha256.Sum256(got) != want {
					failN.Add(1)
					integrityN.Add(1)
					recordErr("payload hash mismatch")
					continue
				}
				okN.Add(1)
			}
		}(w)
	}
	wg.Wait()
	// Conn workers exit asynchronously. A fast host finishes thousands of
	// echoes in the window, and a single 500ms sample still sees them — that
	// is teardown lag, not a leak. Poll until the count settles.
	var afterG int
	settleDeadline := time.Now().Add(10 * time.Second)
	for {
		time.Sleep(200 * time.Millisecond)
		afterG = runtime.NumGoroutine()
		if afterG <= baseG+200 || time.Now().After(settleDeadline) {
			break
		}
	}
	ok, fail := okN.Load(), failN.Load()
	exhausted, integrity := exhaustedN.Load(), integrityN.Load()
	first := "<none>"
	select {
	case first = <-firstErr:
	default:
	}
	t.Logf("soak ok=%d fail=%d integrity=%d resourceExhausted=%d goroutines base=%d after=%d firstErr=%q",
		ok, fail, integrity, exhausted, baseG, afterG, first)

	// Corrupted bytes are never environmental: fail hard immediately.
	if integrity > 0 {
		t.Fatalf("payload integrity corrupted during churn: %d of %d echoes", integrity, ok+fail)
	}
	if exhausted > 0 && ok < 50 {
		t.Skipf("local ephemeral port/socket exhaustion prevented the soak run: ok=%d exhausted=%d", ok, exhausted)
	}
	if ok < minOK {
		t.Fatalf("too few successful echoes ok=%d (min %d): wedged proxy or overloaded host, firstErr=%q", ok, minOK, first)
	}
	if fail*10 > ok {
		t.Fatalf("fail rate too high ok=%d fail=%d firstErr=%q", ok, fail, first)
	}
	if afterG > baseG+200 {
		t.Fatalf("goroutine leak? base=%d after=%d", baseG, afterG)
	}
}

func TestE2E_Resilience_Socks5RudpShortChurn(t *testing.T) {
	const (
		duration = 8 * time.Second
		msgSize  = 4096
		// Liveness gate ("proxy still serving"), NOT a throughput SLO: under
		// -race on a contended full-suite runner rudp handshakes time out far
		// more often; a wedged proxy converges to 0 ok and trips the gate too.
		minOK = 30
		// Under -race, concurrent rudp handshakes self-contend; fewer workers
		// keeps the gate honest without starving the whole window.
		workers = sppRaceWorkers
	)
	type churnResult struct {
		ok, dialFail, dataFail, integrity, exhausted int64
		first                                        string
	}
	// Ephemeral port / fd exhaustion from thousands of sibling short-conn
	// tests is environmental: dial errors classified here gate as exhaustion,
	// not proxy health.
	isExhaustion := func(err string) bool {
		return strings.Contains(err, "can't assign requested address") ||
			strings.Contains(err, "too many open files") ||
			strings.Contains(err, "address already in use")
	}
	runChurn := func() churnResult {
		cfg := testConfig("socks5-rudp-churn-secret")
		h := startSocks5Proxy(t, "rudp", cfg)
		defer h.Close()
		deadline := time.Now().Add(duration)
		var okN, dialFailN, dataFailN, integrityN, exhaustN atomic.Int64
		firstErr := make(chan string, 1)
		recordErr := func(s string) {
			select {
			case firstErr <- s:
			default:
			}
		}
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				payload := make([]byte, msgSize)
				fillPattern(payload, byte(id+11))
				want := sha256.Sum256(payload)
				for time.Now().Before(deadline) {
					c, err := socks5ConnectErr(h.clientAddr, h.echoAddr, 5*time.Second)
					if err != nil {
						// Dial/handshake failures are dominated by host load,
						// not proxy correctness; gated on liveness below.
						dialFailN.Add(1)
						if isExhaustion(err.Error()) {
							exhaustN.Add(1)
						}
						recordErr(fmt.Sprintf("w=%d dial: %v", id, err))
						time.Sleep(20 * time.Millisecond)
						continue
					}
					_ = c.SetDeadline(time.Now().Add(10 * time.Second))
					_, werr := c.Write(payload)
					got := make([]byte, msgSize)
					_, rerr := io.ReadFull(c, got)
					c.Close()
					if werr != nil || rerr != nil {
						dataFailN.Add(1)
						recordErr(fmt.Sprintf("w=%d io: write=%v read=%v", id, werr, rerr))
						continue
					}
					if sha256.Sum256(got) != want {
						// Corrupted bytes are never environmental: hard fail.
						integrityN.Add(1)
						continue
					}
					okN.Add(1)
				}
			}(w)
		}
		wg.Wait()
		res := churnResult{
			ok:        okN.Load(),
			dialFail:  dialFailN.Load(),
			dataFail:  dataFailN.Load(),
			integrity: integrityN.Load(),
			exhausted: exhaustN.Load(),
			first:     "<none>",
		}
		select {
		case res.first = <-firstErr:
		default:
		}
		return res
	}

	// A fully starved runner can starve the whole 8s window (handshakes and
	// even established rudp conns die when retransmit timers slip); liveness
	// or data-rate shortfalls get one retry on a fresh harness before being
	// called a wedge. Integrity violations are never retried.
	res := runChurn()
	t.Logf("socks5/rudp churn ok=%d dialFail=%d dataFail=%d integrity=%d exhausted=%d firstErr=%q",
		res.ok, res.dialFail, res.dataFail, res.integrity, res.exhausted, res.first)
	if res.integrity > 0 {
		t.Fatalf("payload integrity corrupted during churn: %d of %d echoes", res.integrity, res.ok+res.dataFail+res.integrity)
	}
	if res.ok < minOK || res.dataFail*5 > res.ok {
		res = runChurn()
		t.Logf("socks5/rudp churn retry ok=%d dialFail=%d dataFail=%d integrity=%d exhausted=%d firstErr=%q",
			res.ok, res.dialFail, res.dataFail, res.integrity, res.exhausted, res.first)
	}
	if res.integrity > 0 {
		t.Fatalf("payload integrity corrupted during churn: %d of %d echoes", res.integrity, res.ok+res.dataFail+res.integrity)
	}
	if res.exhausted > 0 && res.ok < minOK {
		t.Skipf("local ephemeral port/socket exhaustion prevented the churn run: ok=%d exhaustedDial=%d", res.ok, res.exhausted)
	}
	if res.ok < minOK {
		t.Fatalf("too few ok=%d (min %d): wedged proxy or overloaded host, firstErr=%q", res.ok, minOK, res.first)
	}
	if res.dataFail*5 > res.ok {
		if res.exhausted > 0 {
			t.Skipf("host port exhaustion corrupted the churn data-rate sample: ok=%d dataFail=%d exhaustedDial=%d", res.ok, res.dataFail, res.exhausted)
		}
		t.Fatalf("data failure rate too high ok=%d dataFail=%d firstErr=%q", res.ok, res.dataFail, res.first)
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

			client, err := NewClient(cfg, proto, serverAddr, "sp_"+proto, "PROXY",
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

// TestE2E_KcpFecEnabled runs a full forward-proxy round trip with KCP
// Reed-Solomon FEC (10+3) configured identically on both ends. FEC changes
// the kcp wire framing, so this guards the spp-side config plumbing from the
// flags/Config all the way into the underlay conns on both peers.
func TestE2E_KcpFecEnabled(t *testing.T) {
	cfg := testConfig("kcp-fec-e2e-secret")
	cfg.AuthTimeout = 20
	cfg.KcpFecDataShards = 10
	cfg.KcpFecParityShards = 3
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}

	h := startForwardProxyCfg(t, "kcp", cfg)
	defer h.Close()

	// A multi-KB payload spans many KCP segments so the FEC encoder/decoder
	// actually groups and recovers shards rather than only seeing one packet.
	payload := make([]byte, 256*1024)
	fillPattern(payload, 7)
	conn := h.Dial(20 * time.Second)
	defer conn.Close()
	echoAndHash(t, conn, payload, 60*time.Second)
}

// assertIdleConnClosedByServer connects to a local proxy listener, optionally
// sends a partial handshake, and then goes silent. The server must close the
// conn on its own (handshake watchdog): a read error caused by OUR OWN deadline
// is distinguished from a server-side close and fails the assertion.
func assertIdleConnClosedByServer(t *testing.T, addr, label string, preSend []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("%s: dial: %v", label, err)
	}
	defer conn.Close()

	if len(preSend) > 0 {
		if _, err := conn.Write(preSend); err != nil {
			t.Fatalf("%s: pre-send: %v", label, err)
		}
	}

	const testBudget = 6 * time.Second
	_ = conn.SetDeadline(time.Now().Add(testBudget))
	start := time.Now()
	_, rerr := conn.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("%s: idle conn was not closed", label)
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("%s: hit our own %v read deadline — server never closed the stalled conn: %v",
			label, testBudget, rerr)
	}
	if elapsed > testBudget-time.Second {
		t.Fatalf("%s: close took too long (%v) — watchdog not bounded", label, elapsed)
	}
	t.Logf("%s: server closed stalled conn after %v (%v)", label, elapsed.Round(time.Millisecond), rerr)
}

// TestE2E_Lifecycle_Socks5IdleHandshakeClosed pins the local SOCKS5 handshake
// watchdog: stalled conns are not registered sonny yet, so without the timeout
// they pin a goroutine+fd forever (slowloris). A valid handshake after a
// watchdog sweep must still work.
func TestE2E_Lifecycle_Socks5IdleHandshakeClosed(t *testing.T) {
	cfg := testConfig("socks5-idle-handshake-secret")
	cfg.AuthTimeout = 1
	h := startSocks5Proxy(t, "tcp", cfg)
	defer h.Close()

	// Peer sends nothing at all.
	assertIdleConnClosedByServer(t, h.clientAddr, "socks5/no-bytes", nil)
	// Peer sends only the greeting VER and stalls mid-handshake.
	assertIdleConnClosedByServer(t, h.clientAddr, "socks5/partial-greeting", []byte{0x05})

	// Watchdog must not kill a healthy, fully-handshaked session.
	conn := socks5Connect(t, h.clientAddr, h.echoAddr, 5*time.Second)
	defer conn.Close()
	echoRoundTrip(t, conn, []byte("post-watchdog-socks5"), 10*time.Second)
}

// TestE2E_Lifecycle_HTTPIdleHandshakeClosed is the HTTP-inputer counterpart:
// silence and half-written request lines are bounded; an authorized CONNECT
// after the sweep still proxies.
func TestE2E_Lifecycle_HTTPIdleHandshakeClosed(t *testing.T) {
	cfg := testConfig("http-idle-handshake-secret")
	cfg.AuthTimeout = 1
	h := startHTTPProxy(t, "tcp", cfg)
	defer h.Close()

	assertIdleConnClosedByServer(t, h.clientAddr, "http/no-bytes", nil)
	assertIdleConnClosedByServer(t, h.clientAddr, "http/partial-request",
		[]byte("CONNECT example.com:4"))

	conn := httpConnect(t, h.clientAddr, h.echoAddr, "", "", 5*time.Second)
	defer conn.Close()
	echoRoundTrip(t, conn, []byte("post-watchdog-http"), 10*time.Second)
}

// startReverseSocks5Pair boots a tunnel server and a REVERSE_SOCKS5 client:
// the client asks the server to expose a SOCKS5 listener at exposeAddr, and
// CONNECT requests arriving there are dialed out from the client side. The
// returned echoAddr is reachable "inside" the client network.
func startReverseSocks5Pair(t *testing.T, cfg *Config) (echoAddr, exposeAddr string) {
	t.Helper()
	var stopEcho func()
	echoAddr, stopEcho = startTCPEchoServer(t)
	t.Cleanup(stopEcho)

	serverAddr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	exposeAddr = fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	client, err := NewClient(cfg, "tcp", serverAddr, "e2e_reverse_socks5",
		"REVERSE_SOCKS5", []string{"tcp"}, []string{exposeAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient REVERSE_SOCKS5: %v", err)
	}
	t.Cleanup(client.Close)

	probe, err := waitForPort(exposeAddr, 15*time.Second)
	if err != nil {
		t.Fatalf("wait reverse socks5 expose addr %s: %v", exposeAddr, err)
	}
	probe.Close()
	return echoAddr, exposeAddr
}

// socks5ConnectAuth dials a SOCKS5 proxy, performs RFC 1928 method negotiation
// plus RFC 1929 username/password authentication (empty creds = no-auth), then
// a CONNECT to targetAddr. The returned stream carries application data.
func socks5ConnectAuth(t *testing.T, proxyAddr, targetAddr, user, pass string, timeout time.Duration) net.Conn {
	t.Helper()
	raw, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		t.Fatalf("socks5 dial: %v", err)
	}
	tcpConn := raw.(*net.TCPConn)
	ms := int(timeout / time.Millisecond)
	if err := network.Sock5Handshake(tcpConn, ms, user, pass); err != nil {
		tcpConn.Close()
		t.Fatalf("socks5 handshake: %v", err)
	}
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		tcpConn.Close()
		t.Fatalf("split target addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		tcpConn.Close()
		t.Fatalf("target port: %v", err)
	}
	if err := network.Sock5SetRequest(tcpConn, host, port, ms); err != nil {
		tcpConn.Close()
		t.Fatalf("socks5 connect request: %v", err)
	}
	return tcpConn
}

// TestE2E_ReverseSOCKS5_Connect covers the previously untested REVERSE_SOCKS5
// mode: a SOCKS5 listener exposed on the SERVER side tunnels CONNECT requests
// to destinations reachable only from the client side. Two sequential sonny
// conns also prove the exposed listener keeps serving over one tunnel.
func TestE2E_ReverseSOCKS5_Connect(t *testing.T) {
	cfg := testConfig("reverse-socks5-connect-secret")
	echoAddr, exposeAddr := startReverseSocks5Pair(t, cfg)

	for i := 0; i < 2; i++ {
		conn := socks5Connect(t, exposeAddr, echoAddr, 10*time.Second)
		payload := []byte(fmt.Sprintf("reverse-socks5-connect-%d", i))
		echoRoundTrip(t, conn, payload, 10*time.Second)
		conn.Close()
	}
}

// TestE2E_Lifecycle_ReverseSOCKS5IdleHandshakeClosed verifies the handshake
// watchdog on the SERVER-exposed reverse SOCKS5 listener (the public-internet
// attack surface): a stalled peer is dropped at AuthTimeout; healthy traffic
// afterwards still flows through the same tunnel.
func TestE2E_Lifecycle_ReverseSOCKS5IdleHandshakeClosed(t *testing.T) {
	cfg := testConfig("reverse-socks5-idle-secret")
	cfg.AuthTimeout = 1
	echoAddr, exposeAddr := startReverseSocks5Pair(t, cfg)

	assertIdleConnClosedByServer(t, exposeAddr, "reverse-socks5/no-bytes", nil)
	assertIdleConnClosedByServer(t, exposeAddr, "reverse-socks5/partial-greeting", []byte{0x05})

	conn := socks5Connect(t, exposeAddr, echoAddr, 5*time.Second)
	defer conn.Close()
	echoRoundTrip(t, conn, []byte("post-watchdog-reverse-socks5"), 10*time.Second)
}

// TestE2E_SOCKS5_ConnectUserPassAuth covers RFC 1929 username/password auth on
// the SOCKS5 TCP CONNECT path (previously only UDP ASSOCIATE had auth e2e):
// valid creds proxy data, wrong creds are rejected and the conn closes, a
// no-auth-only client gets X'FF', and the listener stays healthy afterwards.
func TestE2E_SOCKS5_ConnectUserPassAuth(t *testing.T) {
	cfg := testConfig("socks5-connect-auth-secret")
	cfg.Username = "sppuser"
	cfg.Password = "spppass"
	h := startSocks5Proxy(t, "tcp", cfg)
	defer h.Close()

	// 1. Valid credentials: full handshake + CONNECT + echo.
	good := socks5ConnectAuth(t, h.clientAddr, h.echoAddr, cfg.Username, cfg.Password, 5*time.Second)
	echoRoundTrip(t, good, []byte("auth-connect-ok"), 10*time.Second)
	good.Close()

	// 2. Wrong password: gohome client surfaces the RFC 1929 failure status;
	// the server must then close the conn rather than keep it around.
	raw, err := net.DialTimeout("tcp", h.clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial for bad-auth: %v", err)
	}
	if err := network.Sock5Handshake(raw.(*net.TCPConn), 3000, cfg.Username, "wrong-password"); err == nil {
		raw.Close()
		t.Fatal("expected RFC1929 auth failure for wrong password")
	}
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, rerr := raw.Read(make([]byte, 1)); rerr == nil {
		t.Fatal("server kept conn open after failed auth")
	}
	raw.Close()

	// 3. Client offering only NO-AUTH against an auth-required proxy must
	// receive method selection X'FF' (no acceptable methods).
	noAuth, err := net.DialTimeout("tcp", h.clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial for no-auth offer: %v", err)
	}
	if _, err := noAuth.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting write: %v", err)
	}
	sel := make([]byte, 2)
	if _, err := io.ReadFull(noAuth, sel); err != nil {
		t.Fatalf("method selection read: %v", err)
	}
	if sel[0] != 0x05 || sel[1] != 0xFF {
		t.Fatalf("want method selection {0x05, 0xFF}, got %v", sel)
	}
	noAuth.Close()

	// 4. Rejected attempts must not disturb the listener.
	after := socks5ConnectAuth(t, h.clientAddr, h.echoAddr, cfg.Username, cfg.Password, 5*time.Second)
	defer after.Close()
	echoRoundTrip(t, after, []byte("auth-still-ok"), 10*time.Second)
}
