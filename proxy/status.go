package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/spp/version"
)

// PipeStatus is the status snapshot of the session's single underlay conn.
type PipeStatus struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	RttMs int64  `json:"rttMs"`
}

// pipeStatus describes one session's single underlay connection.
func pipeStatusOf(p *ProxyConn) PipeStatus {
	proto, addr := "", ""
	if p.conn != nil {
		proto = p.conn.Name()
		addr = p.conn.Info()
	}
	return PipeStatus{
		Proto: proto,
		Addr:  addr,
		RttMs: int64(p.rtt() / time.Millisecond),
	}
}

// ServiceStatus describes one configured proxy service and its live sonnies.
type ServiceStatus struct {
	Index int    `json:"index"`
	Kind  string `json:"kind"` // input / output
	Proto string `json:"proto"`
	Sonny int    `json:"sonny"`
}

// StatusReport is the JSON document served at /status.
type StatusReport struct {
	Role        string          `json:"role"` // server / client
	Name        string          `json:"name,omitempty"`
	Version     string          `json:"version"`
	GitCommit   string          `json:"gitCommit"`
	UptimeSec   int64           `json:"uptimeSec"`
	Goroutines  int             `json:"goroutines"`
	Established bool            `json:"established"`
	Clients     int             `json:"clients,omitempty"`
	Sonny       int             `json:"sonny"`
	Pipes       []PipeStatus    `json:"pipes"`
	Services    []ServiceStatus `json:"services,omitempty"`
	Counters    State           `json:"counters"`
	Threads     StateThreadNum  `json:"threads"`
}

func baseStatusReport(role, name string) StatusReport {
	return StatusReport{
		Role:       role,
		Name:       name,
		Version:    version.Version,
		GitCommit:  version.GitCommit,
		UptimeSec:  int64(time.Since(procStart) / time.Second),
		Goroutines: runtime.NumGoroutine(),
		Pipes:      []PipeStatus{},
		Services:   []ServiceStatus{},
		Counters:   snapshotState(),
		Threads:    snapshotThreadNum(),
	}
}

// servicesStatus collects per-service sonny counts from input/output lists.
func servicesStatus(inputs []*Inputer, outputs []*Outputer) ([]ServiceStatus, int) {
	services := make([]ServiceStatus, 0, len(inputs)+len(outputs))
	sonny := 0
	for _, in := range inputs {
		n := in.sonnySize()
		sonny += n
		services = append(services, ServiceStatus{
			Index: int(in.serviceIndex),
			Kind:  "input",
			Proto: in.proto,
			Sonny: n,
		})
	}
	for _, out := range outputs {
		n := out.sonnySize()
		sonny += n
		services = append(services, ServiceStatus{
			Index: int(out.serviceIndex),
			Kind:  "output",
			Proto: out.proto,
			Sonny: n,
		})
	}
	return services, sonny
}

// SnapshotStatus builds a status report for a running server, aggregating all
// client sessions, their services, the single underlay pipe, and live counters.
func (s *Server) SnapshotStatus() StatusReport {
	r := baseStatusReport("server", "")
	r.Clients = s.clientSize()

	pipes := make([]PipeStatus, 0)
	services := make([]ServiceStatus, 0)
	sonny := 0
	s.clients.Range(func(_, value interface{}) bool {
		sess, ok := value.(*ClientConn)
		if !ok {
			return true
		}
		inputs, outputs := sess.serviceSnapshot()
		svcs, n := servicesStatus(inputs, outputs)
		services = append(services, svcs...)
		sonny += n
		pipes = append(pipes, pipeStatusOf(&sess.ProxyConn))
		return true
	})
	r.Pipes = pipes
	r.Services = services
	r.Sonny = sonny
	return r
}

// SnapshotStatus builds a status report for a running client and its current
// server session (nil before login / while reconnecting).
func (c *Client) SnapshotStatus() StatusReport {
	r := baseStatusReport("client", c.name)

	c.connMu.Lock()
	sess := c.serverconn
	c.connMu.Unlock()
	if sess == nil {
		return r
	}
	r.Established = sess.isEstablished()

	inputs, outputs := sess.serviceSnapshot()
	svcs, sonny := servicesStatus(inputs, outputs)
	r.Services = svcs
	r.Sonny = sonny
	r.Pipes = []PipeStatus{pipeStatusOf(&sess.ProxyConn)}
	return r
}

// StatusServer serves /healthz and /status over plain HTTP. Bind it to a
// loopback address unless the deployment explicitly secures external access.
type StatusServer struct {
	ln      net.Listener
	httpSrv *http.Server
}

// StartStatusServer binds addr (use "127.0.0.1:0" for an ephemeral port) and
// serves status snapshots produced by report.
func StartStatusServer(addr string, report func() StatusReport) (*StatusServer, error) {
	if report == nil {
		return nil, errors.New("nil status report collector")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(report())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("spp status server: see /healthz and /status\n"))
	})

	s := &StatusServer{ln: ln}
	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			loggo.Error("status server: %s", err.Error())
		}
	}()
	loggo.Info("status server listening on %s", ln.Addr().String())
	return s, nil
}

// Addr returns the bound address (useful with port 0).
func (s *StatusServer) Addr() string {
	return s.ln.Addr().String()
}

// Close gracefully shuts the status server down.
func (s *StatusServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}
