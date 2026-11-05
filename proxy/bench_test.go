package proxy

import (
	"sync/atomic"
	"testing"

	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
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

// BenchmarkPrioQueueContended models the client main channel under
// backpressure: one consumer drains while N sonny copySonnyRecv goroutines
// push into a full queue and block. PopWait used to Broadcast on every pop,
// waking every blocked pusher; per-priority conds wake at most one per
// priority, so ns/op here drops as pusher count grows.
func BenchmarkPrioQueueContended(b *testing.B) {
	for _, pushers := range []int{1, 8, 64} {
		b.Run("pushers_"+itoa(pushers), func(b *testing.B) {
			q := newPrioQueue(64)
			frame := &ProxyFrame{}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < pushers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						q.Push(frame, prioBulk)
					}
				}()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for {
					_, _, ok := q.PopWait(time.Second)
					if ok {
						break
					}
				}
			}
			b.StopTimer()
			// Pushers may be parked in Push on a full queue; Close's
			// broadcast is what wakes them (stop alone cannot).
			q.Close()
			close(stop)
			wg.Wait()
		})
	}
}

// BenchmarkSendWindow measures the per-frame flow-control accounting on the
// copySonnyRecv hot path: one acquire (producer side) and one release
// (father-side window update), which used to cost a mutex pair plus a
// Broadcast per frame even with zero contention.
func BenchmarkSendWindow(b *testing.B) {
	p := &ProxyConn{}
	p.initSendWindow(32)
	wg := thread.NewGroup("bench-window", nil, func() {})
	defer wg.Stop()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !p.acquireSendWindow(wg, MAX_CHUNK_SIZE, 1) {
			b.Fatal("acquire failed")
		}
		p.releaseSendWindow(MAX_CHUNK_SIZE, 1)
	}
}

// BenchmarkSendDataContended models the client main channel: every sonny's
// copySonnyRecv pushes into the one shared ProxyConn while the sendTo
// consumer drains, so conn-state reads (pickSendQ) and the queue itself are
// hit by many goroutines per frame. The old per-frame RWMutex read-lock
// ping-pongs a cache line between cores; the atomic isClosed + plain field
// read does not. The drainer is what keeps pushes from wedging on a full
// queue once calibration pushes b.N past any fixed capacity.
func BenchmarkSendDataContended(b *testing.B) {
	for _, conns := range []int{1, 64} {
		b.Run("conns_"+itoa(conns), func(b *testing.B) {
			p := &ProxyConn{}
			p.sendq = newPrioQueue(4096)
			frame := benchDataFrame(benchPayload(1024, "text"))
			var pushed int64
			stop := make(chan struct{})
			var swg sync.WaitGroup
			var dwg sync.WaitGroup
			dwg.Add(1)
			go func() {
				defer dwg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if v, closed, ok := p.sendq.PopWait(time.Second); ok && !closed {
						_ = v
					}
				}
			}()
			for i := 0; i < conns; i++ {
				swg.Add(1)
				go func() {
					defer swg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						p.SendData(frame, false)
						atomic.AddInt64(&pushed, 1)
					}
				}()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for atomic.LoadInt64(&pushed) < int64(b.N) {
				time.Sleep(100 * time.Microsecond)
			}
			b.StopTimer()
			close(stop)
			p.sendq.Close() // wake a pusher parked in Push
			swg.Wait()
			dwg.Wait()
		})
	}
}

// BenchmarkMainRecvLoopback measures the main-channel receive loop end to end
// over a real TCP socket. The sender replays one pre-sealed DATA frame per
// write (batched to ~64KB so the sender's syscall cost never bottlenecks);
// the receiver runs the real recvFrom loop — length prefix, body, decrypt,
// decompress, unmarshal, queue push — with a drainer popping the queue. The
// timer covers the whole pipeline, and ns/op tracks the receive path, which
// is where the buffered reader cuts per-frame syscalls.
func BenchmarkMainRecvLoopback(b *testing.B) {
	for _, c := range []struct {
		name string
		mix  string
		size int
		enc  string
	}{
		{"16KB_text/chacha20", "text", 16 * 1024, "chacha20"},
		{"16KB_rand/chacha20", "rand", 16 * 1024, "chacha20"},
		{"1KB_text/none", "text", 1024, "none"},
	} {
		b.Run(c.name, func(b *testing.B) {
			var et ENCRYPT_TYPE
			key := ""
			if c.enc == "chacha20" {
				et, key = EncryptChaCha20, "bench-key"
			}
			codec := FrameCodec{
				CompressType:      CompressZstd,
				CompressThreshold: 128,
				EncryptType:       et,
				EncryptKey:        key,
			}

			// Probe a free port the same way the e2e suite does, then bind
			// the gohome listener to it (one bind only).
			pl, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			port := pl.Addr().(*net.TCPAddr).Port
			pl.Close()
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

			type recvResult struct {
				err    error
				frames int64
			}
			recvDone := make(chan recvResult, 1)

			p := &ProxyConn{}
			p.recvq = newPrioQueue(4096)
			p.setCodec(codec)

			wg := thread.NewGroup("bench-recv", nil, func() {})
			defer wg.Stop()

			// Accept through gohome's network stack so recvFrom runs exactly as
			// in production (network.Conn, not net.Conn).
			listener, lerr := network.NewConn("tcp")
			if lerr != nil {
				b.Fatal(lerr)
			}
			listenconn, lerr := listener.Listen(addr)
			if lerr != nil {
				b.Fatal(lerr)
			}
			defer listenconn.Close()

			go func() {
				conn, aerr := listenconn.Accept()
				if aerr != nil {
					recvDone <- recvResult{err: aerr}
					return
				}
				defer conn.Close()

				var processed int64
				drainDone := make(chan struct{})
				go func() {
					defer close(drainDone)
					for {
						v, closed, ok := p.recvq.PopWait(time.Second)
						if !ok && closed {
							return
						}
						if ok {
							if queueFrame(v).Type == FRAME_TYPE_DATA {
								atomic.AddInt64(&processed, 1)
							}
							releasePooled(v)
						}
					}
				}()

				rerr := recvFrom(wg, p, conn, DefaultConfig().MaxMsgSize)
				p.recvq.Close()
				<-drainDone
				recvDone <- recvResult{err: rerr, frames: atomic.LoadInt64(&processed)}
			}()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatal(err)
			}
			tcp := conn.(*net.TCPConn)

			payload := benchPayload(c.size, c.mix)
			sp := &ProxyConn{}
			sp.setCodec(codec)
			mb, err := MarshalSrpFrame(benchDataFrame(payload), codec)
			if err != nil {
				b.Fatal(err)
			}
			sealed, err := sp.sealWire(mb)
			if err != nil {
				b.Fatal(err)
			}
			// Batch frames per Write to ~64KB: keeps the sender at a fraction
			// of the receiver's per-frame cost for both payload sizes.
			framesPerWrite := 64 * 1024 / (4 + len(sealed))
			if framesPerWrite < 1 {
				framesPerWrite = 1
			}
			blob := make([]byte, 0, framesPerWrite*(4+len(sealed)))
			var fb [4]byte
			binary.LittleEndian.PutUint32(fb[:], uint32(len(sealed)))
			for i := 0; i < framesPerWrite; i++ {
				blob = append(blob, fb[:]...)
				blob = append(blob, sealed...)
			}

			b.SetBytes(int64(c.size))
			b.ReportAllocs()
			b.ResetTimer()

			sent := 0
			for sent < b.N {
				if _, err := conn.Write(blob); err != nil {
					b.Fatal(err)
				}
				sent += framesPerWrite
			}
			// Half-close so the receiver drains everything then sees EOF.
			if err := tcp.CloseWrite(); err != nil {
				b.Fatal(err)
			}
			res := <-recvDone
			b.StopTimer()
			conn.Close()

			if res.err != nil && !errors.Is(res.err, io.EOF) {
				// EOF is the normal end: the sender half-closes after the
				// last frame. Anything else is a protocol failure, and the
				// frame-count check below guards against early exits too.
				b.Fatalf("recvFrom: %v", res.err)
			}
			if res.frames < int64(b.N) {
				b.Fatalf("processed %d frames, want >= %d", res.frames, b.N)
			}
		})
	}
}

// BenchmarkCompressGate isolates the adaptive compression gate. With
// incompressible traffic the steady-state marshal cost should sit at the
// no-compress level once the gate enters skip mode (plus amortized probes);
// compressible traffic is unaffected because every attempt succeeds.
func BenchmarkCompressGate(b *testing.B) {
	for _, mix := range []string{"text", "rand"} {
		b.Run(mix+"/32KB", func(b *testing.B) {
			payload := benchPayload(32*1024, mix)
			p := &ProxyConn{}
			p.setCodec(FrameCodec{CompressType: CompressZstd, CompressThreshold: 128})
			codec := p.getCodec()
			plain := make([]byte, 0, 128*1024)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				plain, err = MarshalSrpFrameAppend(plain[:0], benchDataFrame(payload), codec)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMainSendLoopback measures the main-channel send loop end to end:
// the benchmark pushes b.N DATA frames into sendq (blocking when full, which
// is the real backpressure), the receiver side runs the real sendTo — pop,
// marshal, seal, batch, one Write — and a sink drains the wire counting
// bytes. ns/op tracks the send path, where batching amortizes the per-frame
// Write syscall and packet.
func BenchmarkMainSendLoopback(b *testing.B) {
	for _, c := range []struct {
		name string
		mix  string
		size int
		enc  string
	}{
		{"16KB_text/chacha20", "text", 16 * 1024, "chacha20"},
		{"1KB_text/none", "text", 1024, "none"},
	} {
		b.Run(c.name, func(b *testing.B) {
			var et ENCRYPT_TYPE
			key := ""
			if c.enc == "chacha20" {
				et, key = EncryptChaCha20, "bench-key"
			}
			codec := FrameCodec{
				CompressType:      CompressZstd,
				CompressThreshold: 128,
				EncryptType:       et,
				EncryptKey:        key,
			}

			// One frame's sealed wire size is constant for identical
			// plaintext, so the sink can count bytes instead of parsing.
			probe := &ProxyConn{}
			probe.setCodec(codec)
			mb, err := MarshalSrpFrame(benchDataFrame(benchPayload(c.size, c.mix)), codec)
			if err != nil {
				b.Fatal(err)
			}
			sealed, err := probe.sealWire(mb)
			if err != nil {
				b.Fatal(err)
			}
			frameWire := 4 + len(sealed)

			pl, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			port := pl.Addr().(*net.TCPAddr).Port
			pl.Close()
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

			type sendResult struct{ bytes int64 }
			sinkDone := make(chan sendResult, 1)

			// sendTo side: accept through gohome's network stack. Bind before
			// dialing so the client connect never races the listener.
			listener, lerr := network.NewConn("tcp")
			if lerr != nil {
				b.Fatal(lerr)
			}
			listenconn, lerr := listener.Listen(addr)
			if lerr != nil {
				b.Fatal(lerr)
			}
			defer listenconn.Close()

			// Sink: drain the wire, report total bytes seen.
			cli, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatal(err)
			}
			go func() {
				n, _ := io.Copy(io.Discard, cli)
				sinkDone <- sendResult{bytes: n}
			}()

			srv, aerr := listenconn.Accept()
			if aerr != nil {
				b.Fatal(aerr)
			}

			p := &ProxyConn{conn: srv}
			p.sendq = newPrioQueue(4096)
			p.setCodec(codec)

			wg := thread.NewGroup("bench-send", nil, func() {})
			defer wg.Stop()
			var pingflag, pongflag int32
			var pongtime int64
			sendDone := make(chan error, 1)
			go func() {
				sendDone <- sendTo(wg, p.sendq, p, srv, DefaultConfig().MaxMsgSize, &pingflag, &pongflag, &pongtime)
			}()

			frame := benchDataFrame(benchPayload(c.size, c.mix))
			expected := int64(b.N) * int64(frameWire)
			b.SetBytes(int64(c.size))
			b.ReportAllocs()
			b.ResetTimer()

			sent := 0
			for sent < b.N {
				p.SendData(frame, false)
				sent++
			}
			// All frames queued: close the queue so sendTo drains and exits.
			p.sendq.Close()
			if err := <-sendDone; err != nil {
				b.Fatalf("sendTo: %v", err)
			}
			// sendTo is done: closing srv ends the srv->cli direction, which
			// is what the sink reads, so its io.Copy gets EOF and reports.
			srv.Close()
			res := <-sinkDone
			b.StopTimer()
			cli.Close()

			if res.bytes < expected {
				b.Fatalf("sink saw %d bytes, want >= %d", res.bytes, expected)
			}
		})
	}
}

// BenchmarkSonnyRecvLoopback measures the per-sonny receive leg: an app-side
// sender writes 16KB chunks into the sonny conn, recvFromSonny frames them
// (zero-copy into pooled buffers this round) and pushes to recvch, and a
// drainer pops and releases. Frames fragment with TCP segmentation, so the
// sink counts bytes; ns/op tracks the sonny read path per 16KB of payload.
func BenchmarkSonnyRecvLoopback(b *testing.B) {
	const chunk = 16 * 1024

	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	port := pl.Addr().(*net.TCPAddr).Port
	pl.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	type recvResult struct {
		err   error
		bytes int64
	}
	done := make(chan recvResult, 1)

	// Sonny side: accept through gohome's stack, run the real recvFromSonny.
	listener, lerr := network.NewConn("tcp")
	if lerr != nil {
		b.Fatal(lerr)
	}
	listenconn, lerr := listener.Listen(addr)
	if lerr != nil {
		b.Fatal(lerr)
	}
	defer listenconn.Close()

	cli, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}

	srv, aerr := listenconn.Accept()
	if aerr != nil {
		b.Fatal(aerr)
	}

	p := &ProxyConn{conn: srv}
	p.recvch = newMsgChannel(4096)

	wg := thread.NewGroup("bench-sonny", nil, func() {})
	defer wg.Stop()

	go func() {
		var bytes int64
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			// ReadWait drains buffered frames even after Close.
			for {
				v, ok := p.recvch.ReadWait()
				if !ok {
					return
				}
				f := queueFrame(v)
				atomic.AddInt64(&bytes, int64(len(f.DataFrame.Data)))
				releasePooled(v)
			}
		}()
		rerr := recvFromSonny(wg, p, srv, DefaultConfig().MaxMsgSize)
		p.recvch.Close()
		<-drainDone
		done <- recvResult{err: rerr, bytes: atomic.LoadInt64(&bytes)}
	}()
	payload := benchPayload(chunk, "text")
	b.SetBytes(chunk)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cli.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	if err := cli.(*net.TCPConn).CloseWrite(); err != nil {
		b.Fatal(err)
	}
	res := <-done
	b.StopTimer()
	cli.Close()
	srv.Close()

	if res.err != nil && !errors.Is(res.err, io.EOF) {
		b.Fatalf("recvFromSonny: %v", res.err)
	}
	if res.bytes < int64(b.N)*chunk {
		b.Fatalf("sink saw %d bytes, want >= %d", res.bytes, int64(b.N)*chunk)
	}
}

// BenchmarkSonnySendLoopback measures the per-sonny send leg (the download
// terminal): frames are queued into sendch with sequential indexes, the real
// sendToSonny pops, validates, batches (stream targets) and writes to a real
// TCP conn, and a sink counts bytes. ns/op tracks the sonny write path per
// 16KB of payload.
func BenchmarkSonnySendLoopback(b *testing.B) {
	const chunk = 16 * 1024

	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	port := pl.Addr().(*net.TCPAddr).Port
	pl.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	listener, lerr := network.NewConn("tcp")
	if lerr != nil {
		b.Fatal(lerr)
	}
	listenconn, lerr := listener.Listen(addr)
	if lerr != nil {
		b.Fatal(lerr)
	}
	defer listenconn.Close()

	cli, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	sinkDone := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(io.Discard, cli)
		sinkDone <- n
	}()

	srv, aerr := listenconn.Accept()
	if aerr != nil {
		b.Fatal(aerr)
	}

	p := &ProxyConn{conn: srv}
	p.sendch = newMsgChannel(4096)

	wg := thread.NewGroup("bench-sonny-send", nil, func() {})
	defer wg.Stop()
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- sendToSonny(wg, p, nil, p.sendch, srv, DefaultConfig().MaxMsgSize)
	}()

	// Frames must carry the sequence indexes sendToSonny validates.
	frames := make([]*ProxyFrame, 1024)
	for i := range frames {
		f := &ProxyFrame{}
		f.Type = FRAME_TYPE_DATA
		f.DataFrame = &DataFrame{}
		f.DataFrame.Data = benchPayload(chunk, "text")
		f.DataFrame.Index = int32((i + 1) % MAX_INDEX)
		frames[i] = f
	}
	b.SetBytes(chunk)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.SendSonnyData(frames[i%len(frames)], 0)
	}
	p.sendch.Close()
	if err := <-sendDone; err != nil {
		b.Fatalf("sendToSonny: %v", err)
	}
	srv.Close()
	saw := <-sinkDone
	b.StopTimer()
	cli.Close()

	if saw < int64(b.N)*chunk {
		b.Fatalf("sink saw %d bytes, want >= %d", saw, int64(b.N)*chunk)
	}
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
