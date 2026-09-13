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

	client, err := NewClient(cfg, "tcp", serverAddr, "test_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
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

	client, err := NewClient(cfg, "tcp", serverAddr, "client_large", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
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
	client, err := NewClient(cfg, "tcp", serverAddr, "reverse_client", "REVERSE_PROXY", []string{"tcp"}, []string{remoteListenAddr}, []string{echoAddr})
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

	client, err := NewClient(cfg, "tcp", serverAddr, "socks_client", "SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
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

	client, err := NewClient(clientCfg, "tcp", serverAddr, "bad_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{"127.0.0.1:9999"})
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

func init() {
	rand.Seed(time.Now().UnixNano())
}
