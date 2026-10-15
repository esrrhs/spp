package proxy

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"unicode/utf8"
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
