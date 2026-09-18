package proxy

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/network"
)

func testCodec(threshold int, key string) FrameCodec {
	c := FrameCodec{
		CompressThreshold: threshold,
		CompressType:      CompressZstd,
		EncryptKey:        key,
		EncryptType:       EncryptChaCha20,
	}
	if threshold <= 0 {
		c.CompressType = CompressNone
	}
	if key == "" {
		c.EncryptType = EncryptNone
	}
	return c
}

func Test0001(t *testing.T) {
	src := "aaabsfasasdfasfas3rdsfasfdhsafdshsafafafafaffasfsafa1111111111111111111111111111111111111111111111111111111111"
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_DATA
	f.DataFrame = &DataFrame{}
	f.DataFrame.Data = []byte(src)
	fmt.Println(len(f.DataFrame.Data))
	b, err := MarshalSrpFrame(f, testCodec(10, "123123"))
	if err != nil {
		t.Error(err)
	}
	fmt.Println(len(f.DataFrame.Data))
	ff, err := UnmarshalSrpFrame(b, testCodec(10, "123123"))
	if err != nil {
		t.Error(err)
	}
	if string(ff.DataFrame.Data) != src {
		t.Error("data mismatch")
	}
	fmt.Println(string(ff.DataFrame.Data))
}

func TestMarshalUnmarshalAllFrameTypes(t *testing.T) {
	codecKey := testCodec(0, "test-secret-key")
	codecPlain := testCodec(0, "")
	codecComp := testCodec(32, "encrypt-key")

	// 1. FRAME_TYPE_LOGIN
	{
		f := &ProxyFrame{
			Type: FRAME_TYPE_LOGIN,
			LoginFrame: &LoginFrame{
				Name:         "test-client",
				AuthProof:    []byte("dummy-proof"),
				Clienttype:   CLIENT_TYPE_PROXY,
				Proxyproto:   PROXY_PROTO_TCP,
				Fromaddr:     ":8080",
				Toaddr:       ":9090",
				CompressType: CompressZstd,
				EncryptType:  EncryptChaCha20,
			},
		}
		data, err := MarshalSrpFrame(f, codecKey)
		if err != nil {
			t.Fatalf("Marshal LOGIN frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codecKey)
		if err != nil {
			t.Fatalf("Unmarshal LOGIN frame failed: %v", err)
		}
		if res.Type != FRAME_TYPE_LOGIN || res.LoginFrame == nil || res.LoginFrame.Name != "test-client" {
			t.Errorf("LOGIN frame mismatch: %+v", res.LoginFrame)
		}
		if res.LoginFrame.CompressType != CompressZstd || res.LoginFrame.EncryptType != EncryptChaCha20 {
			t.Errorf("LOGIN codec fields mismatch: c=%v e=%v", res.LoginFrame.CompressType, res.LoginFrame.EncryptType)
		}
	}

	// 2. FRAME_TYPE_LOGINRSP
	{
		f := &ProxyFrame{
			Type: FRAME_TYPE_LOGINRSP,
			LoginRspFrame: &LoginRspFrame{
				Ret:          true,
				Msg:          "login success",
				CompressType: CompressZstd,
				EncryptType:  EncryptChaCha20,
			},
		}
		data, err := MarshalSrpFrame(f, codecKey)
		if err != nil {
			t.Fatalf("Marshal LOGINRSP frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codecKey)
		if err != nil {
			t.Fatalf("Unmarshal LOGINRSP frame failed: %v", err)
		}
		if res.Type != FRAME_TYPE_LOGINRSP || !res.LoginRspFrame.Ret || res.LoginRspFrame.Msg != "login success" {
			t.Errorf("LOGINRSP frame mismatch: %+v", res.LoginRspFrame)
		}
	}

	// 3. FRAME_TYPE_DATA without compression
	{
		raw := []byte("plain text data without compression")
		f := &ProxyFrame{
			Type: FRAME_TYPE_DATA,
			DataFrame: &DataFrame{
				Id:   "conn-1",
				Data: raw,
				Crc:  common.GetCrc32(raw),
			},
		}
		data, err := MarshalSrpFrame(f, codecPlain)
		if err != nil {
			t.Fatalf("Marshal DATA frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal DATA frame failed: %v", err)
		}
		if !bytes.Equal(res.DataFrame.Data, raw) || res.DataFrame.Id != "conn-1" {
			t.Errorf("DATA frame content mismatch")
		}
	}

	// 4. FRAME_TYPE_DATA with compression & encryption
	{
		raw := bytes.Repeat([]byte("repetitive data block for compression testing "), 20)
		f := &ProxyFrame{
			Type: FRAME_TYPE_DATA,
			DataFrame: &DataFrame{
				Id:   "conn-2",
				Data: raw,
				Crc:  common.GetCrc32(raw),
			},
		}
		data, err := MarshalSrpFrame(f, codecComp)
		if err != nil {
			t.Fatalf("Marshal compressed/encrypted DATA frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codecComp)
		if err != nil {
			t.Fatalf("Unmarshal compressed/encrypted DATA frame failed: %v", err)
		}
		if !bytes.Equal(res.DataFrame.Data, raw) {
			t.Errorf("DATA frame uncompressed content mismatch")
		}
	}

	// 4b. zlib path
	{
		raw := bytes.Repeat([]byte("zlib compressible payload "), 30)
		f := &ProxyFrame{
			Type: FRAME_TYPE_DATA,
			DataFrame: &DataFrame{
				Id:   "conn-zlib",
				Data: append([]byte(nil), raw...),
				Crc:  common.GetCrc32(raw),
			},
		}
		codec := FrameCodec{CompressThreshold: 16, CompressType: CompressZlib, EncryptType: EncryptNone}
		data, err := MarshalSrpFrame(f, codec)
		if err != nil {
			t.Fatalf("Marshal zlib DATA failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codec)
		if err != nil {
			t.Fatalf("Unmarshal zlib DATA failed: %v", err)
		}
		if !bytes.Equal(res.DataFrame.Data, raw) {
			t.Errorf("zlib DATA mismatch")
		}
	}

	// 5. FRAME_TYPE_PING & PONG
	{
		now := time.Now().UnixNano()
		ping := &ProxyFrame{
			Type:      FRAME_TYPE_PING,
			PingFrame: &PingFrame{Time: now},
		}
		data, err := MarshalSrpFrame(ping, codecPlain)
		if err != nil {
			t.Fatalf("Marshal PING failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal PING failed: %v", err)
		}
		if res.Type != FRAME_TYPE_PING || res.PingFrame.Time != now {
			t.Errorf("PING mismatch")
		}

		pong := &ProxyFrame{
			Type:      FRAME_TYPE_PONG,
			PongFrame: &PongFrame{Time: now},
		}
		dataPong, err := MarshalSrpFrame(pong, codecPlain)
		if err != nil {
			t.Fatalf("Marshal PONG failed: %v", err)
		}
		resPong, err := UnmarshalSrpFrame(dataPong, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal PONG failed: %v", err)
		}
		if resPong.Type != FRAME_TYPE_PONG || resPong.PongFrame.Time != now {
			t.Errorf("PONG mismatch")
		}
	}

	// 6. FRAME_TYPE_OPEN & OPENRSP
	{
		open := &ProxyFrame{
			Type: FRAME_TYPE_OPEN,
			OpenFrame: &OpenConnFrame{
				Id:     "id-1",
				Toaddr: "1.2.3.4:80",
			},
		}
		data, err := MarshalSrpFrame(open, codecPlain)
		if err != nil {
			t.Fatalf("Marshal OPEN failed: %v", err)
		}
		resOpen, err := UnmarshalSrpFrame(data, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal OPEN failed: %v", err)
		}
		if resOpen.OpenFrame.Id != "id-1" {
			t.Errorf("OPEN mismatch")
		}

		openRsp := &ProxyFrame{
			Type: FRAME_TYPE_OPENRSP,
			OpenRspFrame: &OpenConnRspFrame{
				Id:  "id-1",
				Ret: true,
				Msg: "ok",
			},
		}
		dataRsp, err := MarshalSrpFrame(openRsp, codecPlain)
		if err != nil {
			t.Fatalf("Marshal OPENRSP failed: %v", err)
		}
		resRsp, err := UnmarshalSrpFrame(dataRsp, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal OPENRSP failed: %v", err)
		}
		if !resRsp.OpenRspFrame.Ret {
			t.Errorf("OPENRSP mismatch")
		}
	}

	// 7. FRAME_TYPE_CLOSE
	{
		closeF := &ProxyFrame{
			Type:       FRAME_TYPE_CLOSE,
			CloseFrame: &CloseFrame{Id: "id-close"},
		}
		data, err := MarshalSrpFrame(closeF, codecPlain)
		if err != nil {
			t.Fatalf("Marshal CLOSE failed: %v", err)
		}
		resClose, err := UnmarshalSrpFrame(data, codecPlain)
		if err != nil {
			t.Fatalf("Unmarshal CLOSE failed: %v", err)
		}
		if resClose.CloseFrame.Id != "id-close" {
			t.Errorf("CLOSE mismatch")
		}
	}
}

func TestCheckProxyFrameErrors(t *testing.T) {
	codec := testCodec(0, "")
	tests := []*ProxyFrame{
		{Type: FRAME_TYPE_LOGIN, LoginFrame: nil},
		{Type: FRAME_TYPE_LOGINRSP, LoginRspFrame: nil},
		{Type: FRAME_TYPE_DATA, DataFrame: nil},
		{Type: FRAME_TYPE_PING, PingFrame: nil},
		{Type: FRAME_TYPE_PONG, PongFrame: nil},
		{Type: FRAME_TYPE_OPEN, OpenFrame: nil},
		{Type: FRAME_TYPE_OPENRSP, OpenRspFrame: nil},
		{Type: FRAME_TYPE_CLOSE, CloseFrame: nil},
		{Type: FRAME_TYPE_AUTH_CHALLENGE, AuthChallengeFrame: nil},
		{Type: FRAME_TYPE(999)}, // Invalid type
	}

	for i, f := range tests {
		err := checkProxyFame(f)
		if err == nil {
			t.Errorf("case %d expected check error", i)
		}
		_, err = MarshalSrpFrame(f, codec)
		if err == nil {
			t.Errorf("MarshalSrpFrame case %d expected error, got nil", i)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig returned nil")
	}
	if cfg.MaxMsgSize != 1024*1024 {
		t.Errorf("unexpected MaxMsgSize: %d", cfg.MaxMsgSize)
	}
	if cfg.Key != "123456" {
		t.Errorf("unexpected default Key: %s", cfg.Key)
	}
	if cfg.Compress != 128 {
		t.Errorf("unexpected Compress: %d", cfg.Compress)
	}
	if cfg.CompressType != CompressZstd {
		t.Errorf("unexpected CompressType: %v", cfg.CompressType)
	}
	if cfg.EncryptType != EncryptChaCha20 {
		t.Errorf("unexpected EncryptType: %v", cfg.EncryptType)
	}
	if cfg.MaxClient <= 0 || cfg.MaxSonny <= 0 {
		t.Errorf("invalid default limits: client=%d, sonny=%d", cfg.MaxClient, cfg.MaxSonny)
	}
}

func TestNegotiateCodec(t *testing.T) {
	cfg := DefaultConfig()
	c, e, err := negotiateCodec(CompressUnspecified, EncryptUnspecified, cfg)
	if err != nil || c != CompressZstd || e != EncryptChaCha20 {
		t.Fatalf("default negotiate: c=%v e=%v err=%v", c, e, err)
	}
	c, e, err = negotiateCodec(CompressZlib, EncryptNone, cfg)
	if err != nil || c != CompressZlib || e != EncryptNone {
		t.Fatalf("explicit negotiate: c=%v e=%v err=%v", c, e, err)
	}
	cfg.Compress = 0
	c, e, err = negotiateCodec(CompressZstd, EncryptAESGCM, cfg)
	if err != nil || c != CompressNone || e != EncryptAESGCM {
		t.Fatalf("compress off: c=%v e=%v err=%v", c, e, err)
	}
}

func TestAuthProof(t *testing.T) {
	ch, err := makeAuthChallenge()
	if err != nil {
		t.Fatal(err)
	}
	proof := computeAuthProof("secret", ch)
	if !verifyAuthProof("secret", ch, proof) {
		t.Fatal("valid proof rejected")
	}
	if verifyAuthProof("wrong", ch, proof) {
		t.Fatal("invalid key accepted")
	}
	bad := append([]byte(nil), proof[:len(proof)-1]...)
	if verifyAuthProof("secret", ch, bad) {
		t.Fatal("truncated proof accepted")
	}
}

func TestAEADWireRoundTrip(t *testing.T) {
	for _, et := range []ENCRYPT_TYPE{EncryptChaCha20, EncryptAESGCM} {
		p := &ProxyConn{}
		p.setCodec(FrameCodec{
			CompressType: CompressNone,
			EncryptType:  et,
			EncryptKey:   "wire-secret",
		})
		raw := []byte("hello aead frame body " + encryptTypeName(et))
		sealed, err := p.sealWire(raw)
		if err != nil {
			t.Fatalf("%s seal: %v", encryptTypeName(et), err)
		}
		if string(sealed) == string(raw) {
			t.Fatalf("%s seal did not encrypt", encryptTypeName(et))
		}
		plain, err := p.openWire(sealed)
		if err != nil {
			t.Fatalf("%s open: %v", encryptTypeName(et), err)
		}
		if string(plain) != string(raw) {
			t.Fatalf("%s roundtrip mismatch", encryptTypeName(et))
		}
	}
}

func TestMarshalAEADNoInnerEncrypt(t *testing.T) {
	raw := bytes.Repeat([]byte("payload "), 8)
	f := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id:   "1",
			Data: append([]byte(nil), raw...),
			Crc:  common.GetCrc32(raw),
		},
	}
	codec := FrameCodec{CompressThreshold: 0, CompressType: CompressNone, EncryptType: EncryptChaCha20, EncryptKey: "k"}
	data, err := MarshalSrpFrame(f, codec)
	if err != nil {
		t.Fatal(err)
	}
	// DATA bytes inside protobuf should remain plaintext; AEAD is wire-level.
	res, err := UnmarshalSrpFrame(data, codec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.DataFrame.Data, raw) {
		t.Fatal("unexpected data mutation")
	}
}

func TestSetCongestion(t *testing.T) {
	// Test setCongestion with tcp conn (should not panic or error)
	conn, err := network.NewConn("tcp")
	if err != nil {
		t.Skip("tcp conn not available")
	}
	cfg := DefaultConfig()
	cfg.Congestion = "bbr"
	setCongestion(conn, cfg)
}
