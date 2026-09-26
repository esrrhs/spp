package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"testing"
	"time"
)

func getFreePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTCPEchoServer(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo server: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 8192)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					_, err = c.Write(buf[:n])
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	addr := ln.Addr().String()
	cleanup := func() {
		close(stop)
		ln.Close()
	}
	return addr, cleanup
}

func waitForPort(addr string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout waiting for %s", addr)
}

func TestE2E_TCP_ForwardProxy(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-e2e-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial client proxy port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello spp tcp forward proxy")
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Failed to write to proxy: %v", err)
	}

	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("Failed to read from proxy: %v", err)
	}

	if !bytes.Equal(reply, msg) {
		t.Fatalf("Payload mismatch: got %s, want %s", string(reply), string(msg))
	}
}

func TestE2E_TCP_MultiPath_MainChannels(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	port1 := getFreePort(t)
	port2 := getFreePort(t)
	addr1 := fmt.Sprintf("127.0.0.1:%d", port1)
	addr2 := fmt.Sprintf("127.0.0.1:%d", port2)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-e2e-multipath"
	cfg.Encrypt = "test-e2e-multipath-enc"
	cfg.ProbeInter = 1
	cfg.ProbeSize = 1024

	server, err := NewServer(cfg, []string{"tcp", "tcp"}, []string{addr1, addr2})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp", "tcp"}, []string{addr1, addr2}, "mp", "PROXY",
		[]string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Wait until session has both pipes attached.
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.connMu.Lock()
		sess := client.serverconn
		n := 0
		if sess != nil && sess.hub != nil {
			n = sess.hub.liveCount()
		}
		client.connMu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for 2 pipes, have %d", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	conn, err := waitForPort(clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial client proxy port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello multipath spp")
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(reply, msg) {
		t.Fatalf("mismatch: got %s want %s", reply, msg)
	}
}

func TestE2E_TCP_MultiFromaddr_OneMainChannel(t *testing.T) {
	echo1, stop1 := startTCPEchoServer(t)
	defer stop1()
	echo2, stop2 := startTCPEchoServer(t)
	defer stop2()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort1 := getFreePort(t)
	clientPort2 := getFreePort(t)
	clientAddr1 := fmt.Sprintf("127.0.0.1:%d", clientPort1)
	clientAddr2 := fmt.Sprintf("127.0.0.1:%d", clientPort2)

	cfg := DefaultConfig()
	cfg.Key = "test-e2e-multi-svc"
	cfg.Encrypt = "test-e2e-multi-enc"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "multi", "PROXY",
		[]string{"tcp", "tcp"},
		[]string{clientAddr1, clientAddr2},
		[]string{echo1, echo2})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	for i, addr := range []string{clientAddr1, clientAddr2} {
		conn, err := waitForPort(addr, 3*time.Second)
		if err != nil {
			t.Fatalf("Failed to dial client proxy port[%d] %s: %v", i, addr, err)
		}
		msg := []byte(fmt.Sprintf("multi-svc-%d", i))
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write(msg); err != nil {
			conn.Close()
			t.Fatalf("write[%d]: %v", i, err)
		}
		reply := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, reply); err != nil {
			conn.Close()
			t.Fatalf("read[%d]: %v", i, err)
		}
		conn.Close()
		if !bytes.Equal(reply, msg) {
			t.Fatalf("mismatch[%d]: got %s want %s", i, reply, msg)
		}
	}
}

func TestE2E_TCP_ForwardProxy_LargeData(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-e2e-large"
	cfg.Compress = 64

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "client_large", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial client proxy port: %v", err)
	}
	defer conn.Close()

	// 64KB repetitive data to test compression and multi-frame fragmentation
	largeMsg := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEF"), 1500)
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		_, _ = conn.Write(largeMsg)
	}()

	reply := make([]byte, len(largeMsg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("Failed to read large payload: %v", err)
	}

	if !bytes.Equal(reply, largeMsg) {
		t.Fatalf("Large payload mismatch: lengths %d vs %d", len(reply), len(largeMsg))
	}
}

func TestE2E_TCP_ReverseProxy(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	remoteListenPort := getFreePort(t)
	remoteListenAddr := fmt.Sprintf("127.0.0.1:%d", remoteListenPort)

	cfg := DefaultConfig()
	cfg.Key = "test-reverse-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	// In reverse proxy: fromaddr is exposed on server, toaddr is the local service connected by client
	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "reverse_client", "REVERSE_PROXY", []string{"tcp"}, []string{remoteListenAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Wait for server to expose remoteListenAddr
	conn, err := waitForPort(remoteListenAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to reverse proxy port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello spp reverse proxy!")
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Failed to write to reverse proxy: %v", err)
	}

	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("Failed to read from reverse proxy: %v", err)
	}

	if !bytes.Equal(reply, msg) {
		t.Fatalf("Reverse payload mismatch: got %s, want %s", string(reply), string(msg))
	}
}

func TestE2E_SOCKS5_ForwardProxy(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	socksPort := getFreePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

	cfg := DefaultConfig()
	cfg.Key = "test-socks-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "socks_client", "SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(socksAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to socks5 port: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// SOCKS5 Handshake: 0x05 (version 5), 0x01 (1 auth method), 0x00 (NO AUTH)
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("SOCKS5 greeting failed: %v", err)
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("SOCKS5 greeting response failed: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("SOCKS5 server rejected auth: %v", resp)
	}

	// SOCKS5 Connect request to echoAddr
	tcpAddr, err := net.ResolveTCPAddr("tcp", echoAddr)
	if err != nil {
		t.Fatalf("Resolve echo addr failed: %v", err)
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		ip4 = net.IPv4(127, 0, 0, 1)
	}

	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ip4...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(tcpAddr.Port))
	req = append(req, portBuf...)

	if _, err := conn.Write(req); err != nil {
		t.Fatalf("SOCKS5 connect request failed: %v", err)
	}

	// Read SOCKS5 connect response (10 bytes for IPv4)
	connectResp := make([]byte, 10)
	if _, err := io.ReadFull(conn, connectResp); err != nil {
		t.Fatalf("SOCKS5 connect response failed: %v", err)
	}
	if connectResp[1] != 0x00 {
		t.Fatalf("SOCKS5 connection failed with reply code: %d", connectResp[1])
	}

	// Send data over established socks5 tunnel
	testData := []byte("hello socks5 tunnel data")
	if _, err := conn.Write(testData); err != nil {
		t.Fatalf("Failed to write tunnel data: %v", err)
	}

	recvData := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, recvData); err != nil {
		t.Fatalf("Failed to read tunnel data: %v", err)
	}

	if !bytes.Equal(recvData, testData) {
		t.Fatalf("SOCKS5 tunnel echo mismatch: got %s, want %s", string(recvData), string(testData))
	}
}

func TestE2E_AuthFailure(t *testing.T) {
	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	serverCfg := DefaultConfig()
	serverCfg.Key = "correct_password"

	server, err := NewServer(serverCfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	clientCfg := DefaultConfig()
	clientCfg.Key = "wrong_password"

	client, err := NewClient(clientCfg, []string{"tcp"}, []string{serverAddr}, "bad_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{"127.0.0.1:9999"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Wait briefly and verify client proxy port never becomes open because login was rejected
	time.Sleep(500 * time.Millisecond)
	conn, err := net.DialTimeout("tcp", clientAddr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("Client port %s should not accept connections when auth fails", clientAddr)
	}
}

func TestE2E_ConcurrentDownloadAndWebBrowse(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-concurrent-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "concurrent_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Wait for tunnel to be established
	tunnelConn, err := waitForPort(clientAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to client: %v", err)
	}
	tunnelConn.Close()

	// Start a simulated heavy bulk download connection in background
	stopDownload := make(chan struct{})
	downloadDone := make(chan struct{})

	go func() {
		defer close(downloadDone)
		conn, err := net.Dial("tcp", clientAddr)
		if err != nil {
			return
		}
		defer conn.Close()

		chunk := make([]byte, 32*1024)
		for i := range chunk {
			chunk[i] = byte(i % 256)
		}

		// Read loop
		go func() {
			sink := make([]byte, 32*1024)
			for {
				_, err := conn.Read(sink)
				if err != nil {
					return
				}
			}
		}()

		// Write loop simulating heavy download
		for {
			select {
			case <-stopDownload:
				return
			default:
				_, err := conn.Write(chunk)
				if err != nil {
					return
				}
			}
		}
	}()

	// Let the bulk download run and saturate the connection for 300ms
	time.Sleep(300 * time.Millisecond)

	// Concurrently simulate opening multiple quick web pages while heavy download is happening
	for i := 0; i < 5; i++ {
		start := time.Now()
		webConn, err := net.DialTimeout("tcp", clientAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("Web request %d connection failed: %v", i, err)
		}

		reqMsg := fmt.Sprintf("GET /page%d HTTP/1.1\r\nHost: example.com\r\n\r\n", i)
		if _, err := webConn.Write([]byte(reqMsg)); err != nil {
			webConn.Close()
			t.Fatalf("Web request %d write failed: %v", i, err)
		}

		resp := make([]byte, len(reqMsg))
		if _, err := io.ReadFull(webConn, resp); err != nil {
			webConn.Close()
			t.Fatalf("Web request %d read response failed: %v", i, err)
		}

		if string(resp) != reqMsg {
			webConn.Close()
			t.Fatalf("Web request %d response mismatch", i)
		}
		webConn.Close()

		elapsed := time.Since(start)
		t.Logf("Web request %d took %v", i, elapsed)
		// Latency under bulk is best-effort on a single TCP underlay; integrity
		// above is the hard gate. Keep a generous bound only to catch wedged pipes.
		if elapsed > 15*time.Second {
			t.Errorf("Web request %d took too long (%v), pipe may be wedged", i, elapsed)
		}
	}

	// Stop background download
	close(stopDownload)
	<-downloadDone
	time.Sleep(100 * time.Millisecond)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
