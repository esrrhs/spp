package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHttp_AuthCheck(t *testing.T) {
	// No auth required
	if !checkProxyAuth("", "", "") {
		t.Fatalf("expected true when user/pass are empty")
	}
	if !checkProxyAuth("Basic invalid", "", "") {
		t.Fatalf("expected true when user/pass are empty even if header present")
	}

	// Auth required
	user := "admin"
	pass := "secret123"
	if checkProxyAuth("", user, pass) {
		t.Fatalf("expected false when auth header is missing")
	}

	badHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:wrongpass"))
	if checkProxyAuth(badHeader, user, pass) {
		t.Fatalf("expected false for wrong password")
	}

	goodHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret123"))
	if !checkProxyAuth(goodHeader, user, pass) {
		t.Fatalf("expected true for valid credentials")
	}

	// Case-insensitive "basic " prefix
	lowerGoodHeader := "basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret123"))
	if !checkProxyAuth(lowerGoodHeader, user, pass) {
		t.Fatalf("expected true for lowercase basic prefix")
	}
}

func startHTTPTestBackend(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start http backend: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		// Ensure Proxy-Authorization header was stripped
		if r.Header.Get("Proxy-Authorization") != "" {
			http.Error(w, "Proxy-Authorization leaked!", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Custom-Echo", "spp-http-proxy")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from spp http backend"))
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(ln)
	}()

	addr := ln.Addr().String()
	cleanup := func() {
		_ = server.Close()
		_ = ln.Close()
	}
	return addr, cleanup
}

func TestE2E_HTTPProxy_Forward(t *testing.T) {
	backendAddr, stopBackend := startHTTPTestBackend(t)
	defer stopBackend()

	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	httpPort := getFreePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	user := "proxyuser"
	pass := "proxypass"

	cfg := DefaultConfig()
	cfg.Key = "test-http-secret"
	cfg.Username = user
	cfg.Password = pass

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "http_client", "HTTP", []string{"tcp"}, []string{httpAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// 1. Test 407 Authentication Required when no auth provided
	c1, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to http proxy: %v", err)
	}
	defer c1.Close()

	c1.SetDeadline(time.Now().Add(3 * time.Second))
	reqNoAuth := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	if _, err := c1.Write([]byte(reqNoAuth)); err != nil {
		t.Fatalf("Write CONNECT failed: %v", err)
	}

	reader1 := bufio.NewReader(c1)
	respLine, err := reader1.ReadString('\n')
	if err != nil {
		t.Fatalf("Read response failed: %v", err)
	}
	if !strings.Contains(respLine, "407") {
		t.Fatalf("Expected 407 response without auth, got: %s", respLine)
	}
	c1.Close()

	// 2. Test CONNECT method with valid auth
	c2, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to http proxy for CONNECT: %v", err)
	}
	defer c2.Close()

	c2.SetDeadline(time.Now().Add(5 * time.Second))
	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	reqConnect := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", echoAddr, echoAddr, authHeader)
	if _, err := c2.Write([]byte(reqConnect)); err != nil {
		t.Fatalf("Write CONNECT with auth failed: %v", err)
	}

	reader2 := bufio.NewReader(c2)
	connectResp, err := reader2.ReadString('\n')
	if err != nil {
		t.Fatalf("Read CONNECT response failed: %v", err)
	}
	if !strings.Contains(connectResp, "200") {
		t.Fatalf("Expected 200 Connection Established, got: %s", connectResp)
	}
	// Consume empty line of CONNECT response
	_, _ = reader2.ReadString('\n')

	// Echo test over tunnel
	echoData := []byte("hello http connect tunnel")
	if _, err := c2.Write(echoData); err != nil {
		t.Fatalf("Write echoData failed: %v", err)
	}
	recvBuf := make([]byte, len(echoData))
	if _, err := io.ReadFull(reader2, recvBuf); err != nil {
		t.Fatalf("Read echoData failed: %v", err)
	}
	if !bytes.Equal(recvBuf, echoData) {
		t.Fatalf("Echo mismatch: got %s, want %s", string(recvBuf), string(echoData))
	}
	c2.Close()

	// 3. Test plain GET request with auth
	c3, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to http proxy for GET: %v", err)
	}
	defer c3.Close()

	c3.SetDeadline(time.Now().Add(5 * time.Second))
	reqGet := fmt.Sprintf("GET http://%s/hello HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", backendAddr, backendAddr, authHeader)
	if _, err := c3.Write([]byte(reqGet)); err != nil {
		t.Fatalf("Write GET failed: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(c3), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK from backend, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll body failed: %v", err)
	}
	if string(body) != "hello from spp http backend" {
		t.Fatalf("Unexpected body: %s", string(body))
	}
}

func TestE2E_HTTPProxy_Reverse(t *testing.T) {
	backendAddr, stopBackend := startHTTPTestBackend(t)
	defer stopBackend()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	httpPort := getFreePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	user := "revuser"
	pass := "revpass"

	cfg := DefaultConfig()
	cfg.Key = "test-reverse-http-secret"
	cfg.Username = user
	cfg.Password = pass

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	// Reverse HTTP client: connects to server, asks server to listen on httpAddr
	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "rev_http_client", "REVERSE_HTTP", []string{"tcp"}, []string{httpAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// Wait for server to open httpAddr
	c, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to reverse http proxy: %v", err)
	}
	defer c.Close()

	c.SetDeadline(time.Now().Add(5 * time.Second))
	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	reqGet := fmt.Sprintf("GET http://%s/hello HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", backendAddr, backendAddr, authHeader)
	if _, err := c.Write([]byte(reqGet)); err != nil {
		t.Fatalf("Write GET failed: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK from backend, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll body failed: %v", err)
	}
	if string(body) != "hello from spp http backend" {
		t.Fatalf("Unexpected body: %s", string(body))
	}
}

func TestE2E_HTTPProxy_NoAuth(t *testing.T) {
	backendAddr, stopBackend := startHTTPTestBackend(t)
	defer stopBackend()

	echoAddr, stopEcho := startTCPEchoServer(t)
	defer stopEcho()

	serverPort := getFreePort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	httpPort := getFreePort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	cfg := DefaultConfig()
	cfg.Key = "test-noauth-secret"
	// Username and Password left empty

	server, err := NewServer(cfg, []string{"tcp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	client, err := NewClient(cfg, []string{"tcp"}, []string{serverAddr}, "http_client_noauth", "HTTP", []string{"tcp"}, []string{httpAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer client.Close()

	// 1. CONNECT without auth should succeed directly
	c1, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to http proxy: %v", err)
	}
	defer c1.Close()

	c1.SetDeadline(time.Now().Add(3 * time.Second))
	reqConnect := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	if _, err := c1.Write([]byte(reqConnect)); err != nil {
		t.Fatalf("Write CONNECT failed: %v", err)
	}

	reader1 := bufio.NewReader(c1)
	connectResp, err := reader1.ReadString('\n')
	if err != nil {
		t.Fatalf("Read CONNECT response failed: %v", err)
	}
	if !strings.Contains(connectResp, "200") {
		t.Fatalf("Expected 200 Connection Established, got: %s", connectResp)
	}
	_, _ = reader1.ReadString('\n')

	echoData := []byte("noauth echo")
	if _, err := c1.Write(echoData); err != nil {
		t.Fatalf("Write echoData failed: %v", err)
	}
	recvBuf := make([]byte, len(echoData))
	if _, err := io.ReadFull(reader1, recvBuf); err != nil {
		t.Fatalf("Read echoData failed: %v", err)
	}
	if !bytes.Equal(recvBuf, echoData) {
		t.Fatalf("Echo mismatch: got %s, want %s", string(recvBuf), string(echoData))
	}
	c1.Close()

	// 2. GET without auth should succeed directly
	c2, err := waitForPort(httpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to http proxy: %v", err)
	}
	defer c2.Close()

	c2.SetDeadline(time.Now().Add(3 * time.Second))
	reqGet := fmt.Sprintf("GET http://%s/hello HTTP/1.1\r\nHost: %s\r\n\r\n", backendAddr, backendAddr)
	if _, err := c2.Write([]byte(reqGet)); err != nil {
		t.Fatalf("Write GET failed: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK from backend, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll body failed: %v", err)
	}
	if string(body) != "hello from spp http backend" {
		t.Fatalf("Unexpected body: %s", string(body))
	}
}
