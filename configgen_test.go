package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/esrrhs/gohome/network"
)

func TestGenerateConfigs(t *testing.T) {
	dir := t.TempDir()
	if err := generateConfigs(dir, false); err != nil {
		t.Fatal(err)
	}

	serverPath := filepath.Join(dir, defaultGenServerFile)
	serverRaw, err := os.ReadFile(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	var server ConfigFile
	if err := json.Unmarshal(serverRaw, &server); err != nil {
		t.Fatal(err)
	}
	wantProtos := network.SupportReliableProtos()
	if len(server.Proto) != len(wantProtos) {
		t.Fatalf("server proto=%v want all %v", server.Proto, wantProtos)
	}
	if len(server.Listen) != len(server.Proto) {
		t.Fatalf("listen len %d != proto len %d", len(server.Listen), len(server.Proto))
	}
	if server.Key == "" || server.Encrypt == nil || *server.Encrypt == "" {
		t.Fatal("server secrets missing")
	}

	for _, mode := range clientModes {
		path := filepath.Join(dir, mode.File)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", mode.File, err)
		}
		var client ConfigFile
		if err := json.Unmarshal(raw, &client); err != nil {
			t.Fatal(err)
		}
		if client.Type != mode.Type {
			t.Fatalf("%s type=%q want %q", mode.File, client.Type, mode.Type)
		}
		if client.Key != server.Key || *client.Encrypt != *server.Encrypt {
			t.Fatalf("%s secrets mismatch server", mode.File)
		}
		if len(client.Proto) != len(wantProtos) {
			t.Fatalf("%s proto=%v", mode.File, client.Proto)
		}
		if len(client.Servers) != len(client.Proto) {
			t.Fatalf("%s servers len %d != proto %d", mode.File, len(client.Servers), len(client.Proto))
		}
	}

	if err := generateConfigs(dir, false); err == nil {
		t.Fatal("expected error when files exist")
	}
	if err := generateConfigs(dir, true); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultListenAndServerAddrs(t *testing.T) {
	if got := defaultListenAddr("tcp", 0); got != ":8888" {
		t.Fatalf("tcp listen=%s", got)
	}
	if got := defaultListenAddr("ricmp", 2); got != "0.0.0.0" {
		t.Fatalf("ricmp listen=%s", got)
	}
	if got := defaultServerAddr("tcp", "127.0.0.1", 0); got != "127.0.0.1:8888" {
		t.Fatalf("tcp server=%s", got)
	}
	if got := defaultServerAddr("ricmp", "127.0.0.1", 0); got != "127.0.0.1" {
		t.Fatalf("ricmp server=%s", got)
	}
	// ricmp is host-only: an IPv6 server stays a bare literal (no port,
	// no brackets) because net.ResolveIPAddr rejects "[::1]".
	if got := defaultServerAddr("ricmp", "::1", 0); got != "::1" {
		t.Fatalf("ricmp ipv6 server=%s", got)
	}
	// IPv6 literals must be bracketed.
	if got := defaultServerAddr("tcp", "::1", 0); got != "[::1]:8888" {
		t.Fatalf("tcp ipv6 server=%s", got)
	}
	if got := netJoinHostPort("::1", "[::]:8888"); got != "[::1]:8888" {
		t.Fatalf("ipv6 join=%s", got)
	}
	// Addrs without a port (ricmp wildcard) are returned unchanged.
	if got := netJoinHostPort("::1", "0.0.0.0"); got != "0.0.0.0" {
		t.Fatalf("no-port join=%s", got)
	}
}

func TestRandomSecretNotWeak(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, err := randomSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != secretBytes*2 {
			t.Fatalf("len=%d", len(s))
		}
		switch s {
		case "123456", "default", "password", "pass", "secret", "admin":
			t.Fatalf("weak secret generated: %q", s)
		}
	}
}

func TestConfigFileKcpFecFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	content := `{
		"type": "server",
		"proto": ["kcp"],
		"key": "strong-auth-key",
		"kcpfecdata": 10,
		"kcpfecparity": 3
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KcpFecData == nil || *cfg.KcpFecData != 10 {
		t.Fatalf("kcpfecdata not parsed: %+v", cfg.KcpFecData)
	}
	if cfg.KcpFecParity == nil || *cfg.KcpFecParity != 3 {
		t.Fatalf("kcpfecparity not parsed: %+v", cfg.KcpFecParity)
	}
}
