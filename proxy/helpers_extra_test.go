package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
)

// ---- socks5 address helpers --------------------------------------------------

func TestSocks5HostIsIPv6(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", false},
		{"10.20.30.40", false},
		{"::ffff:127.0.0.1", false}, // v4-mapped counts as v4
		{"::1", true},
		{"2001:db8::1", true},
		{"fe80::1%en0", true}, // zone stripped before parse
		{"", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := socks5HostIsIPv6(tc.host); got != tc.want {
			t.Errorf("socks5HostIsIPv6(%q)=%v want %v", tc.host, got, tc.want)
		}
	}
}

func TestSocks5ControlLocalHost(t *testing.T) {
	if got := socks5ControlLocalHost(nil); got != "" {
		t.Fatalf("nil conn: got %q", got)
	}
	if got := socks5ControlLocalHost(newBlockDialConn("garbage-no-port")); got != "" {
		t.Fatalf("unparseable info: got %q", got)
	}
	if got := socks5ControlLocalHost(newBlockDialConn("127.0.0.1:51820<--tcp-->9.9.9.9:1")); got != "127.0.0.1" {
		t.Fatalf("v4 local host: got %q", got)
	}
	if got := socks5ControlLocalHost(newBlockDialConn("[::1]:1080<--tcp-->[fe80::1]:2")); got != "::1" {
		t.Fatalf("v6 local host: got %q", got)
	}
}

func TestSocks5RelayHost(t *testing.T) {
	cases := []struct {
		info string
		want string
	}{
		{"127.0.0.1:9<--tcp-->x:1", "127.0.0.1"},
		{"0.0.0.0:9<--tcp-->x:1", "127.0.0.1"}, // v4 wildcard → v4 loopback
		{"[::]:9<--tcp-->x:1", "::1"},          // v6 wildcard → v6 loopback
		{"[::1]:9<--tcp-->x:1", "::1"},
		{"[fe80::1%en0]:9<--tcp-->x:1", "fe80::1"},    // zone stripped
		{"[::ffff:1.2.3.4]:9<--tcp-->x:1", "1.2.3.4"}, // mapped normalized to v4
		{"not-an-address", "127.0.0.1"},
	}
	for _, tc := range cases {
		if got := socks5RelayHost(newBlockDialConn(tc.info)); got != tc.want {
			t.Errorf("socks5RelayHost(%q)=%q want %q", tc.info, got, tc.want)
		}
	}
}

func TestSocks5BindAddr_FamilySelection(t *testing.T) {
	v4 := newBlockDialConn("127.0.0.1:1080<--tcp-->x:1")
	v6 := newBlockDialConn("[::1]:1080<--tcp-->x:1")
	if got := socks5RelayBindAddr(v4); got != "0.0.0.0:0" {
		t.Fatalf("v4 relay bind: %s", got)
	}
	if got := socks5RelayBindAddr(v6); got != "[::]:0" {
		t.Fatalf("v6 relay bind: %s", got)
	}
	if got := socks5ZeroBind(v4); got != "0.0.0.0:0" {
		t.Fatalf("v4 zero bind: %s", got)
	}
	if got := socks5ZeroBind(v6); got != "[::]:0" {
		t.Fatalf("v6 zero bind: %s", got)
	}
}

func TestGetSocks5RelayAddr(t *testing.T) {
	// Real *net.UDPAddr path (v4).
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	got := getSocks5RelayAddr(pc.LocalAddr(), newBlockDialConn("127.0.0.1:1080<--tcp-->x:1"))
	host, port, err := net.SplitHostPort(got)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port != strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port) {
		t.Fatalf("udp relay addr %q", got)
	}

	// Non-UDP Addr falls back to String parsing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got = getSocks5RelayAddr(ln.Addr(), newBlockDialConn("127.0.0.1:1080<--tcp-->x:1"))
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("tcp addr fallback: %q", got)
	}

	// v6 control conn forces a v6 relay host.
	got = getSocks5RelayAddr(pc.LocalAddr(), newBlockDialConn("[::1]:1080<--tcp-->x:1"))
	if !strings.HasPrefix(got, "[::1]:") {
		t.Fatalf("v6 relay host expected, got %q", got)
	}
}

// ---- http helpers -----------------------------------------------------------

func TestCheckProxyAuth_MalformedHeaders(t *testing.T) {
	const user, pass = "u", "p"
	bad := []string{
		"",          // missing
		"Basic ",    // empty token
		"Basic !!!", // invalid base64
		"Basic " + base64.StdEncoding.EncodeToString([]byte("u:wrong")), // wrong creds
		"Bearer xyz", // wrong scheme
		"Bas x",      // prefix too short
	}
	for _, h := range bad {
		if checkProxyAuth(h, user, pass) {
			t.Fatalf("header %q should be rejected", h)
		}
	}
	// Whitespace around the token is tolerated.
	good := "Basic   " + base64.StdEncoding.EncodeToString([]byte("u:p")) + "  "
	if !checkProxyAuth(good, user, pass) {
		t.Fatal("trimmed valid header rejected")
	}
}

func TestParseHost(t *testing.T) {
	cases := []struct {
		in, def, want string
	}{
		{"example.com", "80", "example.com:80"},
		{"  example.com:8080  ", "80", "example.com:8080"},
		{"[::1]:443", "443", "[::1]:443"},
		{"[2001:db8::1]", "80", "[2001:db8::1]:80"},
		{"127.0.0.1:1", "443", "127.0.0.1:1"},
	}
	for _, tc := range cases {
		if got := parseHost(tc.in, tc.def); got != tc.want {
			t.Errorf("parseHost(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// readerConn is a network.Conn backed by an io.Reader for bufferedConn tests.
type readerConn struct {
	r    io.Reader
	info string
}

func (c *readerConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *readerConn) Write(p []byte) (int, error) { return 0, errors.New("read-only") }
func (c *readerConn) Close() error                { return nil }
func (c *readerConn) Name() string                { return "reader" }
func (c *readerConn) Info() string                { return c.info }
func (c *readerConn) Dial(string) (network.Conn, error) {
	return nil, errors.New("n/a")
}
func (c *readerConn) Listen(string) (network.Conn, error) {
	return nil, errors.New("n/a")
}
func (c *readerConn) Accept() (network.Conn, error) { return nil, errors.New("n/a") }

func TestBufferedConn_PrefixThenBufferThenUnderlying(t *testing.T) {
	// Prefix bytes are served first, then bytes already pulled into the
	// bufio.Reader, then reads fall through to the underlying conn.
	all := bytes.NewReader([]byte("ABCDEFGHIJ"))
	br := bufio.NewReaderSize(all, 4)
	if b, _ := br.ReadByte(); b != 'A' {
		t.Fatal("prime")
	}
	// After ReadByte: br holds "BCD" buffered, all still serves "EFGHIJ".
	wrapped := newPrefixedConn(&readerConn{r: all}, []byte("Z"), br)
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ZBCDEFGHIJ" {
		t.Fatalf("buffered read order wrong: %q", out)
	}
}

func TestBufferedConn_PrefixOnly(t *testing.T) {
	src := bytes.NewReader([]byte("ABCD"))
	wrapped := newPrefixedConn(&readerConn{r: src}, []byte("Z"), nil)
	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ZABCD" {
		t.Fatalf("got %q", out)
	}
}

// ---- multipath formatting ----------------------------------------------------

func TestFormatBps_AndItoa(t *testing.T) {
	cases := []struct {
		bps  int64
		want string
	}{
		{0, "0B/s"},
		{1, "1B/s"},
		{1023, "1023B/s"},
		{1500, "1KB/s"},
		{2 << 20, "2MB/s"},
		// Throughput tiers compare with >=, so pathological negative values
		// always render through the raw B/s path (itoa keeps the minus sign).
		{-1500, "-1500B/s"},
		{-1, "-1B/s"},
	}
	for _, tc := range cases {
		if got := formatBps(tc.bps); got != tc.want {
			t.Errorf("formatBps(%d)=%q want %q", tc.bps, got, tc.want)
		}
	}
	if got := itoa(0); got != "0" {
		t.Fatalf("itoa(0)=%q", got)
	}
}

func TestMainPipe_NoteRTT(t *testing.T) {
	p := &mainPipe{}
	p.noteRTT(0)
	p.noteRTT(-5)
	if atomic.LoadInt64(&p.rttNs) != 0 {
		t.Fatal("non-positive RTT must be ignored")
	}
	p.noteRTT(7 * time.Millisecond)
	if atomic.LoadInt64(&p.rttNs) != int64(7*time.Millisecond) {
		t.Fatal("positive RTT not stored")
	}
}

func TestChannelHub_StatusLine(t *testing.T) {
	hub := newChannelHub(DefaultConfig())
	a := &mainPipe{proto: "tcp", addr: "a"}
	b := &mainPipe{proto: "rudp", addr: "b"}
	hub.add(a)
	hub.add(b)
	atomic.StoreInt64(&a.thrBps, 1<<20)
	atomic.StoreInt64(&a.rttNs, int64(time.Millisecond))
	b.markGray("test gray")

	line := hub.statusLine()
	for _, want := range []string{"tcp=active", "rudp=gray", "thr=", "rtt="} {
		if !strings.Contains(line, want) {
			t.Fatalf("statusLine %q missing %q", line, want)
		}
	}
}

// ---- malformed HTTP clients must never kill the listener ---------------------

func TestE2E_HTTPProxy_MalformedRequests(t *testing.T) {
	cfg := testConfig("http-malformed-secret")
	h := startHTTPProxy(t, "tcp", cfg)
	defer h.Close()

	dialRaw := func(t *testing.T) net.Conn {
		t.Helper()
		c, err := net.DialTimeout("tcp", h.clientAddr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}

	// 1. Garbage request line: conn gets closed, nothing panics server-side.
	c1 := dialRaw(t)
	_ = c1.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c1.Write([]byte("GARBAGE\r\n\r\n"))
	buf := make([]byte, 16)
	if _, err := c1.Read(buf); err == nil {
		c1.Close()
		t.Fatal("malformed request line should close the conn")
	}
	c1.Close()

	// 2. Relative URI without Host: unroutable → conn closed.
	c2 := dialRaw(t)
	_ = c2.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c2.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	if _, err := c2.Read(buf); err == nil {
		c2.Close()
		t.Fatal("hostless relative request should close the conn")
	}
	c2.Close()

	// 3. Truncated headers followed by half-close: header EOF → close.
	c3 := dialRaw(t)
	_ = c3.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c3.Write([]byte("GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nX-Broken: "))
	_ = c3.(*net.TCPConn).CloseWrite()
	if _, err := c3.Read(buf); err == nil {
		c3.Close()
		t.Fatal("truncated headers should close the conn")
	}
	c3.Close()

	// Listener is still alive: a normal CONNECT succeeds afterwards.
	c4 := httpConnect(t, h.clientAddr, h.echoAddr, "", "", 5*time.Second)
	defer c4.Close()
	echoRoundTrip(t, c4, []byte("still-alive-after-garbage"), 5*time.Second)
}
