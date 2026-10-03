package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGoSafe_RootGroupSurvivesCompletedTasks(t *testing.T) {
	g := newTestGroup(t)
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		goSafe(g, "root-task", func() {
			// A plain func() — there is deliberately no way to return an
			// error from here; completed tasks must not exit the group.
			done <- struct{}{}
		})
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("root task did not run")
		}
	}

	// The group must still accept and run new tasks after the first batch.
	ran := make(chan struct{})
	goSafe(g, "root-task-late", func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("group exited even though no task returned an error")
	}
}

func TestGoSafe_PanicInTaskDoesNotKillGroup(t *testing.T) {
	g := newTestGroup(t)
	recovered := make(chan struct{})
	goSafe(g, "panicking-task", func() {
		close(recovered)
		panic("boom: one bad conn must not take down the proxy")
	})
	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("panicking task did not run")
	}

	// Give the panic/recover a moment to unwind.
	deadline := time.Now().Add(2 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		ran := make(chan struct{})
		goSafe(g, "task-after-panic", func() { close(ran) })
		select {
		case <-ran:
			ok = true
		case <-time.After(100 * time.Millisecond):
		}
		if ok {
			break
		}
	}
	if !ok {
		t.Fatal("group died after a task panic; later tasks never ran")
	}
	if g.IsExit() {
		t.Fatal("root group marked exited after a recovered task panic")
	}
}

func TestPipeStateName(t *testing.T) {
	cases := map[int32]string{
		pipeActive: "active",
		pipeGray:   "gray",
		pipeDead:   "dead",
		99:         "unknown",
	}
	for st, want := range cases {
		if got := pipeStateName(st); got != want {
			t.Fatalf("pipeStateName(%d)=%q want %q", st, got, want)
		}
	}
}

func TestStartStatusServer_NilCollector(t *testing.T) {
	if _, err := StartStatusServer("127.0.0.1:0", nil); err == nil {
		t.Fatal("nil collector must error")
	}
	if _, err := StartStatusServer("invalid-addr-x", func() StatusReport { return StatusReport{} }); err == nil {
		t.Fatal("invalid listen address must error")
	}
}

func TestStatusServer_Endpoints(t *testing.T) {
	report := StatusReport{
		Role:       "server",
		Version:    "test-ver",
		UptimeSec:  42,
		Goroutines: 7,
		Pipes:      []PipeStatus{},
	}
	ss, err := StartStatusServer("127.0.0.1:0", func() StatusReport { return report })
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	base := "http://" + ss.Addr()

	// /healthz
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz: code=%d body=%q", resp.StatusCode, body)
	}

	// /status GET → JSON matching the collector output.
	resp, err = http.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var got StatusReport
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("status content-type=%q", ct)
	}
	if got.Role != "server" || got.Version != "test-ver" ||
		got.UptimeSec != 42 || got.Goroutines != 7 {
		t.Fatalf("status body mismatch: %+v", got)
	}

	// HEAD /status is allowed and has no body.
	req, _ := http.NewRequest(http.MethodHead, base+"/status", nil)
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status code=%d", hresp.StatusCode)
	}

	// POST /status → 405.
	req, _ = http.NewRequest(http.MethodPost, base+"/status", strings.NewReader("{}"))
	presp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = presp.Body.Close()
	if presp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status code=%d want 405", presp.StatusCode)
	}

	// Root index advertises the endpoints.
	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(idx), "/healthz") {
		t.Fatalf("index: code=%d body=%q", resp.StatusCode, idx)
	}

	// Unknown path → 404.
	resp, err = http.Get(base + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path code=%d want 404", resp.StatusCode)
	}
}

func TestSnapshotStatus_ClientWithoutSession(t *testing.T) {
	// Zero-value client: no config/server connection needed; snapshot before
	// login (or after all-pipes teardown) must not panic and reports no pipes.
	c := &Client{name: "pre-login"}
	r := c.SnapshotStatus()
	if r.Role != "client" || r.Name != "pre-login" || r.Established {
		t.Fatalf("unexpected pre-login report: %+v", r)
	}
	if len(r.Pipes) != 0 || r.Sonny != 0 {
		t.Fatalf("pre-login report should be empty: %+v", r)
	}
}

func TestStatusServer_GracefulClose(t *testing.T) {
	ss, err := StartStatusServer("127.0.0.1:0", func() StatusReport { return StatusReport{} })
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The listener is gone: new dial must fail and Close is still safe.
	if _, err := net.DialTimeout("tcp", ss.Addr(), time.Second); err == nil {
		t.Fatal("status listener still accepting after Close")
	}
}

func TestSnapshotStatus_ServerAndClient(t *testing.T) {
	h := startForwardProxy(t, "tcp", "status-e2e-secret-ok")
	defer h.Close()

	// Establish a business connection so services/sonny counters move.
	conn := h.Dial(5 * time.Second)
	defer conn.Close()
	echoRoundTrip(t, conn, []byte("status-ping"), 10*time.Second)

	// Client session should be established with one active pipe.
	deadline := time.Now().Add(8 * time.Second)
	var cr StatusReport
	for time.Now().Before(deadline) {
		cr = h.client.SnapshotStatus()
		if cr.Established && len(cr.Pipes) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cr.Role != "client" {
		t.Fatalf("client role=%q", cr.Role)
	}
	if !cr.Established {
		t.Fatal("client snapshot never reported established session")
	}
	if len(cr.Pipes) < 1 {
		t.Fatal("client snapshot has no pipes")
	}
	foundActive := false
	for _, p := range cr.Pipes {
		if p.Proto == "tcp" && p.State == "active" {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("no active tcp pipe: %+v", cr.Pipes)
	}
	if len(cr.Services) == 0 {
		t.Fatal("client snapshot has no services")
	}
	if cr.Version == "" || cr.Goroutines <= 0 {
		t.Fatalf("runtime fields missing: %+v", cr)
	}

	// Server side: one client session with an active pipe and the sonny.
	sr := h.server.SnapshotStatus()
	if sr.Role != "server" {
		t.Fatalf("server role=%q", sr.Role)
	}
	if sr.Clients < 1 {
		t.Fatalf("server clients=%d", sr.Clients)
	}
	if len(sr.Pipes) < 1 {
		t.Fatal("server snapshot has no pipes")
	}
	if sr.Sonny < 1 {
		t.Fatalf("server sonny=%d want >=1", sr.Sonny)
	}

	// Both reports must serialize to valid JSON.
	if _, err := json.Marshal(cr); err != nil {
		t.Fatalf("client report marshal: %v", err)
	}
	if _, err := json.Marshal(sr); err != nil {
		t.Fatalf("server report marshal: %v", err)
	}
}
