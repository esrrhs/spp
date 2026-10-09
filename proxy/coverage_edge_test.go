package proxy

import (
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
)

// ---- codec selection helpers: every branch -------------------------------

func TestEffectiveTypes_AndPredicates(t *testing.T) {
	if got := effectiveCompressType(CompressUnspecified); got != CompressZstd {
		t.Fatalf("unspecified compress -> %v, want zstd", got)
	}
	if got := effectiveCompressType(CompressZlib); got != CompressZlib {
		t.Fatalf("explicit zlib passthrough got %v", got)
	}
	if got := effectiveEncryptType(EncryptUnspecified); got != EncryptChaCha20 {
		t.Fatalf("unspecified encrypt -> %v, want chacha20", got)
	}
	if got := effectiveEncryptType(EncryptAESGCM); got != EncryptAESGCM {
		t.Fatalf("explicit aesgcm passthrough got %v", got)
	}

	if !isAEADEncrypt(EncryptAESGCM) || !isAEADEncrypt(EncryptChaCha20) {
		t.Fatal("aesgcm/chacha must be AEAD")
	}
	if isAEADEncrypt(EncryptNone) {
		t.Fatal("EncryptNone must not be AEAD")
	}
	if !isAEADEncrypt(EncryptUnspecified) {
		t.Fatal("unspecified defaults to chacha20 -> AEAD")
	}
	for _, c := range []COMPRESS_TYPE{CompressNone, CompressZlib, CompressZstd, CompressUnspecified} {
		if !supportedCompressType(c) {
			t.Fatalf("compress %v must be supported", c)
		}
	}
	if supportedCompressType(COMPRESS_TYPE(999)) {
		t.Fatal("unknown compress type must be unsupported")
	}
}

func TestTypeNames_UnknownFallback(t *testing.T) {
	if got := compressTypeName(COMPRESS_TYPE(999)); got != "unspecified" {
		t.Fatalf("compressTypeName bogus=%q", got)
	}
	if got := encryptTypeName(ENCRYPT_TYPE(999)); got != "unspecified" {
		t.Fatalf("encryptTypeName bogus=%q", got)
	}
	if got := compressTypeName(CompressZlib); got != "zlib" {
		t.Fatalf("zlib name=%q", got)
	}
	if got := encryptTypeName(EncryptAESGCM); got != "aes-gcm" {
		t.Fatalf("aes name=%q", got)
	}
}

// ---- setCongestion on every underlay branch -------------------------------

func TestSetCongestion_AllBranches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Congestion = "bbr"
	for _, p := range []string{"tcp", "rudp", "ricmp"} {
		conn, err := network.NewConn(p)
		if conn == nil {
			t.Skipf("%s conn unavailable: %v", p, err)
		}
		setCongestion(conn, cfg) // rudp/ricmp mutate config; tcp is a no-op; none may panic
		conn.Close()
	}
}

// ---- ProxyConn send paths after queue/channel close ------------------------

func TestProxyConn_SendAfterClose_NoPanic(t *testing.T) {
	// Main-queue conn that is closed: pickSendQ returns nil and no sendch is
	// set, so sends must be dropped without panicking.
	pc := &ProxyConn{}
	pc.sendq = newPrioQueue(4)
	pc.recvq = newPrioQueue(4)
	pc.CloseChannels()
	pc.SendFrame(&ProxyFrame{Type: FRAME_TYPE_PING})
	pc.SendData(&ProxyFrame{Type: FRAME_TYPE_DATA}, true)
	if pc.SendSonnyData(&ProxyFrame{Type: FRAME_TYPE_DATA}, 1) != true {
		t.Fatal("SendSonnyData on closed conn should report true")
	}
	pc.SendSonnyClose(&ProxyFrame{Type: FRAME_TYPE_CLOSE})
	pc.RecvSonnyData(&ProxyFrame{Type: FRAME_TYPE_DATA})
}

func TestProxyConn_SendChPath(t *testing.T) {
	// Sonny-style conn with only msg channels: frame/control go through Write.
	pc := &ProxyConn{}
	pc.sendch = newMsgChannel(2)
	pc.recvch = newMsgChannel(2)
	pc.SendFrame(&ProxyFrame{Type: FRAME_TYPE_OPEN})
	pc.SendData(&ProxyFrame{Type: FRAME_TYPE_DATA}, false)
	pc.RecvSonnyData(&ProxyFrame{Type: FRAME_TYPE_DATA})

	// sendch receives the two outbound frames; recvch the inbound one.
	for i := 0; i < 2; i++ {
		select {
		case <-pc.sendch.ch:
		case <-time.After(time.Second):
			t.Fatalf("sendch missing frame %d", i)
		}
	}
	select {
	case <-pc.recvch.ch:
	case <-time.After(time.Second):
		t.Fatal("recvch did not receive the inbound frame")
	}

	// Closing the channels makes subsequent writes safe no-ops.
	pc.CloseChannels()
	pc.SendFrame(&ProxyFrame{Type: FRAME_TYPE_PING})
}

// ---- iniService rejects unknown client types before binding sockets --------

func TestIniService_UnknownClientType(t *testing.T) {
	g := newTestGroup(t)
	cfg := DefaultConfig()
	cfg.Key = "edge-ini-key"

	// Client side: bogus type must error without touching listeners.
	c := &Client{config: cfg, clienttype: CLIENT_TYPE(9999)}
	if err := c.iniService(g, &ServerConn{}); err == nil {
		t.Fatal("client iniService with bogus CLIENT_TYPE must error")
	}

	// Server side: reach the switch (services non-empty) and reject.
	s := &Server{config: cfg}
	sess := &ClientConn{}
	f := &ProxyFrame{LoginFrame: &LoginFrame{
		Clienttype: CLIENT_TYPE(9999),
		Services:   []*LoginService{{Proxyproto: PROXY_PROTO_TCP}},
	}}
	if err := s.iniService(g, f, sess); err == nil {
		t.Fatal("server iniService with bogus CLIENT_TYPE must error")
	}
}

// ---- client processLoginRsp failure marks the conn for close ---------------

func TestClient_ProcessLoginRsp_Rejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "edge-loginrsp-key"
	c := &Client{config: cfg}
	sess := &ServerConn{}
	sess.sendq = newPrioQueue(8)
	sess.recvq = newPrioQueue(8)
	sess.setCodec(defaultFrameCodec(cfg))

	c.processLoginRsp(newTestGroup(t), &ProxyFrame{LoginRspFrame: &LoginRspFrame{
		Ret: false, Msg: "auth proof error",
	}}, sess)
	if !sess.isNeedClose() {
		t.Fatal("rejected LOGINRSP must mark conn needclose")
	}
	if sess.isEstablished() {
		t.Fatal("rejected LOGINRSP must not establish session")
	}
}

// ---- dispatch helpers with unknown ids / services are safe no-ops ----------

func TestProcessDispatch_UnknownIdsAreNoop(t *testing.T) {
	h := startForwardProxy(t, "tcp", "edge-dispatch-key")
	defer h.Close()
	conn := h.Dial(5 * time.Second)
	echoRoundTrip(t, conn, []byte("warmup"), 10*time.Second)
	conn.Close()

	// Client side: inputs populated; unknown id must not panic/match.
	sess := h.client.serverconn
	if sess == nil {
		t.Fatal("no client session")
	}
	missing := "does-not-exist-id"
	h.client.processData(&ProxyFrame{DataFrame: &DataFrame{Id: missing}}, sess)
	h.client.processOpenRsp(&ProxyFrame{OpenRspFrame: &OpenConnRspFrame{Id: missing}}, sess)
	h.client.processClose(&ProxyFrame{CloseFrame: &CloseFrame{Id: missing}}, sess)
	h.client.processOpen(&ProxyFrame{OpenFrame: &OpenConnFrame{Id: missing, ServiceIndex: 0}}, sess)

	// Server side: outputs populated; exercise the same no-match paths.
	var serverSess *ClientConn
	h.server.clients.Range(func(_, v any) bool {
		serverSess = v.(*ClientConn)
		return false
	})
	if serverSess == nil {
		t.Fatal("no server session")
	}
	h.server.processData(&ProxyFrame{DataFrame: &DataFrame{Id: missing}}, serverSess)
	h.server.processClose(&ProxyFrame{CloseFrame: &CloseFrame{Id: missing}}, serverSess)
	h.server.processOpenRsp(&ProxyFrame{OpenRspFrame: &OpenConnRspFrame{Id: missing}}, serverSess)
	h.server.processOpen(&ProxyFrame{OpenFrame: &OpenConnFrame{Id: missing, ServiceIndex: 99}}, serverSess)
}
