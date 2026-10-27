package proxy

import (
	"bytes"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/esrrhs/gohome/thread"
)

// Targeted unit coverage for codec, inputer and socks5 helper branches that
// the e2e suites cannot reach deterministically.

func TestDefaultFrameCodec_AllBranches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Encrypt = "codec-edge-key"
	cfg.CompressType = CompressUnspecified
	cfg.EncryptType = EncryptUnspecified
	c := defaultFrameCodec(cfg)
	if c.CompressType != CompressZstd {
		t.Fatalf("unspecified compress should default to zstd, got %v", c.CompressType)
	}
	if c.EncryptType != EncryptChaCha20 {
		t.Fatalf("unspecified encrypt should default to chacha20, got %v", c.EncryptType)
	}

	cfg.Compress = 0
	c = defaultFrameCodec(cfg)
	if c.CompressType != CompressNone {
		t.Fatalf("compress<=0 should force none, got %v", c.CompressType)
	}

	cfg.Compress = 128
	cfg.Encrypt = ""
	c = defaultFrameCodec(cfg)
	if c.EncryptType != EncryptNone {
		t.Fatalf("empty encrypt key should force none, got %v", c.EncryptType)
	}
}

func TestCompressTypeName_Unknown(t *testing.T) {
	if got := compressTypeName(COMPRESS_TYPE(99)); got == "" || got == "none" || got == "zlib" || got == "zstd" {
		t.Fatalf("unknown compress type name %q", got)
	}
}

func TestNegotiateCodec_UnsupportedTypes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Encrypt = "negotiate-key"
	if _, _, err := negotiateCodec(COMPRESS_TYPE(99), EncryptUnspecified, cfg); err == nil {
		t.Fatal("expected unsupported compress error")
	}
	if _, _, err := negotiateCodec(CompressUnspecified, ENCRYPT_TYPE(99), cfg); err == nil {
		t.Fatal("expected unsupported encrypt error")
	}
}

func TestNewAEAD_KeyErrors(t *testing.T) {
	// Keys are SHA-256 derived to 32 bytes, so only the not-an-AEAD branch
	// can fail; invalid key lengths are unreachable by construction.
	if _, err := newAEAD(EncryptNone, "some-key"); err == nil {
		t.Fatal("expected not-an-AEAD error")
	}
}

func TestSealOpenWire_AeadNotInitialized(t *testing.T) {
	p := &ProxyConn{}
	p.encryptKey = "wire-secret"
	atomic.StoreInt32(&p.encryptType, int32(EncryptAESGCM))
	if _, err := p.sealWire([]byte("x")); err == nil {
		t.Fatal("expected sealWire AEAD-not-initialized error")
	}
	if _, err := p.sealWireAppend(nil, []byte("x")); err == nil {
		t.Fatal("expected sealWireAppend AEAD-not-initialized error")
	}
	if _, err := p.openWire([]byte("x")); err == nil {
		t.Fatal("expected openWire AEAD-not-initialized error")
	}
}

func TestSealWireAppend_GrowAndReuse(t *testing.T) {
	p := &ProxyConn{}
	p.setCodec(FrameCodec{EncryptType: EncryptChaCha20, EncryptKey: "wire-secret-append"})
	plain := []byte("append-path payload")

	// Fresh buffer each time: growth branch (nil dst, no cap) and pre-sized
	// branch (cap >= need) both run; both blobs must open back to plain.
	b1, err := p.sealWireAppend(nil, plain)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := p.openWire(b1)
	if err != nil {
		t.Fatalf("open grown buffer: %v", err)
	}
	if !bytes.Equal(got1, plain) {
		t.Fatal("grown buffer roundtrip mismatch")
	}
	pre := make([]byte, 0, 4096)
	b2, err := p.sealWireAppend(pre, plain)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := p.openWire(b2)
	if err != nil {
		t.Fatalf("open reused buffer: %v", err)
	}
	if !bytes.Equal(got2, plain) {
		t.Fatal("pre-sized buffer roundtrip mismatch")
	}
}

func TestOpenWire_BlobTooShort(t *testing.T) {
	p := &ProxyConn{}
	p.setCodec(FrameCodec{EncryptType: EncryptChaCha20, EncryptKey: "wire-secret-short"})
	if _, err := p.openWire([]byte("tiny")); err == nil {
		t.Fatal("expected blob-too-short error")
	}
}

func TestPrepareFrame_CompressError(t *testing.T) {
	f := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id:   "comp-err",
			Data: bytes.Repeat([]byte("x"), 64),
		},
	}
	codec := FrameCodec{CompressThreshold: 8, CompressType: COMPRESS_TYPE(99)}
	if err := prepareFrame(f, codec); err == nil {
		t.Fatal("expected compress error from invalid type")
	}
	if _, err := MarshalSrpFrame(f, codec); err == nil {
		t.Fatal("expected MarshalSrpFrame to propagate compress error")
	}
	if _, err := MarshalSrpFrameAppend(nil, f, codec); err == nil {
		t.Fatal("expected MarshalSrpFrameAppend to propagate compress error")
	}
}

func TestUnmarshalSrpFrame_DecompressError(t *testing.T) {
	f := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id:           "decomp-err",
			Data:         []byte("not-really-compressed-data-garbage"),
			Compress:     true,
			CompressType: CompressZstd,
		},
	}
	bin, err := MarshalSrpFrame(f, FrameCodec{CompressThreshold: 0, CompressType: CompressNone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalSrpFrame(bin, FrameCodec{CompressThreshold: 0, CompressType: CompressNone}); err == nil {
		t.Fatal("expected decompress error for corrupt compressed data")
	}
}

func TestNewInputer_Errors(t *testing.T) {
	wg := thread.NewGroup("TestNewInputer", nil, nil)
	defer wg.Stop()

	if _, err := NewInputer(wg, "no-such-proto", "127.0.0.1:0", CLIENT_TYPE_PROXY, DefaultConfig(), nil, "t:1", 0); err == nil {
		t.Fatal("expected NewConn error for unknown proto")
	}
	// Valid proto, but the listen address can never bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := NewInputer(wg, "tcp", ln.Addr().String(), CLIENT_TYPE_PROXY, DefaultConfig(), nil, "t:1", 0); err == nil {
		t.Fatal("expected Listen error for busy addr")
	}
	if _, err := NewSocks5Inputer(wg, "no-such-proto", "127.0.0.1:0", CLIENT_TYPE_SOCKS5, DefaultConfig(), nil, 0); err == nil {
		t.Fatal("expected NewConn error for socks5 inputer")
	}
	if _, err := NewSocks5Inputer(wg, "tcp", ln.Addr().String(), CLIENT_TYPE_SOCKS5, DefaultConfig(), nil, 0); err == nil {
		t.Fatal("expected Listen error for socks5 inputer")
	}
}

func TestSpawnSonny_ExitGroupReturnsFalse(t *testing.T) {
	wg := thread.NewGroup("TestSpawnSonny", nil, nil)
	wg.Stop()
	i := &Inputer{fwg: wg}
	if i.spawnSonny("never", func() {}) {
		t.Fatal("spawnSonny must refuse an exiting group")
	}
}

func TestSocks5ControlLocalHost_NilConn(t *testing.T) {
	if got := socks5ControlLocalHost(nil); got != "" {
		t.Fatalf("nil conn should yield empty control host, got %q", got)
	}
}

func TestStatusServerStart_NilCollector(t *testing.T) {
	_, err := StartStatusServer("127.0.0.1:0", nil)
	if err == nil || !strings.Contains(err.Error(), "nil status report collector") {
		t.Fatalf("expected nil-collector error, got %v", err)
	}
}
