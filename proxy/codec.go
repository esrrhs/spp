package proxy

import (
	"errors"
	"strings"
	"sync/atomic"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
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
	EncryptRC4         = ENCRYPT_TYPE_ENCRYPT_RC4
)

// FrameCodec holds per-session compression/encryption settings applied to DATA frames.
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
		et = EncryptRC4
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
	case "rc4":
		return EncryptRC4, nil
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
	case EncryptRC4:
		return "rc4"
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
		return EncryptRC4
	}
	return t
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
	case EncryptNone, EncryptRC4:
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

func encryptPayload(t ENCRYPT_TYPE, key string, src []byte) ([]byte, error) {
	switch effectiveEncryptType(t) {
	case EncryptNone:
		return src, nil
	case EncryptRC4:
		if key == "" {
			return src, nil
		}
		return common.Rc4(key, src)
	default:
		return nil, errors.New("unsupported encrypt type")
	}
}

func (p *ProxyConn) setCodec(c FrameCodec) {
	atomic.StoreInt32(&p.compressType, int32(c.CompressType))
	atomic.StoreInt32(&p.encryptType, int32(c.EncryptType))
	atomic.StoreInt32(&p.compressThreshold, int32(c.CompressThreshold))
	p.mu.Lock()
	p.encryptKey = c.EncryptKey
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

	if f.Type == FRAME_TYPE_DATA && codec.EncryptType != EncryptNone && codec.EncryptKey != "" {
		newb, err := encryptPayload(codec.EncryptType, codec.EncryptKey, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("MarshalSrpFrame Encrypt(%s) from %s %s",
				encryptTypeName(codec.EncryptType), common.GetCrc32(f.DataFrame.Data), common.GetCrc32(newb))
		}
		f.DataFrame.Data = newb
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

	if f.Type == FRAME_TYPE_DATA && codec.EncryptType != EncryptNone && codec.EncryptKey != "" {
		newb, err := encryptPayload(codec.EncryptType, codec.EncryptKey, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("UnmarshalSrpFrame Encrypt(%s) from %s %s",
				encryptTypeName(codec.EncryptType), common.GetCrc32(f.DataFrame.Data), common.GetCrc32(newb))
		}
		f.DataFrame.Data = newb
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
