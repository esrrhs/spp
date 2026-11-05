package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

type Config struct {
	MaxMsgSize                int           // 消息最大长度
	MainBuffer                int           // 主通道buffer最大长度
	ConnBuffer                int           // 每个conn buffer最大长度
	EstablishedTimeout        int           // sonny 等业务连接建立超时
	AuthTimeout               int           // 主通道登录/鉴权超时；超时未 established 则踢掉
	PingInter                 int           // 主通道ping间隔
	PingTimeoutInter          int           // 主通道ping超时间隔
	ConnTimeout               int           // 每个conn的不活跃超时时间
	ConnectTimeout            int           // 每个conn的连接超时
	Key                       string        // 鉴权密钥，必须显式配置，无弱默认
	Encrypt                   string        // 加密密钥，空表示关闭加密，无弱默认
	EncryptType               ENCRYPT_TYPE  // 加密算法，默认 ChaCha20-Poly1305（整帧 AEAD）
	Compress                  int           // 压缩阈值，0 表示关闭
	CompressType              COMPRESS_TYPE // 压缩算法，默认 ZSTD
	ShowPing                  bool          // 是否显示ping
	Username                  string        // 登录用户名
	Password                  string        // 登录密码
	MainWriteChannelTimeoutMs int           // 主通道转发消息超时
	Congestion                string        // 拥塞算法
	KcpFecDataShards          int           // KCP FEC 数据分片数，0 关闭 FEC；启用时客户端与服务端必须配置一致
	KcpFecParityShards        int           // KCP FEC 校验分片数，0 关闭 FEC（如 10 数据 +3 校验，约 30% 冗余）
	RdnsDomain                string        // RDNS 隧道基础域名（例如 tunnel.example.com），使用 rdns 协议时必须配置
	RdnsUpstream              string        // RDNS 上游 DNS 服务器（保留供递归解析）
	RdnsPsk                   string        // RDNS 底层 AES-GCM 载荷加密密钥（空表示不启用）
	RdnsQueryType             string        // RDNS DNS 查询类型，"TXT"（默认）或 "NULL"
}

func DefaultConfig() *Config {
	return &Config{
		MaxMsgSize:                1024 * 1024,
		MainBuffer:                128,
		ConnBuffer:                32,
		EstablishedTimeout:        120,
		AuthTimeout:               15,
		PingInter:                 1,
		PingTimeoutInter:          60,
		ConnTimeout:               0,
		ConnectTimeout:            15,
		Key:                       "",
		Encrypt:                   "",
		EncryptType:               DefaultEncryptType(),
		Compress:                  128,
		CompressType:              CompressZstd,
		ShowPing:                  false,
		Username:                  "",
		Password:                  "",
		MainWriteChannelTimeoutMs: 0,
		Congestion:                "bb",
		KcpFecDataShards:          0,
		KcpFecParityShards:        0,
		RdnsQueryType:             "TXT",
	}
}

// ValidateConfig rejects missing or known-weak secrets.
func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	if strings.TrimSpace(cfg.Key) == "" {
		return errors.New("auth key (-key) is required")
	}
	if isWeakSecret(cfg.Key) {
		return errors.New("auth key (-key) is a known weak default; set a strong key")
	}
	if cfg.Encrypt != "" && isWeakSecret(cfg.Encrypt) {
		return errors.New("encrypt key (-encrypt) is a known weak default; set a strong key or leave empty to disable")
	}
	// KCP FEC is opt-in and changes the wire framing: both shard counts
	// must be positive together and their total must stay within the
	// Reed-Solomon 256-shard limit. FEC-enabled peers cannot interop with
	// FEC-less peers, so the default (0,0) keeps legacy compatibility.
	if (cfg.KcpFecDataShards < 0 || cfg.KcpFecParityShards < 0) ||
		(cfg.KcpFecDataShards == 0) != (cfg.KcpFecParityShards == 0) ||
		cfg.KcpFecDataShards+cfg.KcpFecParityShards > 256 {
		return errors.New("invalid KCP FEC shards: both kcp_fec_data_shards and kcp_fec_parity_shards must be 0 or positive together, and their sum must not exceed 256")
	}
	return nil
}

func isWeakSecret(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "123456", "default", "password", "pass", "secret", "admin":
		return true
	default:
		return false
	}
}

type ProxyConn struct {
	conn        network.Conn
	established int32 // atomic bool
	// Main channel (client/server): single priority queues matching one conn.
	sendq *prioQueue
	recvq *prioQueue
	// Sonny (per-proxy TCP/UDP): plain FIFO is enough.
	// sendq/recvq/sendch/recvch are assigned before the conn is published
	// (sync.Map registration / connMu publish), so hot-path readers can load
	// them without a lock: any observer that can reach this conn also has the
	// happens-before edge from the publishing store.
	sendch             *msgChannel
	recvch             *msgChannel
	actived            int32
	pinged             int32
	recvActive         int32 // atomic: set to 1 whenever wire bytes are received
	sendActive         int32 // atomic: set to 1 whenever non-ping/pong wire bytes are sent
	inRecvBackpressure int32 // atomic: set to 1 while recvFrom is blocked pushing to recvq
	inSendBackpressure int32 // atomic: set to 1 while sendTo is blocked in conn.Write
	lastRecvNano       int64 // atomic: timestamp of last wire byte received
	lastSendNano       int64 // atomic: timestamp of last non-ping/pong wire byte sent
	pongflag           *int32
	pongtime           *int64
	showPing           bool
	sentBytes          int64
	recvBytes          int64
	winMu              sync.Mutex
	winCond            *sync.Cond
	winWaiters         int32 // atomic: acquirers blocked on winCond
	inFlightBytes      int64 // atomic; guarded by winMu only while blocked
	inFlightFrames     int32 // atomic; guarded by winMu only while blocked
	maxWinBytes        int64 // atomic; set once by initSendWindow before first use
	maxWinFrames       int32 // atomic; set once by initSendWindow before first use
	rttNs              int64 // last PONG round-trip time (atomic, status page)
	id                 string
	needclose          int32 // atomic bool
	// connMu serializes the one late conn publish (Outputer.open after its
	// dial) against closeConn's read; every other conn access happens before
	// the conn is published or after teardown finished.
	connMu        sync.Mutex
	authChallenge []byte
	// codec is an immutable per-session codec snapshot (see sessionCodec);
	// swapped atomically by setCodec, read once per frame on the wire path.
	codec        atomic.Pointer[sessionCodec]
	nonceCounter uint64
	isClosed     atomic.Bool
	closeOnce    sync.Once
}

func (p *ProxyConn) Info() string {
	if p.conn != nil {
		return p.conn.Info()
	}
	return "sonny-" + p.id
}

func (p *ProxyConn) closeConn() {
	p.closeOnce.Do(func() {
		p.connMu.Lock()
		c := p.conn
		p.connMu.Unlock()
		if c != nil {
			c.Close()
		}
	})
}

// localHandshakeTimeout is the budget a peer connected to a local proxy
// listener (SOCKS5/HTTP) has to finish its handshake. It reuses the main
// channel's auth budget; fall back to the established timeout when unset.
func localHandshakeTimeout(cfg *Config) time.Duration {
	secs := cfg.AuthTimeout
	if secs <= 0 {
		secs = cfg.EstablishedTimeout
	}
	return time.Duration(secs) * time.Second
}

// handshakeWatchdog bounds a local proxy listener (SOCKS5/HTTP) handshake.
// Handshaking conns are not registered in Inputer.sonny yet, so
// checkSonnyActive cannot bound them; a peer that connects and stays
// silent, or sends only half a request (slowloris / scanner), would
// otherwise pin a goroutine and an fd forever. network.Conn does not expose
// socket deadlines, so the watchdog closes the conn itself, unblocking the
// handshake Read with an error. The returned cancel is idempotent: call it
// as soon as the handshake has completed (deferring it is also safe).
func handshakeWatchdog(p *ProxyConn, timeout time.Duration) func() {
	if timeout <= 0 {
		return func() {}
	}
	var fired atomic.Bool
	t := time.AfterFunc(timeout, func() {
		if fired.CompareAndSwap(false, true) {
			loggo.Warn("local proxy handshake timeout, closing %s", p.Info())
			p.closeConn()
		}
	})
	return func() {
		if fired.CompareAndSwap(false, true) {
			t.Stop()
		}
	}
}

func (p *ProxyConn) setEstablished(v bool) {
	if v {
		atomic.StoreInt32(&p.established, 1)
	} else {
		atomic.StoreInt32(&p.established, 0)
	}
}

func (p *ProxyConn) isEstablished() bool {
	return atomic.LoadInt32(&p.established) != 0
}

func (p *ProxyConn) setNeedClose() {
	atomic.StoreInt32(&p.needclose, 1)
	p.wakeSendWindow()
}

func (p *ProxyConn) isNeedClose() bool {
	return atomic.LoadInt32(&p.needclose) != 0
}

func sonnyWindowFrames(connBuf int) int {
	w := connBuf * 2
	if w < 16 {
		w = 16
	}
	if w > 64 {
		w = 64
	}
	return w
}

func (p *ProxyConn) initSendWindow(connBuf int) {
	w := sonnyWindowFrames(connBuf)
	p.winMu.Lock()
	if p.winCond == nil {
		p.winCond = sync.NewCond(&p.winMu)
	}
	atomic.StoreInt32(&p.maxWinFrames, int32(w))
	atomic.StoreInt64(&p.maxWinBytes, int64(w)*windowBytesPerFrame)
	p.winMu.Unlock()
}

func (p *ProxyConn) wakeSendWindow() {
	p.winMu.Lock()
	if p.winCond != nil {
		p.winCond.Broadcast()
	}
	p.winMu.Unlock()
}

// channelsClosed reads the atomic closed flag; CloseChannels sets it.
func (p *ProxyConn) channelsClosed() bool {
	return p.isClosed.Load()
}

// windowEnabled reports whether per-sonny flow control is active. The window
// fields are set once by initSendWindow before the conn is published, so the
// hot-path read needs no lock.
func (p *ProxyConn) windowEnabled() bool {
	return p.winCond != nil && atomic.LoadInt32(&p.maxWinFrames) > 0 && atomic.LoadInt64(&p.maxWinBytes) > 0
}

// windowClosed reports the abort conditions an acquirer checks before
// reserving: closed, needclose, or the group exiting never gets new credit.
func (p *ProxyConn) windowClosed(wg *thread.Group) bool {
	return p.isClosed.Load() || atomic.LoadInt32(&p.needclose) != 0 || isExit(wg)
}

// reserveWindow does the in-flight add once the caller knows space exists.
// Only copySonnyRecv acquires per conn, so the two counters cannot be raced
// by another acquirer; CAS keeps it robust regardless.
func (p *ProxyConn) reserveWindow(n int64, frames int32) {
	for {
		cur := atomic.LoadInt64(&p.inFlightBytes)
		if atomic.CompareAndSwapInt64(&p.inFlightBytes, cur, cur+n) {
			break
		}
	}
	for {
		cur := atomic.LoadInt32(&p.inFlightFrames)
		if atomic.CompareAndSwapInt32(&p.inFlightFrames, cur, cur+frames) {
			break
		}
	}
}

func (p *ProxyConn) acquireSendWindow(wg *thread.Group, n int64, frames int32) bool {
	if !p.windowEnabled() {
		return true
	}
	// Fast path: uncontended accounting with atomics only. Releases are
	// single-threaded per main channel and only shrink in-flight, so a
	// checked load can never understate the space the add will consume.
	if !p.windowClosed(wg) &&
		atomic.LoadInt64(&p.inFlightBytes)+n < atomic.LoadInt64(&p.maxWinBytes) &&
		atomic.LoadInt32(&p.inFlightFrames)+frames < atomic.LoadInt32(&p.maxWinFrames) {
		p.reserveWindow(n, frames)
		return true
	}
	// Slow path: window exhausted — block. winWaiters must be incremented
	// under winMu BEFORE the condition check: a release that lands between
	// the check and the increment would otherwise see zero waiters and skip
	// the broadcast, losing the wakeup.
	p.winMu.Lock()
	defer p.winMu.Unlock()
	atomic.AddInt32(&p.winWaiters, 1)
	defer atomic.AddInt32(&p.winWaiters, -1)
	for !p.windowClosed(wg) &&
		(atomic.LoadInt64(&p.inFlightBytes) >= atomic.LoadInt64(&p.maxWinBytes) ||
			atomic.LoadInt32(&p.inFlightFrames) >= atomic.LoadInt32(&p.maxWinFrames)) {
		p.winCond.Wait()
	}
	if p.windowClosed(wg) {
		return false
	}
	p.reserveWindow(n, frames)
	return true
}

func (p *ProxyConn) releaseSendWindow(n int64, frames int32) {
	if !p.windowEnabled() {
		return
	}
	atomic.AddInt64(&p.inFlightBytes, -n)
	atomic.AddInt32(&p.inFlightFrames, -frames)
	// Clamp a transient negative (release racing a fresh acquire) back to 0.
	for {
		cur := atomic.LoadInt64(&p.inFlightBytes)
		if cur >= 0 || atomic.CompareAndSwapInt64(&p.inFlightBytes, cur, 0) {
			break
		}
	}
	for {
		cur := atomic.LoadInt32(&p.inFlightFrames)
		if cur >= 0 || atomic.CompareAndSwapInt32(&p.inFlightFrames, cur, 0) {
			break
		}
	}
	// Wake a blocked acquirer only when one exists: the steady state (no
	// backpressure) must not pay a mutex+Broadcast per frame.
	if atomic.LoadInt32(&p.winWaiters) > 0 {
		p.winMu.Lock()
		p.winCond.Broadcast()
		p.winMu.Unlock()
	}
}

const winMsgPrefix = "win:"

func sendWindowUpdate(father *ProxyConn, id string, bytes int64, frames int32) {
	if father == nil || id == "" || (bytes <= 0 && frames <= 0) {
		return
	}
	rf := &ProxyFrame{
		Type: FRAME_TYPE_OPENRSP,
		OpenRspFrame: &OpenConnRspFrame{
			Id:  id,
			Ret: true,
			Msg: winMsgPrefix + strconv.FormatInt(bytes, 10) + ":" + strconv.Itoa(int(frames)),
		},
	}
	father.SendFrame(rf)
}

func parseWindowUpdateMsg(msg string) (bytes int64, frames int32, ok bool) {
	if !strings.HasPrefix(msg, winMsgPrefix) {
		return 0, 0, false
	}
	rest := strings.TrimPrefix(msg, winMsgPrefix)
	parts := strings.SplitN(rest, ":", 2)
	b, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	f := int64(1)
	if len(parts) == 2 {
		if parsedF, err := strconv.ParseInt(parts[1], 10, 32); err == nil {
			f = parsedF
		}
	}
	return b, int32(f), true
}

func (p *ProxyConn) noteRTT(d time.Duration) {
	if d > 0 {
		atomic.StoreInt64(&p.rttNs, int64(d))
	}
}

func (p *ProxyConn) rtt() time.Duration {
	return time.Duration(atomic.LoadInt64(&p.rttNs))
}

// dialWithTimeout dials addr and aborts if it takes longer than timeoutSec.
func dialWithTimeout(conn network.Conn, addr string, timeoutSec int) (network.Conn, error) {
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	type dialResult struct {
		c   network.Conn
		err error
	}
	ch := make(chan dialResult, 1)
	go func() {
		c, err := conn.Dial(addr)
		ch <- dialResult{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		conn.Close()
		// Drain late result to avoid leaking the dialed connection.
		go func() {
			r := <-ch
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, errors.New("dial timeout")
	}
}

// CloseChannels safely closes all internal queues without racing with concurrent send/recv.
func (p *ProxyConn) CloseChannels() {
	if !p.isClosed.CompareAndSwap(false, true) {
		return
	}
	if p.sendq != nil {
		p.sendq.Close()
	}
	if p.recvq != nil {
		p.recvq.Close()
	}
	if p.sendch != nil {
		p.sendch.Close()
	}
	if p.recvch != nil {
		p.recvch.Close()
	}
	p.wakeSendWindow()
}

func (p *ProxyConn) pickSendQ() *prioQueue {
	if p.isClosed.Load() {
		return nil
	}
	return p.sendq
}

func (p *ProxyConn) wakeSendQ() {
	if q := p.pickSendQ(); q != nil {
		q.Wake()
	}
}

func (p *ProxyConn) pickRecvQ() *prioQueue {
	if p.isClosed.Load() {
		return nil
	}
	return p.recvq
}

func (p *ProxyConn) pickSendCh() *msgChannel {
	if p.isClosed.Load() {
		return nil
	}
	return p.sendch
}

func (p *ProxyConn) pickRecvCh() *msgChannel {
	if p.isClosed.Load() {
		return nil
	}
	return p.recvch
}

// SendFrame enqueues a frame on the main priority queue (control jumps ahead).
func (p *ProxyConn) SendFrame(f *ProxyFrame) {
	if q := p.pickSendQ(); q != nil {
		prio := prioControl
		if f.Type == FRAME_TYPE_DATA || f.Type == FRAME_TYPE_CLOSE {
			prio = prioBulk
		}
		q.Push(f, prio)
		return
	}
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(f)
	}
}

// SendDataRaw pushes a queue element (plain frame or pooled envelope) onto
// the main channel, preserving the pool tag for the terminal consumer.
func (p *ProxyConn) SendDataRaw(v interface{}, isInteractive bool) {
	if q := p.pickSendQ(); q != nil {
		prio := prioBulk
		if isInteractive {
			prio = prioInter
		}
		q.Push(v, prio)
		return
	}
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(v)
	}
}

// SendData enqueues DATA: interactive can jump ahead of bulk on the same queue.
func (p *ProxyConn) SendData(f *ProxyFrame, isInteractive bool) {
	if q := p.pickSendQ(); q != nil {
		prio := prioBulk
		if isInteractive {
			prio = prioInter
		}
		q.Push(f, prio)
		return
	}
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(f)
	}
}

// RecvFrame enqueues a received frame; control is popped before DATA.
// DATA must stay FIFO here: sendToSonny requires per-sonny Index order, so
// size-based prioInter jump-ahead reorders chunks of the same TCP stream and
// kills the connection with "index error". Similarly, CLOSE must not overtake
// DATA of the same stream (or premature EOF drops the tail of the payload).
// RecvFrame enqueues a received queue element (plain frame or pooled
// envelope); the pool tag rides along to the terminal consumer. DATA must
// stay FIFO here: sendToSonny requires per-sonny Index order, so
// size-based prioInter jump-ahead reorders chunks of the same TCP stream and
// kills the connection with "index error". Similarly, CLOSE must not overtake
// DATA of the same stream (or premature EOF drops the tail of the payload).
func (p *ProxyConn) RecvFrame(v interface{}) {
	f := queueFrame(v)
	if q := p.pickRecvQ(); q != nil {
		prio := prioControl
		if f.Type == FRAME_TYPE_DATA || f.Type == FRAME_TYPE_CLOSE {
			prio = prioBulk
		}
		q.Push(v, prio)
		return
	}
	if ch := p.pickRecvCh(); ch != nil {
		// Defensive fallback (main conns always have a recvq): drop the pool
		// tag rather than hand an envelope to a plain-frame consumer.
		ch.Write(f)
	}
}

// SendSonnyData safely writes a data frame to sonny's sendch.
// If timeoutMs <= 0, it blocks until buffered or the sonny is closed (no timeout, pure backpressure).
// If timeoutMs > 0, it waits up to timeoutMs.
func (p *ProxyConn) SendSonnyData(f *ProxyFrame, timeoutMs int) bool {
	ch := p.pickSendCh()
	if ch == nil {
		return true
	}
	if timeoutMs <= 0 {
		ch.Write(f)
		return true
	}
	return ch.WriteTimeout(f, timeoutMs)
}

// SendSonnyDataRaw pushes a queue element (plain frame or pooled envelope)
// into sonny's sendch, preserving the pool tag for the terminal consumer.
func (p *ProxyConn) SendSonnyDataRaw(v interface{}, timeoutMs int) bool {
	ch := p.pickSendCh()
	if ch == nil {
		return true
	}
	if timeoutMs <= 0 {
		ch.Write(v)
		return true
	}
	return ch.WriteTimeout(v, timeoutMs)
}

// SendSonnyClose safely writes a close frame to sonny's sendch.
func (p *ProxyConn) SendSonnyClose(f *ProxyFrame) {
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(f)
	}
}

// RecvSonnyData safely writes an incoming queue element (plain frame or
// pooled envelope) from sonny's socket to sonny's recvch.
func (p *ProxyConn) RecvSonnyData(v interface{}) {
	if ch := p.pickRecvCh(); ch != nil {
		ch.Write(v)
	}
}

func checkProxyFame(f *ProxyFrame) error {
	switch f.Type {
	case FRAME_TYPE_LOGIN:
		if f.LoginFrame == nil {
			return errors.New("LoginFrame nil")
		}
	case FRAME_TYPE_LOGINRSP:
		if f.LoginRspFrame == nil {
			return errors.New("LoginRspFrame nil")
		}
	case FRAME_TYPE_DATA:
		if f.DataFrame == nil {
			return errors.New("DataFrame nil")
		}
	case FRAME_TYPE_PING:
		if f.PingFrame == nil {
			return errors.New("PingFrame nil")
		}
	case FRAME_TYPE_PONG:
		if f.PongFrame == nil {
			return errors.New("PongFrame nil")
		}
	case FRAME_TYPE_OPEN:
		if f.OpenFrame == nil {
			return errors.New("OpenFrame nil")
		}
	case FRAME_TYPE_OPENRSP:
		if f.OpenRspFrame == nil {
			return errors.New("OpenRspFrame nil")
		}
	case FRAME_TYPE_CLOSE:
		if f.CloseFrame == nil {
			return errors.New("CloseFrame nil")
		}
	case FRAME_TYPE_AUTH_CHALLENGE:
		if f.AuthChallengeFrame == nil {
			return errors.New("AuthChallengeFrame nil")
		}
	case FRAME_TYPE_CHANNEL_JOIN:
		if f.ChannelJoinFrame == nil {
			return errors.New("ChannelJoinFrame nil")
		}
	case FRAME_TYPE_CHANNEL_JOIN_RSP:
		if f.ChannelJoinRspFrame == nil {
			return errors.New("ChannelJoinRspFrame nil")
		}
	case FRAME_TYPE_SPEEDTEST:
		if f.SpeedTestFrame == nil {
			return errors.New("SpeedTestFrame nil")
		}
	default:
		return errors.New("Type error")
	}

	return nil
}

func isExit(wg *thread.Group) bool {
	if wg == nil {
		return true
	}
	select {
	case <-wg.Done():
		return true
	default:
		return false
	}
}

const (
	MAX_PROTO_PACK_SIZE = 100
)

func readFullConn(conn io.Reader, buf []byte, proxyconn *ProxyConn) error {
	offset := 0
	for offset < len(buf) {
		n, err := conn.Read(buf[offset:])
		if n > 0 {
			offset += n
			now := time.Now().UnixNano()
			atomic.StoreInt32(&proxyconn.pinged, 0)
			atomic.StoreInt32(&proxyconn.recvActive, 1)
			atomic.StoreInt64(&proxyconn.lastRecvNano, now)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pooledFrame carries a frame whose DataFrame.Data came from dataBufPool
// through the download queues (recvq -> bizq -> sendch). The wrapper is the
// pool tag: only wrapped frames are returned to the pool at their terminal
// consumer, so plain and test-constructed frames can never alias a recycled
// buffer. Envelopes are pooled themselves.
type pooledFrame struct {
	f *ProxyFrame
}

var pooledFramePool = sync.Pool{New: func() any {
	return &pooledFrame{}
}}

func wrapPooledFrame(f *ProxyFrame) interface{} {
	pf := pooledFramePool.Get().(*pooledFrame)
	pf.f = f
	return pf
}

// queueFrame extracts the frame from a queue element of either flavor.
func queueFrame(v interface{}) *ProxyFrame {
	if pf, ok := v.(*pooledFrame); ok {
		return pf.f
	}
	return v.(*ProxyFrame)
}

// frameAndPooled splits a queue element into its frame and, when present, its
// pool envelope (so the terminal consumer can recycle both).
func frameAndPooled(v interface{}) (*ProxyFrame, *pooledFrame) {
	if pf, ok := v.(*pooledFrame); ok {
		return pf.f, pf
	}
	return v.(*ProxyFrame), nil
}

// recyclePooledFrame returns an empty envelope to its pool.
func recyclePooledFrame(pf *pooledFrame) {
	if pf != nil {
		pf.f = nil
		pooledFramePool.Put(pf)
	}
}

// releasePooled returns a consumed queue element's payload buffer (and the
// envelope) to the pools. Safe on plain frames: the type assert just fails.
func releasePooled(v interface{}) {
	pf, ok := v.(*pooledFrame)
	if !ok {
		return
	}
	f := pf.f
	pf.f = nil
	pooledFramePool.Put(pf)
	if f != nil && f.DataFrame != nil {
		putPooledDataBuf(f.DataFrame.Data)
	}
}

func recvFrom(wg *thread.Group, proxyconn *ProxyConn, conn network.Conn, maxmsgsize int) error {

	atomic.AddInt32(&gStateThreadNum.RecvThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.RecvThread, -1)

	loggo.Info("recvFrom start %s", conn.Info())
	// Persistent read buffer: the 4-byte length prefix used to cost one Read
	// syscall and the frame body a second — two syscalls per frame on the
	// receive side while sendTo already writes each frame in one call. With a
	// buffer, the length prefix (and small bodies) come from memory and a
	// single refill serves many frames; bodies at least as large as the
	// buffer bypass it and read straight into ds, so large frames still cost
	// one syscall. Message-oriented transports (rudp/kcp/quic) just coalesce
	// their refills the same way. recvFrom is the conn's only reader.
	const recvBufSize = 64 * 1024
	br := bufio.NewReaderSize(conn, recvBufSize)
	bs := make([]byte, 4)
	ds := make([]byte, maxmsgsize+MAX_PROTO_PACK_SIZE+aeadOverhead)
	// Plaintext scratch for AEAD open: reused across frames so the wire path
	// does not allocate + copy a fresh plaintext per frame. (openWireAppend
	// rebinds plain to ds when encryption is off; cipher.AEAD.Open tolerates
	// dst/ciphertext aliasing, so both cases stay correct.)
	plain := make([]byte, 0, recvBufSize)

	for !isExit(wg) {
		if loggo.IsDebug() {
			loggo.Debug("recvFrom start ReadFull len %s", conn.Info())
		}
		err := readFullConn(br, bs, proxyconn)
		if err != nil {
			loggo.Info("recvFrom ReadFull fail: %s %s", conn.Info(), err.Error())
			return err
		}

		msglen := binary.LittleEndian.Uint32(bs)
		maxWire := uint32(maxmsgsize) + MAX_PROTO_PACK_SIZE + aeadOverhead
		if msglen > maxWire || msglen == 0 {
			loggo.Error("recvFrom len fail: %s %d", conn.Info(), msglen)
			return errors.New("msg len fail " + strconv.Itoa(int(msglen)))
		}

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start ReadFull body %s %d", conn.Info(), msglen)
		}
		err = readFullConn(br, ds[0:msglen], proxyconn)
		if err != nil {
			loggo.Info("recvFrom ReadFull fail: %s %s", conn.Info(), err.Error())
			return err
		}

		plain, err = proxyconn.openWireAppend(plain[:0], ds[0:msglen])
		if err != nil {
			loggo.Error("recvFrom openWire fail: %s %s", conn.Info(), err.Error())
			return err
		}

		// Pooled decode: compressed DATA payloads come from dataBufPool and
		// travel wrapped so the terminal consumer (sendToSonny) returns them.
		f, dataPooled, err := UnmarshalSrpFramePooled(plain, proxyconn.getCodec())
		if err != nil {
			loggo.Error("recvFrom UnmarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if f.Type == FRAME_TYPE_PING && proxyconn.pongflag != nil && proxyconn.pongtime != nil {
			processPing(f, proxyconn, proxyconn.pongflag, proxyconn.pongtime)
			atomic.AddInt32(&gState.MainRecvNum, 1)
			atomic.AddInt64(&gState.MainRecvSize, int64(msglen)+4)
			continue
		}
		if f.Type == FRAME_TYPE_PONG && proxyconn.pongflag != nil {
			rtt := processPong(f, proxyconn, proxyconn.showPing)
			proxyconn.noteRTT(rtt)
			atomic.AddInt32(&gState.MainRecvNum, 1)
			atomic.AddInt64(&gState.MainRecvSize, int64(msglen)+4)
			continue
		}

		if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
			loggo.Debug("recvFrom %s %s", conn.Info(), f.Type.String())
			if f.Type == FRAME_TYPE_DATA {
				if f.DataFrame.Crc != "" && common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
					loggo.Error("recvFrom crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
					return errors.New("conn crc error")
				}
			}
		}

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start Write %s", conn.Info())
		}

		var qv interface{} = f
		if dataPooled {
			qv = wrapPooledFrame(f)
		}
		atomic.StoreInt32(&proxyconn.inRecvBackpressure, 1)
		proxyconn.RecvFrame(qv)
		atomic.StoreInt32(&proxyconn.inRecvBackpressure, 0)

		atomic.AddInt32(&gState.MainRecvNum, 1)
		atomic.AddInt64(&gState.MainRecvSize, int64(msglen)+4)
	}

	loggo.Info("recvFrom end %s", conn.Info())
	return nil
}

func sendTo(wg *thread.Group, sendq *prioQueue, proxyconn *ProxyConn, conn network.Conn, maxmsgsize int, pingflag *int32, pongflag *int32, pongtime *int64) error {

	atomic.AddInt32(&gStateThreadNum.SendThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.SendThread, -1)

	loggo.Info("sendTo start %s", conn.Info())

	// One scratch buffer for the whole connection: the 4-byte length prefix
	// and the frame body go out in a single Write. Two Writes per frame meant
	// two syscalls and, on the message-oriented transports (rudp/kcp/quic/
	// ricmp), a lone 4-byte packet ahead of every frame — that doubles the
	// packet rate, which is what a small cloud box runs out of first.
	// Reusing the buffer is safe: every transport we ship copies the bytes
	// inside Write (rudp/kcp/ricmp into their send buffer, tcp into the
	// kernel, quic into the stream) before returning.
	maxWire := uint32(maxmsgsize) + MAX_PROTO_PACK_SIZE + aeadOverhead
	// Start small and let the buffers grow: a connection that only ever
	// carries control frames must not pin a megabyte of scratch for its
	// lifetime, and a busy box has hundreds of these. Growth is handled by
	// MarshalSrpFrameAppend/sealWireAppend, and the grown size is kept.
	const scratchStart = 64 * 1024 // covers a full MAX_CHUNK_SIZE frame
	scratch := scratchStart + aeadOverhead + 64
	if cap1 := int(maxWire) + aeadOverhead + 64; cap1 < scratch {
		scratch = cap1
	}
	wire := make([]byte, 4, scratch)
	plain := make([]byte, 0, scratch)

	for !isExit(wg) {
		// One Write per batch instead of per frame: fewer syscalls for TCP,
		// and proportionally fewer packets for the message-oriented transports
		// (rudp/kcp/ricmp), where packet rate is what a small box runs out of
		// first. The first frame is a blocking pop (or an inline ping/pong);
		// the rest are topped up without blocking. Pop order is priority
		// order and a single Write preserves it; frames arriving during
		// assembly wait at most one batch. Light load degrades to batch of 1.
		// wire is pre-sized per frame so sealWireAppend can never reallocate
		// mid-batch (a realloc would copy only the 4-byte dst, dropping
		// earlier frames' bytes).
		var f *ProxyFrame
		var pv *pooledFrame
		if atomic.LoadInt32(pingflag) > 0 {
			atomic.StoreInt32(pingflag, 0)
			f = &ProxyFrame{}
			f.Type = FRAME_TYPE_PING
			f.PingFrame = &PingFrame{}
			f.PingFrame.Time = time.Now().UnixNano()
		} else if atomic.LoadInt32(pongflag) > 0 {
			atomic.StoreInt32(pongflag, 0)
			f = &ProxyFrame{}
			f.Type = FRAME_TYPE_PONG
			f.PongFrame = &PongFrame{}
			f.PongFrame.Time = atomic.LoadInt64(pongtime)
		} else {
			v, closed, ok := sendq.PopWait(time.Second)
			if !ok {
				if closed {
					break
				}
				continue
			}
			f, pv = frameAndPooled(v)
		}

		const batchMaxFrames = 16
		const batchMaxBytes = 64 * 1024
		wire = wire[:0]
		nbatch := 0
		pingSolo := f.Type == FRAME_TYPE_PING || f.Type == FRAME_TYPE_PONG
		for {
			if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
				loggo.Debug("sendTo %s %s", conn.Info(), f.Type.String())
				if f.Type == FRAME_TYPE_DATA {
					if f.DataFrame.Crc != "" && common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
						loggo.Error("sendTo crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
						return errors.New("conn crc error")
					}
				}
			}

			// The pooled payload (if any) is copied into plain by the marshal
			// and is dead afterwards. Capture it before the marshal: the
			// compression gate swaps Data for its own buffer on success.
			// Control frames have no DataFrame at all.
			var origData []byte
			if f.DataFrame != nil {
				origData = f.DataFrame.Data
			}

			var err error
			plain, err = MarshalSrpFrameAppend(plain[:0], f, proxyconn.getCodec())
			if err != nil {
				loggo.Error("sendTo MarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
				return err
			}

			if pv != nil {
				putPooledDataBuf(origData)
				recyclePooledFrame(pv)
				pv = nil
			}

			if len(plain) == 0 {
				loggo.Error("sendTo len fail: %s %d", conn.Info(), len(plain))
				return errors.New("msg len fail 0")
			}

			if need := 4 + aeadNonceSize + len(plain) + aeadOverhead; cap(wire)-len(wire) < need {
				grown := make([]byte, len(wire), len(wire)+need+256)
				copy(grown, wire)
				wire = grown
			}

			start := len(wire)
			wire = append(wire, 0, 0, 0, 0)
			// Pass the whole buffer up to and including the prefix slot:
			// append extends at the slice's end, so a dst windowed at start
			// would return a slice that drops the earlier frames' bytes.
			wire, err = proxyconn.sealWireAppend(wire[:start+4], plain)
			if err != nil {
				loggo.Error("sendTo sealWire fail: %s %s", conn.Info(), err.Error())
				return err
			}

			msglen := uint32(len(wire) - start - 4)
			if msglen > maxWire || msglen == 0 {
				loggo.Error("sendTo len fail: %s %d", conn.Info(), msglen)
				return errors.New("msg len fail " + strconv.Itoa(int(msglen)))
			}

			if loggo.IsDebug() {
				loggo.Debug("sendTo frame len %s %d", conn.Info(), msglen)
			}
			binary.LittleEndian.PutUint32(wire[start:start+4], msglen)
			nbatch++

			// Ping/pong goes out alone: it is the liveness signal and must
			// not sit behind a full batch being assembled.
			if pingSolo || nbatch >= batchMaxFrames || len(wire) >= batchMaxBytes {
				break
			}
			v, ok := sendq.TryPop()
			if !ok {
				break
			}
			f, pv = frameAndPooled(v)
		}

		if loggo.IsDebug() {
			loggo.Debug("sendTo start Write len %s %d frames %d", conn.Info(), len(wire), nbatch)
		}
		if !pingSolo {
			atomic.StoreInt32(&proxyconn.inSendBackpressure, 1)
		}
		n, err := conn.Write(wire)
		atomic.StoreInt32(&proxyconn.inSendBackpressure, 0)
		if err != nil {
			loggo.Info("sendTo Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if n != len(wire) {
			loggo.Error("sendTo Write len fail: %s %d %d", conn.Info(), n, len(wire))
			return errors.New("len error")
		}

		if !pingSolo {
			atomic.StoreInt64(&proxyconn.lastSendNano, time.Now().UnixNano())
			atomic.StoreInt32(&proxyconn.sendActive, 1)
		}

		atomic.AddInt32(&gState.MainSendNum, int32(nbatch))
		atomic.AddInt64(&gState.MainSendSize, int64(len(wire)))
	}
	loggo.Info("sendTo end %s", conn.Info())
	return nil
}

const (
	MAX_INDEX = 1024
	// Chunk frames to 32KB: halves the per-frame Write syscalls on the target
	// conn (the bulk profile showed them at ~50% of server CPU with 16KB
	// frames, and per-sonny frame arrivals are too sparse for write batching
	// to form). Interleaving granularity coarsens accordingly — the
	// interactive priority still jumps between frames.
	MAX_CHUNK_SIZE = 32 * 1024
	// The per-sonny flow-control window bounds in-flight BYTES; freeze it at
	// its historical 1MB (64 x the old 16KB chunk) so doubling the chunk size
	// does not double the bufferbloat on narrow pipes.
	windowBytesPerFrame     = 16 * 1024
	INTERACTIVE_BYTES_LIMIT = 512 * 1024 // Initial bytes per sonny at prioInter (jumps ahead of bulk)
)

func recvFromSonny(wg *thread.Group, proxyconn *ProxyConn, conn network.Conn, maxmsgsize int) error {
	loggo.Info("recvFromSonny start %s", conn.Info())
	bufSize := maxmsgsize
	if bufSize > MAX_CHUNK_SIZE {
		bufSize = MAX_CHUNK_SIZE
	}

	index := int32(0)
	for !isExit(wg) {
		// Read straight into a pooled buffer: the payload becomes the frame's
		// Data with no copy, and the buffer travels wrapped (pool tag) to the
		// terminal consumer (sendTo's marshal), which returns it.
		ds := getPooledDataBuf()
		if cap(ds) < bufSize {
			// Belt and suspenders for the pool's capacity floor.
			ds = make([]byte, 0, bufSize)
		}
		msglen, err := conn.Read(ds[:bufSize])
		if err != nil {
			putPooledDataBuf(ds)
			loggo.Debug("recvFromSonny Read fail: %s %s", conn.Info(), err)
			if ch := proxyconn.pickRecvCh(); ch != nil {
				// Close recvch so copySonnyRecv drains all already-queued frames
				// into father.sendq before tearing down the sonny group.
				ch.Close()
				return nil
			}
			return err
		}

		if msglen <= 0 {
			putPooledDataBuf(ds)
			loggo.Error("recvFromSonny len error: %s %d", conn.Info(), msglen)
			return errors.New("len error " + strconv.Itoa(msglen))
		}

		f := &ProxyFrame{}
		f.Type = FRAME_TYPE_DATA
		f.DataFrame = &DataFrame{}
		f.DataFrame.Data = ds[:msglen]
		f.DataFrame.Compress = false
		if loggo.IsDebug() {
			f.DataFrame.Crc = common.GetCrc32(f.DataFrame.Data)
		}
		index++
		f.DataFrame.Index = index % MAX_INDEX
		if loggo.IsDebug() {
			loggo.Debug("recvFromSonny %s %d %s %d %p", conn.Info(), msglen, f.DataFrame.Crc, f.DataFrame.Index, f)
		}

		atomic.AddInt32(&gState.RecvNum, 1)
		atomic.AddInt64(&gState.RecvSize, int64(msglen))

		proxyconn.RecvSonnyData(wrapPooledFrame(f))
	}
	loggo.Info("recvFromSonny end %s", conn.Info())
	return nil
}

func sendToSonny(wg *thread.Group, proxyconn *ProxyConn, father *ProxyConn, sendch *msgChannel, conn network.Conn, maxmsgsize int) error {
	loggo.Info("sendToSonny start %s", conn.Info())
	// Stream targets may take one Write per batch instead of per frame (the
	// payload of consecutive frames is one ordered stream). Every transport
	// except udp is a reliable ordered stream at the Write level — kcp/quic
	// write into their session/stream, rudp/ricmp/rhttp loop until the whole
	// buffer is handed to their send machinery — so only datagram targets
	// keep the 1:1 mapping (frames must not be concatenated). batch=1 (the
	// common case when the target keeps up) writes the frame's own buffer
	// directly, zero-copy.
	batchable := conn.Name() != "udp"
	const (
		sendBatchMaxFrames = 16
		sendBatchBytes     = 64 * 1024
	)
	var scratch []byte
	var datas [][]byte
	index := int32(0)
	var ackedBytes int64
	var ackedFrames int32
	stepFrames := int32(1)
	stepBytes := int64(MAX_CHUNK_SIZE)
	if proxyconn != nil {
		proxyconn.winMu.Lock()
		if atomic.LoadInt32(&proxyconn.maxWinFrames) >= 4 {
			stepFrames = atomic.LoadInt32(&proxyconn.maxWinFrames) / 4
		}
		if atomic.LoadInt64(&proxyconn.maxWinBytes) >= 4*MAX_CHUNK_SIZE {
			stepBytes = atomic.LoadInt64(&proxyconn.maxWinBytes) / 4
		}
		proxyconn.winMu.Unlock()
	}
	for {
		ff, ok := sendch.ReadWait()
		if !ok {
			break
		}
		f := queueFrame(ff)
		if f.Type == FRAME_TYPE_CLOSE {
			loggo.Info("sendToSonny close by remote: %s", conn.Info())
			releasePooled(ff)
			return errors.New("close by remote")
		}
		if err := validateSonnyData(f, conn, maxmsgsize, &index); err != nil {
			return err
		}

		// Top up a batch of validated frames. A terminal CLOSE stops the
		// top-up and is handled after the batch goes out — FIFO order holds.
		var raws [sendBatchMaxFrames]interface{}
		raws[0] = ff
		nbatch := 1
		total := len(f.DataFrame.Data)
		pendingClose := interface{}(nil)
		for batchable && nbatch < sendBatchMaxFrames && total < sendBatchBytes {
			more, ok2 := sendch.TryReadWait()
			if !ok2 {
				break
			}
			mf := queueFrame(more)
			if mf.Type == FRAME_TYPE_CLOSE {
				pendingClose = more
				break
			}
			if err := validateSonnyData(mf, conn, maxmsgsize, &index); err != nil {
				for i := 0; i < nbatch; i++ {
					releasePooled(raws[i])
				}
				releasePooled(more)
				return err
			}
			raws[nbatch] = more
			nbatch++
			total += len(mf.DataFrame.Data)
		}
		datas = datas[:0]
		for i := 0; i < nbatch; i++ {
			datas = append(datas, queueFrame(raws[i]).DataFrame.Data)
		}

		var n int
		var err error
		if nbatch == 1 {
			n, err = conn.Write(datas[0])
		} else {
			if cap(scratch)-len(scratch) < total {
				grown := make([]byte, 0, total+4096)
				scratch = grown
			}
			scratch = scratch[:0]
			for i := 0; i < nbatch; i++ {
				scratch = append(scratch, datas[i]...)
			}
			n, err = conn.Write(scratch)
		}
		if err != nil {
			loggo.Debug("sendToSonny Write fail: %s %s", conn.Info(), err)
			return err
		}

		if n != total {
			loggo.Error("sendToSonny Write len fail: %s %d %d", conn.Info(), n, total)
			for i := 0; i < nbatch; i++ {
				releasePooled(raws[i])
			}
			return errors.New("len error")
		}

		// Terminal consumer of the download chain: the payload bytes are in
		// the target conn now, so pooled buffers go back to the pool.
		for i := 0; i < nbatch; i++ {
			releasePooled(raws[i])
		}

		if proxyconn != nil {
			atomic.AddInt32(&proxyconn.actived, 1)
			ackedBytes += int64(total)
			ackedFrames += int32(nbatch)
			if ackedBytes >= stepBytes || ackedFrames >= stepFrames || len(sendch.Ch()) == 0 {
				sendWindowUpdate(father, proxyconn.id, ackedBytes, ackedFrames)
				ackedBytes = 0
				ackedFrames = 0
			}
		}

		if loggo.IsDebug() {
			loggo.Debug("sendToSonny %s %d bytes %d frames", conn.Info(), total, nbatch)
		}

		atomic.AddInt32(&gState.SendNum, int32(nbatch))
		atomic.AddInt64(&gState.SendSize, int64(total))

		if pendingClose != nil {
			releasePooled(pendingClose)
			loggo.Info("sendToSonny close by remote: %s", conn.Info())
			return errors.New("close by remote")
		}
	}
	if proxyconn != nil && ackedFrames > 0 {
		sendWindowUpdate(father, proxyconn.id, ackedBytes, ackedFrames)
	}
	loggo.Info("sendToSonny end %s", conn.Info())
	return nil
}

// validateSonnyData runs the per-frame checks sendToSonny applies before
// writing: no compression flag, sane length, and the expected sequence index.
// *index is the caller's rolling counter and is advanced on success.
func validateSonnyData(f *ProxyFrame, conn network.Conn, maxmsgsize int, index *int32) error {
	if f.DataFrame.Compress {
		loggo.Error("sendToSonny Compress error: %s", conn.Info())
		return errors.New("msg compress error")
	}
	if len(f.DataFrame.Data) <= 0 {
		loggo.Error("sendToSonny len error: %s %d", conn.Info(), len(f.DataFrame.Data))
		return errors.New("len error " + strconv.Itoa(len(f.DataFrame.Data)))
	}
	if len(f.DataFrame.Data) > maxmsgsize {
		loggo.Error("sendToSonny len error: %s %d", conn.Info(), len(f.DataFrame.Data))
		return errors.New("len error " + strconv.Itoa(len(f.DataFrame.Data)))
	}
	if loggo.IsDebug() {
		if f.DataFrame.Crc != "" && f.DataFrame.Crc != common.GetCrc32(f.DataFrame.Data) {
			loggo.Error("sendToSonny crc error: %s %d %s %s", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Crc, common.GetCrc32(f.DataFrame.Data))
			return errors.New("crc error")
		}
	}
	*index++
	*index = *index % MAX_INDEX
	if f.DataFrame.Index != *index {
		loggo.Error("sendToSonny index error: %s %d %d %d", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Index, *index)
		return errors.New("index error")
	}
	return nil
}

func checkPingActive(wg *thread.Group, proxyconn *ProxyConn,
	estimeout int, pinginter int, pingintertimeout int, showping bool, pingflag *int32) error {

	loggo.Info("checkPingActive start %s", proxyconn.Info())

	// 1. 设置整体超时时间
	timeoutTimer := time.NewTimer(time.Second * time.Duration(estimeout))
	defer timeoutTimer.Stop()

	if !proxyconn.isEstablished() {
		estTicker := time.NewTicker(50 * time.Millisecond)
		defer estTicker.Stop()
		for !proxyconn.isEstablished() {
			select {
			// 优先响应退出信号
			case <-wg.Done():
				return nil

			case <-estTicker.C:

			// 整体超时触发
			case <-timeoutTimer.C:
				if !proxyconn.isEstablished() {
					loggo.Info("checkPingActive established timeout %s", proxyconn.Info())
					return errors.New("established timeout")
				}
			}
		}
		estTicker.Stop()
	}
	timeoutTimer.Stop()

	// 直接创建一个周期为 pinginter 的 Ticker
	pingTicker := time.NewTicker(time.Duration(pinginter) * time.Second)
	defer pingTicker.Stop()

	exit := false
	for !exit {
		select {
		// 1. 响应退出信号
		case <-wg.Done():
			exit = true
			break

		// 2. 定时触发 Ping 逻辑
		case <-pingTicker.C:
			// 如果有数据收到，或者正在因为下游反压阻塞接收队列，说明通道是活的，无需发送心跳探活
			recvAct := atomic.SwapInt32(&proxyconn.recvActive, 0)
			inRecvBP := atomic.LoadInt32(&proxyconn.inRecvBackpressure)
			sendAct := atomic.SwapInt32(&proxyconn.sendActive, 0)
			inSendBP := atomic.LoadInt32(&proxyconn.inSendBackpressure)
			if recvAct > 0 || inRecvBP > 0 {
				atomic.StoreInt32(&proxyconn.pinged, 0)
				// 单向大流量场景（例如纯上传或纯下载）：本端持续收到数据（或处于接收反压中），
				// 但反向通道没有任何业务数据发出时，主动在空闲的反向通道回一个轻量 PONG，
				// 让发送端也能感知“数据有来有回”，避免发送端在拥塞方向盲发 PING 导致超时。
				if sendAct == 0 && inSendBP == 0 && proxyconn.pongflag != nil && proxyconn.pongtime != nil {
					atomic.StoreInt64(proxyconn.pongtime, time.Now().UnixNano())
					atomic.StoreInt32(proxyconn.pongflag, 1)
					proxyconn.wakeSendQ()
				}
				continue
			}

			// 检查心跳超时逻辑
			if atomic.LoadInt32(&proxyconn.pinged) > int32(pingintertimeout) {
				loggo.Info("checkPingActive ping pong timeout %s", proxyconn.Info())
				return errors.New("ping pong timeout")
			}

			// 仅在没有流量跑时发送心跳探活
			atomic.AddInt32(pingflag, 1)
			atomic.AddInt32(&proxyconn.pinged, 1)
			proxyconn.wakeSendQ()
			if showping {
				loggo.Info("ping %s", proxyconn.Info())
			}
		}
	}

	loggo.Info("checkPingActive end %s", proxyconn.Info())
	return nil
}

func checkNeedClose(wg *thread.Group, proxyconn *ProxyConn) error {
	loggo.Info("checkNeedClose start %s", proxyconn.Info())

	// 创建定时器
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	exit := false
	for !exit {
		select {
		// 1. 响应退出信号 (Group Stop)
		case <-wg.Done():
			exit = true
			break // 跳出 select，回到 for 检查条件 !exit

		// 2. 定时检查逻辑
		case <-ticker.C:
			if proxyconn.isNeedClose() {
				loggo.Error("checkNeedClose needclose %s", proxyconn.Info())
				// 遇到错误通常直接返回，不需要走 exit 流程
				return errors.New("needclose")
			}
		}
	}

	loggo.Info("checkNeedClose end %s", proxyconn.Info())

	return nil
}

func processPing(f *ProxyFrame, proxyconn *ProxyConn, pongflag *int32, pongtime *int64) {
	atomic.StoreInt64(pongtime, f.PingFrame.Time)
	atomic.AddInt32(pongflag, 1)
	proxyconn.wakeSendQ()
}

func processPong(f *ProxyFrame, proxyconn *ProxyConn, showping bool) time.Duration {
	now := time.Now().UnixNano()
	elapse := time.Duration(now - f.PongFrame.Time)
	atomic.StoreInt32(&proxyconn.pinged, 0)
	atomic.StoreInt64(&proxyconn.lastSendNano, now)
	if showping {
		loggo.Info("pong %s %s", proxyconn.Info(), elapse.String())
	}
	return elapse
}

func checkSonnyActive(wg *thread.Group, proxyconn *ProxyConn, estimeout int, timeout int) error {
	loggo.Info("checkSonnyActive start %s", proxyconn.Info())

	// 1. 设置整体超时时间
	timeoutTimer := time.NewTimer(time.Second * time.Duration(estimeout))
	defer timeoutTimer.Stop()

	select {
	// 优先响应退出信号
	case <-wg.Done():
		break

	// 整体超时触发
	case <-timeoutTimer.C:
		if !proxyconn.isEstablished() {
			loggo.Error("checkSonnyActive established timeout %s", proxyconn.Info())
			return errors.New("established timeout")
		}
		break
	}

	if timeout <= 0 {
		<-wg.Done()
		loggo.Info("checkSonnyActive end %s", proxyconn.Info())
		return nil
	}

	// 直接创建一个周期为 timeout 的 Ticker
	activedTicker := time.NewTicker(time.Duration(timeout) * time.Second)
	defer activedTicker.Stop()

	exit := false
	for !exit {
		select {
		// 1. 响应退出信号
		case <-wg.Done():
			exit = true
			break

		// 2. 定时触发检查逻辑
		case <-activedTicker.C:
			if atomic.LoadInt32(&proxyconn.actived) == 0 {
				loggo.Error("checkSonnyActive timeout %s", proxyconn.Info())
				return errors.New("conn timeout")
			}
			atomic.StoreInt32(&proxyconn.actived, 0)
		}
	}

	loggo.Info("checkSonnyActive end %s", proxyconn.Info())
	return nil
}

func copySonnyRecv(wg *thread.Group, recvch *msgChannel, proxyConn *ProxyConn, father *ProxyConn) error {
	loggo.Info("copySonnyRecv start %s", proxyConn.Info())

	for {
		ff, ok := recvch.ReadWait()
		if !ok {
			break
		}
		f := queueFrame(ff)
		if f.Type != FRAME_TYPE_DATA {
			loggo.Error("copySonnyRecv type error %s %d", proxyConn.Info(), f.Type)
			return errors.New("conn type error")
		}
		if f.DataFrame.Compress {
			loggo.Error("copySonnyRecv compress error %s %d", proxyConn.Info(), f.Type)
			return errors.New("conn compress error")
		}
		if loggo.IsDebug() {
			if f.DataFrame.Crc != "" && common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
				loggo.Error("copySonnyRecv crc error %s %s %s", proxyConn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc)
				return errors.New("conn crc error")
			}
		}
		f.DataFrame.Id = proxyConn.id
		dataLen := len(f.DataFrame.Data)
		if !proxyConn.acquireSendWindow(wg, int64(dataLen), 1) {
			break
		}
		atomic.AddInt32(&proxyConn.actived, 1)

		dataCrc := f.DataFrame.Crc
		curSent := atomic.AddInt64(&proxyConn.sentBytes, int64(dataLen))
		father.SendDataRaw(ff, curSent <= INTERACTIVE_BYTES_LIMIT)

		loggo.Debug("copySonnyRecv %s %d %s %p", proxyConn.id, dataLen, dataCrc, f)
	}
	if !isExit(wg) {
		for !isExit(wg) {
			ch := proxyConn.pickSendCh()
			if ch == nil || len(ch.Ch()) == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		loggo.Info("copySonnyRecv end (recv closed) %s", proxyConn.Info())
		return errors.New("sonny recv closed")
	}
	loggo.Info("copySonnyRecv end %s", proxyConn.Info())
	return nil
}

func closeRemoteConn(proxyConn *ProxyConn, father *ProxyConn) {
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_CLOSE
	f.CloseFrame = &CloseFrame{}
	f.CloseFrame.Id = proxyConn.id

	father.SendFrame(f)
	loggo.Info("closeConn %s", proxyConn.id)
}

type StateThreadNum struct {
	RecvThread          int32
	SendThread          int32
	InputerSonnyThread  int32
	OutputerSonnyThread int32
}

type State struct {
	MainRecvNum  int32
	MainSendNum  int32
	MainRecvSize int64
	MainSendSize int64
	RecvNum      int32
	SendNum      int32
	RecvSize     int64
	SendSize     int64

	RecvCompSaveSize int64
	SendCompSaveSize int64
}

var gStateThreadNum StateThreadNum
var gState State

// procStart is the process-local start time, used for status uptime.
var procStart = time.Now()

// goSafe launches a task on a long-lived root/service group (Client,
// Server, or the groups owning Inputer/Outputer listeners). Such tasks MUST
// NOT affect the group's lifetime:
//
//   - gohome's thread.Group exits the whole group — and all of its children —
//     whenever a task returns a non-nil error, so a per-connection handler
//     returning one would tear down the entire proxy because of a single
//     malformed request. The func() signature makes that mistake
//     compile-time: a func() error cannot be passed as f.
//   - A panic in one connection must not crash the process either: gohome's
//     CrashLog re-panics after logging. Recover here (full stack logged at
//     ERROR) so the group keeps serving, mirroring net/http's per-connection
//     panic isolation.
//
// Pipe/conn-scoped goroutines keep using Group.Go with func() error on their
// own child groups, where returning an error is the intended teardown signal
// for exactly that pipe or connection.
func goSafe(g *thread.Group, name string, f func()) {
	g.Go(name, func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				loggo.Error("goSafe %s recovered from panic: %v\n%s", name, r, debug.Stack())
				err = nil // swallow: keep the long-lived group alive
			}
		}()
		f()
		return nil
	})
}

// snapshotState returns a consistent (field-wise atomic) copy of the global
// counters for the status endpoint.
func snapshotState() State {
	return State{
		MainRecvNum:      atomic.LoadInt32(&gState.MainRecvNum),
		MainSendNum:      atomic.LoadInt32(&gState.MainSendNum),
		MainRecvSize:     atomic.LoadInt64(&gState.MainRecvSize),
		MainSendSize:     atomic.LoadInt64(&gState.MainSendSize),
		RecvNum:          atomic.LoadInt32(&gState.RecvNum),
		SendNum:          atomic.LoadInt32(&gState.SendNum),
		RecvSize:         atomic.LoadInt64(&gState.RecvSize),
		SendSize:         atomic.LoadInt64(&gState.SendSize),
		RecvCompSaveSize: atomic.LoadInt64(&gState.RecvCompSaveSize),
		SendCompSaveSize: atomic.LoadInt64(&gState.SendCompSaveSize),
	}
}

// snapshotThreadNum returns an atomic copy of the live goroutine counters.
func snapshotThreadNum() StateThreadNum {
	return StateThreadNum{
		RecvThread:          atomic.LoadInt32(&gStateThreadNum.RecvThread),
		SendThread:          atomic.LoadInt32(&gStateThreadNum.SendThread),
		InputerSonnyThread:  atomic.LoadInt32(&gStateThreadNum.InputerSonnyThread),
		OutputerSonnyThread: atomic.LoadInt32(&gStateThreadNum.OutputerSonnyThread),
	}
}

func showState(wg *thread.Group) {
	loggo.Info("showState start ")

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	exit := false
	for !exit {
		select {
		case <-wg.Done():
			exit = true
			break

		case <-ticker.C:
			// Snapshot first, then reset field-by-field with atomics: a plain
			// struct write (and StructToTable's plain reads) raced the atomic
			// adds every wire worker makes to these counters.
			snapThreads := snapshotThreadNum()
			snap := snapshotState()
			loggo.Info("showState\n%s\n%s", common.StructToTable(&snapThreads), common.StructToTable(&snap))
			loggo.Info("Goroutine Num: %d", runtime.NumGoroutine())
			atomic.StoreInt32(&gState.MainRecvNum, 0)
			atomic.StoreInt64(&gState.MainRecvSize, 0)
			atomic.StoreInt32(&gState.MainSendNum, 0)
			atomic.StoreInt64(&gState.MainSendSize, 0)
			atomic.StoreInt32(&gState.RecvNum, 0)
			atomic.StoreInt64(&gState.RecvSize, 0)
			atomic.StoreInt32(&gState.SendNum, 0)
			atomic.StoreInt64(&gState.SendSize, 0)
			atomic.StoreInt64(&gState.RecvCompSaveSize, 0)
			atomic.StoreInt64(&gState.SendCompSaveSize, 0)
		}
	}
	loggo.Info("showState end")
}

func setCongestion(c network.Conn, config *Config) {
	if c.Name() == "rudp" {
		cf := c.(*network.RudpConn).GetConfig()
		cf.Congestion = config.Congestion
		c.(*network.RudpConn).SetConfig(cf)
	} else if c.Name() == "ricmp" {
		cf := c.(*network.RicmpConn).GetConfig()
		cf.Congestion = config.Congestion
		c.(*network.RicmpConn).SetConfig(cf)
	} else if c.Name() == "rdns" {
		cf := c.(*network.RdnsConn).GetConfig()
		cf.Congestion = config.Congestion
		c.(*network.RdnsConn).SetConfig(cf)
	}
}

// setKcpConfig applies the optional KCP forward-error-correction shard
// configuration before Listen/Dial. Defaults (0,0) leave FEC disabled.
// FEC wire framing is not backward compatible, so client and server must
// configure identical shard counts.
func setKcpConfig(c network.Conn, config *Config) {
	if c.Name() != "kcp" {
		return
	}
	c.(*network.KcpConn).SetConfig(&network.KcpConfig{
		DataShards:   config.KcpFecDataShards,
		ParityShards: config.KcpFecParityShards,
	})
}

// setRdnsConfig applies the RDNS domain and tunnel configuration before Listen/Dial.
func setRdnsConfig(c network.Conn, config *Config) {
	if c.Name() != "rdns" {
		return
	}
	rc := c.(*network.RdnsConn)
	cf := rc.GetConfig()
	if config.RdnsDomain != "" {
		cf.Domain = config.RdnsDomain
	}
	if config.RdnsUpstream != "" {
		cf.Upstream = config.RdnsUpstream
	}
	if config.RdnsPsk != "" {
		cf.PSK = config.RdnsPsk
	}
	if config.RdnsQueryType != "" {
		cf.QueryType = config.RdnsQueryType
	}
	rc.SetConfig(cf)
}

// setUnderlayTuning applies all spp-side underlay tuning (congestion
// control, KCP FEC, RDNS) to a freshly constructed conn.
func setUnderlayTuning(c network.Conn, config *Config) {
	setCongestion(c, config)
	setKcpConfig(c, config)
	setRdnsConfig(c, config)
}
