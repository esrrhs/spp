package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepeatableFlags_StringAndSet(t *testing.T) {
	// Empty flags render as "" (serverAddrs/listenAddrs/toFlags) and
	// proxyproto/proto flags default their String to "tcp".
	if (&fromFlags{}).String() != "" {
		t.Fatal("empty fromFlags String should be empty")
	}
	if (&toFlags{}).String() != "" {
		t.Fatal("empty toFlags String should be empty")
	}
	if (&listenAddrs{}).String() != "" {
		t.Fatal("empty listenAddrs String should be empty")
	}
	if (&serverAddrs{}).String() != "" {
		t.Fatal("empty serverAddrs String should be empty")
	}
	if (&protoFlags{}).String() != "tcp" {
		t.Fatal("empty protoFlags String should default to tcp")
	}
	if (&proxyprotoFlags{}).String() != "tcp" {
		t.Fatal("empty proxyprotoFlags String should default to tcp")
	}

	// Set appends in order and String joins with commas.
	var f fromFlags
	if err := f.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("b"); err != nil {
		t.Fatal(err)
	}
	if got := f.String(); got != "a,b" {
		t.Fatalf("fromFlags String=%q want a,b", got)
	}
	if len(f) != 2 || f[0] != "a" || f[1] != "b" {
		t.Fatalf("fromFlags append order wrong: %v", []string(f))
	}

	var p protoFlags
	_ = p.Set("tcp")
	_ = p.Set("rudp")
	if got := p.String(); got != "tcp,rudp" {
		t.Fatalf("protoFlags String=%q", got)
	}

	var s serverAddrs
	_ = s.Set("1.1.1.1:1")
	_ = s.Set("2.2.2.2:2")
	if got := s.String(); got != "1.1.1.1:1,2.2.2.2:2" {
		t.Fatalf("serverAddrs String=%q", got)
	}

	var l listenAddrs
	_ = l.Set(":1")
	if got := l.String(); got != ":1" {
		t.Fatalf("listenAddrs String=%q", got)
	}

	var pp proxyprotoFlags
	_ = pp.Set("udp")
	if got := pp.String(); got != "udp" {
		t.Fatalf("proxyprotoFlags String=%q", got)
	}

	var tt toFlags
	_ = tt.Set("x")
	if got := tt.String(); got != "x" {
		t.Fatalf("toFlags String=%q", got)
	}
}

func TestConfigFile_StatusAddrRoundTrip(t *testing.T) {
	cfg := ConfigFile{Type: "server", StatusAddr: "127.0.0.1:6060"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"statusaddr":"127.0.0.1:6060"`) {
		t.Fatalf("statusaddr missing from json: %s", raw)
	}
	var back ConfigFile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.StatusAddr != "127.0.0.1:6060" {
		t.Fatalf("statusaddr roundtrip=%q", back.StatusAddr)
	}
}

func TestLoadConfigFile_Errors(t *testing.T) {
	if _, err := loadConfigFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing config file must error")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfigFile(bad)
	if err == nil || !strings.Contains(err.Error(), "invalid json config file") {
		t.Fatalf("want invalid json error, got: %v", err)
	}
}
