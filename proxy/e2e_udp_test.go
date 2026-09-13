//go:build !race

package proxy

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"
)

func startUDPEchoServer(t *testing.T) (string, func()) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start UDP echo server: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				select {
				case <-stop:
					return
				default:
					return
				}
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String(), func() {
		close(stop)
		pc.Close()
	}
}

func TestE2E_UDP_ForwardProxy(t *testing.T) {
	echoAddr, stopEcho := startUDPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	clientUDPPort := getFreePort(t)
	clientUDPAddr := fmt.Sprintf("127.0.0.1:%d", clientUDPPort)

	cfg := DefaultConfig()
	cfg.Key = "test-udp-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, "tcp", serverAddr, "test_udp_client", "PROXY", []string{"udp"}, []string{clientUDPAddr}, []string{echoAddr})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	time.Sleep(300 * time.Millisecond)

	rConn, err := net.Dial("udp", clientUDPAddr)
	if err != nil {
		t.Fatalf("Failed to dial UDP proxy: %v", err)
	}
	defer rConn.Close()

	msg := []byte("hello spp udp forward proxy")
	deadline := time.Now().Add(4 * time.Second)
	reply := make([]byte, 1024)
	var n int
	for time.Now().Before(deadline) {
		rConn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		_, _ = rConn.Write(msg)
		n, err = rConn.Read(reply)
		if err == nil && n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Failed to read UDP reply within deadline: %v", err)
	}

	if !bytes.Equal(reply[:n], msg) {
		t.Fatalf("UDP reply mismatch: got %s, want %s", string(reply[:n]), string(msg))
	}

	rConn.Close()
	time.Sleep(100 * time.Millisecond)
}
