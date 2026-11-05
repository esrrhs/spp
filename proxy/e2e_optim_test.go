package proxy

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2E_CompressGate_DynamicTrafficTransition tests that the adaptive compression
// gate in a live end-to-end proxy tunnel correctly navigates through:
// 1. Incompressible random traffic -> triggers gate skip mode (skipping zstd attempts).
// 2. Compressible text traffic -> gate probe detects compressible data and re-enables compression.
// 3. Subsequent incompressible traffic -> gate engages skip mode again.
// All payloads must be echoed back with 100% SHA-256 byte fidelity, and the compression
// metrics must reflect active byte savings.
func TestE2E_CompressGate_DynamicTrafficTransition(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := testConfig("gate-dyn-secret")
	cfg.CompressType = CompressZstd
	cfg.Compress = 128

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, "tcp", serverAddr, "test_client_gate", "PROXY",
		[]string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(clientAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("waitForPort: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Phase 1: Incompressible random frames (20 frames x 8KB = 160KB).
	// With gateFailThreshold = 8, consecutive failures transition the gate into skip mode.
	rnd := newBenchRand(987654321)
	echoBuf := make([]byte, 8192)
	for i := 0; i < 20; i++ {
		randChunk := make([]byte, 8192)
		for j := range randChunk {
			randChunk[j] = byte(rnd.next())
		}
		wantHash := sha256.Sum256(randChunk)

		if _, err := conn.Write(randChunk); err != nil {
			t.Fatalf("phase 1 write %d: %v", i, err)
		}
		if _, err := io.ReadFull(conn, echoBuf); err != nil {
			t.Fatalf("phase 1 read %d: %v", i, err)
		}
		if sha256.Sum256(echoBuf) != wantHash {
			t.Fatalf("phase 1 data corruption at frame %d", i)
		}
	}

	savedBefore := atomic.LoadInt64(&gState.SendCompSaveSize)

	// Phase 2: Highly compressible text frames (32 frames x 8KB = 256KB).
	// Because skip mode probes every 16 frames, within 16 frames the gate probe
	// encounters compressible data, resets failure/skip counts, and re-engages compression.
	textPattern := []byte("SPP-COMPRESS-GATE-DYNAMIC-E2E-TEST-PATTERN-REPEATED-")
	textChunk := bytes.Repeat(textPattern, (8192/len(textPattern))+1)[:8192]
	textHash := sha256.Sum256(textChunk)

	for i := 0; i < 32; i++ {
		if _, err := conn.Write(textChunk); err != nil {
			t.Fatalf("phase 2 write %d: %v", i, err)
		}
		if _, err := io.ReadFull(conn, echoBuf); err != nil {
			t.Fatalf("phase 2 read %d: %v", i, err)
		}
		if sha256.Sum256(echoBuf) != textHash {
			t.Fatalf("phase 2 data corruption at frame %d", i)
		}
	}

	savedAfter := atomic.LoadInt64(&gState.SendCompSaveSize)
	if savedAfter <= savedBefore {
		t.Fatalf("expected compression savings after compressible text frames, before=%d after=%d",
			savedBefore, savedAfter)
	}

	// Phase 3: Incompressible random frames again (20 frames x 8KB = 160KB).
	// The gate must smoothly transition back to skip mode without issue.
	for i := 0; i < 20; i++ {
		randChunk := make([]byte, 8192)
		for j := range randChunk {
			randChunk[j] = byte(rnd.next())
		}
		wantHash := sha256.Sum256(randChunk)

		if _, err := conn.Write(randChunk); err != nil {
			t.Fatalf("phase 3 write %d: %v", i, err)
		}
		if _, err := io.ReadFull(conn, echoBuf); err != nil {
			t.Fatalf("phase 3 read %d: %v", i, err)
		}
		if sha256.Sum256(echoBuf) != wantHash {
			t.Fatalf("phase 3 data corruption at frame %d", i)
		}
	}
}

// TestE2E_AEAD_HardwareDefaultAndConfigured verifies end-to-end data transfer
// when using the hardware AEAD default (DefaultEncryptType()) and configured AEADs.
func TestE2E_AEAD_HardwareDefaultAndConfigured(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	cases := []struct {
		name       string
		clientEnc  ENCRYPT_TYPE
		serverEnc  ENCRYPT_TYPE
		encryptKey string
	}{
		{"BothUnspecified_HardwareAEAD", EncryptUnspecified, EncryptUnspecified, "hw-secret-1"},
		{"BothAESGCM", EncryptAESGCM, EncryptAESGCM, "hw-secret-2"},
		{"BothChaCha20", EncryptChaCha20, EncryptChaCha20, "hw-secret-3"},
		{"ClientHardwareDefault_ServerHardwareExplicit", EncryptUnspecified, DefaultEncryptType(), "hw-secret-4"},
		{"ClientHardwareExplicit_ServerHardwareDefault", DefaultEncryptType(), EncryptUnspecified, "hw-secret-5"},
	}

	payload := make([]byte, 4096)
	fillPattern(payload, 42)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			serverPort := getFreePort(t)
			serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
			clientPort := getFreePort(t)
			clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

			scfg := testConfig("auth-key")
			scfg.Encrypt = tc.encryptKey
			scfg.EncryptType = tc.serverEnc

			ccfg := testConfig("auth-key")
			ccfg.Encrypt = tc.encryptKey
			ccfg.EncryptType = tc.clientEnc

			server, err := NewServer(scfg, []string{"tcp"}, []string{serverAddr})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			defer server.Close()

			client, err := NewClient(ccfg, "tcp", serverAddr, "client_"+tc.name, "PROXY",
				[]string{"tcp"}, []string{clientAddr}, []string{echoAddr})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			defer client.Close()

			conn, err := waitForPort(clientAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("waitForPort: %v", err)
			}
			defer conn.Close()

			echoAndHash(t, conn, payload, 10*time.Second)
		})
	}
}

// TestE2E_SonnyBatchWrite_PendingCloseIntegrity guards the sendToSonny batching
// path: when a client blasts multiple small chunks and immediately closes the socket,
// sendToSonny on the target side must flush all queued data frames in its batch before
// processing the trailing CLOSE frame, ensuring zero trailing bytes are lost.
func TestE2E_SonnyBatchWrite_PendingCloseIntegrity(t *testing.T) {
	type connResult struct {
		data []byte
		err  error
	}
	results := make(chan connResult, 50)

	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sink listen: %v", err)
	}
	defer sinkLn.Close()
	sinkAddr := sinkLn.Addr().String()

	stopSink := make(chan struct{})
	go func() {
		for {
			conn, err := sinkLn.Accept()
			if err != nil {
				select {
				case <-stopSink:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read handshake ping "SYNC" and respond with "ACK!"
				syncBuf := make([]byte, 4)
				if _, err := io.ReadFull(c, syncBuf); err != nil {
					results <- connResult{err: err}
					return
				}
				if string(syncBuf) != "SYNC" {
					results <- connResult{err: fmt.Errorf("unexpected sync: %q", string(syncBuf))}
					return
				}
				if _, err := c.Write([]byte("ACK!")); err != nil {
					results <- connResult{err: err}
					return
				}
				// Now read all data until EOF
				buf, err := io.ReadAll(c)
				results <- connResult{data: buf, err: err}
			}(conn)
		}
	}()
	defer close(stopSink)

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)
	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := testConfig("pending-close-secret")
	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, "tcp", serverAddr, "test_client_pending_close", "PROXY",
		[]string{"tcp"}, []string{clientAddr}, []string{sinkAddr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	// Wait briefly for client listener to bind
	time.Sleep(50 * time.Millisecond)

	const iterations = 10
	const chunksPerConn = 12
	const chunkSize = 512
	expectedTotal := chunksPerConn * chunkSize

	for i := 0; i < iterations; i++ {
		conn, err := net.DialTimeout("tcp", clientAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("iter %d dial: %v", i, err)
		}

		// 1. Establish connection through the proxy
		if _, err := conn.Write([]byte("SYNC")); err != nil {
			conn.Close()
			t.Fatalf("iter %d write SYNC: %v", i, err)
		}
		ackBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, ackBuf); err != nil || string(ackBuf) != "ACK!" {
			conn.Close()
			t.Fatalf("iter %d read ACK!: %v (%s)", i, err, string(ackBuf))
		}

		// 2. Blast all chunks rapidly and close immediately
		allData := make([]byte, 0, expectedTotal)
		for j := 0; j < chunksPerConn; j++ {
			chunk := make([]byte, chunkSize)
			for k := range chunk {
				chunk[k] = byte((i*chunksPerConn + j + k) & 0xff)
			}
			allData = append(allData, chunk...)
			if _, err := conn.Write(chunk); err != nil {
				conn.Close()
				t.Fatalf("iter %d write chunk %d: %v", i, j, err)
			}
		}

		// Close client connection immediately. sendToSonny on the target side receives all
		// DATA frames followed by CLOSE, and must flush all DATA frames through its batch write
		// path before closing the target connection.
		conn.Close()

		select {
		case res := <-results:
			if res.err != nil {
				t.Fatalf("iter %d sink read err: %v", i, res.err)
			}
			if len(res.data) != expectedTotal {
				t.Fatalf("iter %d got %d bytes, want %d (trailing bytes dropped!)",
					i, len(res.data), expectedTotal)
			}
			if !bytes.Equal(res.data, allData) {
				t.Fatalf("iter %d data mismatch in received batch", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d timeout waiting for sink result", i)
		}
	}
}
