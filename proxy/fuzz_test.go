package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/esrrhs/gohome/thread"
)

// Fuzz targets for the wire codec and the local-proxy request parsing.
//
// They run as ordinary tests over the seed corpus during `go test`, and can
// be driven for real with e.g.
//
//	go test ./proxy/ -run FuzzMarshalUnmarshalFrame -fuzz FuzzMarshalUnmarshalFrame -fuzztime=2m
//
// Every target asserts two things: never panic, and never silently corrupt
// (an invariant that must hold for whatever bytes the engine throws at it).
// Inputs are length-capped where a target would otherwise let the engine
// generate a decompression bomb (see fuzzBombNote below).

// fuzzFrameCodecs is the codec matrix the frame targets replay their input
// against. Keeping the matrix fixed and fuzzing the payload is what catches
// codec asymmetry — a payload that marshals but fails to unmarshal, or one
// that comes back with different bytes.
func fuzzFrameCodecs() []FrameCodec {
	return []FrameCodec{
		{CompressThreshold: 0, CompressType: CompressNone, EncryptType: EncryptNone},
		{CompressThreshold: 1, CompressType: CompressZstd, EncryptType: EncryptNone},
		{CompressThreshold: 1, CompressType: CompressZlib, EncryptType: EncryptNone},
		{CompressThreshold: 128, CompressType: CompressZstd, EncryptType: EncryptNone},
	}
}

// FuzzMarshalUnmarshalFrame: a DATA frame must survive marshal → unmarshal
// byte-identically for every codec, whatever the payload. This is the
// invariant the whole proxy rests on: a single asymmetric payload silently
// corrupts a stream (or trips the CRC check and kills the conn).
func FuzzMarshalUnmarshalFrame(f *testing.F) {
	f.Add([]byte(nil), "id", int32(0))
	f.Add([]byte("hello"), "c1", int32(7))
	f.Add(bytes.Repeat([]byte("A"), 4096), "big", int32(-1))
	f.Add([]byte{0x00, 0xff, 0x80, 0x7f}, "", int32(1))
	f.Add(bytes.Repeat([]byte{0}, 200), "zeros", int32(MAX_INDEX-1))

	f.Fuzz(func(t *testing.T, data []byte, id string, index int32) {
		if len(data) > 1<<16 {
			// Keep the target fast: big payloads are covered by the
			// boundary table test, not by the engine.
			t.Skip()
		}
		if !utf8.ValidString(id) {
			// proto3 strings must be valid UTF-8; the wire layer never
			// produces such ids, so skip instead of reporting a bogus
			// failure for an input the protocol cannot even express.
			t.Skip()
		}

		for _, codec := range fuzzFrameCodecs() {
			src := &ProxyFrame{
				Type: FRAME_TYPE_DATA,
				DataFrame: &DataFrame{
					Id:    id,
					Data:  append([]byte(nil), data...),
					Index: index,
				},
			}
			blob, err := MarshalSrpFrame(src, codec)
			if err != nil {
				t.Fatalf("marshal codec=%+v len=%d: %v", codec, len(data), err)
			}
			got, err := UnmarshalSrpFrame(blob, codec)
			if err != nil {
				t.Fatalf("unmarshal codec=%+v len=%d compressed=%v: %v",
					codec, len(data), src.DataFrame.Compress, err)
			}
			if got.DataFrame == nil {
				t.Fatalf("unmarshal codec=%+v: nil DataFrame", codec)
			}
			if got.DataFrame.Id != id {
				t.Fatalf("id mismatch: got %q want %q", got.DataFrame.Id, id)
			}
			if got.DataFrame.Index != index {
				t.Fatalf("index mismatch: got %d want %d", got.DataFrame.Index, index)
			}
			if !bytes.Equal(got.DataFrame.Data, data) {
				t.Fatalf("payload mismatch codec=%+v compressed=%v: got %d bytes want %d",
					codec, src.DataFrame.Compress, len(got.DataFrame.Data), len(data))
			}
		}
	})
}

// fuzzBombNote: UnmarshalSrpFrame decompresses a compressed DATA frame with
// no output bound, and zstd compresses a run of zeros ~5000:1 — a 200-byte
// frame expands to 1MB. A fuzz engine that finds that would happily OOM the
// worker, so this target caps its input. The amplification itself is tracked
// separately; it is a real hole, not something to paper over here.
const fuzzMaxBlob = 4096

// FuzzUnmarshalFrameBytes: arbitrary bytes off the wire must be rejected
// cleanly, never panic and never produce a frame that fails validation.
func FuzzUnmarshalFrameBytes(f *testing.F) {
	seed, err := MarshalSrpFrame(&ProxyFrame{
		Type:      FRAME_TYPE_DATA,
		DataFrame: &DataFrame{Id: "seed", Data: []byte("hello")},
	}, FrameCodec{})
	if err != nil {
		f.Fatalf("seed marshal: %v", err)
	}
	f.Add([]byte(nil))
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte("not a frame at all"))
	f.Add(seed)

	f.Fuzz(func(t *testing.T, blob []byte) {
		if len(blob) > fuzzMaxBlob {
			t.Skip() // see fuzzBombNote
		}
		for _, codec := range fuzzFrameCodecs() {
			got, err := UnmarshalSrpFrame(blob, codec)
			if err != nil {
				if got != nil {
					t.Fatalf("unmarshal returned a frame together with an error: %v", err)
				}
				continue
			}
			if got == nil {
				t.Fatal("unmarshal succeeded with a nil frame")
			}
			if err := checkProxyFame(got); err != nil {
				t.Fatalf("accepted a frame that fails validation: %v", err)
			}
		}
	})
}

// FuzzWireSealOpen: the AEAD wire layer must round-trip any payload and
// reject any foreign blob. Covers nonce handling (prefix + counter) and the
// short-blob guard in openWire.
func FuzzWireSealOpen(f *testing.F) {
	f.Add([]byte(nil), int32(0))
	f.Add([]byte("hello"), int32(int(EncryptChaCha20)))
	f.Add(bytes.Repeat([]byte("x"), 4096), int32(int(EncryptAESGCM)))
	f.Add([]byte{0x00}, int32(int(EncryptNone)))

	f.Fuzz(func(t *testing.T, payload []byte, encryptTypeInt int32) {
		if len(payload) > 1<<16 {
			t.Skip()
		}
		et := ENCRYPT_TYPE(encryptTypeInt)
		if !isAEADEncrypt(et) && et != EncryptNone {
			t.Skip() // unknown/non-AEAD: not a wire mode we ship
		}
		key := ""
		if et != EncryptNone {
			key = "fuzz-wire-key"
		}

		var p ProxyConn
		p.setCodec(FrameCodec{EncryptKey: key, EncryptType: et})

		sealed, err := p.sealWire(payload)
		if err != nil {
			t.Fatalf("sealWire encrypt=%v: %v", et, err)
		}
		opened, err := p.openWire(sealed)
		if err != nil {
			t.Fatalf("openWire round-trip encrypt=%v len=%d: %v", et, len(payload), err)
		}
		if !bytes.Equal(opened, payload) {
			t.Fatalf("round-trip mismatch encrypt=%v: got %d bytes want %d",
				et, len(opened), len(payload))
		}

		// A blob from a different key must never decrypt.
		if et != EncryptNone {
			var other ProxyConn
			other.setCodec(FrameCodec{EncryptKey: "another-key", EncryptType: et})
			if _, err := other.openWire(sealed); err == nil {
				t.Fatalf("openWire accepted a blob sealed under a different key (encrypt=%v)", et)
			}
		}
	})
}

// FuzzParseHost: the HTTP proxy builds the dial address from whatever the
// client put in the request line / Host header, so this runs on
// attacker-controlled strings. It must never panic, and what it returns must
// be either a dialable host:port pair or an explicit "unparseable" verdict.
// Three found bugs live in testdata/fuzz as regression seeds: ":]" was
// wrapped into "[:]]:0" and "[[" into "[[:0" — bracket soup that is neither
// dialable nor readable in the logs — and "[:]" into "[:]:0". A fourth
// class, malformed ports ("host:abc", ":" with a broken default port), is
// now rejected by validPort.
func FuzzParseHost(f *testing.F) {
	f.Add("example.com", "80")
	f.Add("example.com:8080", "80")
	f.Add("[::1]", "443")
	f.Add("[::1]:8080", "443")
	f.Add("", "80")
	f.Add("  example.com  ", "80")
	f.Add("host:", "80")
	f.Add("::1", "80")
	f.Add("example.com:abc", "80")
	f.Add(":", ":")

	f.Fuzz(func(t *testing.T, rawHost string, defaultPort string) {
		got := parseHost(rawHost, defaultPort)
		if !validPort(defaultPort) {
			// No branch can build a dialable address out of a malformed
			// default port, so the only correct answer is a refusal.
			if got != "" {
				t.Fatalf("parseHost(%q, %q) = %q, want rejection of a malformed default port", rawHost, defaultPort, got)
			}
			return
		}
		if got == "" {
			// The "unparseable" verdict is only allowed for a host that
			// cannot be dialed anyway: a colon that is not an IPv6 literal,
			// or stray brackets. Everything else must resolve.
			host := strings.TrimSpace(rawHost)
			if !strings.Contains(host, ":") && !strings.ContainsAny(host, "[]") {
				t.Fatalf("parseHost(%q, %q) gave up on a usable host", rawHost, defaultPort)
			}
			if net.ParseIP(host) != nil {
				t.Fatalf("parseHost(%q, %q) gave up on a valid IP literal", rawHost, defaultPort)
			}
			return
		}
		if !strings.Contains(got, ":") {
			t.Fatalf("parseHost(%q, %q) = %q, want a host:port pair", rawHost, defaultPort, got)
		}
		// A bracketed host must be a real IP: that is the only thing
		// JoinHostPort's brackets are allowed to wrap.
		if strings.HasPrefix(got, "[") {
			end := strings.Index(got, "]")
			if end < 0 {
				t.Fatalf("parseHost(%q, %q) = %q, unterminated bracket", rawHost, defaultPort, got)
			}
			if net.ParseIP(got[1:end]) == nil {
				t.Fatalf("parseHost(%q, %q) = %q, bracketed non-IP host", rawHost, defaultPort, got)
			}
		}
		// Whatever comes back must be a real address: the stdlib can split it
		// again, so the caller can hand it to net.Dial as-is.
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Fatalf("parseHost(%q, %q) = %q, not splittable: %v", rawHost, defaultPort, got, err)
		}
		// An explicit port in the request must survive: the proxy must not
		// silently dial the default instead.
		// (parseHost trims first, so compare against the trimmed input: a
		// whitespace-only port is not a port, it falls back to the default.)
		if _, wantPort, serr := net.SplitHostPort(strings.TrimSpace(rawHost)); serr == nil && validPort(wantPort) {
			if _, gotPort, _ := net.SplitHostPort(got); gotPort != wantPort {
				t.Fatalf("parseHost(%q, %q) = %q, port %q want %q", rawHost, defaultPort, got, gotPort, wantPort)
			}
		}
	})
}

// FuzzCompressRoundTrip: whatever bytes go in must come back out. Exercises
// the boundary sizes the engine likes to find (empty, 1 byte, threshold ± 1)
// against both compressors.
func FuzzCompressRoundTrip(f *testing.F) {
	f.Add([]byte(nil), int32(int(CompressNone)))
	f.Add([]byte("a"), int32(int(CompressZlib)))
	f.Add(bytes.Repeat([]byte("ab"), 1000), int32(int(CompressZstd)))
	f.Add(bytes.Repeat([]byte{0}, 5000), int32(int(CompressZstd)))

	f.Fuzz(func(t *testing.T, src []byte, ctInt int32) {
		if len(src) > 1<<16 {
			t.Skip()
		}
		ct := COMPRESS_TYPE(ctInt)
		if !supportedCompressType(ct) {
			t.Skip() // not a compressor we ship
		}
		packed, err := compressPayload(ct, src)
		if err != nil {
			t.Fatalf("compress %s len=%d: %v", compressTypeName(ct), len(src), err)
		}
		back, err := decompressPayload(ct, packed)
		if err != nil {
			t.Fatalf("decompress %s len=%d packed=%d: %v", compressTypeName(ct), len(src), len(packed), err)
		}
		if !bytes.Equal(back, src) {
			t.Fatalf("round-trip mismatch %s: got %d bytes want %d",
				compressTypeName(ct), len(back), len(src))
		}
	})
}

// ---------------------------------------------------------------------------
// Length-prefixed wire framing (recvFrom): the 4-byte little-endian header is
// spp's own format and is what decides how many body bytes to read, so every
// bounds decision on the receive side starts here.
// ---------------------------------------------------------------------------

const (
	fuzzStreamMaxMsg   = 4096 // small maxmsgsize keeps per-frame allocations cheap
	fuzzStreamMaxBytes = 64 << 10
)

type fuzzCodecVariant struct {
	name  string
	codec FrameCodec
}

// fuzzStreamVariants mirrors what a real session can negotiate: plain, both
// compressors with the gate enabled, and both AEADs sealing the whole frame.
func fuzzStreamVariants() []fuzzCodecVariant {
	const key = "fuzz-stream-key"
	return []fuzzCodecVariant{
		{"plain", FrameCodec{}},
		{"zlib", FrameCodec{CompressThreshold: 1, CompressType: CompressZlib}},
		{"zstd", FrameCodec{CompressThreshold: 1, CompressType: CompressZstd}},
		{"aes-gcm", FrameCodec{EncryptKey: key, EncryptType: EncryptAESGCM}},
		{"chacha20", FrameCodec{EncryptKey: key, EncryptType: EncryptChaCha20}},
	}
}

// fuzzStreamFrame builds one complete wire frame (prefix + body) the way
// sendTo does, optionally sealed like an AEAD session.
func fuzzStreamFrame(tb testing.TB, codec FrameCodec, payload []byte) []byte {
	tb.Helper()
	f := &ProxyFrame{
		Type: FRAME_TYPE_DATA,
		DataFrame: &DataFrame{
			Id:    "seed",
			Data:  append([]byte(nil), payload...),
			Index: 1,
		},
	}
	body, err := MarshalSrpFrame(f, codec)
	if err != nil {
		tb.Fatalf("seed marshal codec=%+v: %v", codec, err)
	}
	if codec.EncryptKey != "" && isAEADEncrypt(codec.EncryptType) {
		pc := &ProxyConn{}
		pc.setCodec(codec)
		body, err = pc.sealWire(body)
		if err != nil {
			tb.Fatalf("seed seal codec=%+v: %v", codec, err)
		}
	}
	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

func fuzzStreamPrefixLen(l uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, l)
	return b
}

// FuzzFrameStream: arbitrary bytes are fed to the real recvFrom loop. This
// covers the cases the payload-level targets cannot see: zero/oversized
// length fields (must be rejected before the body is read), truncated
// bodies, several frames back to back, and sealed frames whose tag/nonce
// bytes are garbage. recvFrom returns an error for any finite stream; the
// assertion that matters is that the length guard always fires first for
// an unreadable header, so a huge prefix can never make it allocate or
// block on a body that will not arrive.
func FuzzFrameStream(f *testing.F) {
	variants := fuzzStreamVariants()
	for _, v := range variants {
		one := fuzzStreamFrame(f, v.codec, []byte("hello"))
		two := append(fuzzStreamFrame(f, v.codec, bytes.Repeat([]byte{0}, 3000)),
			fuzzStreamFrame(f, v.codec, []byte("world"))...)
		f.Add(one)
		f.Add(two)
		f.Add(one[:len(one)-1]) // complete header, body cut short
	}
	f.Add([]byte(nil))
	f.Add([]byte{0x00})
	f.Add(fuzzStreamPrefixLen(0))
	f.Add(fuzzStreamPrefixLen(0xffffffff))
	maxWire := uint32(fuzzStreamMaxMsg) + MAX_PROTO_PACK_SIZE + aeadOverhead
	f.Add(fuzzStreamPrefixLen(maxWire + 1))

	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > fuzzStreamMaxBytes {
			t.Skip() // see fuzzBombNote: keeps decompression bounded
		}
		maxWire := uint32(fuzzStreamMaxMsg) + MAX_PROTO_PACK_SIZE + aeadOverhead
		for _, v := range variants {
			pc := &ProxyConn{}
			pc.setCodec(v.codec)
			// No recvq/recvch: RecvFrame drops the frame instead of blocking
			// on a queue nobody drains.
			wg := thread.NewGroup("fuzz frame stream", nil, func() {})
			err := recvFrom(wg, pc, &readerConn{r: bytes.NewReader(stream), info: "fuzz"}, fuzzStreamMaxMsg)
			if err == nil {
				t.Fatalf("%s: recvFrom accepted a finite stream without error", v.name)
			}
			if len(stream) < 4 {
				continue
			}
			l := binary.LittleEndian.Uint32(stream[:4])
			if l != 0 && l <= maxWire {
				continue
			}
			// Header is unreadable by construction: the guard must reject it
			// up front instead of reading (or waiting for) l body bytes.
			if !strings.HasPrefix(err.Error(), "msg len fail") {
				t.Fatalf("%s: prefix %d bypassed the length guard, got %v", v.name, l, err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// AEAD tamper resistance: every byte of a sealed blob is attacker-reachable
// (it arrives from the peer), so a flip anywhere — including the nonce — must
// be rejected, and nonces must never repeat on one connection.
// ---------------------------------------------------------------------------

// FuzzWireTamper: flips a bit at an arbitrary position of a sealed blob,
// truncates it, extends it, and replays it under a fresh connection. Only
// claims two invariants beyond FuzzWireSealOpen: authentication covers the
// nonce as well as the ciphertext, and the counter never reuses a nonce.
func FuzzWireTamper(f *testing.F) {
	f.Add([]byte(nil), int32(int(EncryptAESGCM)), uint8(0), uint8(1))
	f.Add([]byte("hello"), int32(int(EncryptChaCha20)), uint8(7), uint8(3))
	f.Add(bytes.Repeat([]byte("x"), 4096), int32(int(EncryptAESGCM)), uint8(250), uint8(7))
	f.Add([]byte{0x00, 0xff}, int32(int(EncryptUnspecified)), uint8(13), uint8(0))

	f.Fuzz(func(t *testing.T, payload []byte, etInt int32, pos uint8, bit uint8) {
		if len(payload) > fuzzStreamMaxMsg {
			t.Skip()
		}
		et := ENCRYPT_TYPE(etInt)
		if !isAEADEncrypt(et) {
			t.Skip() // plaintext mode has nothing to authenticate
		}
		var p ProxyConn
		p.setCodec(FrameCodec{EncryptKey: "fuzz-tamper-key", EncryptType: et})

		sealed, err := p.sealWire(payload)
		if err != nil {
			t.Fatalf("sealWire encrypt=%v: %v", et, err)
		}
		if want := len(payload) + aeadOverhead; len(sealed) != want {
			t.Fatalf("sealWire encrypt=%v: blob %d bytes, want %d", et, len(sealed), want)
		}

		// A fresh connection holding the same key must open it: nonce state
		// travels in the blob, not in the conn.
		var fresh ProxyConn
		fresh.setCodec(FrameCodec{EncryptKey: "fuzz-tamper-key", EncryptType: et})
		opened, err := fresh.openWire(sealed)
		if err != nil {
			t.Fatalf("fresh conn rejected my own blob encrypt=%v: %v", et, err)
		}
		if !bytes.Equal(opened, payload) {
			t.Fatalf("fresh conn decrypt mismatch encrypt=%v", et)
		}

		// Nonce reuse check: a second seal on the same conn must differ.
		sealed2, err := p.sealWire(payload)
		if err != nil {
			t.Fatalf("second sealWire encrypt=%v: %v", et, err)
		}
		if bytes.Equal(sealed[:aeadNonceSize], sealed2[:aeadNonceSize]) {
			t.Fatalf("encrypt=%v reused a nonce across two seals", et)
		}

		// Flip one bit anywhere in the blob (nonce, ciphertext, or tag).
		mutated := append([]byte(nil), sealed...)
		idx := int(pos) % len(mutated)
		mutated[idx] ^= byte(1) << (bit % 8)
		if _, err := p.openWire(mutated); err == nil {
			t.Fatalf("encrypt=%v accepted a blob with bit %d of byte %d flipped", et, bit%8, idx)
		}

		// Drop up to 16 trailing bytes: tag-only, ciphertext+tag, and nonce
		// truncations all have to fail.
		cut := 1 + int(pos)%16
		if cut > len(sealed)-aeadNonceSize {
			cut = len(sealed) - aeadNonceSize
		}
		if _, err := p.openWire(sealed[:len(sealed)-cut]); err == nil {
			t.Fatalf("encrypt=%v accepted a blob truncated by %d bytes", et, cut)
		}

		// Trailing garbage must not be accepted either.
		if _, err := p.openWire(append(append([]byte(nil), sealed...), 0x00)); err == nil {
			t.Fatalf("encrypt=%v accepted a blob with a trailing byte", et)
		}

		// Anything shorter than nonce+tag is rejected before touching the
		// cipher, with the dedicated guard message.
		if len(sealed) > aeadOverhead {
			if _, err := p.openWire(sealed[:aeadNonceSize]); err == nil {
				t.Fatalf("encrypt=%v accepted a nonce-only blob", et)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Window updates: the credit the peer reports travels in OPENRSP.Msg
// ("win:<bytes>:<frames>") and drives flow control, so it is wire input.
// ---------------------------------------------------------------------------

// FuzzParseWindowUpdateMsg: arbitrary msg text must parse or be refused, never
// panic, and every canonical message this code itself emits must round-trip
// exactly (that is the contract sendWindowUpdate relies on).
//
// Known finding (reported, not fixed here): negative credits are accepted —
// "win:-100:0" returns ok with bytes=-100. They only ever subtract credit
// (releaseSendWindow clamps at zero), so this is a hardening gap rather than
// a memory-safety bug; the target below deliberately does not assert on it.
func FuzzParseWindowUpdateMsg(f *testing.F) {
	f.Add("win:100:1", int64(100), int32(1))
	f.Add("win:0:0", int64(0), int32(0))
	f.Add("win:", int64(0), int32(1))
	f.Add("win::", int64(0), int32(1))
	f.Add("win:-5:-1", int64(-5), int32(-1))
	f.Add("WIN:1:1", int64(1), int32(1))
	f.Add("win:1:abc", int64(1), int32(1))
	f.Add("win:9223372036854775807:2147483647", int64(9223372036854775807), int32(2147483647))
	f.Add("win:-9223372036854775808:-2147483648", int64(-9223372036854775808), int32(-2147483648))

	f.Fuzz(func(t *testing.T, msg string, credit int64, frames int32) {
		if _, _, ok := parseWindowUpdateMsg(msg); ok && !strings.HasPrefix(msg, winMsgPrefix) {
			t.Fatalf("parseWindowUpdateMsg(%q) succeeded without the %q prefix", msg, winMsgPrefix)
		}

		canonical := winMsgPrefix + strconv.FormatInt(credit, 10) + ":" + strconv.Itoa(int(frames))
		gotB, gotF, ok := parseWindowUpdateMsg(canonical)
		if !ok {
			t.Fatalf("parseWindowUpdateMsg(%q) rejected a canonical message", canonical)
		}
		if gotB != credit || gotF != frames {
			t.Fatalf("parseWindowUpdateMsg(%q) = (%d,%d), want (%d,%d)",
				canonical, gotB, gotF, credit, frames)
		}
	})
}

// ---------------------------------------------------------------------------
// HTTP CONNECT prefix buffering: bytes already read off the socket while
// parsing the request must be handed to the tunnel in order, byte for byte.
// ---------------------------------------------------------------------------

// FuzzBufferedConnRead: arbitrary prefix, arbitrary remaining stream, arbitrary
// read sizes, and a bufio.Reader that may already hold bytes. The concatenation
// of all Read calls must equal prefix+stream exactly — no loss, no duplication,
// no (0,nil) spin — which is the only property the tunnel depends on.
func FuzzBufferedConnRead(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 OK\r\n\r\n"), []byte("body-1"), uint8(3), uint8(4))
	f.Add([]byte(nil), []byte(nil), uint8(1), uint8(0))
	f.Add([]byte(nil), bytes.Repeat([]byte("Z"), 1000), uint8(63), uint8(15))
	f.Add([]byte("x"), []byte("y"), uint8(0), uint8(2))

	f.Fuzz(func(t *testing.T, prefix, stream []byte, chunk uint8, prime uint8) {
		if len(prefix) > 512 || len(stream) > 4096 {
			t.Skip()
		}
		under := &readerConn{r: bytes.NewReader(stream), info: "fuzz"}
		var br *bufio.Reader
		if prime > 0 {
			size := 16 + int(prime%30)
			br = bufio.NewReaderSize(under, size)
			// Peek fills the reader's buffer without consuming: the bytes
			// must still arrive through bufferedConn.Read. The error (EOF for
			// a short stream) is irrelevant, whatever got buffered is what
			// the next Read must return.
			_, _ = br.Peek(int(prime) % (size + 1))
		}
		c := newPrefixedConn(under, append([]byte(nil), prefix...), br)

		want := append(append([]byte(nil), prefix...), stream...)
		got := make([]byte, 0, len(want))
		buf := make([]byte, 1+int(chunk%64))
		for i := 0; ; i++ {
			if i > len(want)+2 {
				t.Fatalf("Read did not terminate after %d calls (%d/%d bytes)", i, len(got), len(want))
			}
			n, err := c.Read(buf)
			if n < 0 || n > len(buf) {
				t.Fatalf("Read returned n=%d for a %d-byte buffer", n, len(buf))
			}
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
			if n == 0 && len(got) < len(want) {
				t.Fatalf("Read returned (0,nil) with %d of %d bytes left", len(want)-len(got), len(want))
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("read %d bytes, want %d (prefix %d + stream %d)",
				len(got), len(want), len(prefix), len(stream))
		}
	})
}

// ---------------------------------------------------------------------------
// Handshake negotiation and authentication.
// ---------------------------------------------------------------------------

// FuzzNegotiateCodec: the client requests codec types off the wire and the
// server config gates them. Whatever the combination, an accepted negotiation
// must yield a codec this build can actually run, and the config gates
// (Compress<=0, Encrypt=="") must win over the request.
func FuzzNegotiateCodec(f *testing.F) {
	f.Add(int32(0), int32(0), int32(0), int32(0), int32(128), "")
	f.Add(int32(int(CompressZstd)), int32(int(EncryptChaCha20)), int32(int(CompressZlib)), int32(int(EncryptAESGCM)), int32(0), "")
	f.Add(int32(int(CompressZlib)), int32(int(EncryptNone)), int32(int(CompressZstd)), int32(int(EncryptAESGCM)), int32(128), "k")
	f.Add(int32(99), int32(0), int32(0), int32(0), int32(128), "")
	f.Add(int32(int(CompressNone)), int32(int(EncryptAESGCM)), int32(int(CompressZstd)), int32(int(EncryptChaCha20)), int32(-1), "k")

	f.Fuzz(func(t *testing.T, reqC, reqE, cfgC, cfgE, compress int32, key string) {
		cfg := &Config{
			Compress:     int(compress),
			CompressType: COMPRESS_TYPE(cfgC),
			EncryptType:  ENCRYPT_TYPE(cfgE),
			Encrypt:      key,
		}
		c, e, err := negotiateCodec(COMPRESS_TYPE(reqC), ENCRYPT_TYPE(reqE), cfg)
		if err != nil {
			return // refusal is always a valid answer
		}
		if !supportedCompressType(c) || !supportedEncryptType(e) {
			t.Fatalf("negotiated an unsupported codec: compress=%d encrypt=%d", c, e)
		}
		if cfg.Compress <= 0 && c != CompressNone {
			t.Fatalf("negotiated compress=%d although config disabled compression", c)
		}
		if cfg.Encrypt == "" && e != EncryptNone {
			t.Fatalf("negotiated encrypt=%d although config has no key", e)
		}
		// A supported, explicit request must be honored when the server
		// allows it.
		if COMPRESS_TYPE(reqC) != CompressUnspecified && supportedCompressType(COMPRESS_TYPE(reqC)) && cfg.Compress > 0 {
			if want := effectiveCompressType(COMPRESS_TYPE(reqC)); c != want {
				t.Fatalf("compress request %d not honored: got %d", reqC, c)
			}
		}
		if ENCRYPT_TYPE(reqE) != EncryptUnspecified && supportedEncryptType(ENCRYPT_TYPE(reqE)) && cfg.Encrypt != "" {
			if want := effectiveEncryptType(ENCRYPT_TYPE(reqE)); e != want {
				t.Fatalf("encrypt request %d not honored: got %d", reqE, e)
			}
		}

		// The codec the conns are actually built with must agree with the
		// same gates.
		dc := defaultFrameCodec(cfg)
		if cfg.Compress <= 0 && effectiveCompressType(dc.CompressType) != CompressNone {
			t.Fatalf("defaultFrameCodec compress=%d although config disabled compression", dc.CompressType)
		}
		if cfg.Encrypt == "" && effectiveEncryptType(dc.EncryptType) != EncryptNone {
			t.Fatalf("defaultFrameCodec encrypt=%d although config has no key", dc.EncryptType)
		}
	})
}

// FuzzVerifyAuthProof: the server checks an HMAC the client computes from
// a challenge and the shared key; both challenge and proof are wire input.
// The proof must only ever be accepted when it is exactly
// HMAC-SHA256(key, challenge) — a mismatch, a truncation, an extra byte,
// or an empty challenge/proof have to fail.
func FuzzVerifyAuthProof(f *testing.F) {
	f.Add("key", []byte("challenge"), []byte("proof"), uint8(0))
	f.Add("", []byte(nil), []byte(nil), uint8(0))
	f.Add("key", []byte{0x01}, bytes.Repeat([]byte{0x02}, 32), uint8(3))

	f.Fuzz(func(t *testing.T, key string, challenge, proof []byte, pos uint8) {
		if len(challenge) > 256 || len(proof) > 256 {
			t.Skip()
		}
		if len(challenge) == 0 || len(proof) == 0 {
			if verifyAuthProof(key, challenge, proof) {
				t.Fatalf("accepted empty challenge=%d proof=%d", len(challenge), len(proof))
			}
			return
		}
		if verifyAuthProof(key, challenge, proof) && !hmacEqualProofs(key, challenge, proof) {
			t.Fatalf("accepted a proof that is not HMAC(key=%q, challenge)", key)
		}

		good := computeAuthProof(key, challenge)
		if !verifyAuthProof(key, challenge, good) {
			t.Fatalf("rejected the correct proof for key=%q challenge=%d bytes", key, len(challenge))
		}

		bad := append([]byte(nil), good...)
		bad[int(pos)%len(bad)] ^= byte(1) << (pos % 8)
		if verifyAuthProof(key, challenge, bad) {
			t.Fatalf("accepted a tampered proof (byte %d)", int(pos)%len(bad))
		}

		if verifyAuthProof(key, challenge, good[:len(good)-1]) {
			t.Fatalf("accepted a truncated proof")
		}
		if verifyAuthProof(key, challenge, append(append([]byte(nil), good...), 0)) {
			t.Fatalf("accepted a proof with a trailing byte")
		}
		if verifyAuthProof(key+"x", challenge, good) {
			t.Fatalf("accepted a proof computed under a different key")
		}
	})
}

// hmacEqualProofs re-derives the expected proof; kept separate so the fuzz
// assertion above reads as "accepted implies exact" instead of duplicating
// the comparison inline.
func hmacEqualProofs(key string, challenge, proof []byte) bool {
	return bytes.Equal(computeAuthProof(key, challenge), proof)
}
