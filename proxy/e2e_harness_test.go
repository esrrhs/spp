package proxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
)

// e2eHarness boots a proxy pair for E2E scenarios.
type e2eHarness struct {
	t            *testing.T
	proto        string
	cfg          *Config
	echoAddr     string
	stopEcho     func()
	serverAddrs  []string
	clientAddr   string // local listen (PROXY/SOCKS5/HTTP fromaddr) or reverse expose addr
	server       *Server
	client       *Client
	mode         string
}

func freeListenAddr(t *testing.T, proto string) string {
	t.Helper()
	switch proto {
	case "tcp", "rhttp":
		return fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	case "rudp", "kcp", "quic":
		return fmt.Sprintf("127.0.0.1:%d", getFreeUDPPort(t))
	case "ricmp":
		// ricmp binds ICMP; address is host only.
		return "127.0.0.1"
	default:
		t.Fatalf("unsupported underlay proto %q", proto)
		return ""
	}
}

func testConfig(key string) *Config {
	cfg := DefaultConfig()
	cfg.Key = key
	cfg.Encrypt = ""
	cfg.EncryptType = EncryptNone
	cfg.MaxSonny = 200000
	cfg.MaxClient = 200000
	return cfg
}

func withCodec(cfg *Config, compress COMPRESS_TYPE, encrypt ENCRYPT_TYPE, encKey string) *Config {
	cfg.CompressType = compress
	if compress == CompressNone {
		cfg.Compress = 0
	} else {
		cfg.Compress = 128
	}
	cfg.EncryptType = encrypt
	cfg.Encrypt = encKey
	if encKey == "" {
		cfg.EncryptType = EncryptNone
	}
	return cfg
}

// CI-safe core underlays (no privileges).
var matrixUnderlaysCore = []string{"tcp", "rudp", "kcp", "quic"}

// Full underlay set for matrix (rhttp needs nothing special; ricmp needs CAP_NET_RAW/root).
var matrixUnderlays = []string{"tcp", "rudp", "kcp", "quic", "rhttp", "ricmp"}

var matrixModes = []string{"PROXY", "SOCKS5", "HTTP", "REVERSE_PROXY"}

type matrixCodec struct {
	name     string
	compress COMPRESS_TYPE
	encrypt  ENCRYPT_TYPE
	encKey   string
}

// Full compress × encrypt cartesian (excluding UNSPECIFIED).
func matrixCodecs() []matrixCodec {
	comps := []struct {
		name string
		t    COMPRESS_TYPE
	}{
		{"none", CompressNone},
		{"zlib", CompressZlib},
		{"zstd", CompressZstd},
	}
	encs := []struct {
		name string
		t    ENCRYPT_TYPE
		key  string
	}{
		{"none", EncryptNone, ""},
		{"aesgcm", EncryptAESGCM, "matrix-aesgcm-secret-ok"},
		{"chacha", EncryptChaCha20, "matrix-chacha-secret-ok"},
	}
	out := make([]matrixCodec, 0, len(comps)*len(encs))
	for _, c := range comps {
		for _, e := range encs {
			out = append(out, matrixCodec{
				name:     c.name + "/" + e.name,
				compress: c.t,
				encrypt:  e.t,
				encKey:   e.key,
			})
		}
	}
	return out
}

// selectedUnderlays honors SPP_MATRIX_UNDERLAY for CI shards (e.g. "tcp", "ricmp").
func selectedUnderlays() []string {
	if v := strings.TrimSpace(os.Getenv("SPP_MATRIX_UNDERLAY")); v != "" {
		return []string{v}
	}
	return matrixUnderlays
}

func startModeProxy(t *testing.T, mode, proto string, cfg *Config) *e2eHarness {
	t.Helper()
	switch mode {
	case "PROXY":
		return startForwardProxyCfg(t, proto, cfg)
	case "SOCKS5":
		return startSocks5Proxy(t, proto, cfg)
	case "HTTP":
		cfg.Username = ""
		cfg.Password = ""
		return startHTTPProxy(t, proto, cfg)
	case "REVERSE_PROXY":
		return startReverseProxy(t, proto, cfg)
	default:
		t.Fatalf("unknown mode %s", mode)
		return nil
	}
}

func echoViaMode(t *testing.T, h *e2eHarness, mode string, payload []byte, timeout time.Duration) {
	t.Helper()
	switch mode {
	case "SOCKS5":
		c := socks5Connect(t, h.clientAddr, h.echoAddr, timeout)
		defer c.Close()
		echoAndHash(t, c, payload, timeout)
	case "HTTP":
		c := httpConnect(t, h.clientAddr, h.echoAddr, "", "", timeout)
		defer c.Close()
		echoAndHash(t, c, payload, timeout)
	default: // PROXY, REVERSE_PROXY
		c := h.Dial(timeout)
		defer c.Close()
		echoAndHash(t, c, payload, timeout)
	}
}

func startForwardProxy(t *testing.T, proto, key string) *e2eHarness {
	t.Helper()
	return startProxyMode(t, proto, testConfig(key), "PROXY", nil)
}

func startForwardProxyCfg(t *testing.T, proto string, cfg *Config) *e2eHarness {
	t.Helper()
	return startProxyMode(t, proto, cfg, "PROXY", nil)
}

func startSocks5Proxy(t *testing.T, proto string, cfg *Config) *e2eHarness {
	t.Helper()
	return startProxyMode(t, proto, cfg, "SOCKS5", nil)
}

func startHTTPProxy(t *testing.T, proto string, cfg *Config) *e2eHarness {
	t.Helper()
	return startProxyMode(t, proto, cfg, "HTTP", nil)
}

func startReverseProxy(t *testing.T, proto string, cfg *Config) *e2eHarness {
	t.Helper()
	return startProxyMode(t, proto, cfg, "REVERSE_PROXY", nil)
}

func startProxyMode(t *testing.T, proto string, cfg *Config, mode string, extraServerProto []string) *e2eHarness {
	t.Helper()
	h := &e2eHarness{t: t, proto: proto, cfg: cfg, mode: mode}
	h.echoAddr, h.stopEcho = startTCPEchoServer(t)

	protos := []string{proto}
	addrs := []string{freeListenAddr(t, proto)}
	if len(extraServerProto) > 0 {
		protos = append(protos, extraServerProto...)
		for _, p := range extraServerProto {
			addrs = append(addrs, freeListenAddr(t, p))
		}
	}
	h.serverAddrs = addrs
	h.clientAddr = fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

	var err error
	h.server, err = NewServer(h.cfg, protos, addrs)
	if err != nil {
		h.stopEcho()
		t.Fatalf("NewServer(%v): %v", protos, err)
	}

	var clientProtos, fromAddrs, toAddrs []string
	switch mode {
	case "PROXY":
		clientProtos = []string{"tcp"}
		fromAddrs = []string{h.clientAddr}
		toAddrs = []string{h.echoAddr}
	case "SOCKS5", "HTTP":
		clientProtos = []string{"tcp"}
		fromAddrs = []string{h.clientAddr}
		toAddrs = nil
	case "REVERSE_PROXY":
		clientProtos = []string{"tcp"}
		fromAddrs = []string{h.clientAddr} // exposed on server side via reverse
		toAddrs = []string{h.echoAddr}
	default:
		h.server.Close()
		h.stopEcho()
		t.Fatalf("unknown mode %s", mode)
	}

	h.client, err = NewClient(h.cfg, protos, addrs, "e2e_"+mode+"_"+proto, mode, clientProtos, fromAddrs, toAddrs)
	if err != nil {
		h.server.Close()
		h.stopEcho()
		t.Fatalf("NewClient(%s/%s): %v", mode, proto, err)
	}

	waitAddr := h.clientAddr
	conn, err := waitForPort(waitAddr, 15*time.Second)
	if err != nil {
		h.Close()
		t.Fatalf("wait %s %s/%s: %v", waitAddr, mode, proto, err)
	}
	conn.Close()
	return h
}

func startMultiPathProxy(t *testing.T, cfg *Config) *e2eHarness {
	t.Helper()
	h := &e2eHarness{t: t, proto: "tcp+tcp", cfg: cfg, mode: "PROXY"}
	h.echoAddr, h.stopEcho = startTCPEchoServer(t)
	addr1 := freeListenAddr(t, "tcp")
	addr2 := freeListenAddr(t, "tcp")
	h.serverAddrs = []string{addr1, addr2}
	h.clientAddr = fmt.Sprintf("127.0.0.1:%d", getFreePort(t))

	var err error
	h.server, err = NewServer(cfg, []string{"tcp", "tcp"}, h.serverAddrs)
	if err != nil {
		h.stopEcho()
		t.Fatalf("NewServer multipath: %v", err)
	}
	h.client, err = NewClient(cfg, []string{"tcp", "tcp"}, h.serverAddrs, "e2e_mp", "PROXY",
		[]string{"tcp"}, []string{h.clientAddr}, []string{h.echoAddr})
	if err != nil {
		h.server.Close()
		h.stopEcho()
		t.Fatalf("NewClient multipath: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		h.client.connMu.Lock()
		sess := h.client.serverconn
		n := 0
		if sess != nil && sess.hub != nil {
			n = sess.hub.liveCount()
		}
		h.client.connMu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			h.Close()
			t.Fatalf("timed out waiting for 2 pipes, have %d", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn, err := waitForPort(h.clientAddr, 5*time.Second)
	if err != nil {
		h.Close()
		t.Fatalf("wait multipath client: %v", err)
	}
	conn.Close()
	return h
}

func (h *e2eHarness) Close() {
	if h.client != nil {
		h.client.Close()
	}
	if h.server != nil {
		h.server.Close()
	}
	if h.stopEcho != nil {
		h.stopEcho()
	}
}

func (h *e2eHarness) Dial(timeout time.Duration) net.Conn {
	h.t.Helper()
	conn, err := waitForPort(h.clientAddr, timeout)
	if err != nil {
		h.t.Fatalf("dial proxy: %v", err)
	}
	return conn
}

func (h *e2eHarness) livePipes() int {
	h.client.connMu.Lock()
	defer h.client.connMu.Unlock()
	if h.client.serverconn == nil || h.client.serverconn.hub == nil {
		return 0
	}
	return h.client.serverconn.hub.liveCount()
}

func (h *e2eHarness) killOnePipe() {
	h.t.Helper()
	h.client.connMu.Lock()
	sess := h.client.serverconn
	h.client.connMu.Unlock()
	if sess == nil || sess.hub == nil {
		h.t.Fatal("no session hub")
	}
	pipes := sess.hub.snapshot()
	if len(pipes) == 0 {
		h.t.Fatal("no pipes to kill")
	}
	pipes[0].conn.Close()
}

func fillPattern(b []byte, seed byte) {
	for i := range b {
		b[i] = byte(i) + seed
	}
}

func echoRoundTrip(t *testing.T, conn net.Conn, payload []byte, timeout time.Duration) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatalf("echo integrity mismatch (len=%d)", len(payload))
	}
}

func echoAndHash(t *testing.T, conn net.Conn, payload []byte, timeout time.Duration) [32]byte {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	want := sha256.Sum256(payload)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read full: %v", err)
	}
	sum := sha256.Sum256(got)
	if sum != want {
		t.Fatalf("payload hash mismatch")
	}
	return sum
}

func socks5Connect(t *testing.T, proxyAddr, targetAddr string, timeout time.Duration) net.Conn {
	t.Helper()
	conn, err := socks5ConnectErr(proxyAddr, targetAddr, timeout)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return conn
}

func socks5ConnectErr(proxyAddr, targetAddr string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 greet: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil || resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 greet resp: %v %v", resp, err)
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", targetAddr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("resolve: %w", err)
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		ip4 = net.IPv4(127, 0, 0, 1)
	}
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ip4...)
	pb := make([]byte, 2)
	binary.BigEndian.PutUint16(pb, uint16(tcpAddr.Port))
	req = append(req, pb...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect: %w", err)
	}
	cr := make([]byte, 10)
	if _, err := io.ReadFull(conn, cr); err != nil || cr[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect resp: %v %v", cr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func httpConnect(t *testing.T, proxyAddr, targetAddr, user, pass string, timeout time.Duration) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		t.Fatalf("http dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var auth string
	if user != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", targetAddr, targetAddr, auth)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatalf("http CONNECT write: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		conn.Close()
		t.Fatalf("http CONNECT resp: %q err=%v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			t.Fatalf("http CONNECT headers: %v", err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}

func underlayMayNeedRoot(proto string) bool {
	return proto == "ricmp"
}

func canListenRicmp() bool {
	c, err := network.NewConn("ricmp")
	if err != nil {
		return false
	}
	defer c.Close()
	l, err := c.Listen("127.0.0.1")
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func skipIfUnderlayUnavailable(t *testing.T, proto string) {
	t.Helper()
	if underlayMayNeedRoot(proto) {
		if os.Geteuid() != 0 && !canListenRicmp() {
			t.Skipf("%s requires root/CAP_NET_RAW", proto)
		}
	}
}

func netemRequired() bool {
	if os.Getenv("SPP_REQUIRE_NETEM") == "1" {
		return true
	}
	return os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("SPP_NETEM_JOB") == "1"
}

func runTC(args ...string) (string, error) {
	cmd := exec.Command("tc", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), nil
	}
	// Permission / missing NET_ADMIN: retry with sudo (CI runners).
	sudoArgs := append([]string{"tc"}, args...)
	cmd = exec.Command("sudo", sudoArgs...)
	out2, err2 := cmd.CombinedOutput()
	if err2 != nil {
		return string(out) + string(out2), err
	}
	return string(out2), nil
}

// applyMildNetem installs a mild loss+reorder+delay qdisc on lo for the test lifetime.
// Upper-layer (rudp/kcp/quic) must still deliver intact streams.
func applyMildNetem(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		if netemRequired() {
			t.Fatal("netem required but GOOS is not linux")
		}
		t.Skip("netem only on linux")
	}
	if _, err := exec.LookPath("tc"); err != nil {
		if netemRequired() {
			t.Fatalf("tc required for netem: %v", err)
		}
		t.Skip("tc not available")
	}

	// Mild impairment: 20ms±5ms delay, 3% loss, ~10% reordered.
	args := []string{
		"qdisc", "replace", "dev", "lo", "root", "netem",
		"delay", "20ms", "5ms",
		"loss", "3%",
		"reorder", "10%", "25%",
	}
	if out, err := runTC(args...); err != nil {
		if netemRequired() {
			t.Fatalf("netem setup failed: %v (%s)", err, out)
		}
		t.Skipf("netem setup failed: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		_, _ = runTC("qdisc", "del", "dev", "lo", "root")
	})
}
