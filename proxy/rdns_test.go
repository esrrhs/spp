package proxy

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestE2E_RDNS_ForwardProxy(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreeUDPPort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-rdns-auth-key"
	cfg.RdnsDomain = "tunnel.rdns.test"
	cfg.RdnsQueryType = "TXT"

	server, err := NewServer(cfg, []string{"rdns"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, "rdns", serverAddr, "test_rdns_client", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(clientAddr, 8*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial client proxy port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello-rdns-proxy-traffic")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Failed to write to proxy conn: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Failed to read from proxy conn: %v", err)
	}

	if !bytes.Equal(buf, msg) {
		t.Fatalf("Echo mismatch: got %q, want %q", buf, msg)
	}
}

func TestE2E_RDNS_PSK_And_NullQuery(t *testing.T) {
	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreeUDPPort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientPort := getFreePort(t)
	clientAddr := fmt.Sprintf("127.0.0.1:%d", clientPort)

	cfg := DefaultConfig()
	cfg.Key = "test-rdns-auth-key"
	cfg.RdnsDomain = "null.rdns.test"
	cfg.RdnsPsk = "secret-rdns-psk"
	cfg.RdnsQueryType = "NULL"

	server, err := NewServer(cfg, []string{"rdns"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, "rdns", serverAddr, "test_rdns_client_psk", "PROXY", []string{"tcp"}, []string{clientAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	conn, err := waitForPort(clientAddr, 8*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial client proxy port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello-rdns-psk-null-query")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Failed to write to proxy conn: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Failed to read from proxy conn: %v", err)
	}

	if !bytes.Equal(buf, msg) {
		t.Fatalf("Echo mismatch: got %q, want %q", buf, msg)
	}
}
