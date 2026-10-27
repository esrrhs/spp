package proxy

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Exhaustive mechanical coverage of the generated protobuf code: every enum's
// accessors, every message's accessors/getters, and reflection-driven
// marshal/unmarshal roundtrips with all fields populated.

type enumLike interface {
	String() string
	Number() protoreflect.EnumNumber
	Descriptor() protoreflect.EnumDescriptor
	Type() protoreflect.EnumType
	EnumDescriptor() ([]byte, []int)
}

func enumPtr[T ~int32](v T) *T {
	type enumT interface{ Enum() *T }
	return any(v).(enumT).Enum()
}

func TestGeneratedEnumsAccessors(t *testing.T) {
	enums := []struct {
		name string
		vals []enumLike
	}{
		{"PROXY_PROTO", []enumLike{
			PROXY_PROTO_TCP, PROXY_PROTO_UDP, PROXY_PROTO_RUDP, PROXY_PROTO_RICMP, PROXY_PROTO_KCP,
		}},
		{"CLIENT_TYPE", []enumLike{
			CLIENT_TYPE_PROXY, CLIENT_TYPE_REVERSE_PROXY, CLIENT_TYPE_SOCKS5, CLIENT_TYPE_REVERSE_SOCKS5,
			CLIENT_TYPE_SS_PROXY, CLIENT_TYPE_HTTP, CLIENT_TYPE_REVERSE_HTTP,
		}},
		{"COMPRESS_TYPE", []enumLike{
			COMPRESS_TYPE_COMPRESS_UNSPECIFIED, COMPRESS_TYPE_COMPRESS_NONE, COMPRESS_TYPE_COMPRESS_ZLIB, COMPRESS_TYPE_COMPRESS_ZSTD,
		}},
		{"ENCRYPT_TYPE", []enumLike{
			ENCRYPT_TYPE_ENCRYPT_UNSPECIFIED, ENCRYPT_TYPE_ENCRYPT_NONE, ENCRYPT_TYPE_ENCRYPT_AES_GCM, ENCRYPT_TYPE_ENCRYPT_CHACHA20,
		}},
		{"FRAME_TYPE", []enumLike{
			FRAME_TYPE_LOGIN, FRAME_TYPE_LOGINRSP, FRAME_TYPE_DATA, FRAME_TYPE_PING, FRAME_TYPE_PONG,
			FRAME_TYPE_OPEN, FRAME_TYPE_OPENRSP, FRAME_TYPE_CLOSE, FRAME_TYPE_AUTH_CHALLENGE,
			FRAME_TYPE_CHANNEL_JOIN, FRAME_TYPE_CHANNEL_JOIN_RSP, FRAME_TYPE_SPEEDTEST,
		}},
	}
	for _, e := range enums {
		for _, v := range e.vals {
			_ = v.Number()
			if got := v.String(); got == "" || got == "<unknown>" {
				t.Errorf("%s value %d has unexpected String(): %q", e.name, v.Number(), got)
			}
			if v.Type() == nil {
				t.Errorf("%s value %d Type() nil", e.name, v.Number())
			} else if v.Type().Descriptor().Name() != protoreflect.Name(e.name) {
				t.Errorf("%s Type name mismatch: %q", e.name, v.Type().Descriptor().Name())
			}
			if v.Descriptor() == nil {
				t.Errorf("%s value %d Descriptor() nil", e.name, v.Number())
			}
			if fd, idx := v.EnumDescriptor(); fd == nil || len(fd) == 0 || len(idx) == 0 || idx[0] < 0 {
				t.Errorf("%s value %d EnumDescriptor() bad", e.name, v.Number())
			}
		}
	}
	// Enum() returns a pointer to an equivalent value, per concrete type.
	if p := enumPtr(PROXY_PROTO_KCP); p == nil || *p != PROXY_PROTO_KCP {
		t.Errorf("PROXY_PROTO Enum() mismatch")
	}
	if p := enumPtr(CLIENT_TYPE_SOCKS5); p == nil || *p != CLIENT_TYPE_SOCKS5 {
		t.Errorf("CLIENT_TYPE Enum() mismatch")
	}
	if p := enumPtr(COMPRESS_TYPE_COMPRESS_ZSTD); p == nil || *p != COMPRESS_TYPE_COMPRESS_ZSTD {
		t.Errorf("COMPRESS_TYPE Enum() mismatch")
	}
	if p := enumPtr(ENCRYPT_TYPE_ENCRYPT_AES_GCM); p == nil || *p != ENCRYPT_TYPE_ENCRYPT_AES_GCM {
		t.Errorf("ENCRYPT_TYPE Enum() mismatch")
	}
	if p := enumPtr(FRAME_TYPE_SPEEDTEST); p == nil || *p != FRAME_TYPE_SPEEDTEST {
		t.Errorf("FRAME_TYPE Enum() mismatch")
	}
}

type messageLike interface {
	proto.Message
	Reset()
	String() string
	ProtoMessage()
	Descriptor() ([]byte, []int)
}

func TestGeneratedMessagesAccessors(t *testing.T) {
	// Nil-receiver getters return defaults instead of panicking.
	func() {
		var m *LoginService
		_ = m.GetProxyproto()
		_ = m.GetFromaddr()
		_ = m.GetToaddr()
	}()
	func() {
		var m *LoginFrame
		_ = m.GetProxyproto()
		_ = m.GetClienttype()
		_ = m.GetFromaddr()
		_ = m.GetToaddr()
		_ = m.GetName()
		_ = m.GetCompressType()
		_ = m.GetEncryptType()
		_ = m.GetAuthProof()
		_ = m.GetServices()
	}()
	func() {
		var m *LoginRspFrame
		_ = m.GetRet()
		_ = m.GetMsg()
		_ = m.GetCompressType()
		_ = m.GetEncryptType()
		_ = m.GetSessionId()
	}()
	func() { var m *AuthChallengeFrame; _ = m.GetChallenge() }()
	func() { var m *ChannelJoinFrame; _ = m.GetSessionId(); _ = m.GetAuthProof() }()
	func() { var m *ChannelJoinRspFrame; _ = m.GetRet(); _ = m.GetMsg() }()
	func() { var m *SpeedTestFrame; _ = m.GetId(); _ = m.GetSendTime(); _ = m.GetPayload(); _ = m.GetEcho() }()
	func() { var m *PingFrame; _ = m.GetTime() }()
	func() { var m *PongFrame; _ = m.GetTime() }()
	func() {
		var m *OpenConnFrame
		_ = m.GetId()
		_ = m.GetToaddr()
		_ = m.GetProxyproto()
		_ = m.GetServiceIndex()
	}()
	func() { var m *OpenConnRspFrame; _ = m.GetId(); _ = m.GetRet(); _ = m.GetMsg() }()
	func() { var m *CloseFrame; _ = m.GetId() }()
	func() {
		var m *DataFrame
		_ = m.GetId()
		_ = m.GetCompress()
		_ = m.GetCrc()
		_ = m.GetData()
		_ = m.GetIndex()
		_ = m.GetCompressType()
	}()
	func() {
		var m *ProxyFrame
		_ = m.GetType()
		_ = m.GetLoginFrame()
		_ = m.GetLoginRspFrame()
		_ = m.GetDataFrame()
		_ = m.GetPingFrame()
		_ = m.GetPongFrame()
		_ = m.GetOpenFrame()
		_ = m.GetOpenRspFrame()
		_ = m.GetCloseFrame()
		_ = m.GetAuthChallengeFrame()
		_ = m.GetChannelJoinFrame()
		_ = m.GetChannelJoinRspFrame()
		_ = m.GetSpeedTestFrame()
	}()

	authProof := []byte{1, 2, 3, 4, 5}
	challenge := []byte{9, 8, 7}
	payload := []byte("speed-test-payload")
	svcs := []*LoginService{
		{Proxyproto: PROXY_PROTO_TCP, Fromaddr: "127.0.0.1:80", Toaddr: "example.com:80"},
		{Proxyproto: PROXY_PROTO_UDP, Fromaddr: "0.0.0.0:53", Toaddr: "8.8.8.8:53"},
	}

	// Populated-receiver getters.
	ls := &LoginService{Proxyproto: PROXY_PROTO_RUDP, Fromaddr: "f:1", Toaddr: "t:2"}
	_, _, _ = ls.GetProxyproto(), ls.GetFromaddr(), ls.GetToaddr()
	lf := &LoginFrame{
		Proxyproto: PROXY_PROTO_KCP, Clienttype: CLIENT_TYPE_SOCKS5,
		Fromaddr: "fa:1", Toaddr: "ta:2", Name: "client-tag",
		CompressType: COMPRESS_TYPE_COMPRESS_ZSTD, EncryptType: ENCRYPT_TYPE_ENCRYPT_AES_GCM,
		AuthProof: authProof, Services: svcs,
	}
	_, _, _, _, _, _, _, _, _ = lf.GetProxyproto(), lf.GetClienttype(), lf.GetFromaddr(), lf.GetToaddr(), lf.GetName(), lf.GetCompressType(), lf.GetEncryptType(), lf.GetAuthProof(), lf.GetServices()
	lr := &LoginRspFrame{Ret: true, Msg: "welcome", CompressType: COMPRESS_TYPE_COMPRESS_ZLIB, EncryptType: ENCRYPT_TYPE_ENCRYPT_CHACHA20, SessionId: 123456789}
	_, _, _, _, _ = lr.GetRet(), lr.GetMsg(), lr.GetCompressType(), lr.GetEncryptType(), lr.GetSessionId()
	ac := &AuthChallengeFrame{Challenge: challenge}
	_ = ac.GetChallenge()
	cj := &ChannelJoinFrame{SessionId: 42, AuthProof: authProof}
	_, _ = cj.GetSessionId(), cj.GetAuthProof()
	cjr := &ChannelJoinRspFrame{Ret: true, Msg: "joined"}
	_, _ = cjr.GetRet(), cjr.GetMsg()
	st := &SpeedTestFrame{Id: -7, SendTime: 987654321, Payload: payload, Echo: true}
	_, _, _, _ = st.GetId(), st.GetSendTime(), st.GetPayload(), st.GetEcho()
	pg := &PingFrame{Time: 1690000000000}
	_ = pg.GetTime()
	pn := &PongFrame{Time: 1690000000001}
	_ = pn.GetTime()
	oc := &OpenConnFrame{Id: "conn-abc", Toaddr: "10.0.0.1:443", Proxyproto: PROXY_PROTO_RICMP, ServiceIndex: 3}
	_, _, _, _ = oc.GetId(), oc.GetToaddr(), oc.GetProxyproto(), oc.GetServiceIndex()
	ocr := &OpenConnRspFrame{Id: "conn-abc", Ret: true, Msg: "opened"}
	_, _, _ = ocr.GetId(), ocr.GetRet(), ocr.GetMsg()
	cf := &CloseFrame{Id: "conn-abc"}
	_ = cf.GetId()
	df := &DataFrame{Id: "data-1", Compress: true, Crc: "crc123", Data: payload, Index: 9, CompressType: COMPRESS_TYPE_COMPRESS_ZSTD}
	_, _, _, _, _, _ = df.GetId(), df.GetCompress(), df.GetCrc(), df.GetData(), df.GetIndex(), df.GetCompressType()
	pf := &ProxyFrame{
		Type:                FRAME_TYPE_DATA,
		LoginFrame:          &LoginFrame{Fromaddr: "fa"},
		LoginRspFrame:       &LoginRspFrame{Ret: true},
		DataFrame:           df,
		PingFrame:           &PingFrame{Time: 1},
		PongFrame:           &PongFrame{Time: 2},
		OpenFrame:           oc,
		OpenRspFrame:        ocr,
		CloseFrame:          cf,
		AuthChallengeFrame:  ac,
		ChannelJoinFrame:    cj,
		ChannelJoinRspFrame: cjr,
		SpeedTestFrame:      st,
	}
	_, _, _, _, _, _, _, _, _, _, _, _, _ = pf.GetType(), pf.GetLoginFrame(), pf.GetLoginRspFrame(), pf.GetDataFrame(), pf.GetPingFrame(), pf.GetPongFrame(), pf.GetOpenFrame(), pf.GetOpenRspFrame(), pf.GetCloseFrame(), pf.GetAuthChallengeFrame(), pf.GetChannelJoinFrame(), pf.GetChannelJoinRspFrame(), pf.GetSpeedTestFrame()

	// Nil-receiver ProtoReflect still returns a valid (invalid-state) message.
	for _, nilReflect := range []func() protoreflect.Message{
		func() protoreflect.Message { var m *LoginService; return m.ProtoReflect() },
		func() protoreflect.Message { var m *LoginFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *LoginRspFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *AuthChallengeFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *ChannelJoinFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *ChannelJoinRspFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *SpeedTestFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *PingFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *PongFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *OpenConnFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *OpenConnRspFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *CloseFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *DataFrame; return m.ProtoReflect() },
		func() protoreflect.Message { var m *ProxyFrame; return m.ProtoReflect() },
	} {
		if nilReflect() == nil {
			t.Errorf("nil-receiver ProtoReflect returned nil")
		}
	}

	msgs := []messageLike{ls, lf, lr, ac, cj, cjr, st, pg, pn, oc, ocr, cf, df, pf}
	for _, m := range msgs {
		name := string(m.ProtoReflect().Descriptor().Name())

		if s := m.String(); strings.TrimSpace(s) == "" {
			t.Errorf("%s String() unexpectedly empty", name)
		}
		m.Reset()
		if s := m.String(); strings.TrimSpace(s) != "" {
			t.Errorf("%s after Reset String() = %q", name, s)
		}
		if m.ProtoReflect().Type() == nil {
			t.Errorf("%s ProtoReflect type nil", name)
		}
		if fd, idx := m.Descriptor(); fd == nil || len(fd) == 0 || len(idx) == 0 || idx[0] < 0 {
			t.Errorf("%s Descriptor() bad", name)
		}
		m.ProtoMessage()

		// Marshal -> Unmarshal roundtrip with all fields populated.
		bin, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		if proto.Size(m) != len(bin) {
			t.Errorf("%s Size=%d len=%d", name, proto.Size(m), len(bin))
		}
		rt := proto.Clone(m)
		rt.(interface{ Reset() }).Reset()
		if err := proto.Unmarshal(bin, rt); err != nil {
			t.Fatalf("%s unmarshal: %v", name, err)
		}
		if !proto.Equal(m, rt) {
			t.Errorf("%s roundtrip mismatch:\nwant %v\ngot  %v", name, m, rt)
		}
	}
}

func TestGeneratedMessagesUnmarshalErrors(t *testing.T) {
	full := &OpenConnFrame{Id: "xyz", Toaddr: "a:1", Proxyproto: PROXY_PROTO_UDP, ServiceIndex: 2}
	bin, err := proto.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	// Truncated buffers must error or parse a valid prefix, never panic.
	for n := 1; n < len(bin); n++ {
		m := &OpenConnFrame{}
		_ = proto.Unmarshal(bin[:n], m)
	}
	// A dangling end-group tag always errors; a varint for a string field is
	// retained as unknown in proto3, so also assert it does not panic.
	bad := []byte{0x0b}
	m := &OpenConnFrame{}
	if err := proto.Unmarshal(bad, m); err == nil {
		t.Errorf("expected end-group error, got none")
	}
	mixed := []byte{0x08, 0x2a}
	m3 := &OpenConnFrame{}
	if err := proto.Unmarshal(mixed, m3); err != nil {
		t.Fatalf("varint-for-string unmarshal: %v", err)
	}
	if m3.ProtoReflect().GetUnknown() == nil {
		t.Errorf("mismatched wire-type field not retained as unknown")
	}
	// Unknown fields are retained through a roundtrip.
	unknown := append(append([]byte{}, bin...), 0xf8, 0x01, 0x01) // field 31, varint 1
	m2 := &OpenConnFrame{}
	if err := proto.Unmarshal(unknown, m2); err != nil {
		t.Fatalf("unknown-field unmarshal: %v", err)
	}
	if m2.ProtoReflect().GetUnknown() == nil {
		t.Errorf("unknown fields not retained")
	}
}
