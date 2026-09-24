package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
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

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_udp_client", "PROXY", []string{"udp"}, []string{clientUDPAddr}, []string{echoAddr})
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

func TestE2E_SOCKS5_UDPAssociate(t *testing.T) {
	echoAddr, stopEcho := startUDPEchoServer(t)
	defer stopEcho()

	echoHost, echoPortStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort failed: %v", err)
	}
	echoPort, _ := strconv.Atoi(echoPortStr)

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	socksPort := getFreePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

	cfg := DefaultConfig()
	cfg.Key = "test-socks5-udp-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_socks5_udp", "SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probeConn, err := waitForPort(socksAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to wait for socks5 port %s: %v", socksAddr, err)
	}
	probeConn.Close()

	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("Dial socks5 failed: %v", err)
	}
	defer c.Close()
	tcpConn := c.(*net.TCPConn)

	if err := network.Sock5Handshake(tcpConn, 3000, "", ""); err != nil {
		t.Fatalf("SOCKS5 handshake failed: %v", err)
	}

	relayAddr, err := network.Sock5SetUDPRequest(tcpConn, "0.0.0.0", 0, 3000)
	if err != nil {
		t.Fatalf("Sock5SetUDPRequest failed: %v", err)
	}
	if relayAddr == "" {
		t.Fatalf("empty relay address returned")
	}

	udpConn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("Dial UDP relay failed: %v", err)
	}
	defer udpConn.Close()

	testMsg := []byte("hello socks5 udp associate proxy")
	pkt, err := network.Sock5PackUDP(echoHost, echoPort, testMsg)
	if err != nil {
		t.Fatalf("Sock5PackUDP failed: %v", err)
	}

	replyBuf := make([]byte, 2048)
	deadline := time.Now().Add(4 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		udpConn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := udpConn.Write(pkt); err != nil {
			t.Fatalf("udpConn Write failed: %v", err)
		}
		n, err = udpConn.Read(replyBuf)
		if err == nil && n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Failed to read UDP reply within deadline: %v", err)
	}

	host, port, data, err := network.Sock5UnpackUDP(replyBuf[:n])
	if err != nil {
		t.Fatalf("Sock5UnpackUDP failed: %v", err)
	}
	if host != echoHost || port != echoPort {
		t.Fatalf("UDP reply source mismatch: got %s:%d, want %s:%d", host, port, echoHost, echoPort)
	}
	if !bytes.Equal(data, testMsg) {
		t.Fatalf("UDP reply payload mismatch: got %s, want %s", string(data), string(testMsg))
	}
}

func TestE2E_SOCKS5_SimultaneousTCPAndUDP(t *testing.T) {
	tcpEchoAddr, stopTCPEcho := startTCPEchoServer(t)
	defer stopTCPEcho()

	udpEchoAddr, stopUDPEcho := startUDPEchoServer(t)
	defer stopUDPEcho()

	udpEchoHost, udpEchoPortStr, err := net.SplitHostPort(udpEchoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort udp failed: %v", err)
	}
	udpEchoPort, _ := strconv.Atoi(udpEchoPortStr)

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	socksPort := getFreePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

	cfg := DefaultConfig()
	cfg.Key = "test-socks5-dual-secret"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_socks5_dual", "SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probeConn, err := waitForPort(socksAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to wait for socks5 port %s: %v", socksAddr, err)
	}
	probeConn.Close()

	// 1. TCP CONNECT proxy client
	tcpDone := make(chan error, 1)
	go func() {
		c, err := net.Dial("tcp", socksAddr)
		if err != nil {
			tcpDone <- fmt.Errorf("dial socks5 tcp failed: %w", err)
			return
		}
		defer c.Close()
		tcpConn := c.(*net.TCPConn)

		if err := network.Sock5Handshake(tcpConn, 3000, "", ""); err != nil {
			tcpDone <- fmt.Errorf("handshake failed: %w", err)
			return
		}
		tcpHost, tcpPortStr, _ := net.SplitHostPort(tcpEchoAddr)
		tcpPort, _ := strconv.Atoi(tcpPortStr)
		if err := network.Sock5SetRequest(tcpConn, tcpHost, tcpPort, 3000); err != nil {
			tcpDone <- fmt.Errorf("socks5 connect failed: %w", err)
			return
		}

		msg := []byte("hello simultaneous tcp")
		if _, err := tcpConn.Write(msg); err != nil {
			tcpDone <- fmt.Errorf("write tcp data failed: %w", err)
			return
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(tcpConn, buf); err != nil {
			tcpDone <- fmt.Errorf("read tcp data failed: %w", err)
			return
		}
		if !bytes.Equal(buf, msg) {
			tcpDone <- fmt.Errorf("tcp echo mismatch: got %s, want %s", string(buf), string(msg))
			return
		}
		tcpDone <- nil
	}()

	// 2. UDP ASSOCIATE proxy client
	udpDone := make(chan error, 1)
	go func() {
		c, err := net.Dial("tcp", socksAddr)
		if err != nil {
			udpDone <- fmt.Errorf("dial socks5 udp ctrl failed: %w", err)
			return
		}
		defer c.Close()
		tcpConn := c.(*net.TCPConn)

		if err := network.Sock5Handshake(tcpConn, 3000, "", ""); err != nil {
			udpDone <- fmt.Errorf("udp handshake failed: %w", err)
			return
		}
		relayAddr, err := network.Sock5SetUDPRequest(tcpConn, "0.0.0.0", 0, 3000)
		if err != nil {
			udpDone <- fmt.Errorf("udp associate failed: %w", err)
			return
		}

		uConn, err := net.Dial("udp", relayAddr)
		if err != nil {
			udpDone <- fmt.Errorf("dial udp relay failed: %w", err)
			return
		}
		defer uConn.Close()

		msg := []byte("hello simultaneous udp")
		pkt, err := network.Sock5PackUDP(udpEchoHost, udpEchoPort, msg)
		if err != nil {
			udpDone <- fmt.Errorf("pack udp failed: %w", err)
			return
		}

		replyBuf := make([]byte, 2048)
		deadline := time.Now().Add(4 * time.Second)
		var n int
		for time.Now().Before(deadline) {
			uConn.SetDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := uConn.Write(pkt); err != nil {
				udpDone <- fmt.Errorf("write udp failed: %w", err)
				return
			}
			n, err = uConn.Read(replyBuf)
			if err == nil && n > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			udpDone <- fmt.Errorf("read udp reply failed: %w", err)
			return
		}

		host, port, data, err := network.Sock5UnpackUDP(replyBuf[:n])
		if err != nil {
			udpDone <- fmt.Errorf("unpack udp reply failed: %w", err)
			return
		}
		if host != udpEchoHost || port != udpEchoPort {
			udpDone <- fmt.Errorf("udp reply src mismatch: got %s:%d, want %s:%d", host, port, udpEchoHost, udpEchoPort)
			return
		}
		if !bytes.Equal(data, msg) {
			udpDone <- fmt.Errorf("udp reply data mismatch: got %s, want %s", string(data), string(msg))
			return
		}
		udpDone <- nil
	}()

	select {
	case err := <-tcpDone:
		if err != nil {
			t.Fatalf("TCP test failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("TCP test timed out")
	}

	select {
	case err := <-udpDone:
		if err != nil {
			t.Fatalf("UDP test failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("UDP test timed out")
	}
}

func TestE2E_SOCKS5_UDPAssociateAuth(t *testing.T) {
	echoAddr, stopEcho := startUDPEchoServer(t)
	defer stopEcho()

	echoHost, echoPortStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort failed: %v", err)
	}
	echoPort, _ := strconv.Atoi(echoPortStr)

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	socksPort := getFreePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

	cfg := DefaultConfig()
	cfg.Key = "test-socks5-auth-secret"
	cfg.Username = "myuser"
	cfg.Password = "mypassword"

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "test_socks5_auth", "SOCKS5", []string{"tcp"}, []string{socksAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	probeConn, err := waitForPort(socksAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to wait for socks5 port %s: %v", socksAddr, err)
	}
	probeConn.Close()

	// 1. Test wrong credentials fails handshake
	cBad, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("Dial socks5 failed: %v", err)
	}
	defer cBad.Close()
	tcpConnBad := cBad.(*net.TCPConn)
	if err := network.Sock5Handshake(tcpConnBad, 3000, "myuser", "wrongpass"); err == nil {
		t.Fatalf("Expected SOCKS5 handshake with wrong password to fail")
	}

	// 2. Test correct credentials succeeds handshake and UDP association
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatalf("Dial socks5 failed: %v", err)
	}
	defer c.Close()
	tcpConn := c.(*net.TCPConn)

	if err := network.Sock5Handshake(tcpConn, 3000, "myuser", "mypassword"); err != nil {
		t.Fatalf("SOCKS5 handshake with correct credentials failed: %v", err)
	}

	relayAddr, err := network.Sock5SetUDPRequest(tcpConn, "0.0.0.0", 0, 3000)
	if err != nil {
		t.Fatalf("Sock5SetUDPRequest failed: %v", err)
	}

	udpConn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("Dial UDP relay failed: %v", err)
	}
	defer udpConn.Close()

	testMsg := []byte("hello authenticated socks5 udp")
	pkt, err := network.Sock5PackUDP(echoHost, echoPort, testMsg)
	if err != nil {
		t.Fatalf("Sock5PackUDP failed: %v", err)
	}

	replyBuf := make([]byte, 2048)
	deadline := time.Now().Add(4 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		udpConn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := udpConn.Write(pkt); err != nil {
			t.Fatalf("udpConn Write failed: %v", err)
		}
		n, err = udpConn.Read(replyBuf)
		if err == nil && n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Failed to read UDP reply within deadline: %v", err)
	}

	host, port, data, err := network.Sock5UnpackUDP(replyBuf[:n])
	if err != nil {
		t.Fatalf("Sock5UnpackUDP failed: %v", err)
	}
	if host != echoHost || port != echoPort {
		t.Fatalf("UDP reply source mismatch: got %s:%d, want %s:%d", host, port, echoHost, echoPort)
	}
	if !bytes.Equal(data, testMsg) {
		t.Fatalf("UDP reply payload mismatch: got %s, want %s", string(data), string(testMsg))
	}
}
