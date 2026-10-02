package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
)

// fakeSocksConn is a minimal network.Conn stub that only serves Info().
type fakeSocksConn struct {
	info string
}

func (f *fakeSocksConn) Read(p []byte) (int, error)          { return 0, io.EOF }
func (f *fakeSocksConn) Write(p []byte) (int, error)         { return len(p), nil }
func (f *fakeSocksConn) Close() error                        { return nil }
func (f *fakeSocksConn) Name() string                        { return "tcp" }
func (f *fakeSocksConn) Info() string                        { return f.info }
func (f *fakeSocksConn) Dial(string) (network.Conn, error)   { return nil, nil }
func (f *fakeSocksConn) Listen(string) (network.Conn, error) { return nil, nil }
func (f *fakeSocksConn) Accept() (network.Conn, error)       { return nil, nil }

func TestSocks5IPv6Helpers(t *testing.T) {
	cases := []struct {
		name     string
		info     string
		host     string
		bind     string
		zeroBind string
	}{
		{"ipv6 loopback", "[::1]:1080<--tcp-->[::1]:50000", "::1", "[::]:0", "[::]:0"},
		{"ipv6 global", "[2001:db8::1]:1080<--tcp-->[2001:db8::2]:50000", "2001:db8::1", "[::]:0", "[::]:0"},
		{"ipv6 link-local zone", "[fe80::1%lo0]:1080<--tcp-->[fe80::2%lo0]:50000", "fe80::1", "[::]:0", "[::]:0"},
		{"ipv6 wildcard listener", "[::]:1080<--tcp-->[::1]:50000", "::1", "[::]:0", "[::]:0"},
		{"ipv4 loopback", "127.0.0.1:1080<--tcp-->127.0.0.1:50000", "127.0.0.1", "0.0.0.0:0", "0.0.0.0:0"},
		{"ipv4 wildcard listener", "0.0.0.0:1080<--tcp-->127.0.0.1:50000", "127.0.0.1", "0.0.0.0:0", "0.0.0.0:0"},
		{"ipv4-mapped v6 counts as v4", "[::ffff:127.0.0.1]:1080<--tcp-->[::ffff:127.0.0.1]:50000", "127.0.0.1", "0.0.0.0:0", "0.0.0.0:0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeSocksConn{info: tc.info}
			if got := socks5RelayHost(c); got != tc.host {
				t.Errorf("relayHost = %q, want %q", got, tc.host)
			}
			if got := socks5RelayBindAddr(c); got != tc.bind {
				t.Errorf("relayBindAddr = %q, want %q", got, tc.bind)
			}
			if got := socks5ZeroBind(c); got != tc.zeroBind {
				t.Errorf("zeroBind = %q, want %q", got, tc.zeroBind)
			}
		})
	}

	if got := socks5RelayHost(nil); got != "127.0.0.1" {
		t.Errorf("nil relayHost = %q, want 127.0.0.1", got)
	}
	if got := socks5RelayBindAddr(nil); got != "0.0.0.0:0" {
		t.Errorf("nil relayBindAddr = %q, want 0.0.0.0:0", got)
	}

	if !socks5HostIsIPv6("::1") || !socks5HostIsIPv6("fe80::1%lo0") {
		t.Error("loopback/link-local v6 not detected as IPv6")
	}
	for _, h := range []string{"127.0.0.1", "::ffff:127.0.0.1", "", "localhost"} {
		if socks5HostIsIPv6(h) {
			t.Errorf("%q must not be detected as IPv6", h)
		}
	}
}

func TestGetSocks5RelayAddrIPv6(t *testing.T) {
	v6Conn := &fakeSocksConn{info: "[::1]:1080<--tcp-->[::1]:50000"}
	udp6 := &net.UDPAddr{IP: net.ParseIP("::"), Port: 5353}
	if got := getSocks5RelayAddr(udp6, v6Conn); got != "[::1]:5353" {
		t.Errorf("v6 relay addr = %q, want [::1]:5353", got)
	}

	v4Conn := &fakeSocksConn{info: "127.0.0.1:1080<--tcp-->127.0.0.1:50000"}
	udp4 := &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 5353}
	if got := getSocks5RelayAddr(udp4, v4Conn); got != "127.0.0.1:5353" {
		t.Errorf("v4 relay addr = %q, want 127.0.0.1:5353", got)
	}
}

func skipIfNoIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = l.Close()
}

func getFreeV6Port(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTCP6EchoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("failed to start v6 echo server: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		close(stop)
		_ = ln.Close()
	}
}

func startUDP6EchoServer(t *testing.T) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Fatalf("failed to start v6 UDP echo server: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				select {
				case <-stop:
					return
				default:
					return
				}
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String(), func() {
		close(stop)
		_ = pc.Close()
	}
}

// startIPv6Mode boots a fully IPv6 (underlay + local listener + target)
// proxy pair for PROXY/SOCKS5/HTTP modes.
func startIPv6Mode(t *testing.T, mode string) *e2eHarness {
	t.Helper()
	skipIfNoIPv6(t)

	h := &e2eHarness{t: t, proto: "tcp", mode: mode}
	h.cfg = testConfig("test-ipv6-secret-" + strings.ToLower(mode))
	h.echoAddr, h.stopEcho = startTCP6EchoServer(t)

	h.serverAddrs = []string{net.JoinHostPort("::1", strconv.Itoa(getFreeV6Port(t)))}
	h.clientAddr = net.JoinHostPort("::1", strconv.Itoa(getFreeV6Port(t)))

	var err error
	h.server, err = NewServer(h.cfg, []string{"tcp"}, h.serverAddrs)
	if err != nil {
		h.stopEcho()
		t.Fatalf("NewServer v6: %v", err)
	}

	var toAddrs []string
	if mode == "PROXY" {
		toAddrs = []string{h.echoAddr}
	}
	h.client, err = NewClient(h.cfg, []string{"tcp"}, h.serverAddrs, "e2e_ipv6_"+mode,
		mode, []string{"tcp"}, []string{h.clientAddr}, toAddrs)
	if err != nil {
		h.Close()
		t.Fatalf("NewClient v6(%s): %v", mode, err)
	}

	conn, err := waitForPort(h.clientAddr, 10*time.Second)
	if err != nil {
		h.Close()
		t.Fatalf("wait v6 client addr %s: %v", h.clientAddr, err)
	}
	conn.Close()
	return h
}

func TestE2E_IPv6_TCP_ForwardProxy(t *testing.T) {
	h := startIPv6Mode(t, "PROXY")
	defer h.Close()

	c := h.Dial(5 * time.Second)
	defer c.Close()
	echoAndHash(t, c, []byte("hello spp over ipv6 forward proxy"), 5*time.Second)
}

func TestE2E_IPv6_SOCKS5Connect(t *testing.T) {
	h := startIPv6Mode(t, "SOCKS5")
	defer h.Close()

	// Request the IPv6 echo target with ATYP=IP6 (socks5Connect encodes
	// the address family derived from the resolved target).
	c := socks5Connect(t, h.clientAddr, h.echoAddr, 5*time.Second)
	defer c.Close()
	echoAndHash(t, c, []byte("hello spp over ipv6 socks5"), 5*time.Second)
}

func TestE2E_IPv6_HTTPConnect(t *testing.T) {
	h := startIPv6Mode(t, "HTTP")
	defer h.Close()

	c := httpConnect(t, h.clientAddr, h.echoAddr, "", "", 5*time.Second)
	defer c.Close()
	echoAndHash(t, c, []byte("hello spp over ipv6 http connect"), 5*time.Second)
}

func TestE2E_IPv6_SOCKS5UDPAssociate(t *testing.T) {
	skipIfNoIPv6(t)

	echoAddr, stopEcho := startUDP6EchoServer(t)
	defer stopEcho()

	echoHost, echoPortStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort failed: %v", err)
	}
	echoPort, _ := strconv.Atoi(echoPortStr)

	cfg := DefaultConfig()
	cfg.Key = "test-ipv6-socks5-udp-secret"

	serverAddr := net.JoinHostPort("::1", strconv.Itoa(getFreeV6Port(t)))
	socksAddr := net.JoinHostPort("::1", strconv.Itoa(getFreeV6Port(t)))

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_ipv6_socks5_udp",
		"SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probe, err := waitForPort(socksAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("wait socks5 %s: %v", socksAddr, err)
	}
	probe.Close()

	tcpRaw, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("dial socks5: %v", err)
	}
	defer tcpRaw.Close()
	tcpConn := tcpRaw.(*net.TCPConn)

	if err := network.Sock5Handshake(tcpConn, 3000, "", ""); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	relayAddr, err := network.Sock5SetUDPRequest(tcpConn, "::", 0, 3000)
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	relayHost, _, err := net.SplitHostPort(relayAddr)
	if err != nil {
		t.Fatalf("relay addr %q unparseable: %v", relayAddr, err)
	}
	if !socks5HostIsIPv6(relayHost) {
		t.Fatalf("relay addr %q is not IPv6", relayAddr)
	}

	udpConn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("dial udp relay %s: %v", relayAddr, err)
	}
	defer udpConn.Close()

	msg := []byte("hello socks5 udp over ipv6")
	pkt, err := network.Sock5PackUDP(echoHost, echoPort, msg)
	if err != nil {
		t.Fatalf("pack udp: %v", err)
	}

	replyBuf := make([]byte, 2048)
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		_ = udpConn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := udpConn.Write(pkt); err != nil {
			t.Fatalf("write udp: %v", err)
		}
		n, err = udpConn.Read(replyBuf)
		if err == nil && n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || n == 0 {
		t.Fatalf("no UDP reply within deadline: %v", err)
	}

	host, port, data, err := network.Sock5UnpackUDP(replyBuf[:n])
	if err != nil {
		t.Fatalf("unpack udp: %v", err)
	}
	if host != echoHost || port != echoPort {
		t.Fatalf("udp reply source = %s:%d, want %s:%d", host, port, echoHost, echoPort)
	}
	if !bytes.Equal(data, msg) {
		t.Fatalf("udp payload mismatch: %q != %q", string(data), string(msg))
	}
}

// canListenRicmp6 reports whether a raw ip6:icmp socket can be opened here
// (requires root/CAP_NET_RAW plus a usable IPv6 stack).
func canListenRicmp6(t *testing.T) bool {
	t.Helper()
	c, err := network.NewConn("ricmp")
	if err != nil {
		return false
	}
	defer c.Close()
	ln, err := c.Listen("::1")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func TestE2E_IPv6_RICMP_ForwardProxy(t *testing.T) {
	skipIfNoIPv6(t)
	if !canListenRicmp6(t) {
		t.Skip("ricmp over IPv6 requires root/CAP_NET_RAW and an ip6:icmp stack")
	}

	h := &e2eHarness{t: t, proto: "ricmp", mode: "PROXY"}
	h.cfg = testConfig("test-ipv6-ricmp-secret")
	h.echoAddr, h.stopEcho = startTCP6EchoServer(t)

	// ricmp carries no port: both listen and server addresses are bare hosts.
	h.serverAddrs = []string{"::1"}
	h.clientAddr = net.JoinHostPort("::1", strconv.Itoa(getFreeV6Port(t)))

	var err error
	h.server, err = NewServer(h.cfg, []string{"ricmp"}, h.serverAddrs)
	if err != nil {
		h.stopEcho()
		t.Fatalf("NewServer v6 ricmp: %v", err)
	}

	h.client, err = NewClient(h.cfg, []string{"ricmp"}, h.serverAddrs, "e2e_ipv6_ricmp_PROXY",
		"PROXY", []string{"tcp"}, []string{h.clientAddr}, []string{h.echoAddr})
	if err != nil {
		h.Close()
		t.Fatalf("NewClient v6 ricmp: %v", err)
	}

	probe, err := waitForPort(h.clientAddr, 10*time.Second)
	if err != nil {
		h.Close()
		t.Fatalf("wait v6 client addr %s: %v", h.clientAddr, err)
	}
	probe.Close()

	c := h.Dial(10 * time.Second)
	defer c.Close()
	echoAndHash(t, c, []byte("hello spp over ricmp+ipv6"), 10*time.Second)
}

func TestSocks5ReplyIPv6WireFormat(t *testing.T) {
	// Verify the server-side reply writer emits a 22-byte ATYP=IP6 reply for
	// an IPv6 BND addr (vs 10 bytes for IPv4).
	cases := []struct {
		bnd    string
		atyp   byte
		length int
	}{
		{"[::]:0", 0x04, 4 + 16 + 2},
		{"0.0.0.0:0", 0x01, 4 + 4 + 2},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("atyp%d", tc.atyp), func(t *testing.T) {
			var buf bytes.Buffer
			if err := network.Sock5SendConnectReply(&buf, 0x00, tc.bnd); err != nil {
				t.Fatal(err)
			}
			if buf.Len() != tc.length {
				t.Fatalf("reply len = %d, want %d", buf.Len(), tc.length)
			}
			if buf.Bytes()[3] != tc.atyp {
				t.Fatalf("atyp = %d, want %d", buf.Bytes()[3], tc.atyp)
			}
		})
	}
}
