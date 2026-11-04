package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/sys/cpu"
	"google.golang.org/protobuf/proto"
)

// Convenience aliases for generated enum constants.
const (
	CompressUnspecified = COMPRESS_TYPE_COMPRESS_UNSPECIFIED
	CompressNone        = COMPRESS_TYPE_COMPRESS_NONE
	CompressZlib        = COMPRESS_TYPE_COMPRESS_ZLIB
	CompressZstd        = COMPRESS_TYPE_COMPRESS_ZSTD

	EncryptUnspecified = ENCRYPT_TYPE_ENCRYPT_UNSPECIFIED
	EncryptNone        = ENCRYPT_TYPE_ENCRYPT_NONE
	EncryptAESGCM      = ENCRYPT_TYPE_ENCRYPT_AES_GCM
	EncryptChaCha20    = ENCRYPT_TYPE_ENCRYPT_CHACHA20

	authChallengeLen = 32
	aeadNonceSize    = 12
	aeadOverhead     = aeadNonceSize + 16 // nonce + Poly1305/GCM tag
)

// FrameCodec holds per-session compression/encryption settings.
// AEAD types seal the entire protobuf on the wire (including LOGIN).
// gate is nil in hand-built codecs (tests, benchmarks): those always attempt
// compression exactly as before; only conns wired through setCodec get the
// adaptive gate.
type FrameCodec struct {
	CompressThreshold int
	CompressType      COMPRESS_TYPE
	EncryptKey        string
	EncryptType       ENCRYPT_TYPE
	gate              *compressGate
}

// DefaultEncryptType picks the default wire AEAD for this machine: AES-GCM
// where the CPU has AES hardware (AES-NI / ARM crypto extensions — the common
// case on server chips, and measurably faster than ChaCha20 on the seal/open
// path there), ChaCha20 where software AES would be slower. Every peer that
// speaks the current negotiation protocol supports both (they were introduced
// together in v0.10.0), so the choice needs no fallback machinery.
func DefaultEncryptType() ENCRYPT_TYPE {
	if cpu.X86.HasAES || cpu.ARM64.HasAES {
		return EncryptAESGCM
	}
	return EncryptChaCha20
}

func defaultFrameCodec(cfg *Config) FrameCodec {
	ct := cfg.CompressType
	if ct == CompressUnspecified {
		ct = CompressZstd
	}
	et := cfg.EncryptType
	if et == EncryptUnspecified {
		et = DefaultEncryptType()
	}
	if cfg.Compress <= 0 {
		ct = CompressNone
	}
	if cfg.Encrypt == "" || et == EncryptNone {
		et = EncryptNone
	}
	return FrameCodec{
		CompressThreshold: cfg.Compress,
		CompressType:      ct,
		EncryptKey:        cfg.Encrypt,
		EncryptType:       et,
	}
}

func ParseCompressType(s string) (COMPRESS_TYPE, error) {
	return parseCompressType(s)
}

func ParseEncryptType(s string) (ENCRYPT_TYPE, error) {
	return parseEncryptType(s)
}

func parseCompressType(s string) (COMPRESS_TYPE, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default", "unspecified":
		return CompressUnspecified, nil
	case "none", "off", "0":
		return CompressNone, nil
	case "zlib":
		return CompressZlib, nil
	case "zstd":
		return CompressZstd, nil
	default:
		return CompressUnspecified, errors.New("unsupported compress type: " + s)
	}
}

func parseEncryptType(s string) (ENCRYPT_TYPE, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default", "unspecified":
		return EncryptUnspecified, nil
	case "none", "off", "0":
		return EncryptNone, nil
	case "aes-gcm", "aesgcm", "aes":
		return EncryptAESGCM, nil
	case "chacha20", "chacha20-poly1305", "chacha":
		return EncryptChaCha20, nil
	default:
		return EncryptUnspecified, errors.New("unsupported encrypt type: " + s)
	}
}

func compressTypeName(t COMPRESS_TYPE) string {
	switch t {
	case CompressNone:
		return "none"
	case CompressZlib:
		return "zlib"
	case CompressZstd:
		return "zstd"
	default:
		return "unspecified"
	}
}

func encryptTypeName(t ENCRYPT_TYPE) string {
	switch t {
	case EncryptNone:
		return "none"
	case EncryptAESGCM:
		return "aes-gcm"
	case EncryptChaCha20:
		return "chacha20"
	default:
		return "unspecified"
	}
}

func effectiveCompressType(t COMPRESS_TYPE) COMPRESS_TYPE {
	if t == CompressUnspecified {
		return CompressZstd
	}
	return t
}

func effectiveEncryptType(t ENCRYPT_TYPE) ENCRYPT_TYPE {
	if t == EncryptUnspecified {
		return EncryptChaCha20
	}
	return t
}

func isAEADEncrypt(t ENCRYPT_TYPE) bool {
	switch effectiveEncryptType(t) {
	case EncryptAESGCM, EncryptChaCha20:
		return true
	default:
		return false
	}
}

func supportedCompressType(t COMPRESS_TYPE) bool {
	switch effectiveCompressType(t) {
	case CompressNone, CompressZlib, CompressZstd:
		return true
	default:
		return false
	}
}

func supportedEncryptType(t ENCRYPT_TYPE) bool {
	switch effectiveEncryptType(t) {
	case EncryptNone, EncryptAESGCM, EncryptChaCha20:
		return true
	default:
		return false
	}
}

// negotiateCodec picks session codecs from client request and server config.
func negotiateCodec(reqCompress COMPRESS_TYPE, reqEncrypt ENCRYPT_TYPE, cfg *Config) (COMPRESS_TYPE, ENCRYPT_TYPE, error) {
	wantC := reqCompress
	if wantC == CompressUnspecified {
		wantC = cfg.CompressType
	}
	wantC = effectiveCompressType(wantC)
	if cfg.Compress <= 0 {
		wantC = CompressNone
	}
	if !supportedCompressType(wantC) {
		return 0, 0, errors.New("unsupported compress type " + compressTypeName(wantC))
	}

	wantE := reqEncrypt
	if wantE == EncryptUnspecified {
		wantE = cfg.EncryptType
	}
	wantE = effectiveEncryptType(wantE)
	if cfg.Encrypt == "" {
		wantE = EncryptNone
	}
	if !supportedEncryptType(wantE) {
		return 0, 0, errors.New("unsupported encrypt type " + encryptTypeName(wantE))
	}
	return wantC, wantE, nil
}

func compressPayload(t COMPRESS_TYPE, src []byte) ([]byte, error) {
	switch effectiveCompressType(t) {
	case CompressNone:
		return src, nil
	case CompressZlib:
		return common.CompressData(src), nil
	case CompressZstd:
		return common.CompressDataZstd(src), nil
	default:
		return nil, errors.New("unsupported compress type")
	}
}

func deriveAEADKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func newAEAD(t ENCRYPT_TYPE, key string) (cipher.AEAD, error) {
	k := deriveAEADKey(key)
	switch effectiveEncryptType(t) {
	case EncryptAESGCM:
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	case EncryptChaCha20:
		return chacha20poly1305.New(k)
	default:
		return nil, errors.New("not an AEAD encrypt type")
	}
}

func makeAuthChallenge() ([]byte, error) {
	b := make([]byte, authChallengeLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func computeAuthProof(authKey string, challenge []byte) []byte {
	mac := hmac.New(sha256.New, []byte(authKey))
	mac.Write(challenge)
	return mac.Sum(nil)
}

func verifyAuthProof(authKey string, challenge, proof []byte) bool {
	if len(challenge) == 0 || len(proof) == 0 {
		return false
	}
	expected := computeAuthProof(authKey, challenge)
	return hmac.Equal(expected, proof)
}

// compressGate adaptively skips compression attempts on incompressible
// streams. A failed attempt still pays the full zstd pass (and its output
// allocation) for nothing — on TLS-like traffic that is dead weight on every
// frame of the hot path. After gateFailThreshold consecutive attempts that
// produced no shrinkage, the gate stops trying for gateSkipFrames frames,
// re-probing every gateProbeInterval frames so compressible traffic (a TLS
// stream that later carries HTTP body text, say) is picked back up within a
// few hundred frames. Skipping is always wire-safe: an uncompressed frame
// carries Compress=false and the receiver handles both forms.
// Single-writer (the conn's send loop); atomics keep it robust regardless.
type compressGate struct {
	skips int32 // frames left in skip mode (atomic)
	fails int32 // consecutive failed attempts (atomic)
}

const (
	gateFailThreshold = 8   // consecutive failures before skipping
	gateSkipFrames    = 128 // frames to skip before full re-probing
	gateProbeInterval = 16  // while skipping, still try every 16th frame
)

// allow reports whether this frame should attempt compression. nil disables
// the gate (always true).
func (g *compressGate) allow() bool {
	if g == nil {
		return true
	}
	s := atomic.LoadInt32(&g.skips)
	if s <= 0 {
		return true
	}
	// Decrement so skip mode eventually expires even without successes;
	// probe once every gateProbeInterval frames to re-detect compressible
	// traffic. (Multiple senders cannot happen per conn, but the CAS-style
	// decrement keeps the accounting exact regardless.)
	for {
		if atomic.CompareAndSwapInt32(&g.skips, s, s-1) {
			break
		}
		s = atomic.LoadInt32(&g.skips)
		if s <= 0 {
			return true
		}
	}
	return s%gateProbeInterval == 0
}

// record feeds an attempt outcome (ok = the frame actually shrank).
func (g *compressGate) record(ok bool) {
	if g == nil {
		return
	}
	if ok {
		atomic.StoreInt32(&g.fails, 0)
		atomic.StoreInt32(&g.skips, 0)
		return
	}
	if atomic.AddInt32(&g.fails, 1) >= gateFailThreshold {
		atomic.StoreInt32(&g.skips, gateSkipFrames)
		atomic.StoreInt32(&g.fails, 0)
	}
}

// sessionCodec is an immutable per-session codec snapshot. setCodec builds it
// once and publishes it via atomic.Pointer, so the per-frame wire path
// (sealWireAppend / openWire / getCodec) pays a single atomic load instead of
// three RWMutex read-locks plus several atomic field loads per frame. The
// nonce prefix lives in the snapshot too: it is fixed for the lifetime of one
// codec instance, and the monotonic nonceCounter (atomic) keeps nonces unique
// across snapshots.
type sessionCodec struct {
	encryptKey        string
	compressType      COMPRESS_TYPE
	encryptType       ENCRYPT_TYPE
	compressThreshold int
	gate              *compressGate
	aead              cipher.AEAD
	noncePrefix       [4]byte
}

func (p *ProxyConn) setCodec(c FrameCodec) {
	sc := &sessionCodec{
		compressType:      c.CompressType,
		encryptType:       c.EncryptType,
		compressThreshold: c.CompressThreshold,
		encryptKey:        c.EncryptKey,
	}
	if effectiveCompressType(c.CompressType) != CompressNone && c.CompressThreshold > 0 {
		sc.gate = &compressGate{}
	}
	if c.EncryptKey != "" && isAEADEncrypt(c.EncryptType) {
		aead, err := newAEAD(c.EncryptType, c.EncryptKey)
		if err == nil {
			sc.aead = aead
			if _, err := rand.Read(sc.noncePrefix[:]); err != nil {
				sc.aead = nil
			}
		} else {
			loggo.Error("setCodec AEAD init fail: %s", err.Error())
		}
	}
	p.codec.Store(sc)
	atomic.StoreUint64(&p.nonceCounter, 0)
}

func (p *ProxyConn) getCodec() FrameCodec {
	sc := p.codec.Load()
	if sc == nil {
		return FrameCodec{}
	}
	return FrameCodec{
		CompressThreshold: sc.compressThreshold,
		CompressType:      sc.compressType,
		EncryptKey:        sc.encryptKey,
		EncryptType:       sc.encryptType,
		gate:              sc.gate,
	}
}

// nextNonce fills buf with the per-conn prefix plus the next counter value.
// buf is a stack array so the hot seal path does not allocate.
func (p *ProxyConn) nextNonce(buf *[aeadNonceSize]byte, sc *sessionCodec) {
	copy(buf[:4], sc.noncePrefix[:])
	ctr := atomic.AddUint64(&p.nonceCounter, 1)
	binary.BigEndian.PutUint64(buf[4:], ctr)
}

// sealWire AEAD-seals a marshaled protobuf when using AES-GCM/ChaCha20.
// NONE returns plaintext unchanged.
func (p *ProxyConn) sealWire(plain []byte) ([]byte, error) {
	return p.sealWireAppend(nil, plain)
}

// sealWireAppend appends the sealed blob after dst's existing bytes and
// returns the extended buffer (same base array). The caller reserves the
// frame's 4-byte length prefix inside dst before calling — for a single frame
// that is a 4-byte buffer; for a batch it is the whole wire built so far plus
// the next prefix slot. Lets sendTo reuse one scratch buffer for the whole
// connection instead of allocating per frame.
func (p *ProxyConn) sealWireAppend(dst []byte, plain []byte) ([]byte, error) {
	sc := p.codec.Load()
	if sc == nil || !isAEADEncrypt(sc.encryptType) || sc.encryptKey == "" {
		return append(dst, plain...), nil
	}
	aead := sc.aead
	if aead == nil {
		return nil, errors.New("AEAD not initialized")
	}
	var nonce [aeadNonceSize]byte
	p.nextNonce(&nonce, sc)
	need := aeadNonceSize + len(plain) + aead.Overhead()
	if cap(dst)-len(dst) < need {
		grown := make([]byte, len(dst), len(dst)+need+64)
		copy(grown, dst)
		dst = grown
	}
	dst = append(dst, nonce[:]...)
	return aead.Seal(dst, nonce[:], plain, nil), nil
}

// openWire AEAD-opens a wire blob when using AES-GCM/ChaCha20.
// NONE returns the blob unchanged.
func (p *ProxyConn) openWire(blob []byte) ([]byte, error) {
	return p.openWireAppend(nil, blob)
}

// openWireAppend opens into dst, so the recv loop can reuse one scratch
// buffer across frames instead of allocating a fresh plaintext per frame.
// NONE returns the blob unchanged (no copy), mirroring sealWireAppend.
func (p *ProxyConn) openWireAppend(dst []byte, blob []byte) ([]byte, error) {
	sc := p.codec.Load()
	if sc == nil || !isAEADEncrypt(sc.encryptType) || sc.encryptKey == "" {
		return blob, nil
	}
	aead := sc.aead
	if aead == nil {
		return nil, errors.New("AEAD not initialized")
	}
	if len(blob) < aeadNonceSize+aead.Overhead() {
		return nil, errors.New("AEAD blob too short")
	}
	return aead.Open(dst[:0], blob[:aeadNonceSize], blob[aeadNonceSize:], nil)
}

// prepareFrame validates the frame and compresses its payload when the codec
// asks for it. Shared by MarshalSrpFrame and MarshalSrpFrameAppend.
func prepareFrame(f *ProxyFrame, codec FrameCodec) error {
	err := checkProxyFame(f)
	if err != nil {
		return err
	}

	if f.Type == FRAME_TYPE_DATA &&
		codec.CompressType != CompressNone &&
		codec.CompressThreshold > 0 &&
		len(f.DataFrame.Data) > codec.CompressThreshold &&
		!f.DataFrame.Compress {
		if !codec.gate.allow() {
			return nil
		}
		newb, err := compressPayload(codec.CompressType, f.DataFrame.Data)
		if err != nil {
			return err
		}
		if len(newb) < len(f.DataFrame.Data) {
			if loggo.IsDebug() {
				loggo.Debug("MarshalSrpFrame Compress(%s) from %d %d",
					compressTypeName(codec.CompressType), len(f.DataFrame.Data), len(newb))
			}
			codec.gate.record(true)
			atomic.AddInt64(&gState.SendCompSaveSize, int64(len(f.DataFrame.Data)-len(newb)))
			f.DataFrame.Data = newb
			f.DataFrame.Compress = true
			f.DataFrame.CompressType = codec.CompressType
		} else {
			codec.gate.record(false)
		}
	}
	return nil
}

func MarshalSrpFrame(f *ProxyFrame, codec FrameCodec) ([]byte, error) {
	if err := prepareFrame(f, codec); err != nil {
		return nil, err
	}
	mb, err := proto.Marshal(f)
	if err != nil {
		return nil, err
	}
	return mb, err
}

// MarshalSrpFrameAppend marshals into dst instead of a fresh buffer, so the
// send loop can reuse one scratch buffer across frames.
func MarshalSrpFrameAppend(dst []byte, f *ProxyFrame, codec FrameCodec) ([]byte, error) {
	if err := prepareFrame(f, codec); err != nil {
		return nil, err
	}
	return proto.MarshalOptions{}.MarshalAppend(dst, f)
}

// frameBufPool recycles the decompressed DATA buffers of the download path.
// The memory profile shows the per-frame zstd output allocation as ~96% of all
// allocated bytes on a busy proxy; decoding into a pooled buffer and returning
// it after the payload's last use (sendToSonny's Write) removes that churn.
// Only frames explicitly wrapped as pooled ever touch the pool, so plain and
// test-constructed frames can never alias a recycled buffer.
const (
	frameBufCap     = 32 * 1024      // covers a full MAX_CHUNK_SIZE decompressed frame
	frameBufMinCap  = MAX_CHUNK_SIZE // recvFromSonny reads need at least this much
	frameBufMaxCap  = 64 * 1024      // buffers grown beyond this are dropped, not pooled
	frameMaxDecoded = 8 << 20        // matches gohome's zstd decoder limit
)

var dataBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, frameBufCap)
	return &b
}}

// frameZstdDecoder mirrors gohome's shared decoder settings; DecodeAll into a
// pooled buffer needs a decoder owned by this package.
var frameZstdDecoder, _ = zstd.NewReader(nil,
	zstd.WithDecoderConcurrency(1),
	zstd.WithDecoderMaxMemory(frameMaxDecoded),
)

// getPooledDataBuf returns an empty buffer from the pool.
func getPooledDataBuf() []byte {
	return (*dataBufPool.Get().(*[]byte))[:0]
}

// putPooledDataBuf returns an exclusively-owned buffer to the pool. Buffers
// outside [frameBufMinCap, frameBufMaxCap] are dropped: recvFromSonny reads
// require the lower bound, and the bound keeps small payload buffers (e.g.
// proto-owned frames of a few bytes, whose cap protobuf sizes exactly) from
// poisoning the pool.
func putPooledDataBuf(buf []byte) {
	if cap(buf) < frameBufMinCap || cap(buf) > frameBufMaxCap {
		return
	}
	b := buf[:0]
	dataBufPool.Put(&b)
}

// decompressPayloadPooled is decompressPayload with the zstd output taken from
// dataBufPool. pooled=true means the caller owns the buffer exclusively and
// must putPooledDataBuf it after its last use.
func decompressPayloadPooled(t COMPRESS_TYPE, src []byte) (out []byte, pooled bool, err error) {
	switch effectiveCompressType(t) {
	case CompressNone:
		return src, false, nil
	case CompressZstd:
		buf := getPooledDataBuf()
		out, err = frameZstdDecoder.DecodeAll(src, buf)
		if err != nil {
			putPooledDataBuf(buf)
			return nil, false, err
		}
		if len(out) > frameMaxDecoded {
			putPooledDataBuf(out)
			return nil, false, errors.New("decompressed data too large")
		}
		return out, true, nil
	case CompressZlib:
		out, err := common.DeCompressData(src)
		return out, false, err
	default:
		return nil, false, errors.New("unsupported compress type")
	}
}

func decompressPayload(t COMPRESS_TYPE, src []byte) ([]byte, error) {
	out, _, err := decompressPayloadPooled(t, src)
	return out, err
}

func UnmarshalSrpFrame(b []byte, codec FrameCodec) (*ProxyFrame, error) {
	f, _, err := unmarshalSrpFrame(b, codec, false)
	return f, err
}

// UnmarshalSrpFramePooled takes the decompressed DATA payload from the shared
// buffer pool. pooled=true means f.DataFrame.Data must be returned to the pool
// with putPooledDataBuf after its last use.
func UnmarshalSrpFramePooled(b []byte, codec FrameCodec) (f *ProxyFrame, pooled bool, err error) {
	return unmarshalSrpFrame(b, codec, true)
}

func unmarshalSrpFrame(b []byte, codec FrameCodec, usePool bool) (*ProxyFrame, bool, error) {
	f := &ProxyFrame{}
	err := proto.Unmarshal(b, f)
	if err != nil {
		return nil, false, err
	}

	err = checkProxyFame(f)
	if err != nil {
		return nil, false, err
	}

	if f.Type == FRAME_TYPE_DATA && f.DataFrame.Compress {
		ct := f.DataFrame.CompressType
		if ct == CompressUnspecified || ct == CompressNone {
			ct = codec.CompressType
		}
		var newb []byte
		var pooled bool
		if usePool {
			newb, pooled, err = decompressPayloadPooled(ct, f.DataFrame.Data)
		} else {
			newb, err = decompressPayload(ct, f.DataFrame.Data)
		}
		if err != nil {
			return nil, false, err
		}
		if loggo.IsDebug() {
			loggo.Debug("UnmarshalSrpFrame Compress(%s) from %d %d",
				compressTypeName(ct), len(f.DataFrame.Data), len(newb))
		}
		atomic.AddInt64(&gState.RecvCompSaveSize, int64(len(newb)-len(f.DataFrame.Data)))
		f.DataFrame.Data = newb
		f.DataFrame.Compress = false
		f.DataFrame.CompressType = CompressUnspecified
		return f, pooled, nil
	}

	// Uncompressed DATA: the payload is proto.Unmarshal's fresh, exclusively
	// owned buffer, equally safe to hand to the pool for the terminal
	// consumer to recycle (TLS-like rand traffic, where nothing compresses,
	// pays a 16KB allocation per frame without this).
	if usePool && f.Type == FRAME_TYPE_DATA && f.DataFrame != nil {
		return f, true, nil
	}

	return f, false, nil
}
