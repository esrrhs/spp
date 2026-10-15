package proxy

import (
	"testing"

	"io"

	"github.com/esrrhs/gohome/loggo"
)

// Keep the benchmark output readable: the codec logs every compressed frame
// at debug level, which would otherwise bury the ns/op lines.
// NoLogFile matters: Ini() otherwise rewrites os.Stderr to a log file, which
// would swallow -race reports and test failures.
func init() {
	loggo.Ini(loggo.Config{
		Level:     loggo.LEVEL_ERROR,
		Prefix:    "spp-bench",
		NoLogFile: true,
	})
	loggo.SetPrinter(io.Discard)
}

// Micro-benchmarks for the per-byte hot path: every frame a proxy forwards
// pays marshal + (compress) + (encrypt) + one queue hop. End-to-end loopback
// throughput swings ±20% on a busy machine, so these are the stable numbers
// to compare optimizations against — especially for the small-CPU cloud boxes
// this runs on, where ns/byte and allocs/op are what actually bound the
// throughput.

func benchProxyConn(b *testing.B, ct COMPRESS_TYPE, et ENCRYPT_TYPE, key string, threshold int) *ProxyConn {
	p := &ProxyConn{}
	p.setCodec(FrameCodec{
		CompressType:      ct,
		CompressThreshold: threshold,
		EncryptType:       et,
		EncryptKey:        key,
	})
	return p
}

// benchPayload builds a payload of the given size. mix selects how
// compressible it is: "text" is what the loadgen sends (very compressible),
// "rand" is closer to real traffic (TLS/media), which is where compression
// earns nothing and only costs CPU.
func benchPayload(size int, mix string) []byte {
	buf := make([]byte, size)
	switch mix {
	case "rand":
		rnd := newBenchRand(0x9e3779b9)
		for i := range buf {
			buf[i] = byte(rnd.next())
		}
	default: // "text"
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	return buf
}

// xorshift keeps the benchmark deterministic without math/rand's global lock.
type benchRand struct{ s uint64 }

func newBenchRand(seed uint64) *benchRand { return &benchRand{s: seed | 1} }

func (r *benchRand) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

func benchDataFrame(payload []byte) *ProxyFrame {
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_DATA
	f.DataFrame = &DataFrame{Data: payload}
	return f
}

// BenchmarkMarshalSrpFrame isolates frame encoding (compression included).
func BenchmarkMarshalSrpFrame(b *testing.B) {
	for _, mix := range []string{"text", "rand"} {
		for _, size := range []int{1024, 32 * 1024} {
			for _, ct := range []COMPRESS_TYPE{CompressNone, CompressZstd} {
				name := mix + "/" + byteSize(size) + "/" + compressTypeName(ct)
				b.Run(name, func(b *testing.B) {
					payload := benchPayload(size, mix)
					codec := FrameCodec{CompressType: ct, CompressThreshold: 128}
					b.SetBytes(int64(size))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						f := benchDataFrame(payload)
						if _, err := MarshalSrpFrame(f, codec); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// BenchmarkSendPath covers what sendTo does per frame: encode, then seal.
// "alloc" is the current path (fresh buffer per frame); "reuse" feeds a
// scratch buffer, which is what sendTo does after the single-Write change.
func BenchmarkSendPath(b *testing.B) {
	for _, path := range []string{"alloc", "reuse"} {
		for _, enc := range []string{"none", "chacha20", "aesgcm"} {
			name := path + "/" + enc
			b.Run(name, func(b *testing.B) {
				var et ENCRYPT_TYPE
				key := ""
				switch enc {
				case "chacha20":
					et, key = EncryptChaCha20, "bench-key"
				case "aesgcm":
					et, key = EncryptAESGCM, "bench-key"
				}
				p := benchProxyConn(b, CompressZstd, et, key, 128)
				payload := benchPayload(32*1024, "text")
				codec := p.getCodec()
				wire := make([]byte, 4, 128*1024)
				plain := make([]byte, 0, 128*1024)
				b.SetBytes(int64(len(payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					f := benchDataFrame(payload)
					var sealed []byte
					var err error
					if path == "alloc" {
						var mb []byte
						mb, err = MarshalSrpFrame(f, codec)
						if err != nil {
							b.Fatal(err)
						}
						sealed, err = p.sealWire(mb)
					} else {
						plain, err = MarshalSrpFrameAppend(plain[:0], f, codec)
						if err != nil {
							b.Fatal(err)
						}
						sealed, err = p.sealWireAppend(wire[:4], plain)
					}
					if err != nil {
						b.Fatal(err)
					}
					_ = sealed
				}
			})
		}
	}
}

// BenchmarkUnmarshalSrpFrame is the receive side: decrypt-free here, but it
// pays the decompression and protobuf decode on every inbound frame.
func BenchmarkUnmarshalSrpFrame(b *testing.B) {
	for _, mix := range []string{"text", "rand"} {
		b.Run(mix, func(b *testing.B) {
			payload := benchPayload(32*1024, mix)
			codec := FrameCodec{CompressType: CompressZstd, CompressThreshold: 128}
			f := benchDataFrame(payload)
			mb, err := MarshalSrpFrame(f, codec)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := UnmarshalSrpFrame(mb, codec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPrioQueue measures the cost of one queue hop. Every frame crosses
// a prioQueue, and on the client all sonny conns share the single main-channel
// queue, so this is the contention point on a multi-connection box.
func BenchmarkPrioQueue(b *testing.B) {
	// Reuse one frame so the reported allocs are the queue's own, not the
	// benchmark's.
	frame := &ProxyFrame{}
	b.Run("pushpop", func(b *testing.B) {
		q := newPrioQueue(128)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			q.Push(frame, prioBulk)
			_, _, _ = q.PopWait(0)
		}
	})
	b.Run("pushpop_control", func(b *testing.B) {
		q := newPrioQueue(128)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			q.Push(frame, prioControl)
			_, _, _ = q.PopWait(0)
		}
	})
}

func byteSize(n int) string {
	switch {
	case n >= 1024*1024:
		return itoa(n/(1024*1024)) + "MB"
	case n >= 1024:
		return itoa(n/1024) + "KB"
	default:
		return itoa(n) + "B"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
