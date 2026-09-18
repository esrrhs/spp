package proxy

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelHub_PickBestAndGray(t *testing.T) {
	cfg := DefaultConfig()
	hub := newChannelHub(cfg)

	slow := &mainPipe{proto: "tcp", addr: "a"}
	fast := &mainPipe{proto: "rudp", addr: "b"}
	hub.add(slow)
	hub.add(fast)

	atomic.StoreInt64(&slow.thrBps, 1<<20)  // 1 MB/s
	atomic.StoreInt64(&fast.thrBps, 10<<20) // 10 MB/s
	atomic.StoreInt64(&slow.rttNs, int64(50*time.Millisecond))
	atomic.StoreInt64(&fast.rttNs, int64(20*time.Millisecond))

	best := hub.pickBest()
	if best != fast {
		t.Fatalf("expected fast pipe, got %v", best.proto)
	}

	fast.markGray("simulated failure")
	if !fast.isGray() || fast.isActive() {
		t.Fatal("fast should be gray")
	}
	best = hub.pickBest()
	if best != slow {
		t.Fatalf("after gray, expected slow, got %v", best.proto)
	}
	if hub.activeCount() != 1 {
		t.Fatalf("activeCount=%d want 1", hub.activeCount())
	}

	fast.markActive("recovered")
	best = hub.pickBest()
	if best != fast {
		t.Fatalf("after recover, expected fast, got %v", best.proto)
	}
}

func TestChannelHub_RouteFrame(t *testing.T) {
	cfg := DefaultConfig()
	hub := newChannelHub(cfg)
	p := &mainPipe{proto: "tcp", addr: "x"}
	p.sendq = newPrioQueue(8)
	hub.add(p)
	atomic.StoreInt64(&p.thrBps, 1000)

	f := &ProxyFrame{Type: FRAME_TYPE_OPEN, OpenFrame: &OpenConnFrame{Id: "1"}}
	hub.routeFrame(f, false)
	v, _, ok := p.sendq.PopWait(time.Second)
	if !ok || v.(*ProxyFrame).Type != FRAME_TYPE_OPEN {
		t.Fatal("frame not routed to pipe")
	}
}

func TestChannelHub_RouteFrame_AllGrayDrops(t *testing.T) {
	hub := newChannelHub(DefaultConfig())
	p := &mainPipe{proto: "tcp", addr: "x"}
	p.sendq = newPrioQueue(8)
	hub.add(p)
	p.markGray("down")

	hub.routeFrame(&ProxyFrame{Type: FRAME_TYPE_OPEN, OpenFrame: &OpenConnFrame{Id: "1"}}, false)
	if _, _, ok := p.sendq.PopWait(20 * time.Millisecond); ok {
		t.Fatal("expected drop when no active pipe")
	}
	if hub.pickBest() != nil {
		t.Fatal("pickBest should be nil when all gray")
	}
}

func TestChannelHub_RemoveMarksDead(t *testing.T) {
	hub := newChannelHub(DefaultConfig())
	a := &mainPipe{proto: "tcp", addr: "a"}
	b := &mainPipe{proto: "rudp", addr: "b"}
	a.sendq = newPrioQueue(4)
	b.sendq = newPrioQueue(4)
	hub.add(a)
	hub.add(b)
	atomic.StoreInt64(&a.thrBps, 100)
	atomic.StoreInt64(&b.thrBps, 200)

	hub.remove(b)
	if atomic.LoadInt32(&b.state) != pipeDead {
		t.Fatal("removed pipe should be dead")
	}
	if hub.liveCount() != 1 || hub.pickBest() != a {
		t.Fatalf("live=%d best=%v", hub.liveCount(), hub.pickBest())
	}
}

func TestChannelHub_RouteFrame_NotesTraffic(t *testing.T) {
	hub := newChannelHub(DefaultConfig())
	p := &mainPipe{proto: "tcp", addr: "x"}
	p.sendq = newPrioQueue(8)
	hub.add(p)

	payload := make([]byte, 4096)
	hub.routeFrame(&ProxyFrame{
		Type:      FRAME_TYPE_DATA,
		DataFrame: &DataFrame{Id: "1", Data: payload},
	}, false)
	if atomic.LoadInt64(&p.winBytes) != 4096 {
		t.Fatalf("winBytes=%d want 4096", atomic.LoadInt64(&p.winBytes))
	}
	if _, _, ok := p.sendq.PopWait(time.Second); !ok {
		t.Fatal("data frame not enqueued")
	}
}

func TestMainPipe_RefreshThroughputAndScore(t *testing.T) {
	p := &mainPipe{proto: "tcp", addr: "x"}
	atomic.StoreInt64(&p.winStartNs, time.Now().Add(-2*time.Second).UnixNano())
	atomic.StoreInt64(&p.winBytes, 2<<20) // 2 MiB over ~2s → ~1 MiB/s
	atomic.StoreInt64(&p.rttNs, int64(10*time.Millisecond))

	p.refreshThroughput()
	thr := atomic.LoadInt64(&p.thrBps)
	if thr < 500<<10 || thr > 2<<20 {
		t.Fatalf("unexpected thrBps=%d (%s)", thr, formatBps(thr))
	}

	lowRTT := p.score()
	atomic.StoreInt64(&p.rttNs, int64(200*time.Millisecond))
	highRTT := p.score()
	if lowRTT <= highRTT {
		t.Fatalf("lower RTT should score higher: %d vs %d", lowRTT, highRTT)
	}
}

func TestClient_ProcessSpeedTest_RecoverGray(t *testing.T) {
	c := &Client{config: DefaultConfig()}
	pipe := &mainPipe{proto: "rudp", addr: "x"}
	atomic.StoreInt32(&pipe.state, pipeGray)

	sendTime := time.Now().Add(-20 * time.Millisecond).UnixNano()
	payload := make([]byte, 32*1024)
	c.processSpeedTest(&ProxyFrame{
		Type: FRAME_TYPE_SPEEDTEST,
		SpeedTestFrame: &SpeedTestFrame{
			Id:       1,
			SendTime: sendTime,
			Payload:  payload,
			Echo:     true,
		},
	}, pipe)

	if !pipe.isActive() {
		t.Fatal("probe echo should mark pipe active")
	}
	thr := atomic.LoadInt64(&pipe.thrBps)
	if thr <= 0 {
		t.Fatal("thrBps should be updated from probe")
	}
	rtt := atomic.LoadInt64(&pipe.rttNs)
	if rtt <= 0 {
		t.Fatal("rtt should be updated from probe")
	}
}

func TestClient_ProcessSpeedTest_IgnoresNonEcho(t *testing.T) {
	c := &Client{config: DefaultConfig()}
	pipe := &mainPipe{proto: "tcp", addr: "x"}
	atomic.StoreInt32(&pipe.state, pipeGray)

	c.processSpeedTest(&ProxyFrame{
		Type: FRAME_TYPE_SPEEDTEST,
		SpeedTestFrame: &SpeedTestFrame{
			Id:       1,
			SendTime: time.Now().UnixNano(),
			Payload:  []byte("x"),
			Echo:     false,
		},
	}, pipe)

	if !pipe.isGray() {
		t.Fatal("non-echo SPEEDTEST must not change gray state")
	}
	if atomic.LoadInt64(&pipe.thrBps) != 0 {
		t.Fatal("non-echo must not update thrBps")
	}
}

func TestServer_ProcessSpeedTest_Echo(t *testing.T) {
	s := &Server{config: DefaultConfig()}
	pipe := &mainPipe{proto: "tcp", addr: "x"}
	pipe.sendq = newPrioQueue(8)

	payload := []byte("probe-payload")
	sendTime := int64(12345)
	s.processSpeedTest(&ProxyFrame{
		Type: FRAME_TYPE_SPEEDTEST,
		SpeedTestFrame: &SpeedTestFrame{
			Id:       7,
			SendTime: sendTime,
			Payload:  payload,
			Echo:     false,
		},
	}, pipe)

	v, _, ok := pipe.sendq.PopWait(time.Second)
	if !ok {
		t.Fatal("expected echo frame")
	}
	echo := v.(*ProxyFrame)
	if echo.Type != FRAME_TYPE_SPEEDTEST || echo.SpeedTestFrame == nil {
		t.Fatalf("bad echo type: %+v", echo)
	}
	st := echo.SpeedTestFrame
	if !st.Echo || st.Id != 7 || st.SendTime != sendTime || string(st.Payload) != string(payload) {
		t.Fatalf("echo mismatch: %+v", st)
	}

	// Echo frames must not be echoed again.
	s.processSpeedTest(echo, pipe)
	if _, _, ok := pipe.sendq.PopWait(20 * time.Millisecond); ok {
		t.Fatal("must not re-echo an echo frame")
	}
}

func TestChannelHub_GrayThenProbeThenPrefer(t *testing.T) {
	// End-to-end of the selector policy without real network:
	// gray preferred pipe → traffic falls back → probe recovers → prefer again.
	hub := newChannelHub(DefaultConfig())
	slow := &mainPipe{proto: "tcp", addr: "slow"}
	fast := &mainPipe{proto: "rudp", addr: "fast"}
	slow.sendq = newPrioQueue(8)
	fast.sendq = newPrioQueue(8)
	hub.add(slow)
	hub.add(fast)
	atomic.StoreInt64(&slow.thrBps, 1<<20)
	atomic.StoreInt64(&fast.thrBps, 10<<20)

	fast.markGray("timeout")
	hub.routeFrame(&ProxyFrame{Type: FRAME_TYPE_OPEN, OpenFrame: &OpenConnFrame{Id: "1"}}, false)
	if _, _, ok := slow.sendq.PopWait(time.Second); !ok {
		t.Fatal("should fall back to slow while fast is gray")
	}
	if _, _, ok := fast.sendq.PopWait(20 * time.Millisecond); ok {
		t.Fatal("gray pipe must not receive business frames")
	}

	c := &Client{config: DefaultConfig()}
	c.processSpeedTest(&ProxyFrame{
		Type: FRAME_TYPE_SPEEDTEST,
		SpeedTestFrame: &SpeedTestFrame{
			Id:       1,
			SendTime: time.Now().Add(-5 * time.Millisecond).UnixNano(),
			Payload:  make([]byte, 64*1024),
			Echo:     true,
		},
	}, fast)
	if !fast.isActive() {
		t.Fatal("probe should reactivate fast")
	}

	hub.routeFrame(&ProxyFrame{Type: FRAME_TYPE_OPEN, OpenFrame: &OpenConnFrame{Id: "2"}}, false)
	if _, _, ok := fast.sendq.PopWait(time.Second); !ok {
		t.Fatal("after probe recover, should prefer fast again")
	}
}
