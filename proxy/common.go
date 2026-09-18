package proxy

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
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
	MaxMsgSize                int          // 消息最大长度
	MainBuffer                int          // 主通道buffer最大长度
	ConnBuffer                int          // 每个conn buffer最大长度
	EstablishedTimeout        int          // 主通道登录超时
	PingInter                 int          // 主通道ping间隔
	PingTimeoutInter          int          // 主通道ping超时间隔
	ConnTimeout               int          // 每个conn的不活跃超时时间
	ConnectTimeout            int          // 每个conn的连接超时
	Key                       string       // 鉴权密钥，必须显式配置，无弱默认
	Encrypt                   string       // 加密密钥，空表示关闭加密，无弱默认
	EncryptType               ENCRYPT_TYPE // 加密算法，默认 ChaCha20-Poly1305（整帧 AEAD）
	Compress                  int          // 压缩阈值，0 表示关闭
	CompressType              COMPRESS_TYPE // 压缩算法，默认 ZSTD
	ShowPing                  bool         // 是否显示ping
	Username                  string       // 登录用户名
	Password                  string       // 登录密码
	MaxClient                 int          // 最大客户端数目
	MaxSonny                  int          // 最大连接数目
	MainWriteChannelTimeoutMs int          // 主通道转发消息超时
	Congestion                string       // 拥塞算法
	ProbeInter                int          // 多通道测速间隔（秒）
	ProbeSize                 int          // 测速 payload 字节数
}

func DefaultConfig() *Config {
	return &Config{
		MaxMsgSize:                1024 * 1024,
		MainBuffer:                64,
		ConnBuffer:                16,
		EstablishedTimeout:        30,
		PingInter:                 1,
		PingTimeoutInter:          30,
		ConnTimeout:               60,
		ConnectTimeout:            10,
		Key:                       "",
		Encrypt:                   "",
		EncryptType:               EncryptChaCha20,
		Compress:                  128,
		CompressType:              CompressZstd,
		ShowPing:                  false,
		Username:                  "",
		Password:                  "",
		MaxClient:                 10000,
		MaxSonny:                  10240,
		MainWriteChannelTimeoutMs: 1000,
		Congestion:                "bb",
		ProbeInter:                5,
		ProbeSize:                 32 * 1024,
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

// frameRouter selects an underlay pipe for outbound session frames.
type frameRouter interface {
	routeFrame(f *ProxyFrame, interactive bool)
}

type ProxyConn struct {
	conn        network.Conn
	established int32 // atomic bool
	// Main channel (client/server): single priority queues matching one pipe.
	sendq *prioQueue
	recvq *prioQueue
	// Sonny (per-proxy TCP/UDP): plain FIFO is enough.
	sendch *msgChannel
	recvch *msgChannel
	actived           int32
	pinged            int32
	sentBytes         int64
	id                string
	needclose         int32 // atomic bool
	compressType      int32 // COMPRESS_TYPE
	encryptType       int32 // ENCRYPT_TYPE
	compressThreshold int32
	encryptKey        string // guarded by mu
	aead              cipher.AEAD
	noncePrefix       [4]byte
	nonceCounter      uint64
	mu                sync.RWMutex
	isClosed          bool
	closeOnce         sync.Once
	// If set, SendFrame/SendData delegate to multi-pipe selection.
	router frameRouter
}

func (p *ProxyConn) closeConn() {
	p.closeOnce.Do(func() {
		if p.conn != nil {
			p.conn.Close()
		}
	})
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
}

func (p *ProxyConn) isNeedClose() bool {
	return atomic.LoadInt32(&p.needclose) != 0
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosed {
		return
	}
	p.isClosed = true
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
}

func (p *ProxyConn) pickSendQ() *prioQueue {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isClosed {
		return nil
	}
	return p.sendq
}

func (p *ProxyConn) pickRecvQ() *prioQueue {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isClosed {
		return nil
	}
	return p.recvq
}

func (p *ProxyConn) pickSendCh() *msgChannel {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isClosed {
		return nil
	}
	return p.sendch
}

func (p *ProxyConn) pickRecvCh() *msgChannel {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isClosed {
		return nil
	}
	return p.recvch
}

// SendFrame enqueues a frame on the main priority queue (control jumps ahead).
func (p *ProxyConn) SendFrame(f *ProxyFrame) {
	if p.router != nil {
		p.router.routeFrame(f, false)
		return
	}
	if q := p.pickSendQ(); q != nil {
		prio := prioControl
		if f.Type == FRAME_TYPE_DATA || f.Type == FRAME_TYPE_PING || f.Type == FRAME_TYPE_PONG || f.Type == FRAME_TYPE_SPEEDTEST {
			prio = prioBulk
		}
		q.Push(f, prio)
		return
	}
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(f)
	}
}

// SendData enqueues DATA: interactive can jump ahead of bulk on the same queue.
func (p *ProxyConn) SendData(f *ProxyFrame, isInteractive bool) {
	if p.router != nil {
		p.router.routeFrame(f, isInteractive)
		return
	}
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
func (p *ProxyConn) RecvFrame(f *ProxyFrame) {
	if q := p.pickRecvQ(); q != nil {
		prio := prioControl
		if f.Type == FRAME_TYPE_DATA || f.Type == FRAME_TYPE_PING || f.Type == FRAME_TYPE_PONG || f.Type == FRAME_TYPE_SPEEDTEST {
			prio = prioBulk
		}
		q.Push(f, prio)
		return
	}
	if ch := p.pickRecvCh(); ch != nil {
		ch.Write(f)
	}
}

// SendSonnyData safely writes a data frame to sonny's sendch with timeout.
func (p *ProxyConn) SendSonnyData(f *ProxyFrame, timeoutMs int) bool {
	ch := p.pickSendCh()
	if ch == nil {
		return true
	}
	return ch.WriteTimeout(f, timeoutMs)
}

// SendSonnyClose safely writes a close frame to sonny's sendch.
func (p *ProxyConn) SendSonnyClose(f *ProxyFrame) {
	if ch := p.pickSendCh(); ch != nil {
		ch.Write(f)
	}
}

// RecvSonnyData safely writes an incoming frame from sonny's socket to sonny's recvch.
func (p *ProxyConn) RecvSonnyData(f *ProxyFrame) {
	if ch := p.pickRecvCh(); ch != nil {
		ch.Write(f)
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

func recvFrom(wg *thread.Group, proxyconn *ProxyConn, conn network.Conn, maxmsgsize int) error {

	atomic.AddInt32(&gStateThreadNum.RecvThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.RecvThread, -1)

	loggo.Info("recvFrom start %s", conn.Info())
	bs := make([]byte, 4)
	ds := make([]byte, maxmsgsize+MAX_PROTO_PACK_SIZE+aeadOverhead)

	for !isExit(wg) {
		if loggo.IsDebug() {
			loggo.Debug("recvFrom start ReadFull len %s", conn.Info())
		}
		_, err := io.ReadFull(conn, bs)
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
		_, err = io.ReadFull(conn, ds[0:msglen])
		if err != nil {
			loggo.Info("recvFrom ReadFull fail: %s %s", conn.Info(), err.Error())
			return err
		}

		plain, err := proxyconn.openWire(ds[0:msglen])
		if err != nil {
			loggo.Error("recvFrom openWire fail: %s %s", conn.Info(), err.Error())
			return err
		}

		f, err := UnmarshalSrpFrame(plain, proxyconn.getCodec())
		if err != nil {
			loggo.Error("recvFrom UnmarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
			loggo.Debug("recvFrom %s %s", conn.Info(), f.Type.String())
			if f.Type == FRAME_TYPE_DATA {
				if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
					loggo.Error("recvFrom crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
					return errors.New("conn crc error")
				}
			}
		}

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start Write %s", conn.Info())
		}

		proxyconn.RecvFrame(f)

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
	bs := make([]byte, 4)

	for !isExit(wg) {
		var f *ProxyFrame
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
			f.PongFrame.Time = *pongtime
		} else {
			v, closed, ok := sendq.PopWait(time.Second)
			if !ok {
				if closed {
					break
				}
				continue
			}
			f = v.(*ProxyFrame)
		}
		if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
			loggo.Debug("sendTo %s %s", conn.Info(), f.Type.String())
			if f.Type == FRAME_TYPE_DATA {
				if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
					loggo.Error("sendTo crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
					return errors.New("conn crc error")
				}
			}
		}

		mb, err := MarshalSrpFrame(f, proxyconn.getCodec())
		if err != nil {
			loggo.Error("sendTo MarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
			return err
		}

		mb, err = proxyconn.sealWire(mb)
		if err != nil {
			loggo.Error("sendTo sealWire fail: %s %s", conn.Info(), err.Error())
			return err
		}

		msglen := uint32(len(mb))
		maxWire := uint32(maxmsgsize) + MAX_PROTO_PACK_SIZE + aeadOverhead
		if msglen > maxWire || msglen == 0 {
			loggo.Error("sendTo len fail: %s %d", conn.Info(), msglen)
			return errors.New("msg len fail " + strconv.Itoa(int(msglen)))
		}

		if loggo.IsDebug() {
			loggo.Debug("sendTo start Write len %s", conn.Info())
		}
		binary.LittleEndian.PutUint32(bs, msglen)
		_, err = conn.Write(bs)
		if err != nil {
			loggo.Info("sendTo Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if loggo.IsDebug() {
			loggo.Debug("sendTo start Write body %s %d", conn.Info(), msglen)
		}
		n, err := conn.Write(mb)
		if err != nil {
			loggo.Info("sendTo Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if n != len(mb) {
			loggo.Error("sendTo Write len fail: %s %d %d", conn.Info(), n, len(mb))
			return errors.New("len error")
		}

		atomic.AddInt32(&gState.MainSendNum, 1)
		atomic.AddInt64(&gState.MainSendSize, int64(msglen)+4)
	}
	loggo.Info("sendTo end %s", conn.Info())
	return nil
}

const (
	MAX_INDEX               = 1024
	MAX_CHUNK_SIZE          = 32 * 1024  // Chunk frames to 32KB to allow fair interleaving and prevent head-of-line blocking
	INTERACTIVE_BYTES_LIMIT = 256 * 1024 // Initial bytes per sonny at prioInter (jumps ahead of bulk)
)

func recvFromSonny(wg *thread.Group, proxyconn *ProxyConn, conn network.Conn, maxmsgsize int) error {
	loggo.Info("recvFromSonny start %s", conn.Info())
	bufSize := maxmsgsize
	if bufSize > MAX_CHUNK_SIZE {
		bufSize = MAX_CHUNK_SIZE
	}
	ds := make([]byte, bufSize)

	index := int32(0)
	for !isExit(wg) {
		msglen, err := conn.Read(ds)
		if err != nil {
			loggo.Info("recvFromSonny Read fail: %s %s", conn.Info(), err.Error())
			if err == io.EOF {
				return nil
			}
			return err
		}

		if msglen <= 0 {
			loggo.Error("recvFromSonny len error: %s %d", conn.Info(), msglen)
			return errors.New("len error " + strconv.Itoa(msglen))
		}

		f := &ProxyFrame{}
		f.Type = FRAME_TYPE_DATA
		f.DataFrame = &DataFrame{}
		f.DataFrame.Data = make([]byte, msglen)
		copy(f.DataFrame.Data, ds[0:msglen])
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

		proxyconn.RecvSonnyData(f)
	}
	loggo.Info("recvFromSonny end %s", conn.Info())
	return nil
}

func sendToSonny(wg *thread.Group, sendch *msgChannel, conn network.Conn, maxmsgsize int) error {
	loggo.Info("sendToSonny start %s", conn.Info())
	index := int32(0)
	for !isExit(wg) {
		var ff interface{}
		select {
		case ff = <-sendch.Ch():
		case <-sendch.Done():
			ff = nil
		}
		if ff == nil {
			break
		}
		f := ff.(*ProxyFrame)
		if f.Type == FRAME_TYPE_CLOSE {
			loggo.Info("sendToSonny close by remote: %s", conn.Info())
			return errors.New("close by remote")
		}
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
			if f.DataFrame.Crc != common.GetCrc32(f.DataFrame.Data) {
				loggo.Error("sendToSonny crc error: %s %d %s %s", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Crc, common.GetCrc32(f.DataFrame.Data))
				return errors.New("crc error")
			}
		}

		index++
		index = index % MAX_INDEX
		if f.DataFrame.Index != index {
			loggo.Error("sendToSonny index error: %s %d %d %d", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Index, index)
			return errors.New("index error")
		}

		n, err := conn.Write(f.DataFrame.Data)
		if err != nil {
			loggo.Info("sendToSonny Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if n != len(f.DataFrame.Data) {
			loggo.Error("sendToSonny Write len fail: %s %d %d", conn.Info(), n, len(f.DataFrame.Data))
			return errors.New("len error")
		}

		if loggo.IsDebug() {
			loggo.Debug("sendToSonny %s %d %s %d", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Crc, f.DataFrame.Index)
		}

		atomic.AddInt32(&gState.SendNum, 1)
		atomic.AddInt64(&gState.SendSize, int64(len(f.DataFrame.Data)))
	}
	loggo.Info("sendToSonny end %s", conn.Info())
	return nil
}

func checkPingActive(wg *thread.Group, proxyconn *ProxyConn,
	estimeout int, pinginter int, pingintertimeout int, showping bool, pingflag *int32) error {

	loggo.Info("checkPingActive start %s", proxyconn.conn.Info())

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
			loggo.Info("checkPingActive established timeout %s", proxyconn.conn.Info())
			return errors.New("established timeout")
		}
		break
	}

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
			// 检查心跳超时逻辑
			if atomic.LoadInt32(&proxyconn.pinged) > int32(pingintertimeout) {
				loggo.Info("checkPingActive ping pong timeout %s", proxyconn.conn.Info())
				return errors.New("ping pong timeout")
			}

			// 发送心跳逻辑
			atomic.AddInt32(pingflag, 1)
			atomic.AddInt32(&proxyconn.pinged, 1)
			if showping {
				loggo.Info("ping %s", proxyconn.conn.Info())
			}
		}
	}

	loggo.Info("checkPingActive end %s", proxyconn.conn.Info())
	return nil
}

func checkNeedClose(wg *thread.Group, proxyconn *ProxyConn) error {
	loggo.Info("checkNeedClose start %s", proxyconn.conn.Info())

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
				loggo.Error("checkNeedClose needclose %s", proxyconn.conn.Info())
				// 遇到错误通常直接返回，不需要走 exit 流程
				return errors.New("needclose")
			}
		}
	}

	loggo.Info("checkNeedClose end %s", proxyconn.conn.Info())

	return nil
}

func processPing(f *ProxyFrame, proxyconn *ProxyConn, pongflag *int32, pongtime *int64) {
	atomic.AddInt32(pongflag, 1)
	*pongtime = f.PingFrame.Time
}

func processPong(f *ProxyFrame, proxyconn *ProxyConn, showping bool) time.Duration {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	atomic.StoreInt32(&proxyconn.pinged, 0)
	if showping {
		loggo.Info("pong %s %s", proxyconn.conn.Info(), elapse.String())
	}
	return elapse
}

func checkSonnyActive(wg *thread.Group, proxyconn *ProxyConn, estimeout int, timeout int) error {
	loggo.Info("checkSonnyActive start %s", proxyconn.conn.Info())

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
			loggo.Error("checkSonnyActive established timeout %s", proxyconn.conn.Info())
			return errors.New("established timeout")
		}
		break
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

		// 2. 定时触发 Ping 逻辑
		case <-activedTicker.C:
			if atomic.LoadInt32(&proxyconn.actived) == 0 {
				loggo.Error("checkSonnyActive timeout %s", proxyconn.conn.Info())
				return errors.New("conn timeout")
			}
			atomic.StoreInt32(&proxyconn.actived, 0)
		}
	}

	loggo.Info("checkSonnyActive end %s", proxyconn.conn.Info())
	return nil
}

func copySonnyRecv(wg *thread.Group, recvch *msgChannel, proxyConn *ProxyConn, father *ProxyConn) error {
	loggo.Info("copySonnyRecv start %s", proxyConn.conn.Info())

	for !isExit(wg) {
		var ff interface{}
		select {
		case ff = <-recvch.Ch():
		case <-recvch.Done():
			ff = nil
		}
		if ff == nil {
			break
		}
		f := ff.(*ProxyFrame)
		if f.Type != FRAME_TYPE_DATA {
			loggo.Error("copySonnyRecv type error %s %d", proxyConn.conn.Info(), f.Type)
			return errors.New("conn type error")
		}
		if f.DataFrame.Compress {
			loggo.Error("copySonnyRecv compress error %s %d", proxyConn.conn.Info(), f.Type)
			return errors.New("conn compress error")
		}
		if loggo.IsDebug() {
			if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
				loggo.Error("copySonnyRecv crc error %s %s %s", proxyConn.conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc)
				return errors.New("conn crc error")
			}
		}
		f.DataFrame.Id = proxyConn.id
		atomic.AddInt32(&proxyConn.actived, 1)

		dataLen := len(f.DataFrame.Data)
		dataCrc := f.DataFrame.Crc
		curSent := atomic.AddInt64(&proxyConn.sentBytes, int64(dataLen))
		father.SendData(f, curSent <= INTERACTIVE_BYTES_LIMIT)

		loggo.Debug("copySonnyRecv %s %d %s %p", proxyConn.id, dataLen, dataCrc, f)
	}
	loggo.Info("copySonnyRecv end %s", proxyConn.conn.Info())
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

func showState(wg *thread.Group) error {
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
			loggo.Info("showState\n%s\n%s", common.StructToTable(&gStateThreadNum), common.StructToTable(&gState))
			loggo.Info("Goroutine Num: %d", runtime.NumGoroutine())
			gState = State{}
		}
	}
	loggo.Info("showState end")
	return nil
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
	}
}
