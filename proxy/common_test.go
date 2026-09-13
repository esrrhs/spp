package proxy

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/network"
)

func Test0001(t *testing.T) {
	src := "aaabsfasasdfasfas3rdsfasfdhsafdshsafafafafaffasfsafa1111111111111111111111111111111111111111111111111111111111"
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_DATA
	f.DataFrame = &DataFrame{}
	f.DataFrame.Data = []byte(src)
	fmt.Println(len(f.DataFrame.Data))
	b, err := MarshalSrpFrame(f, 10, "123123")
	if err != nil {
		t.Error(err)
	}
	fmt.Println(len(f.DataFrame.Data))
	ff, err := UnmarshalSrpFrame(b, "123123")
	if err != nil {
		t.Error(err)
	}
	if string(ff.DataFrame.Data) != src {
		t.Error("data mismatch")
	}
	fmt.Println(string(ff.DataFrame.Data))
}

func TestMarshalUnmarshalAllFrameTypes(t *testing.T) {
	testKey := "test-secret-key"

	// 1. FRAME_TYPE_LOGIN
	{
		f := &ProxyFrame{
			Type: FRAME_TYPE_LOGIN,
			LoginFrame: &LoginFrame{
				Name:       "test-client",
				Key:        "secret",
				Clienttype: CLIENT_TYPE_PROXY,
				Proxyproto: PROXY_PROTO_TCP,
				Fromaddr:   ":8080",
				Toaddr:     ":9090",
			},
		}
		data, err := MarshalSrpFrame(f, 0, testKey)
		if err != nil {
			t.Fatalf("Marshal LOGIN frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, testKey)
		if err != nil {
			t.Fatalf("Unmarshal LOGIN frame failed: %v", err)
		}
		if res.Type != FRAME_TYPE_LOGIN || res.LoginFrame == nil || res.LoginFrame.Name != "test-client" {
			t.Errorf("LOGIN frame mismatch: %+v", res.LoginFrame)
		}
	}

	// 2. FRAME_TYPE_LOGINRSP
	{
		f := &ProxyFrame{
			Type: FRAME_TYPE_LOGINRSP,
			LoginRspFrame: &LoginRspFrame{
				Ret: true,
				Msg: "login success",
			},
		}
		data, err := MarshalSrpFrame(f, 0, testKey)
		if err != nil {
			t.Fatalf("Marshal LOGINRSP frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, testKey)
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
		data, err := MarshalSrpFrame(f, 0, "")
		if err != nil {
			t.Fatalf("Marshal DATA frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, "")
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
		data, err := MarshalSrpFrame(f, 32, "encrypt-key")
		if err != nil {
			t.Fatalf("Marshal compressed/encrypted DATA frame failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, "encrypt-key")
		if err != nil {
			t.Fatalf("Unmarshal compressed/encrypted DATA frame failed: %v", err)
		}
		if !bytes.Equal(res.DataFrame.Data, raw) {
			t.Errorf("DATA frame uncompressed content mismatch")
		}
	}

	// 5. FRAME_TYPE_PING & PONG
	{
		now := time.Now().UnixNano()
		ping := &ProxyFrame{
			Type:      FRAME_TYPE_PING,
			PingFrame: &PingFrame{Time: now},
		}
		data, err := MarshalSrpFrame(ping, 0, "")
		if err != nil {
			t.Fatalf("Marshal PING failed: %v", err)
		}
		res, err := UnmarshalSrpFrame(data, "")
		if err != nil {
			t.Fatalf("Unmarshal PING failed: %v", err)
		}
		if res.Type != FRAME_TYPE_PING || res.PingFrame.Time != now {
			t.Errorf("PING frame mismatch")
		}

		pong := &ProxyFrame{
			Type:      FRAME_TYPE_PONG,
			PongFrame: &PongFrame{Time: now},
		}
		dataPong, err := MarshalSrpFrame(pong, 0, "")
		if err != nil {
			t.Fatalf("Marshal PONG failed: %v", err)
		}
		resPong, err := UnmarshalSrpFrame(dataPong, "")
		if err != nil {
			t.Fatalf("Unmarshal PONG failed: %v", err)
		}
		if resPong.Type != FRAME_TYPE_PONG || resPong.PongFrame.Time != now {
			t.Errorf("PONG frame mismatch")
		}
	}

	// 6. FRAME_TYPE_OPEN & OPENRSP
	{
		open := &ProxyFrame{
			Type: FRAME_TYPE_OPEN,
			OpenFrame: &OpenConnFrame{
				Id:     "session-123",
				Toaddr: "127.0.0.1:80",
			},
		}
		data, err := MarshalSrpFrame(open, 0, "")
		if err != nil {
			t.Fatalf("Marshal OPEN failed: %v", err)
		}
		resOpen, err := UnmarshalSrpFrame(data, "")
		if err != nil {
			t.Fatalf("Unmarshal OPEN failed: %v", err)
		}
		if resOpen.OpenFrame.Id != "session-123" || resOpen.OpenFrame.Toaddr != "127.0.0.1:80" {
			t.Errorf("OPEN frame mismatch")
		}

		openRsp := &ProxyFrame{
			Type: FRAME_TYPE_OPENRSP,
			OpenRspFrame: &OpenConnRspFrame{
				Id:  "session-123",
				Ret: true,
				Msg: "ok",
			},
		}
		dataRsp, err := MarshalSrpFrame(openRsp, 0, "")
		if err != nil {
			t.Fatalf("Marshal OPENRSP failed: %v", err)
		}
		resRsp, err := UnmarshalSrpFrame(dataRsp, "")
		if err != nil {
			t.Fatalf("Unmarshal OPENRSP failed: %v", err)
		}
		if !resRsp.OpenRspFrame.Ret || resRsp.OpenRspFrame.Id != "session-123" {
			t.Errorf("OPENRSP frame mismatch")
		}
	}

	// 7. FRAME_TYPE_CLOSE
	{
		closeF := &ProxyFrame{
			Type:       FRAME_TYPE_CLOSE,
			CloseFrame: &CloseFrame{Id: "session-close-1"},
		}
		data, err := MarshalSrpFrame(closeF, 0, "")
		if err != nil {
			t.Fatalf("Marshal CLOSE failed: %v", err)
		}
		resClose, err := UnmarshalSrpFrame(data, "")
		if err != nil {
			t.Fatalf("Unmarshal CLOSE failed: %v", err)
		}
		if resClose.CloseFrame.Id != "session-close-1" {
			t.Errorf("CLOSE frame mismatch")
		}
	}
}

func TestCheckProxyFrameErrors(t *testing.T) {
	// Frame with missing payload should return error
	tests := []*ProxyFrame{
		{Type: FRAME_TYPE_LOGIN, LoginFrame: nil},
		{Type: FRAME_TYPE_LOGINRSP, LoginRspFrame: nil},
		{Type: FRAME_TYPE_DATA, DataFrame: nil},
		{Type: FRAME_TYPE_PING, PingFrame: nil},
		{Type: FRAME_TYPE_PONG, PongFrame: nil},
		{Type: FRAME_TYPE_OPEN, OpenFrame: nil},
		{Type: FRAME_TYPE_OPENRSP, OpenRspFrame: nil},
		{Type: FRAME_TYPE_CLOSE, CloseFrame: nil},
		{Type: FRAME_TYPE(999)}, // Invalid type
	}

	for i, f := range tests {
		err := checkProxyFame(f)
		if err == nil {
			t.Errorf("case %d expected error, got nil", i)
		}
		_, err = MarshalSrpFrame(f, 0, "")
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
	if cfg.MaxClient <= 0 || cfg.MaxSonny <= 0 {
		t.Errorf("invalid default limits: client=%d, sonny=%d", cfg.MaxClient, cfg.MaxSonny)
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
