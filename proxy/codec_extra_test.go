package proxy

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/esrrhs/gohome/common"
)

func TestCompressPayload_ErrorsAndPassthrough(t *testing.T) {
	src := []byte("hello")

	// COMPRESS_NONE returns the input untouched (no allocation semantics
	// relied upon, but content must match).
	out, err := compressPayload(CompressNone, src)
	if err != nil || string(out) != string(src) {
		t.Fatalf("none passthrough: out=%q err=%v", out, err)
	}

	if _, err := compressPayload(COMPRESS_TYPE(99), src); err == nil {
		t.Fatal("unsupported compress type should error on compress")
	}
	if _, err := decompressPayload(COMPRESS_TYPE(99), src); err == nil {
		t.Fatal("unsupported compress type should error on decompress")
	}
	if _, err := decompressPayload(CompressNone, src); err != nil {
		t.Fatalf("none decompress passthrough: %v", err)
	}
}

func TestCompressDecompress_RoundTrip(t *testing.T) {
	for _, ct := range []COMPRESS_TYPE{CompressZlib, CompressZstd} {
		raw := bytes.Repeat([]byte("round-trip payload block "), 64)
		c, err := compressPayload(ct, raw)
		if err != nil {
			t.Fatalf("%s compress: %v", compressTypeName(ct), err)
		}
		back, err := decompressPayload(ct, c)
		if err != nil {
			t.Fatalf("%s decompress: %v", compressTypeName(ct), err)
		}
		if !bytes.Equal(back, raw) {
			t.Fatalf("%s roundtrip mismatch", compressTypeName(ct))
		}
	}
}

// Incompressible data that ends up larger after compression must be sent
// uncompressed instead of inflating every frame.
func TestMarshal_IncompressibleDataStaysPlaintext(t *testing.T) {
	noise := make([]byte, 8192)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []COMPRESS_TYPE{CompressZlib, CompressZstd} {
		f := &ProxyFrame{
			Type: FRAME_TYPE_DATA,
			DataFrame: &DataFrame{
				Id:   "noise",
				Data: append([]byte(nil), noise...),
			},
		}
		codec := FrameCodec{CompressThreshold: 128, CompressType: ct, EncryptType: EncryptNone}
		if _, err := MarshalSrpFrame(f, codec); err != nil {
			t.Fatalf("%s marshal: %v", compressTypeName(ct), err)
		}
		if f.DataFrame.Compress {
			t.Fatalf("%s must not flag incompressible payload as compressed", compressTypeName(ct))
		}
	}
}

// Small frames below the threshold must never be compressed.
func TestMarshal_BelowThresholdNotCompressed(t *testing.T) {
	raw := []byte("small")
	f := &ProxyFrame{Type: FRAME_TYPE_DATA, DataFrame: &DataFrame{Id: "s", Data: raw}}
	codec := FrameCodec{CompressThreshold: 128, CompressType: CompressZstd, EncryptType: EncryptNone}
	if _, err := MarshalSrpFrame(f, codec); err != nil {
		t.Fatal(err)
	}
	if f.DataFrame.Compress {
		t.Fatal("sub-threshold frame must stay uncompressed")
	}
}

// Garbage inside the "compressed" envelope must surface a decode error
// instead of silently delivering corrupt bytes.
func TestUnmarshal_CorruptCompressedFails(t *testing.T) {
	f := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id:           "corrupt",
			Data:         []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x01, 0x02, 0x03}, // not valid zstd
			Compress:     true,
			CompressType: CompressZstd,
		},
	}
	codec := FrameCodec{CompressThreshold: 1, CompressType: CompressZstd, EncryptType: EncryptNone}
	b, err := MarshalSrpFrame(f, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalSrpFrame(b, codec); err == nil {
		t.Fatal("corrupt zstd payload must fail to decode")
	}

	// Frames marked compressed with UNSPECIFIED type fall back to the
	// session codec; garbage zlib must likewise error.
	f2 := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id: "corrupt2", Data: []byte{0x78, 0x9c, 0xff, 0xff},
			Compress: true, CompressType: CompressUnspecified,
		},
	}
	b2, err := MarshalSrpFrame(f2, FrameCodec{CompressType: CompressNone, EncryptType: EncryptNone})
	if err != nil {
		t.Fatal(err)
	}
	zlibCodec := FrameCodec{CompressThreshold: 1, CompressType: CompressZlib, EncryptType: EncryptNone}
	if _, err := UnmarshalSrpFrame(b2, zlibCodec); err == nil {
		t.Fatal("corrupt zlib payload must fail to decode")
	}
}

func TestUnmarshal_GarbageBytesError(t *testing.T) {
	if _, err := UnmarshalSrpFrame([]byte{0xff, 0xff, 0xff, 0xff}, testCodec(0, "")); err == nil {
		t.Fatal("invalid protobuf bytes must error")
	}
}

func TestAEAD_TamperAndTruncationRejected(t *testing.T) {
	p := &ProxyConn{}
	p.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: EncryptChaCha20, EncryptKey: "wire-secret"})
	raw := []byte("authenticated payload")
	sealed, err := p.sealWire(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Flip a ciphertext bit: authentication must fail.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := p.openWire(tampered); err == nil {
		t.Fatal("tampered AEAD blob accepted")
	}

	// Truncated blobs are rejected.
	if _, err := p.openWire(sealed[:3]); err == nil {
		t.Fatal("truncated AEAD blob accepted")
	}

	// A peer holding a different key cannot open the blob.
	other := &ProxyConn{}
	other.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: EncryptChaCha20, EncryptKey: "other-secret"})
	if _, err := other.openWire(sealed); err == nil {
		t.Fatal("AEAD blob opened with wrong key")
	}
}

func TestAEAD_NonceUniqueness(t *testing.T) {
	for _, et := range []ENCRYPT_TYPE{EncryptChaCha20, EncryptAESGCM} {
		p := &ProxyConn{}
		p.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: et, EncryptKey: "nonce-key"})
		seen := make(map[string]struct{}, 2000)
		plain := []byte("x")
		for i := 0; i < 2000; i++ {
			s, err := p.sealWire(plain)
			if err != nil {
				t.Fatal(err)
			}
			nonce := string(s[:aeadNonceSize])
			if _, dup := seen[nonce]; dup {
				t.Fatalf("%s nonce reused at iteration %d", encryptTypeName(et), i)
			}
			seen[nonce] = struct{}{}
		}
	}
}

func TestWireCodec_NoEncryptionPassthrough(t *testing.T) {
	p := &ProxyConn{}
	// EncryptType set but key empty: plaintext on the wire.
	p.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: EncryptChaCha20, EncryptKey: ""})
	raw := []byte("plain body")
	out, err := p.sealWire(raw)
	if err != nil || string(out) != string(raw) {
		t.Fatalf("empty key must passthrough: err=%v", err)
	}
	back, err := p.openWire(raw)
	if err != nil || string(back) != string(raw) {
		t.Fatalf("empty key open must passthrough: err=%v", err)
	}
}

func TestVerifyAuthProof_EmptyInputs(t *testing.T) {
	if verifyAuthProof("k", nil, []byte("proof")) {
		t.Fatal("empty challenge must be rejected")
	}
	if verifyAuthProof("k", []byte("challenge"), nil) {
		t.Fatal("empty proof must be rejected")
	}
}

func TestDefaultFrameCodec_Branches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Key = "auth"
	// Default config ships without an encrypt key: zstd + no wire encryption.
	c := defaultFrameCodec(cfg)
	if c.CompressType != CompressZstd || c.EncryptType != EncryptNone {
		t.Fatalf("unexpected default codec: %v %v", c.CompressType, c.EncryptType)
	}
	// With an encrypt key set, the chacha20 default kicks in.
	cfg.Encrypt = "wire-key"
	c = defaultFrameCodec(cfg)
	if c.CompressType != CompressZstd || c.EncryptType != EncryptChaCha20 {
		t.Fatalf("expected zstd+chacha20 with key: %v %v", c.CompressType, c.EncryptType)
	}

	// Compression disabled forces NONE even when a type is set.
	cfg2 := DefaultConfig()
	cfg2.Compress = 0
	c2 := defaultFrameCodec(cfg2)
	if c2.CompressType != CompressNone {
		t.Fatalf("compress<=0 must force none, got %v", c2.CompressType)
	}

	// Empty encrypt key forces NONE.
	cfg3 := DefaultConfig()
	cfg3.Encrypt = ""
	c3 := defaultFrameCodec(cfg3)
	if c3.EncryptType != EncryptNone {
		t.Fatalf("empty encrypt must force none, got %v", c3.EncryptType)
	}

	// Explicit EncryptNone is honored.
	cfg4 := DefaultConfig()
	cfg4.Encrypt = "key"
	cfg4.EncryptType = EncryptNone
	c4 := defaultFrameCodec(cfg4)
	if c4.EncryptType != EncryptNone {
		t.Fatalf("explicit none got %v", c4.EncryptType)
	}
}

func TestTypeName_DefaultBranches(t *testing.T) {
	if compressTypeName(COMPRESS_TYPE(99)) != "unspecified" {
		t.Fatal("unknown compress type name")
	}
	if encryptTypeName(ENCRYPT_TYPE(99)) != "unspecified" {
		t.Fatal("unknown encrypt type name")
	}
	if supportedCompressType(COMPRESS_TYPE(99)) {
		t.Fatal("unknown compress type reported supported")
	}
	if supportedEncryptType(ENCRYPT_TYPE(99)) {
		t.Fatal("unknown encrypt type reported supported")
	}
	if !isAEADEncrypt(EncryptAESGCM) || !isAEADEncrypt(EncryptChaCha20) {
		t.Fatal("AEAD types misclassified")
	}
	if isAEADEncrypt(EncryptNone) || isAEADEncrypt(ENCRYPT_TYPE(99)) {
		t.Fatal("non-AEAD types misclassified")
	}
}

func TestNewAEAD_UnknownTypeErrors(t *testing.T) {
	if _, err := newAEAD(ENCRYPT_TYPE(99), "k"); err == nil {
		t.Fatal("unknown AEAD type must error")
	}
	if a, err := newAEAD(EncryptAESGCM, "k"); err != nil || a == nil {
		t.Fatalf("AES-GCM init: %v %v", a, err)
	}
	if a, err := newAEAD(EncryptChaCha20, "k"); err != nil || a == nil {
		t.Fatalf("chacha20 init: %v %v", a, err)
	}
}

// Codec switch on an existing conn resets the nonce counter and AEAD state;
// seals after the switch must still open with the newly negotiated codec.
func TestSetCodec_RenegotiateRoundTrip(t *testing.T) {
	p := &ProxyConn{}
	p.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: EncryptChaCha20, EncryptKey: "k1"})
	s1, err := p.sealWire([]byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	p.setCodec(FrameCodec{CompressType: CompressNone, EncryptType: EncryptAESGCM, EncryptKey: "k2"})
	s2, err := p.sealWire([]byte("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(s1[:aeadNonceSize], s2[:aeadNonceSize]) {
		t.Fatal("nonce prefix should be regenerated when codec is reset")
	}
	back, err := p.openWire(s2)
	if err != nil || string(back) != "v2" {
		t.Fatalf("post-renegotiate roundtrip: %q err=%v", back, err)
	}
}

// Assert the CRC helper behavior the debug-path checks rely on is stable.
func TestCRC32_Stable(t *testing.T) {
	data := []byte("crc-input")
	a := common.GetCrc32(data)
	b := common.GetCrc32(append([]byte(nil), data...))
	if a != b || a == "" {
		t.Fatalf("crc unstable: %q %q", a, b)
	}
	if common.GetCrc32([]byte("other")) == a {
		t.Fatal("crc collided for distinct input")
	}
}
