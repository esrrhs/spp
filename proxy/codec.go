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
	"sync/atomic"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/crypto/chacha20poly1305"
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
type FrameCodec struct {
	CompressThreshold int
	CompressType      COMPRESS_TYPE
	EncryptKey        string
	EncryptType       ENCRYPT_TYPE
}

func defaultFrameCodec(cfg *Config) FrameCodec {
	ct := cfg.CompressType
	if ct == CompressUnspecified {
		ct = CompressZstd
	}
	et := cfg.EncryptType
	if et == EncryptUnspecified {
		et = EncryptChaCha20
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

func decompressPayload(t COMPRESS_TYPE, src []byte) ([]byte, error) {
	switch effectiveCompressType(t) {
	case CompressNone:
		return src, nil
	case CompressZlib:
		return common.DeCompressData(src)
	case CompressZstd:
		return common.DeCompressDataZstd(src)
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

func (p *ProxyConn) setCodec(c FrameCodec) {
	atomic.StoreInt32(&p.compressType, int32(c.CompressType))
	atomic.StoreInt32(&p.encryptType, int32(c.EncryptType))
	atomic.StoreInt32(&p.compressThreshold, int32(c.CompressThreshold))
	p.mu.Lock()
	p.encryptKey = c.EncryptKey
	p.aead = nil
	if c.EncryptKey != "" && isAEADEncrypt(c.EncryptType) {
		aead, err := newAEAD(c.EncryptType, c.EncryptKey)
		if err == nil {
			p.aead = aead
			if _, err := rand.Read(p.noncePrefix[:]); err != nil {
				p.aead = nil
			}
			atomic.StoreUint64(&p.nonceCounter, 0)
		} else {
			loggo.Error("setCodec AEAD init fail: %s", err.Error())
		}
	}
	p.mu.Unlock()
}

func (p *ProxyConn) getCodec() FrameCodec {
	p.mu.RLock()
	key := p.encryptKey
	p.mu.RUnlock()
	return FrameCodec{
		CompressThreshold: int(atomic.LoadInt32(&p.compressThreshold)),
		CompressType:      COMPRESS_TYPE(atomic.LoadInt32(&p.compressType)),
		EncryptKey:        key,
		EncryptType:       ENCRYPT_TYPE(atomic.LoadInt32(&p.encryptType)),
	}
}

func (p *ProxyConn) nextNonce() ([]byte, error) {
	n := make([]byte, aeadNonceSize)
	p.mu.RLock()
	copy(n[:4], p.noncePrefix[:])
	p.mu.RUnlock()
	ctr := atomic.AddUint64(&p.nonceCounter, 1)
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n, nil
}

// sealWire AEAD-seals a marshaled protobuf when using AES-GCM/ChaCha20.
// NONE returns plaintext unchanged.
func (p *ProxyConn) sealWire(plain []byte) ([]byte, error) {
	codec := p.getCodec()
	if !isAEADEncrypt(codec.EncryptType) || codec.EncryptKey == "" {
		return plain, nil
	}
	p.mu.RLock()
	aead := p.aead
	p.mu.RUnlock()
	if aead == nil {
		return nil, errors.New("AEAD not initialized")
	}
	nonce, err := p.nextNonce()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(nonce)+len(plain)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plain, nil)
	return out, nil
}

// openWire AEAD-opens a wire blob when using AES-GCM/ChaCha20.
func (p *ProxyConn) openWire(blob []byte) ([]byte, error) {
	codec := p.getCodec()
	if !isAEADEncrypt(codec.EncryptType) || codec.EncryptKey == "" {
		return blob, nil
	}
	p.mu.RLock()
	aead := p.aead
	p.mu.RUnlock()
	if aead == nil {
		return nil, errors.New("AEAD not initialized")
	}
	if len(blob) < aeadNonceSize+aead.Overhead() {
		return nil, errors.New("AEAD blob too short")
	}
	nonce := blob[:aeadNonceSize]
	return aead.Open(nil, nonce, blob[aeadNonceSize:], nil)
}

func MarshalSrpFrame(f *ProxyFrame, codec FrameCodec) ([]byte, error) {
	err := checkProxyFame(f)
	if err != nil {
		return nil, err
	}

	if f.Type == FRAME_TYPE_DATA &&
		codec.CompressType != CompressNone &&
		codec.CompressThreshold > 0 &&
		len(f.DataFrame.Data) > codec.CompressThreshold &&
		!f.DataFrame.Compress {
		newb, err := compressPayload(codec.CompressType, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if len(newb) < len(f.DataFrame.Data) {
			if loggo.IsDebug() {
				loggo.Debug("MarshalSrpFrame Compress(%s) from %d %d",
					compressTypeName(codec.CompressType), len(f.DataFrame.Data), len(newb))
			}
			atomic.AddInt64(&gState.SendCompSaveSize, int64(len(f.DataFrame.Data)-len(newb)))
			f.DataFrame.Data = newb
			f.DataFrame.Compress = true
			f.DataFrame.CompressType = codec.CompressType
		}
	}

	mb, err := proto.Marshal(f)
	if err != nil {
		return nil, err
	}
	return mb, err
}

func UnmarshalSrpFrame(b []byte, codec FrameCodec) (*ProxyFrame, error) {
	f := &ProxyFrame{}
	err := proto.Unmarshal(b, f)
	if err != nil {
		return nil, err
	}

	err = checkProxyFame(f)
	if err != nil {
		return nil, err
	}

	if f.Type == FRAME_TYPE_DATA && f.DataFrame.Compress {
		ct := f.DataFrame.CompressType
		if ct == CompressUnspecified || ct == CompressNone {
			ct = codec.CompressType
		}
		newb, err := decompressPayload(ct, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("UnmarshalSrpFrame Compress(%s) from %d %d",
				compressTypeName(ct), len(f.DataFrame.Data), len(newb))
		}
		atomic.AddInt64(&gState.RecvCompSaveSize, int64(len(newb)-len(f.DataFrame.Data)))
		f.DataFrame.Data = newb
		f.DataFrame.Compress = false
		f.DataFrame.CompressType = CompressUnspecified
	}

	return f, nil
}
