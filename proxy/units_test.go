package proxy

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

// ---- helpers ---------------------------------------------------------------

func newTestGroup(t *testing.T) *thread.Group {
	t.Helper()
	g := thread.NewGroup("test-"+t.Name(), nil, nil)
	t.Cleanup(func() {
		g.Stop()
		_ = g.Wait()
	})
	return g
}

func newFatherWithQueue(buf int) *ProxyConn {
	p := &ProxyConn{}
	p.sendq = newPrioQueue(buf)
	p.recvq = newPrioQueue(buf)
	return p
}

func popOpenRsp(t *testing.T, q *prioQueue, timeout time.Duration) *OpenConnRspFrame {
	t.Helper()
	v, closed, ok := q.PopWait(timeout)
	if !ok || closed {
		t.Fatalf("OPENRSP not delivered (ok=%v closed=%v)", ok, closed)
	}
	f := v.(*ProxyFrame)
	if f.Type != FRAME_TYPE_OPENRSP || f.OpenRspFrame == nil {
		t.Fatalf("expected OPENRSP, got %v", f.Type)
	}
	return f.OpenRspFrame
}

// blockDialConn is a network.Conn whose Dial blocks until Close, used to
// exercise dialWithTimeout without any real black-hole routing.
type blockDialConn struct {
	info   string
	closed chan struct{}
	once   chan struct{}
}

func newBlockDialConn(info string) *blockDialConn {
	return &blockDialConn{info: info, closed: make(chan struct{}), once: make(chan struct{})}
}

func (b *blockDialConn) Read(p []byte) (int, error)  { <-b.closed; return 0, io.EOF }
func (b *blockDialConn) Write(p []byte) (int, error) { <-b.closed; return 0, io.EOF }
func (b *blockDialConn) Close() error {
	select {
	case <-b.once:
	default:
		close(b.once)
	}
	close(b.closed)
	return nil
}
func (b *blockDialConn) Name() string { return "blockdial" }
func (b *blockDialConn) Info() string { return b.info }
func (b *blockDialConn) Dial(dst string) (network.Conn, error) {
	<-b.closed
	return nil, errors.New("dial aborted by close")
}
func (b *blockDialConn) Listen(dst string) (network.Conn, error) {
	return nil, errors.New("not supported")
}
func (b *blockDialConn) Accept() (network.Conn, error) {
	<-b.closed
	return nil, errors.New("accept aborted by close")
}

// ---- dialWithTimeout --------------------------------------------------------

func TestDialWithTimeout_Success(t *testing.T) {
	echoAddr, stop := startTCPEchoServer(t)
	defer stop()
	c, err := network.NewConn("tcp")
	if err != nil {
		t.Fatal(err)
	}
	got, err := dialWithTimeout(c, echoAddr, 3)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer got.Close()
	payload := []byte("dial-timeout-ok")
	if _, err := got.Write(payload); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(got, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(payload) {
		t.Fatal("echo mismatch")
	}
}

func TestDialWithTimeout_AbortsBlockingDial(t *testing.T) {
	block := newBlockDialConn("127.0.0.1:1<--blockdial-->x")
	start := time.Now()
	_, err := dialWithTimeout(block, "10.0.0.1:80", 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected dial timeout error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
	if elapsed < 800*time.Millisecond {
		t.Fatalf("returned before the configured timeout: %s", elapsed)
	}
}

func TestDialWithTimeout_NonPositiveClampsToDefault(t *testing.T) {
	block := newBlockDialConn("clamp")
	done := make(chan error, 1)
	go func() {
		_, err := dialWithTimeout(block, "x", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("non-positive timeout should clamp to 10s, not hang forever")
	}
}

// ---- Outputer ---------------------------------------------------------------

func TestNewOutputer_InvalidProto(t *testing.T) {
	g := newTestGroup(t)
	if _, err := NewOutputer(g, "bogus-proto", CLIENT_TYPE_PROXY, DefaultConfig(), newFatherWithQueue(4), 0); err == nil {
		t.Fatal("invalid proto must error")
	}
	if _, err := NewSSOutputer(g, "bogus-proto", CLIENT_TYPE_SS_PROXY, DefaultConfig(), newFatherWithQueue(4), 0); err == nil {
		t.Fatal("invalid proto must error")
	}
}

func TestOutputer_SS_NoEnvRejected(t *testing.T) {
	t.Setenv("SS_LOCAL_HOST", "")
	t.Setenv("SS_LOCAL_PORT", "")
	g := newTestGroup(t)
	o, err := NewSSOutputer(g, "tcp", CLIENT_TYPE_SS_PROXY, DefaultConfig(), newFatherWithQueue(8), 0)
	if err != nil {
		t.Fatal(err)
	}
	o.processOpenFrame(&ProxyFrame{
		Type:      FRAME_TYPE_OPEN,
		OpenFrame: &OpenConnFrame{Id: "ss1", Toaddr: "ignored:80"},
	})
	rsp := popOpenRsp(t, o.father.sendq, time.Second)
	if rsp.Ret || rsp.Msg != "ss no env" {
		t.Fatalf("got ret=%v msg=%q", rsp.Ret, rsp.Msg)
	}
}

func TestOutputer_MaxSonnyRejected(t *testing.T) {
	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.MaxSonny = 0
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, cfg, newFatherWithQueue(8), 0)
	if err != nil {
		t.Fatal(err)
	}
	o.processOpenFrame(&ProxyFrame{
		Type:      FRAME_TYPE_OPEN,
		OpenFrame: &OpenConnFrame{Id: "c1", Toaddr: "127.0.0.1:1"},
	})
	rsp := popOpenRsp(t, o.father.sendq, time.Second)
	if rsp.Ret || rsp.Msg != "max sonny" {
		t.Fatalf("got ret=%v msg=%q", rsp.Ret, rsp.Msg)
	}
}

func TestOutputer_DuplicateIDRejected(t *testing.T) {
	g := newTestGroup(t)
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, DefaultConfig(), newFatherWithQueue(8), 0)
	if err != nil {
		t.Fatal(err)
	}
	o.sonny.Store("dup", &ProxyConn{id: "dup"})
	o.processOpenFrame(&ProxyFrame{
		Type:      FRAME_TYPE_OPEN,
		OpenFrame: &OpenConnFrame{Id: "dup", Toaddr: "127.0.0.1:1"},
	})
	rsp := popOpenRsp(t, o.father.sendq, time.Second)
	if rsp.Ret || rsp.Msg != "Conn id fail" {
		t.Fatalf("got ret=%v msg=%q", rsp.Ret, rsp.Msg)
	}
}

func TestOutputer_DialFailureReported(t *testing.T) {
	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.ConnectTimeout = 1
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, cfg, newFatherWithQueue(8), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Port 1 on loopback is refused immediately; if it somehow accepts, the
	// OPENRSP is still valid either way, so assert on the failure contract.
	o.processOpenFrame(&ProxyFrame{
		Type:      FRAME_TYPE_OPEN,
		OpenFrame: &OpenConnFrame{Id: "df", Toaddr: "127.0.0.1:1"},
	})
	rsp := popOpenRsp(t, o.father.sendq, 5*time.Second)
	if rsp.Ret {
		t.Skip("port 1 unexpectedly accepted a connection")
	}
	if rsp.Msg == "" || rsp.Id != "df" {
		t.Fatalf("bad failure rsp: %+v", rsp)
	}
}

func TestOutputer_OpenSuccessAndLifecycle(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.ConnectTimeout = 5
	father := newFatherWithQueue(8)
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, cfg, father, 2)
	if err != nil {
		t.Fatal(err)
	}
	o.processOpenFrame(&ProxyFrame{
		Type:      FRAME_TYPE_OPEN,
		OpenFrame: &OpenConnFrame{Id: "ok1", Toaddr: echoAddr, Proxyproto: PROXY_PROTO_TCP},
	})
	rsp := popOpenRsp(t, father.sendq, 5*time.Second)
	if !rsp.Ret {
		t.Fatalf("open failed: %s", rsp.Msg)
	}

	// Sonny accounting tracks the new connection.
	deadline := time.Now().Add(3 * time.Second)
	for o.sonnySize() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o.sonnySize() != 1 || !o.hasSonny("ok1") {
		t.Fatalf("sonny not registered: size=%d", o.sonnySize())
	}

	// Close must mark all sonnies needclose and close the proto conn.
	o.Close()
}

func TestOutputer_FrameRoutingUnknownAndStuckSonny(t *testing.T) {
	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.MainWriteChannelTimeoutMs = 10
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, cfg, newFatherWithQueue(8), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown ids are no-ops (must not panic).
	o.processDataFrame(&ProxyFrame{Type: FRAME_TYPE_DATA, DataFrame: &DataFrame{Id: "x", Data: []byte("d")}})
	o.processCloseFrame(&ProxyFrame{Type: FRAME_TYPE_CLOSE, CloseFrame: &CloseFrame{Id: "x"}})

	// A sonny whose send channel is full gets flagged needclose on data.
	stuck := &ProxyConn{id: "stuck"}
	stuck.sendch = newMsgChannel(1)
	stuck.sendch.Write("occupant")
	o.sonny.Store("stuck", stuck)
	o.processDataFrame(&ProxyFrame{Type: FRAME_TYPE_DATA, DataFrame: &DataFrame{Id: "stuck", Data: []byte("d")}})
	if !stuck.isNeedClose() {
		t.Fatal("stuck sonny should be marked needclose")
	}

	// Close frame for a known sonny is delivered to its channel.
	other := &ProxyConn{id: "other"}
	other.sendch = newMsgChannel(2)
	o.sonny.Store("other", other)
	o.processCloseFrame(&ProxyFrame{Type: FRAME_TYPE_CLOSE, CloseFrame: &CloseFrame{Id: "other"}})
	ff := <-other.sendch.Ch()
	if ff.(*ProxyFrame).Type != FRAME_TYPE_CLOSE {
		t.Fatal("close frame not delivered")
	}

	// Negative accounting is clamped.
	atomic.StoreInt32(&o.sonnyNum, -7)
	if o.sonnySize() != 0 {
		t.Fatalf("negative sonny count should clamp to 0, got %d", o.sonnySize())
	}
}

// ---- Inputer ----------------------------------------------------------------

func TestNewInputer_InvalidProto(t *testing.T) {
	g := newTestGroup(t)
	_, err := NewInputer(g, "bogus", "127.0.0.1:0", CLIENT_TYPE_PROXY, DefaultConfig(), newFatherWithQueue(4), "x", 0)
	if err == nil {
		t.Fatal("invalid proto must error")
	}
	if _, err := NewHttpInputer(g, "bogus", "127.0.0.1:0", CLIENT_TYPE_HTTP, DefaultConfig(), newFatherWithQueue(4), 0); err == nil {
		t.Fatal("http inputer invalid proto must error")
	}
}

func TestInputer_FrameRoutingAndOpen(t *testing.T) {
	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.MainWriteChannelTimeoutMs = 10
	father := newFatherWithQueue(8)
	in, err := NewInputer(g, "tcp", "127.0.0.1:0", CLIENT_TYPE_PROXY, cfg, father, "1.2.3.4:80", 3)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown ids are ignored.
	in.processDataFrame(&ProxyFrame{Type: FRAME_TYPE_DATA, DataFrame: &DataFrame{Id: "nope"}})
	in.processCloseFrame(&ProxyFrame{Type: FRAME_TYPE_CLOSE, CloseFrame: &CloseFrame{Id: "nope"}})
	in.processOpenRspFrame(&ProxyFrame{Type: FRAME_TYPE_OPENRSP, OpenRspFrame: &OpenConnRspFrame{Id: "nope", Ret: true}})

	// OPENRSP transitions a registered sonny.
	pc := &ProxyConn{id: "s1"}
	in.sonny.Store("s1", pc)
	in.processOpenRspFrame(&ProxyFrame{Type: FRAME_TYPE_OPENRSP, OpenRspFrame: &OpenConnRspFrame{Id: "s1", Ret: false}})
	if !pc.isNeedClose() {
		t.Fatal("failed OPENRSP must mark needclose")
	}
	pc2 := &ProxyConn{id: "s2"}
	in.sonny.Store("s2", pc2)
	in.processOpenRspFrame(&ProxyFrame{Type: FRAME_TYPE_OPENRSP, OpenRspFrame: &OpenConnRspFrame{Id: "s2", Ret: true}})
	if !pc2.isEstablished() {
		t.Fatal("successful OPENRSP must set established")
	}

	// openConn emits a well-formed OPEN carrying service index and proto.
	pc3 := &ProxyConn{id: "s3"}
	in.openConn(pc3, "5.6.7.8:9")
	v, _, ok := father.sendq.PopWait(time.Second)
	if !ok {
		t.Fatal("OPEN frame not sent")
	}
	of := v.(*ProxyFrame)
	if of.Type != FRAME_TYPE_OPEN || of.OpenFrame.Toaddr != "5.6.7.8:9" ||
		of.OpenFrame.ServiceIndex != 3 || of.OpenFrame.Proxyproto != PROXY_PROTO_TCP {
		t.Fatalf("OPEN frame malformed: %+v", of.OpenFrame)
	}

	// Full-channel data marks the sonny needclose.
	stuck := &ProxyConn{id: "stuck"}
	stuck.sendch = newMsgChannel(1)
	stuck.sendch.Write("x")
	in.sonny.Store("stuck", stuck)
	in.processDataFrame(&ProxyFrame{Type: FRAME_TYPE_DATA, DataFrame: &DataFrame{Id: "stuck", Data: []byte("y")}})
	if !stuck.isNeedClose() {
		t.Fatal("stuck input sonny should be needclose")
	}

	atomic.StoreInt32(&in.sonnyNum, -3)
	if in.sonnySize() != 0 {
		t.Fatal("negative sonny count should clamp to 0")
	}

	in.Close()
}

// ---- Server: config & iniService --------------------------------------------

func TestNewServer_ConfigValidation(t *testing.T) {
	if _, err := NewServer(nil, nil, nil); err == nil {
		t.Fatal("nil config must error (missing key)")
	}
	cfg := DefaultConfig()
	cfg.Key = "strong-server-key"
	if _, err := NewServer(cfg, []string{"tcp"}, []string{"127.0.0.1:bad"}); err == nil {
		t.Fatal("invalid listen address must error")
	}
	if _, err := NewServer(cfg, []string{"bogus"}, []string{"127.0.0.1:0"}); err == nil {
		t.Fatal("invalid proto must error")
	}
}

func newDirectServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	g := thread.NewGroup("direct-server", nil, nil)
	t.Cleanup(func() { g.Stop(); _ = g.Wait() })
	return &Server{config: cfg, wg: g}
}

func TestServer_IniService_ErrorBranches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "strong-server-key"
	s := newDirectServer(t, cfg)
	sess := &ClientConn{}

	// Unknown client type.
	err := s.iniService(s.wg, &ProxyFrame{LoginFrame: &LoginFrame{Clienttype: CLIENT_TYPE(99), Services: []*LoginService{{Proxyproto: PROXY_PROTO_TCP}}}}, sess)
	if err == nil {
		t.Fatal("unknown client type must error")
	}

	// A login carrying neither Services nor legacy fields is normalized to
	// the single legacy fallback service (proto zero = TCP); iniService must
	// accept it rather than crash.
	sess2 := &ClientConn{}
	err = s.iniService(s.wg, &ProxyFrame{LoginFrame: &LoginFrame{Clienttype: CLIENT_TYPE_PROXY}}, sess2)
	if err != nil {
		t.Fatalf("legacy-empty login should normalize to a fallback service: %v", err)
	}
	_, outputs := sess2.serviceSnapshot()
	if len(outputs) != 1 {
		t.Fatalf("want 1 fallback output, got %d", len(outputs))
	}
	sess2.closeServices()
}

func TestServer_IniService_AllKnownTypes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "strong-server-key"
	s := newDirectServer(t, cfg)

	cases := []struct {
		name string
		ct   CLIENT_TYPE
	}{
		{"proxy", CLIENT_TYPE_PROXY},
		{"socks5", CLIENT_TYPE_SOCKS5},
		{"http", CLIENT_TYPE_HTTP},
		{"reverse_proxy", CLIENT_TYPE_REVERSE_PROXY},
		{"reverse_socks5", CLIENT_TYPE_REVERSE_SOCKS5},
		{"reverse_http", CLIENT_TYPE_REVERSE_HTTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &ClientConn{}
			f := &ProxyFrame{LoginFrame: &LoginFrame{Clienttype: tc.ct, Services: []*LoginService{{
				Proxyproto: PROXY_PROTO_TCP,
				Fromaddr:   "127.0.0.1:0",
				Toaddr:     "127.0.0.1:1",
			}}}}
			if err := s.iniService(s.wg, f, sess); err != nil {
				t.Fatalf("iniService %s: %v", tc.name, err)
			}
			inputs, outputs := sess.serviceSnapshot()
			if len(inputs)+len(outputs) != 1 {
				t.Fatalf("%s: want 1 service, got %d+%d", tc.name, len(inputs), len(outputs))
			}
			sess.closeServices()
		})
	}
}

func TestLoginServicesOf_LegacyFields(t *testing.T) {
	// Explicit services win.
	explicit := []*LoginService{{Proxyproto: PROXY_PROTO_UDP}}
	f := &LoginFrame{Services: explicit, Proxyproto: PROXY_PROTO_TCP}
	if got := loginServicesOf(f); len(got) != 1 || got[0].Proxyproto != PROXY_PROTO_UDP {
		t.Fatalf("explicit services not used: %+v", got)
	}
	// Empty services fall back to the legacy top-level fields.
	f2 := &LoginFrame{Proxyproto: PROXY_PROTO_TCP, Fromaddr: ":1", Toaddr: ":2"}
	got := loginServicesOf(f2)
	if len(got) != 1 || got[0].Proxyproto != PROXY_PROTO_TCP || got[0].Fromaddr != ":1" || got[0].Toaddr != ":2" {
		t.Fatalf("legacy fallback wrong: %+v", got)
	}
}

func TestServer_ClientSizeClamp(t *testing.T) {
	s := &Server{}
	atomic.StoreInt32(&s.clientNum, -9)
	if s.clientSize() != 0 {
		t.Fatal("negative client count should clamp to 0")
	}
}

// ---- Server: login / join state machine -------------------------------------

func newPipeWithQueues(cfg *Config) *mainPipe {
	// A fresh (unconnected) TcpConn is nil-safe: Info() reports "empty tcp
	// conn", Close() is harmless. Real servePipe always carries a concrete
	// accepted conn; direct state-machine tests use the empty stand-in.
	c, _ := network.NewConn("tcp")
	p := &mainPipe{ProxyConn: ProxyConn{conn: c}, proto: "tcp", addr: "testpipe"}
	p.sendq = newPrioQueue(64)
	p.recvq = newPrioQueue(64)
	p.setCodec(defaultFrameCodec(cfg))
	return p
}

func startUnitTestServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	s, err := NewServer(cfg, []string{"tcp"}, []string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestServer_LoginAndJoin_StateMachine(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "unit-auth-key"
	s := startUnitTestServer(t, cfg)

	// First pipe logs in with a valid challenge proof.
	p1 := newPipeWithQueues(cfg)
	ch1, err := makeAuthChallenge()
	if err != nil {
		t.Fatal(err)
	}
	p1.authChallenge = ch1
	var ref1 atomic.Pointer[ClientConn]
	login := &ProxyFrame{LoginFrame: &LoginFrame{
		Name:       "unit",
		Clienttype: CLIENT_TYPE_PROXY,
		AuthProof:  computeAuthProof(cfg.Key, ch1),
		Services:   []*LoginService{{Proxyproto: PROXY_PROTO_TCP}},
	}}
	s.processLogin(login, p1, &ref1)
	lr := mustPopLoginRsp(t, p1.sendq)
	if !lr.Ret {
		t.Fatalf("login rejected: %s", lr.Msg)
	}
	sess := ref1.Load()
	if sess == nil || lr.SessionId == 0 {
		t.Fatal("session not established")
	}
	if s.clientSize() != 1 {
		t.Fatalf("clientSize=%d", s.clientSize())
	}

	// A second LOGIN (with a fresh valid challenge proof) on a pipe already
	// bound to a session is rejected by the established-session guard.
	ch1b, _ := makeAuthChallenge()
	p1.authChallenge = ch1b
	s.processLogin(&ProxyFrame{LoginFrame: &LoginFrame{
		Clienttype: CLIENT_TYPE_PROXY,
		AuthProof:  computeAuthProof(cfg.Key, ch1b),
		Services:   []*LoginService{{Proxyproto: PROXY_PROTO_TCP}},
	}}, p1, &ref1)
	lr2 := mustPopLoginRsp(t, p1.sendq)
	if lr2.Ret || lr2.Msg != "has established before" {
		t.Fatalf("re-login got ret=%v msg=%q", lr2.Ret, lr2.Msg)
	}

	// Second pipe joins the existing session with a fresh challenge.
	p2 := newPipeWithQueues(cfg)
	ch2, _ := makeAuthChallenge()
	p2.authChallenge = ch2
	var ref2 atomic.Pointer[ClientConn]
	s.processChannelJoin(&ProxyFrame{ChannelJoinFrame: &ChannelJoinFrame{
		SessionId: lr.SessionId,
		AuthProof: computeAuthProof(cfg.Key, ch2),
	}}, p2, &ref2)
	jr := mustPopJoinRsp(t, p2.sendq)
	if !jr.Ret {
		t.Fatalf("channel join rejected: %s", jr.Msg)
	}
	if ref2.Load() != sess {
		t.Fatal("join attached to wrong session")
	}
	if sess.hub.liveCount() != 2 {
		t.Fatalf("hub live=%d want 2", sess.hub.liveCount())
	}

	// Join with an unknown session id is rejected.
	p3 := newPipeWithQueues(cfg)
	ch3, _ := makeAuthChallenge()
	p3.authChallenge = ch3
	var ref3 atomic.Pointer[ClientConn]
	s.processChannelJoin(&ProxyFrame{ChannelJoinFrame: &ChannelJoinFrame{
		SessionId: 999999,
		AuthProof: computeAuthProof(cfg.Key, ch3),
	}}, p3, &ref3)
	jr2 := mustPopJoinRsp(t, p3.sendq)
	if jr2.Ret || jr2.Msg != "unknown session" {
		t.Fatalf("unknown session got ret=%v msg=%q", jr2.Ret, jr2.Msg)
	}
}

func TestServer_Login_AuthAndMaxClient(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "unit-auth-key-2"
	s := startUnitTestServer(t, cfg)

	// Bad proof is rejected and the pipe is killed.
	p := newPipeWithQueues(cfg)
	p.authChallenge = []byte("the-challenge")
	var ref atomic.Pointer[ClientConn]
	s.processLogin(&ProxyFrame{LoginFrame: &LoginFrame{
		Clienttype: CLIENT_TYPE_PROXY, AuthProof: []byte("wrong-proof"),
	}}, p, &ref)
	lr := mustPopLoginRsp(t, p.sendq)
	if lr.Ret || lr.Msg != "auth proof error" {
		t.Fatalf("bad proof got ret=%v msg=%q", lr.Ret, lr.Msg)
	}
	if !p.isNeedClose() {
		t.Fatal("bad proof must mark pipe needclose")
	}

	// MaxClient zero rejects new sessions.
	s.config.MaxClient = 0
	p2 := newPipeWithQueues(cfg)
	ch, _ := makeAuthChallenge()
	p2.authChallenge = ch
	var ref2 atomic.Pointer[ClientConn]
	s.processLogin(&ProxyFrame{LoginFrame: &LoginFrame{
		Clienttype: CLIENT_TYPE_PROXY,
		AuthProof:  computeAuthProof(cfg.Key, ch),
		Services:   []*LoginService{{Proxyproto: PROXY_PROTO_TCP}},
	}}, p2, &ref2)
	lr2 := mustPopLoginRsp(t, p2.sendq)
	if lr2.Ret || lr2.Msg != "max client" {
		t.Fatalf("maxclient got ret=%v msg=%q", lr2.Ret, lr2.Msg)
	}
}

func TestServer_ChannelJoin_BadProof(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "join-auth-key"
	s := startUnitTestServer(t, cfg)
	p := newPipeWithQueues(cfg)
	p.authChallenge = []byte("ch")
	var ref atomic.Pointer[ClientConn]
	s.processChannelJoin(&ProxyFrame{ChannelJoinFrame: &ChannelJoinFrame{
		SessionId: 1, AuthProof: []byte("bad"),
	}}, p, &ref)
	jr := mustPopJoinRsp(t, p.sendq)
	if jr.Ret || jr.Msg != "auth proof error" {
		t.Fatalf("bad join proof got ret=%v msg=%q", jr.Ret, jr.Msg)
	}
	if !p.isNeedClose() {
		t.Fatal("bad join proof must needclose the pipe")
	}
}

func mustPopLoginRsp(t *testing.T, q *prioQueue) *LoginRspFrame {
	t.Helper()
	v, closed, ok := q.PopWait(3 * time.Second)
	if !ok || closed {
		t.Fatalf("LOGINRSP not delivered")
	}
	f := v.(*ProxyFrame)
	if f.Type != FRAME_TYPE_LOGINRSP || f.LoginRspFrame == nil {
		t.Fatalf("expected LOGINRSP, got %v", f.Type)
	}
	return f.LoginRspFrame
}

func mustPopJoinRsp(t *testing.T, q *prioQueue) *ChannelJoinRspFrame {
	t.Helper()
	v, closed, ok := q.PopWait(3 * time.Second)
	if !ok || closed {
		t.Fatal("JOINRSP not delivered")
	}
	f := v.(*ProxyFrame)
	if f.Type != FRAME_TYPE_CHANNEL_JOIN_RSP || f.ChannelJoinRspFrame == nil {
		t.Fatalf("expected CHANNEL_JOIN_RSP, got %v", f.Type)
	}
	return f.ChannelJoinRspFrame
}

// ---- routing & client probe edge cases --------------------------------------

func TestRouteOpenToOutput_IndexGuards(t *testing.T) {
	// Empty outputs: silently ignored.
	routeOpenToOutput(&ProxyFrame{OpenFrame: &OpenConnFrame{ServiceIndex: 0}}, nil)

	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.MaxSonny = 0 // makes dispatch return immediately with an OPENRSP
	father := newFatherWithQueue(8)
	o, err := NewOutputer(g, "tcp", CLIENT_TYPE_PROXY, cfg, father, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Out-of-range index is dropped.
	routeOpenToOutput(&ProxyFrame{OpenFrame: &OpenConnFrame{Id: "x", ServiceIndex: 5}}, []*Outputer{o})
	// Valid index dispatches and produces the "max sonny" response.
	routeOpenToOutput(&ProxyFrame{OpenFrame: &OpenConnFrame{Id: "x", ServiceIndex: 0, Toaddr: "127.0.0.1:1"}}, []*Outputer{o})
	rsp := popOpenRsp(t, father.sendq, time.Second)
	if rsp.Ret || rsp.Msg != "max sonny" {
		t.Fatalf("dispatch got ret=%v msg=%q", rsp.Ret, rsp.Msg)
	}
}

func TestClient_ProcessSpeedTest_EdgeCases(t *testing.T) {
	c := &Client{config: DefaultConfig()}
	p := &mainPipe{proto: "tcp", addr: "x"}
	p.sendq = newPrioQueue(8)

	// Nil SpeedTestFrame must not panic and must not reactivate.
	atomic.StoreInt32(&p.state, pipeGray)
	c.processSpeedTest(&ProxyFrame{Type: FRAME_TYPE_SPEEDTEST}, p)
	if !p.isGray() {
		t.Fatal("nil speedtest must be ignored")
	}

	// Echo timestamp in the future (elapsed<=0) is ignored.
	c.processSpeedTest(&ProxyFrame{SpeedTestFrame: &SpeedTestFrame{
		SendTime: time.Now().Add(time.Hour).UnixNano(), Payload: []byte("z"), Echo: true,
	}}, p)
	if !p.isGray() || atomic.LoadInt64(&p.thrBps) != 0 {
		t.Fatal("future-dated echo must be ignored")
	}

	// Empty payload is counted as 1 byte for the bandwidth estimate.
	c.processSpeedTest(&ProxyFrame{SpeedTestFrame: &SpeedTestFrame{
		SendTime: time.Now().Add(-time.Millisecond).UnixNano(), Payload: nil, Echo: true,
	}}, p)
	if !p.isActive() || atomic.LoadInt64(&p.thrBps) <= 0 {
		t.Fatal("valid echo with empty payload should activate and score the pipe")
	}
}
