package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"
)

type frameConn interface {
	io.ReadWriter
}

func writeProxyFrame(t *testing.T, conn frameConn, f *ProxyFrame) {
	t.Helper()
	codec := FrameCodec{CompressType: CompressNone, EncryptType: EncryptNone}
	body, err := MarshalSrpFrame(f, codec)
	if err != nil {
		t.Fatalf("MarshalSrpFrame: %v", err)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
}

func readProxyFrame(t *testing.T, conn frameConn) *ProxyFrame {
	t.Helper()
	if sd, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = sd.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	done := make(chan struct{})
	var (
		f   *ProxyFrame
		err error
	)
	go func() {
		defer close(done)
		f, err = readProxyFrameSync(conn)
	}()
	select {
	case <-done:
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		return f
	case <-time.After(15 * time.Second):
		t.Fatal("read frame timeout")
		return nil
	}
}

func readProxyFrameSync(conn frameConn) (*ProxyFrame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > 1024*1024 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	codec := FrameCodec{CompressType: CompressNone, EncryptType: EncryptNone}
	return UnmarshalSrpFrame(body, codec)
}

// authFailAttempt dials server, completes challenge+bad LOGIN, returns the
// client-side socket (server should tear the pipe down via setNeedClose).
func authFailAttempt(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ch := readProxyFrame(t, conn)
	if ch.Type != FRAME_TYPE_AUTH_CHALLENGE {
		conn.Close()
		t.Fatalf("want AUTH_CHALLENGE got %v", ch.Type)
	}
	login := &ProxyFrame{
		Type: FRAME_TYPE_LOGIN,
		LoginFrame: &LoginFrame{
			Clienttype:   CLIENT_TYPE_PROXY,
			Name:         "attacker",
			AuthProof:    []byte("not-a-valid-hmac-proof!!!!!!!!!!!"),
			CompressType: CompressNone,
			EncryptType:  EncryptNone,
			Services: []*LoginService{{
				Proxyproto: PROXY_PROTO_TCP,
				Fromaddr:   ":0",
				Toaddr:     "127.0.0.1:1",
			}},
		},
	}
	writeProxyFrame(t, conn, login)
	rsp := readProxyFrame(t, conn)
	if rsp.Type != FRAME_TYPE_LOGINRSP || rsp.LoginRspFrame == nil || rsp.LoginRspFrame.Ret {
		conn.Close()
		t.Fatalf("want failed LOGINRSP, got %+v", rsp.LoginRspFrame)
	}
	return conn
}

func sampleMaxGoroutines(d time.Duration, every time.Duration) (max int) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if n := runtime.NumGoroutine(); n > max {
			max = n
		}
		time.Sleep(every)
	}
	return max
}

func waitGoroutinesNear(base, slack int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+slack {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestAuthFail_ClosesServerPipeQuickly: auth fail must tear down servePipe
// (checkNeedClose ~1s), even if the client keeps TCP open.
func TestAuthFail_ClosesServerPipeQuickly(t *testing.T) {
	port := getFreePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.Key = "auth-fail-close-secret"
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.CompressType = CompressNone
	cfg.AuthTimeout = 5

	srv, err := NewServer(cfg, []string{"tcp"}, []string{addr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	const holders = 20
	conns := make([]net.Conn, 0, holders)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < holders; i++ {
		conns = append(conns, authFailAttempt(t, addr))
	}

	if srv.clientSize() != 0 {
		t.Fatalf("clientNum=%d want 0 (auth failed)", srv.clientSize())
	}

	// checkNeedClose ticks every 1s; give a little slack for teardown.
	if !waitGoroutinesNear(base, holders+15, 4*time.Second) {
		t.Fatalf("server pipes not released after auth fail: g=%d base=%d", runtime.NumGoroutine(), base)
	}
	t.Logf("baseline_goroutines=%d after_release=%d holders=%d", base, runtime.NumGoroutine(), holders)
}

// TestAuthFailFlood_WrongKeyClients: many spp clients with wrong key reconnect.
func TestAuthFailFlood_WrongKeyClients(t *testing.T) {
	port := getFreePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.Key = "auth-fail-flood-server-secret"
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.CompressType = CompressNone
	cfg.AuthTimeout = 5

	srv, err := NewServer(cfg, []string{"tcp"}, []string{addr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	const nClients = 30
	const duration = 8 * time.Second

	var clients []*Client
	for i := 0; i < nClients; i++ {
		cport := getFreePort(t)
		caddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cport))
		ccfg := DefaultConfig()
		ccfg.Key = "wrong-key-not-matching-server"
		ccfg.Encrypt = ""
		ccfg.EncryptType = EncryptNone
		ccfg.CompressType = CompressNone
		c, err := NewClient(ccfg, []string{"tcp"}, []string{addr}, "bad", "PROXY",
			[]string{"tcp"}, []string{caddr}, []string{"127.0.0.1:9"})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()

	peak := sampleMaxGoroutines(duration, 100*time.Millisecond)
	maxClients := 0
	for i := 0; i < 20; i++ {
		if n := srv.clientSize(); n > maxClients {
			maxClients = n
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("wrong_key_clients=%d duration=%s baseline_g=%d peak_g=%d delta=%d max_clientNum=%d",
		nClients, duration, base, peak, peak-base, maxClients)
	if maxClients != 0 {
		t.Fatalf("auth-fail flood must not create sessions: clientNum max=%d", maxClients)
	}
}

// TestAuthFailFlood_HoldOpenReleased: attacker keeps sockets open after bad
// LOGIN; server must still release pipes (~1s NeedClose + AuthTimeout backup).
func TestAuthFailFlood_HoldOpenReleased(t *testing.T) {
	port := getFreePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.Key = "auth-fail-burst-secret"
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.CompressType = CompressNone
	cfg.AuthTimeout = 5

	srv, err := NewServer(cfg, []string{"tcp"}, []string{addr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	const holders = 50
	conns := make([]net.Conn, 0, holders)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	start := time.Now()
	for i := 0; i < holders; i++ {
		conns = append(conns, authFailAttempt(t, addr))
	}
	dialElapsed := time.Since(start)

	peakDuring := sampleMaxGoroutines(1500*time.Millisecond, 50*time.Millisecond)

	if !waitGoroutinesNear(base, holders+20, 5*time.Second) {
		t.Fatalf("hold-open burst pipes not released: g=%d base=%d", runtime.NumGoroutine(), base)
	}

	t.Logf("hold_open_released holders=%d dial_elapsed=%s baseline_g=%d peak_during=%d after=%d clientNum=%d",
		holders, dialElapsed, base, peakDuring, runtime.NumGoroutine(), srv.clientSize())

	if srv.clientSize() != 0 {
		t.Fatalf("clientNum=%d want 0", srv.clientSize())
	}
}
