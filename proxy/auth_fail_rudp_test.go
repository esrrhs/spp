package proxy

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
)

func getFreeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func authFailAttemptRudp(t *testing.T, addr string) network.Conn {
	t.Helper()
	dialer, err := network.NewConn("rudp")
	if dialer == nil {
		t.Fatalf("NewConn rudp: %v", err)
	}
	conn, err := dialWithTimeout(dialer, addr, 10)
	if err != nil {
		t.Fatalf("rudp dial %s: %v", addr, err)
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
			Name:         "rudp-attacker",
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

func runPprofTop(profPath string) (string, error) {
	cmd := exec.Command("go", "tool", "pprof", "-top", "-cum", profPath)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestAuthFailFlood_RudpHoldOpenReleased: after bad LOGIN, server must release
// RUDP pipes even if the client keeps the Conn open.
func TestAuthFailFlood_RudpHoldOpenReleased(t *testing.T) {
	port := getFreeUDPPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.Key = "auth-fail-rudp-hold-secret"
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.CompressType = CompressNone
	cfg.AuthTimeout = 5

	srv, err := NewServer(cfg, []string{"rudp"}, []string{addr})
	if err != nil {
		t.Fatalf("NewServer rudp: %v", err)
	}
	defer srv.Close()
	time.Sleep(300 * time.Millisecond)

	runtime.GC()
	var baseMem runtime.MemStats
	runtime.ReadMemStats(&baseMem)
	baseG := runtime.NumGoroutine()

	const holders = 40
	conns := make([]network.Conn, 0, holders)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	start := time.Now()
	for i := 0; i < holders; i++ {
		conns = append(conns, authFailAttemptRudp(t, addr))
	}
	dialElapsed := time.Since(start)

	gAfterDial := runtime.NumGoroutine()

	profPath := filepath.Join(t.TempDir(), "auth_fail_rudp_hold.pprof")
	keepPath := filepath.Join("..", "auth_fail_rudp_hold.pprof")

	f, err := os.Create(profPath)
	if err != nil {
		t.Fatalf("create pprof: %v", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		t.Fatalf("StartCPUProfile: %v", err)
	}

	// Profile while waiting for NeedClose teardown (~1s ticker).
	peakDuring := sampleMaxGoroutines(3*time.Second, 100*time.Millisecond)

	pprof.StopCPUProfile()
	f.Close()

	// Client-side RUDP Conn still runs update loops while held; close them and
	// confirm the process recovers (server pipes already NeedClose'd above).
	for _, c := range conns {
		c.Close()
	}
	conns = nil

	if !waitGoroutinesNear(baseG, 30, 8*time.Second) {
		t.Fatalf("goroutines not recovered after auth-fail + client close: g=%d base=%d", runtime.NumGoroutine(), baseG)
	}

	runtime.GC()
	var midMem runtime.MemStats
	runtime.ReadMemStats(&midMem)

	if data, err := os.ReadFile(profPath); err == nil {
		_ = os.WriteFile(keepPath, data, 0o644)
	}

	t.Logf("rudp_hold_released holders=%d dial_elapsed=%s baseline_g=%d after_dial_g=%d peak_during=%d after=%d",
		holders, dialElapsed.Round(time.Millisecond), baseG, gAfterDial, peakDuring, runtime.NumGoroutine())
	t.Logf("mem heap_inuse=%dMB sys=%dMB alloc_delta=%dMB clientNum=%d pprof=%s",
		midMem.HeapInuse/1024/1024,
		midMem.Sys/1024/1024,
		int64(midMem.TotalAlloc-baseMem.TotalAlloc)/1024/1024,
		srv.clientSize(),
		keepPath)

	if srv.clientSize() != 0 {
		t.Fatalf("clientNum=%d want 0", srv.clientSize())
	}

	out, err := runPprofTop(profPath)
	if err != nil {
		t.Logf("pprof top error: %v\n%s", err, out)
	} else {
		t.Logf("pprof -top -cum:\n%s", out)
	}
}

func TestAuthFailFlood_RudpWrongKeyClients(t *testing.T) {
	port := getFreeUDPPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.Key = "auth-fail-rudp-flood-secret"
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.CompressType = CompressNone
	cfg.AuthTimeout = 5

	srv, err := NewServer(cfg, []string{"rudp"}, []string{addr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	time.Sleep(300 * time.Millisecond)
	baseG := runtime.NumGoroutine()

	const nClients = 20
	const duration = 10 * time.Second

	var clients []*Client
	for i := 0; i < nClients; i++ {
		cport := getFreePort(t)
		caddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cport))
		ccfg := DefaultConfig()
		ccfg.Key = "wrong-rudp-client-key"
		ccfg.Encrypt = ""
		ccfg.EncryptType = EncryptNone
		ccfg.CompressType = CompressNone
		c, err := NewClient(ccfg, []string{"rudp"}, []string{addr}, "bad-rudp", "PROXY",
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

	profPath := filepath.Join(t.TempDir(), "auth_fail_rudp_wrongkey.pprof")
	keepPath := filepath.Join("..", "auth_fail_rudp_wrongkey.pprof")
	f, err := os.Create(profPath)
	if err != nil {
		t.Fatalf("create pprof: %v", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		t.Fatalf("StartCPUProfile: %v", err)
	}
	peakG := sampleMaxGoroutines(duration, 100*time.Millisecond)
	pprof.StopCPUProfile()
	f.Close()
	if data, err := os.ReadFile(profPath); err == nil {
		_ = os.WriteFile(keepPath, data, 0o644)
	}

	t.Logf("rudp_wrong_key clients=%d duration=%s baseline_g=%d peak_g=%d delta=%d clientNum=%d pprof=%s",
		nClients, duration, baseG, peakG, peakG-baseG, srv.clientSize(), keepPath)

	out, err := runPprofTop(profPath)
	if err != nil {
		t.Logf("pprof top error: %v\n%s", err, out)
	} else {
		t.Logf("pprof -top -cum:\n%s", out)
	}

	if srv.clientSize() != 0 {
		t.Fatalf("clientNum=%d want 0", srv.clientSize())
	}
}
