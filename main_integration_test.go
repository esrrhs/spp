package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Subprocess integration coverage for main(): build the real binary once and
// drive startup, the status endpoint, config-file merging, validation rejects,
// and graceful SIGTERM shutdown end to end.

var (
	sppBinOnce sync.Once
	sppBinPath string
	sppBinErr  error

	// When SPP_IT_COVERDIR is set, the integration binary is built with
	// binary coverage and every child writes GOCOVERDIR profiles there. Merge
	// afterwards with `go tool covdata textfmt -i=$SPP_IT_COVERDIR -o child.out`.
	sppCoverDir = os.Getenv("SPP_IT_COVERDIR")
)

func sppBinary(t *testing.T) string {
	t.Helper()
	sppBinOnce.Do(func() {
		// A package-level temp dir (not a test-scoped t.TempDir) so the binary
		// survives across the integration tests that share this build.
		dir, err := os.MkdirTemp("", "spp-it-build-")
		if err != nil {
			sppBinErr = err
			return
		}
		sppBinPath = filepath.Join(dir, "spp-it")
		build := []string{"build", "-o", sppBinPath}
		if sppCoverDir != "" {
			build = append(build, "-cover", "-coverpkg=github.com/esrrhs/spp/...")
			if err := os.MkdirAll(sppCoverDir, 0o755); err != nil {
				sppBinErr = err
				return
			}
		}
		build = append(build, ".")
		cmd := exec.Command("go", build...)
		if out, err := cmd.CombinedOutput(); err != nil {
			sppBinErr = err
			_ = os.WriteFile(filepath.Join(dir, "build.log"), out, 0o644)
		}
	})
	if sppBinErr != nil {
		t.Skipf("cannot build spp binary for integration test: %v", sppBinErr)
	}
	return sppBinPath
}

// newSppCmd builds an exec.Cmd and, when subprocess coverage is enabled, points
// the child at the shared GOCOVERDIR.
func newSppCmd(t *testing.T, args []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(sppBinary(t), args...)
	if sppCoverDir != "" {
		cmd.Env = append(os.Environ(), "GOCOVERDIR="+sppCoverDir)
	}
	return cmd
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startSpp launches the binary and waits for the given URL to respond 200.
func startSpp(t *testing.T, args []string, healthURL string) *exec.Cmd {
	t.Helper()
	cmd := newSppCmd(t, args)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start spp: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	if healthURL != "" {
		deadline := time.Now().Add(10 * time.Second)
		client := &http.Client{Timeout: 500 * time.Millisecond}
		for time.Now().Before(deadline) {
			if resp, err := client.Get(healthURL + "/healthz"); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return cmd
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("spp never became healthy: %v", args)
	}
	return cmd
}

func TestIntegration_ServerFlags_StatusAndGracefulShutdown(t *testing.T) {
	listen := freePort(t)
	status := freePort(t)
	health := "http://127.0.0.1:" + itoaPort(status)
	cmd := startSpp(t, []string{
		"-nolog", "1", "-noprint", "1",
		"-type", "server",
		"-proto", "tcp", "-listen", "127.0.0.1:" + itoaPort(listen),
		"-key", "it-strong-auth-key",
		"-encrypt", "it-strong-encrypt-key",
		"-statusaddr", "127.0.0.1:" + itoaPort(status),
	}, health)

	// /status returns a JSON document for the running server.
	resp, err := http.Get(health + "/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"role":"server"`) {
		t.Fatalf("status code=%d body=%s", resp.StatusCode, body)
	}

	// Graceful shutdown via SIGTERM must succeed.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exited with error after SIGTERM: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("server did not exit within 8s of SIGTERM")
	}
}

func TestIntegration_ServerFromConfigFile(t *testing.T) {
	listen := freePort(t)
	status := freePort(t)
	health := "http://127.0.0.1:" + itoaPort(status)

	cfg := `{
		"type": "server",
		"proto": ["tcp"],
		"listen": ["127.0.0.1:` + itoaPort(listen) + `"],
		"key": "it-config-auth-key",
		"encrypt": "it-config-encrypt-key",
		"statusaddr": "127.0.0.1:` + itoaPort(status) + `",
		"nolog": 1,
		"noprint": 1
	}`
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := startSpp(t, []string{"-config", path}, health)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
}

func TestIntegration_InvalidArgsExitWithoutServing(t *testing.T) {
	sppBinary(t) // ensure the shared binary is built before the cases run
	cases := [][]string{
		{"-type", "bogus", "-key", "it-strong-auth-key"},
		{"-type", "proxy_client", "-proto", "tcp", "-proto", "rudp",
			"-server", "127.0.0.1:1", "-fromaddr", ":8080", "-toaddr", ":8080",
			"-proxyproto", "tcp", "-key", "it-strong-auth-key"},
		{"-type", "server", "-proto", "tcp", "-listen", ":1", "-listen", ":2",
			"-key", "it-strong-auth-key"},
	}
	for _, args := range cases {
		cmd := newSppCmd(t, args)
		out, _ := cmd.CombinedOutput()
		// main() returns after flag.Usage(); the process must terminate promptly
		// (it never blocks serving) and print a usage/error line.
		if !strings.Contains(string(out), "Usage of") {
			t.Fatalf("args %v did not print usage, output:\n%s", args, out)
		}
	}
}

func itoaPort(p int) string {
	return strconv.Itoa(p)
}
