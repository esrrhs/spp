package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/spp/version"
)

// In-process coverage for runMain: flag shortcuts, config merging, validation
// rejects, server/client startup with the status endpoint, and graceful
// shutdown via the injectable signal channel.

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stdout = old
	}()
	fn()
	w.Close()
	return <-done
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stderr = old
	}()
	fn()
	w.Close()
	return <-done
}

// withFakeSignal swaps sigNotify for a channel the test controls and restores
// it afterwards; the returned func sends the fake shutdown signal.
func withFakeSignal(t *testing.T) func() {
	t.Helper()
	ch := make(chan os.Signal, 1)
	old := sigNotify
	sigNotify = func() <-chan os.Signal { return ch }
	t.Cleanup(func() { sigNotify = old })
	return func() { ch <- os.Interrupt }
}

// waitHealthy polls the status server until /healthz answers 200.
func waitHealthy(t *testing.T, statusPort int) {
	t.Helper()
	url := "http://127.0.0.1:" + itoaPort(statusPort) + "/healthz"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("spp never became healthy on %s", url)
}

// runMainAsync runs runMain in a goroutine and returns the exit-code channel.
func runMainAsync(args []string) <-chan int {
	done := make(chan int, 1)
	go func() { done <- runMain(args) }()
	return done
}

func TestRunMain_VersionFlags(t *testing.T) {
	out := captureStdout(t, func() {
		if code := runMain([]string{"-version"}); code != 0 {
			t.Fatalf("version exit=%d", code)
		}
	})
	if !strings.Contains(out, version.Full()) {
		t.Fatalf("version output %q missing full version", out)
	}
	out = captureStdout(t, func() {
		if code := runMain([]string{"-v"}); code != 0 {
			t.Fatalf("v exit=%d", code)
		}
	})
	if strings.TrimSpace(out) == "" {
		t.Fatal("-v printed nothing")
	}
}

func TestRunMain_Genconfig(t *testing.T) {
	dir := t.TempDir()
	if code := runMain([]string{"-genconfig", "-outdir", dir}); code != 0 {
		t.Fatalf("genconfig exit=%d", code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("genconfig produced no files: %v", err)
	}
}

func TestRunMain_GenconfigFail(t *testing.T) {
	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut := captureStderr(t, func() {
		if code := runMain([]string{"-genconfig", "-outdir", file}); code != 1 {
			t.Fatalf("genconfig failure exit=%d, want 1", code)
		}
	})
	if !strings.Contains(errOut, "genconfig failed") {
		t.Fatalf("stderr %q missing genconfig failure", errOut)
	}
}

func TestRunMain_BadFlag(t *testing.T) {
	errOut := captureStderr(t, func() {
		if code := runMain([]string{"-definitely-not-a-flag"}); code != 2 {
			t.Fatalf("bad flag exit=%d, want 2", code)
		}
	})
	if !strings.Contains(errOut, "Usage of") {
		t.Fatalf("bad flag output %q missing usage", errOut)
	}
}

func TestRunMain_ValidateRejectPrintsUsage(t *testing.T) {
	var out, errOut string
	out = captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			if code := runMain([]string{"-type", "bogus", "-key", "runmain-key"}); code != 0 {
				t.Fatalf("validate reject exit=%d", code)
			}
		})
	})
	if !strings.Contains(out, "must be server") {
		t.Fatalf("validate reject stdout %q missing error", out)
	}
	if !strings.Contains(errOut, "Usage of") {
		t.Fatalf("validate reject stderr %q missing usage", errOut)
	}
}

func TestRunMain_ConfigFileErrors(t *testing.T) {
	out := captureStdout(t, func() {
		if code := runMain([]string{"-config", filepath.Join(t.TempDir(), "missing.json")}); code != 0 {
			t.Fatalf("missing config exit=%d", code)
		}
	})
	if !strings.Contains(out, "Error loading config file") {
		t.Fatalf("output %q missing config error", out)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		if code := runMain([]string{"-config", bad}); code != 0 {
			t.Fatalf("bad json config exit=%d", code)
		}
	})
	if !strings.Contains(out, "invalid json config file") {
		t.Fatalf("output %q missing invalid-json error", out)
	}
}

func TestRunMain_InvalidCodecTypes(t *testing.T) {
	listen := freePort(t)
	for _, tc := range []struct{ flag, val string }{
		{"-compresstype", "bogus"},
		{"-encrypttype", "bogus"},
	} {
		code := runMain([]string{
			"-nolog", "1", "-noprint", "1",
			"-type", "server",
			"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
			"-key", "runmain-key", tc.flag, tc.val,
		})
		if code != 0 {
			t.Fatalf("%s %s exit=%d", tc.flag, tc.val, code)
		}
	}
}

func TestRunMain_WeakKeyRejected(t *testing.T) {
	listen := freePort(t)
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
		"-key", "123456",
	}); code != 0 {
		t.Fatalf("weak key exit=%d", code)
	}
	// Client path hits the same ValidateConfig inside NewClient.
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "proxy_client",
		"-proto", "tcp", "-server", "127.0.0.1:1",
		"-fromaddr", ":18080", "-toaddr", "127.0.0.1:18081",
		"-proxyproto", "tcp", "-key", "123456",
	}); code != 0 {
		t.Fatalf("weak key client exit=%d", code)
	}
}

func TestRunMain_NewServerListenFail(t *testing.T) {
	// Occupy the port first so Listen fails inside NewServer.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", ln.Addr().String(),
		"-key", "runmain-key",
	}); code != 0 {
		t.Fatalf("listen conflict exit=%d", code)
	}
}

func TestRunMain_ServerLifecycleStatusAndShutdown(t *testing.T) {
	shutdown := withFakeSignal(t)
	listen := freePort(t)
	status := freePort(t)
	done := runMainAsync([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
		"-key", "runmain-key",
		"-encrypt", "runmain-encrypt",
		"-statusaddr", "127.0.0.1:" + itoaPort(status),
		"-profile", itoaPort(freePort(t)),
	})
	waitHealthy(t, status)

	resp, err := http.Get("http://127.0.0.1:" + itoaPort(status) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"role":"server"`) {
		t.Fatalf("status code=%d body=%s", resp.StatusCode, body)
	}

	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("server exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("server did not exit after signal")
	}
}

func TestRunMain_ServerFromConfigFile(t *testing.T) {
	shutdown := withFakeSignal(t)
	listen := freePort(t)
	status := freePort(t)
	cfg := `{
		"type": "server",
		"proto": ["tcp"],
		"listen": ["127.0.0.1:` + itoaPort(listen) + `"],
		"key": "runmain-config-key",
		"statusaddr": "127.0.0.1:` + itoaPort(status) + `",
		"nolog": 1,
		"noprint": 1
	}`
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	done := runMainAsync([]string{"-config", path})
	waitHealthy(t, status)
	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("config-file server exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("config-file server did not exit after signal")
	}
}

func TestRunMain_ClientLifecycle(t *testing.T) {
	shutdown := withFakeSignal(t)
	status := freePort(t)
	done := runMainAsync([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "proxy_client",
		"-proto", "tcp", "-server", "127.0.0.1:1",
		"-fromaddr", ":0", "-toaddr", "127.0.0.1:1",
		"-proxyproto", "tcp",
		"-key", "runmain-key",
		"-statusaddr", "127.0.0.1:" + itoaPort(status),
	})
	waitHealthy(t, status)
	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("client exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("client did not exit after signal")
	}
}

func TestRunMain_StatusAddrConflict(t *testing.T) {
	// Pre-bind the status port so StartStatusServer fails; runMain must close
	// the server and return cleanly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	listen := freePort(t)
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
		"-key", "runmain-key",
		"-statusaddr", ln.Addr().String(),
	}); code != 0 {
		t.Fatalf("status conflict exit=%d", code)
	}
}

// A config file with every field populated covers all file->flag merge
// branches in one shot.
func TestRunMain_ConfigFileMergesAllFields(t *testing.T) {
	shutdown := withFakeSignal(t)
	listen := freePort(t)
	status := freePort(t)
	cfg := `{
		"type": "server",
		"proto": ["tcp"],
		"listen": ["127.0.0.1:` + itoaPort(listen) + `"],
		"name": "cfg-name",
		"key": "cfg-full-merge-key",
		"encrypt": "cfg-full-encrypt",
		"encrypttype": "aes-gcm",
		"compress": 64,
		"compresstype": "zlib",
		"nolog": 1,
		"noprint": 1,
		"loglevel": "info",
		"ping": true,
		"username": "cfguser",
		"password": "cfgpass",
		"kcpfecdata": 0,
		"kcpfecparity": 0,
		"statusaddr": "127.0.0.1:` + itoaPort(status) + `"
	}`
	path := filepath.Join(t.TempDir(), "full.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	done := runMainAsync([]string{"-config", path})
	waitHealthy(t, status)
	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("full-merge config exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("full-merge config server did not exit after signal")
	}
}

// Empty -compresstype/-encrypttype fall back to the session defaults.
func TestRunMain_EmptyCodecTypesUseDefaults(t *testing.T) {
	shutdown := withFakeSignal(t)
	listen := freePort(t)
	status := freePort(t)
	done := runMainAsync([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
		"-key", "runmain-defaults-key",
		"-encrypt", "runmain-defaults-enc",
		"-compresstype", "", "-encrypttype", "",
		"-statusaddr", "127.0.0.1:" + itoaPort(status),
	})
	waitHealthy(t, status)
	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("empty codec types exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("server did not exit after signal")
	}
}

// Client with a conflicting status port must close the client and exit 0.
func TestRunMain_ClientStatusAddrConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "proxy_client",
		"-proto", "tcp", "-server", "127.0.0.1:1",
		"-fromaddr", ":0", "-toaddr", "127.0.0.1:1",
		"-proxyproto", "tcp",
		"-key", "runmain-key",
		"-statusaddr", ln.Addr().String(),
	}); code != 0 {
		t.Fatalf("client status conflict exit=%d", code)
	}
}

// A client config file fills proxyproto/server/fromaddr/toaddr/profile, the
// branches a server-only file never touches.
func TestRunMain_ClientConfigFileFields(t *testing.T) {
	shutdown := withFakeSignal(t)
	status := freePort(t)
	profile := freePort(t)
	cfg := `{
		"type": "proxy_client",
		"proxyproto": ["tcp"],
		"server": "127.0.0.1:1",
		"fromaddr": [":0"],
		"toaddr": ["127.0.0.1:9"],
		"name": "from-file",
		"key": "cfg-client-merge-key",
		"nolog": 1,
		"noprint": 1,
		"profile": ` + itoaPort(profile) + `,
		"statusaddr": "127.0.0.1:` + itoaPort(status) + `"
	}`
	path := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	done := runMainAsync([]string{"-config", path})
	waitHealthy(t, status)
	shutdown()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("client config exit=%d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("client config did not exit after signal")
	}
}

// An unknown proxy proto passes flag validation and fails inside NewClient.
func TestRunMain_NewClientProxyProtoFail(t *testing.T) {
	if code := runMain([]string{
		"-nolog", "1", "-noprint", "1",
		"-type", "socks5_client",
		"-proto", "tcp", "-server", "127.0.0.1:1",
		"-fromaddr", ":0", "-proxyproto", "not-a-proto",
		"-key", "runmain-key",
	}); code != 0 {
		t.Fatalf("bad proxyproto exit=%d", code)
	}
}
