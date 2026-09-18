package proxy

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
)

const (
	pipeActive int32 = iota
	pipeGray
	pipeDead
)

// channelHub picks the best active underlay pipe for outbound business frames,
// greys unhealthy pipes, and keeps probing grey ones until they recover.
type channelHub struct {
	mu    sync.Mutex
	pipes []*mainPipe
	cfg   *Config
}

type mainPipe struct {
	ProxyConn
	proto string
	addr  string
	hub   *channelHub

	state int32 // pipeActive / pipeGray / pipeDead

	rttNs      int64
	thrBps     int64
	winBytes   int64
	winStartNs int64

	probeID       int64
	probeStart    int64 // unix nano when SPEEDTEST was sent
	authChallenge []byte
}

func newChannelHub(cfg *Config) *channelHub {
	return &channelHub{cfg: cfg}
}

func (h *channelHub) add(p *mainPipe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p.hub = h
	atomic.StoreInt32(&p.state, pipeActive)
	atomic.StoreInt64(&p.winStartNs, time.Now().UnixNano())
	h.pipes = append(h.pipes, p)
	loggo.Info("channelHub add pipe %s %s (n=%d)", p.proto, p.addr, len(h.pipes))
}

func (h *channelHub) remove(p *mainPipe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.pipes[:0]
	for _, x := range h.pipes {
		if x != p {
			out = append(out, x)
		}
	}
	h.pipes = out
	atomic.StoreInt32(&p.state, pipeDead)
	loggo.Info("channelHub remove pipe %s %s (n=%d)", p.proto, p.addr, len(h.pipes))
}

func (h *channelHub) activeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.pipes {
		if atomic.LoadInt32(&p.state) == pipeActive {
			n++
		}
	}
	return n
}

func (h *channelHub) liveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pipes)
}

func (h *channelHub) snapshot() []*mainPipe {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*mainPipe, len(h.pipes))
	copy(out, h.pipes)
	return out
}

func (p *mainPipe) markGray(reason string) {
	if atomic.CompareAndSwapInt32(&p.state, pipeActive, pipeGray) {
		loggo.Warn("pipe gray %s %s: %s", p.proto, p.addr, reason)
	}
}

func (p *mainPipe) markActive(reason string) {
	prev := atomic.SwapInt32(&p.state, pipeActive)
	if prev != pipeActive {
		loggo.Info("pipe active %s %s: %s", p.proto, p.addr, reason)
	}
}

func (p *mainPipe) isActive() bool {
	return atomic.LoadInt32(&p.state) == pipeActive
}

func (p *mainPipe) isGray() bool {
	return atomic.LoadInt32(&p.state) == pipeGray
}

func (p *mainPipe) noteTraffic(n int) {
	atomic.AddInt64(&p.winBytes, int64(n))
}

func (p *mainPipe) noteRTT(rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	atomic.StoreInt64(&p.rttNs, int64(rtt))
}

func (p *mainPipe) refreshThroughput() {
	now := time.Now().UnixNano()
	start := atomic.LoadInt64(&p.winStartNs)
	if start == 0 {
		atomic.StoreInt64(&p.winStartNs, now)
		return
	}
	elapsed := now - start
	if elapsed < int64(time.Second) {
		return
	}
	bytes := atomic.SwapInt64(&p.winBytes, 0)
	atomic.StoreInt64(&p.winStartNs, now)
	bps := bytes * int64(time.Second) / elapsed
	// Blend with previous so brief idle windows don't instantly zero the score.
	prev := atomic.LoadInt64(&p.thrBps)
	if prev > 0 {
		bps = (prev + bps*2) / 3
	}
	atomic.StoreInt64(&p.thrBps, bps)
}

func (p *mainPipe) score() int64 {
	p.refreshThroughput()
	thr := atomic.LoadInt64(&p.thrBps)
	rtt := atomic.LoadInt64(&p.rttNs)
	if rtt <= 0 {
		rtt = int64(time.Second)
	}
	// Prefer throughput; break ties with lower RTT (ns → smaller penalty).
	return thr*1000 - rtt/int64(time.Microsecond)
}

// pickBest returns the highest-scoring active pipe, or nil.
func (h *channelHub) pickBest() *mainPipe {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *mainPipe
	var bestScore int64 = -1 << 62
	for _, p := range h.pipes {
		if atomic.LoadInt32(&p.state) != pipeActive {
			continue
		}
		s := p.score()
		if best == nil || s > bestScore {
			best = p
			bestScore = s
		}
	}
	return best
}

// routeFrame implements frameRouter for the logical session ProxyConn.
func (h *channelHub) routeFrame(f *ProxyFrame, interactive bool) {
	p := h.pickBest()
	if p == nil {
		loggo.Warn("channelHub no active pipe, drop frame %s", f.Type.String())
		return
	}
	if f.Type == FRAME_TYPE_DATA && f.DataFrame != nil {
		p.noteTraffic(len(f.DataFrame.Data))
	}
	if interactive {
		p.SendData(f, true)
	} else if f.Type == FRAME_TYPE_DATA {
		p.SendData(f, false)
	} else {
		p.SendFrame(f)
	}
}

func (h *channelHub) statusLine() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := ""
	for i, p := range h.pipes {
		if i > 0 {
			s += " | "
		}
		st := "active"
		switch atomic.LoadInt32(&p.state) {
		case pipeGray:
			st = "gray"
		case pipeDead:
			st = "dead"
		}
		s += p.proto + "=" + st +
			" thr=" + formatBps(atomic.LoadInt64(&p.thrBps)) +
			" rtt=" + time.Duration(atomic.LoadInt64(&p.rttNs)).String()
	}
	return s
}

func formatBps(bps int64) string {
	if bps >= 1<<20 {
		return itoa(bps>>20) + "MB/s"
	}
	if bps >= 1<<10 {
		return itoa(bps>>10) + "KB/s"
	}
	return itoa(bps) + "B/s"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
