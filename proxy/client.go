package proxy

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

// ServerConn is the logical client↔server session. Business Inputer/Outputer
// attach here; underlay traffic is load-balanced across hub pipes.
type ServerConn struct {
	ProxyConn
	outputs   []*Outputer
	inputs    []*Inputer
	svcMu     sync.Mutex // guards inputs/outputs vs process* readers
	hub       *channelHub
	sessionID uint64
}

func (s *ServerConn) closeServices() {
	s.svcMu.Lock()
	outs := s.outputs
	ins := s.inputs
	s.outputs = nil
	s.inputs = nil
	s.svcMu.Unlock()
	for _, o := range outs {
		o.Close()
	}
	for _, i := range ins {
		i.Close()
	}
}

func (s *ServerConn) serviceSnapshot() (inputs []*Inputer, outputs []*Outputer) {
	s.svcMu.Lock()
	defer s.svcMu.Unlock()
	return s.inputs, s.outputs
}

func (s *ServerConn) appendInput(in *Inputer) {
	s.svcMu.Lock()
	s.inputs = append(s.inputs, in)
	s.svcMu.Unlock()
}

func (s *ServerConn) appendOutput(out *Outputer) {
	s.svcMu.Lock()
	s.outputs = append(s.outputs, out)
	s.svcMu.Unlock()
}

type Client struct {
	config       *Config
	servers      []string
	serverprotos []string
	name         string
	clienttype   CLIENT_TYPE
	proxyproto   []PROXY_PROTO
	fromaddr     []string
	toaddr       []string
	serverconn   *ServerConn
	connMu       sync.Mutex
	loginFlight  int32 // 1 while primary LOGIN in flight
	pipeLive     []int32
	wg           *thread.Group
}

// NewClient dials one or more underlay (proto, addr) pairs that share one logical session.
// len(servers) must equal len(serverprotos), or be 1 (same addr for every proto).
func NewClient(config *Config, serverprotos []string, servers []string, name string, clienttypestr string, proxyprotostr []string, fromaddr []string, toaddr []string) (*Client, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	if len(serverprotos) == 0 {
		return nil, errors.New("no server proto")
	}
	if len(servers) == 0 {
		return nil, errors.New("no server addr")
	}
	if len(servers) == 1 && len(serverprotos) > 1 {
		expanded := make([]string, len(serverprotos))
		for i := range expanded {
			expanded[i] = servers[0]
		}
		servers = expanded
	}
	if len(servers) != len(serverprotos) {
		return nil, errors.New("server proto/addr len mismatch")
	}

	for _, sp := range serverprotos {
		cn, err := network.NewConn(sp)
		if cn == nil {
			return nil, err
		}
		setCongestion(cn, config)
		cn.Close()
	}

	clienttypestr = strings.ToUpper(clienttypestr)
	clienttype, ok := CLIENT_TYPE_value[clienttypestr]
	if !ok {
		return nil, errors.New("no CLIENT_TYPE " + clienttypestr)
	}

	var proxyproto []PROXY_PROTO
	for i := range proxyprotostr {
		p, ok := PROXY_PROTO_value[strings.ToUpper(proxyprotostr[i])]
		if !ok {
			return nil, errors.New("no PROXY_PROTO " + proxyprotostr[i])
		}
		proxyproto = append(proxyproto, PROXY_PROTO(p))
	}

	wg := thread.NewGroup("Client"+" "+clienttypestr, nil, nil)

	c := &Client{
		config:       config,
		servers:      servers,
		serverprotos: serverprotos,
		name:         name,
		clienttype:   CLIENT_TYPE(clienttype),
		proxyproto:   proxyproto,
		fromaddr:     fromaddr,
		toaddr:       toaddr,
		pipeLive:     make([]int32, len(serverprotos)),
		wg:           wg,
	}

	wg.Go("Client state"+" "+clienttypestr, func() error {
		return showState(wg)
	})

	wg.Go("Client connect", func() error {
		return c.connect()
	})

	return c, nil
}

func (c *Client) Close() {
	c.wg.Stop()
	c.wg.Wait()
}

func (c *Client) connect() error {
	loggo.Info("connect start protos=%v addrs=%v", c.serverprotos, c.servers)

	checkTicker := time.NewTicker(time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case <-c.wg.Done():
			loggo.Info("connect end")
			return nil
		case <-checkTicker.C:
			for i := range c.serverprotos {
				if atomic.LoadInt32(&c.pipeLive[i]) != 0 {
					continue
				}
				idx := i
				proto := c.serverprotos[idx]
				addr := c.servers[idx]
				dialer, err := network.NewConn(proto)
				if dialer == nil {
					loggo.Error("connect NewConn fail: %s %s %v", proto, addr, err)
					continue
				}
				setCongestion(dialer, c.config)
				targetconn, err := dialWithTimeout(dialer, addr, c.config.ConnectTimeout)
				if err != nil {
					dialer.Close()
					loggo.Error("connect Dial fail: %s %s %s", proto, addr, err.Error())
					continue
				}
				atomic.StoreInt32(&c.pipeLive[idx], 1)
				c.wg.Go("Client usePipe "+proto+" "+addr, func() error {
					defer atomic.StoreInt32(&c.pipeLive[idx], 0)
					return c.usePipe(idx, proto, addr, targetconn)
				})
			}
		}
	}
}

func (c *Client) usePipe(index int, proto, addr string, conn network.Conn) error {
	loggo.Info("usePipe start %s %s", proto, addr)

	pipe := &mainPipe{
		ProxyConn: ProxyConn{conn: conn},
		proto:     proto,
		addr:      addr,
	}
	sendq := newPrioQueue(c.config.MainBuffer)
	recvq := newPrioQueue(c.config.MainBuffer)
	pipe.sendq = sendq
	pipe.recvq = recvq
	pipe.setCodec(defaultFrameCodec(c.config))

	wg := thread.NewGroup("Client usePipe "+proto+" "+addr, c.wg, func() {
		loggo.Info("pipe group exit %s %s", proto, addr)
		if pipe.hub != nil {
			pipe.hub.remove(pipe)
		}
		pipe.conn.Close()
		pipe.CloseChannels()
		c.onPipeGone()
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Client recvFrom "+proto, func() error {
		return recvFrom(wg, &pipe.ProxyConn, pipe.conn, c.config.MaxMsgSize)
	})
	wg.Go("Client sendTo "+proto, func() error {
		return sendTo(wg, sendq, &pipe.ProxyConn, pipe.conn, c.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})
	// kcp/quic Accept only completes after the client writes application data.
	// Without a nudge, server Accept and client waiting for AUTH_CHALLENGE deadlock.
	atomic.StoreInt32(&pingflag, 1)
	wg.Go("Client checkPingActive "+proto, func() error {
		authTimeout := c.config.AuthTimeout
		if authTimeout <= 0 {
			authTimeout = c.config.EstablishedTimeout
		}
		err := checkPingActive(wg, &pipe.ProxyConn, authTimeout, c.config.PingInter, c.config.PingTimeoutInter, c.config.ShowPing, &pingflag)
		if err != nil {
			pipe.markGray(err.Error())
		}
		return err
	})
	wg.Go("Client checkNeedClose "+proto, func() error {
		return checkNeedClose(wg, &pipe.ProxyConn)
	})
	wg.Go("Client processPipe "+proto, func() error {
		return c.processPipe(wg, recvq, pipe, &pongflag, &pongtime)
	})
	wg.Go("Client probePipe "+proto, func() error {
		return c.probePipe(wg, pipe)
	})

	wg.Wait()
	loggo.Info("usePipe close %s %s", proto, addr)
	return nil
}

func (c *Client) onPipeGone() {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	sess := c.serverconn
	if sess == nil || sess.hub == nil {
		return
	}
	if sess.hub.liveCount() > 0 {
		return
	}
	loggo.Info("all pipes gone, tear down session")
	sess.closeServices()
	sess.CloseChannels()
	sess.setEstablished(false)
	c.serverconn = nil
	atomic.StoreInt32(&c.loginFlight, 0)
}

func (c *Client) ensureSession() *ServerConn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.serverconn != nil {
		return c.serverconn
	}
	hub := newChannelHub(c.config)
	sess := &ServerConn{
		ProxyConn: ProxyConn{
			recvq:  newPrioQueue(c.config.MainBuffer),
			router: hub,
		},
		hub: hub,
	}
	sess.setCodec(defaultFrameCodec(c.config))
	c.serverconn = sess

	c.wg.Go("Client processSession", func() error {
		return c.processSession(c.wg, sess)
	})
	return sess
}

func (c *Client) processPipe(wg *thread.Group, recvq *prioQueue, pipe *mainPipe, pongflag *int32, pongtime *int64) error {
	loggo.Info("processPipe start %s %s", pipe.proto, pipe.addr)

	for !isExit(wg) {
		v, closed, ok := recvq.PopWait(time.Second)
		if !ok {
			if closed {
				break
			}
			continue
		}
		f := v.(*ProxyFrame)

		switch f.Type {
		case FRAME_TYPE_AUTH_CHALLENGE:
			c.onAuthChallenge(pipe, f.AuthChallengeFrame.Challenge)

		case FRAME_TYPE_LOGINRSP:
			c.processLoginRsp(f, pipe)

		case FRAME_TYPE_CHANNEL_JOIN_RSP:
			c.processJoinRsp(f, pipe)

		case FRAME_TYPE_PING:
			processPing(f, &pipe.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			rtt := processPong(f, &pipe.ProxyConn, c.config.ShowPing)
			pipe.noteRTT(rtt)

		case FRAME_TYPE_SPEEDTEST:
			c.processSpeedTest(f, pipe)

		case FRAME_TYPE_DATA, FRAME_TYPE_OPEN, FRAME_TYPE_OPENRSP, FRAME_TYPE_CLOSE:
			sess := c.currentSession()
			if sess != nil {
				sess.RecvFrame(f)
			}

		default:
			loggo.Error("processPipe unexpected %s on %s", f.Type.String(), pipe.proto)
		}
	}
	loggo.Info("processPipe end %s %s", pipe.proto, pipe.addr)
	return nil
}

func (c *Client) currentSession() *ServerConn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.serverconn
}

func (c *Client) onAuthChallenge(pipe *mainPipe, challenge []byte) {
	// Wait briefly if another pipe is logging in.
	deadline := time.Now().Add(time.Duration(c.config.EstablishedTimeout) * time.Second)
	for {
		c.connMu.Lock()
		sess := c.serverconn
		established := sess != nil && sess.isEstablished()
		sid := uint64(0)
		if sess != nil {
			sid = sess.sessionID
		}
		flight := atomic.LoadInt32(&c.loginFlight)
		c.connMu.Unlock()

		if established && sid != 0 {
			c.sendChannelJoin(pipe, sid, challenge)
			return
		}
		if flight == 0 && atomic.CompareAndSwapInt32(&c.loginFlight, 0, 1) {
			c.loginWithChallenge(pipe, challenge)
			return
		}
		if time.Now().After(deadline) {
			loggo.Error("onAuthChallenge timeout waiting for session on %s", pipe.proto)
			pipe.setNeedClose()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *Client) sendChannelJoin(pipe *mainPipe, sessionID uint64, challenge []byte) {
	f := &ProxyFrame{
		Type: FRAME_TYPE_CHANNEL_JOIN,
		ChannelJoinFrame: &ChannelJoinFrame{
			SessionId: sessionID,
			AuthProof: computeAuthProof(c.config.Key, challenge),
		},
	}
	pipe.SendFrame(f)
	loggo.Info("channel join %s %s session=%d", pipe.proto, pipe.addr, sessionID)
}

func (c *Client) buildLoginServices() []*LoginService {
	n := len(c.proxyproto)
	out := make([]*LoginService, 0, n)
	for i := 0; i < n; i++ {
		svc := &LoginService{Proxyproto: c.proxyproto[i]}
		if i < len(c.fromaddr) {
			svc.Fromaddr = c.fromaddr[i]
		}
		if i < len(c.toaddr) {
			svc.Toaddr = c.toaddr[i]
		}
		out = append(out, svc)
	}
	return out
}

func (c *Client) loginWithChallenge(pipe *mainPipe, challenge []byte) {
	codec := pipe.getCodec()
	services := c.buildLoginServices()

	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_LOGIN
	f.LoginFrame = &LoginFrame{}
	f.LoginFrame.Clienttype = c.clienttype
	f.LoginFrame.Name = c.name
	f.LoginFrame.AuthProof = computeAuthProof(c.config.Key, challenge)
	f.LoginFrame.CompressType = codec.CompressType
	f.LoginFrame.EncryptType = codec.EncryptType
	f.LoginFrame.Services = services
	if len(services) > 0 {
		f.LoginFrame.Proxyproto = services[0].Proxyproto
		f.LoginFrame.Fromaddr = services[0].Fromaddr
		f.LoginFrame.Toaddr = services[0].Toaddr
	}

	pipe.SendFrame(f)
	loggo.Info("start login via %s %s name=%s services=%d (hmac)",
		pipe.proto, pipe.addr, f.LoginFrame.Name, len(services))
}

func (c *Client) processLoginRsp(f *ProxyFrame, pipe *mainPipe) {
	if !f.LoginRspFrame.Ret {
		atomic.StoreInt32(&c.loginFlight, 0)
		pipe.setNeedClose()
		loggo.Error("processLoginRsp fail %s %s", pipe.proto, f.LoginRspFrame.Msg)
		return
	}

	sess := c.ensureSession()

	codec := pipe.getCodec()
	if f.LoginRspFrame.CompressType != CompressUnspecified {
		codec.CompressType = f.LoginRspFrame.CompressType
	}
	if f.LoginRspFrame.EncryptType != EncryptUnspecified {
		codec.EncryptType = f.LoginRspFrame.EncryptType
	}
	pipe.setCodec(codec)
	sess.setCodec(codec)

	c.connMu.Lock()
	sess.sessionID = f.LoginRspFrame.SessionId
	needInit := !sess.isEstablished()
	c.connMu.Unlock()

	if needInit {
		err := c.iniService(c.wg, sess)
		if err != nil {
			atomic.StoreInt32(&c.loginFlight, 0)
			pipe.setNeedClose()
			loggo.Error("processLoginRsp iniService fail %s", err)
			return
		}
		sess.setEstablished(true)
	}

	sess.hub.add(pipe)
	pipe.markActive("login ok")
	pipe.setEstablished(true)
	atomic.StoreInt32(&c.loginFlight, 0)

	loggo.Info("processLoginRsp ok via %s session=%d compress=%s encrypt=%s",
		pipe.proto, sess.sessionID, compressTypeName(codec.CompressType), encryptTypeName(codec.EncryptType))
}

func (c *Client) processJoinRsp(f *ProxyFrame, pipe *mainPipe) {
	if !f.ChannelJoinRspFrame.Ret {
		pipe.setNeedClose()
		loggo.Error("processJoinRsp fail %s %s", pipe.proto, f.ChannelJoinRspFrame.Msg)
		return
	}
	sess := c.currentSession()
	if sess == nil || !sess.isEstablished() {
		pipe.setNeedClose()
		loggo.Error("processJoinRsp no session for %s", pipe.proto)
		return
	}
	pipe.setCodec(sess.getCodec())
	sess.hub.add(pipe)
	pipe.markActive("join ok")
	pipe.setEstablished(true)
	loggo.Info("processJoinRsp ok %s %s session=%d", pipe.proto, pipe.addr, sess.sessionID)
}

func (c *Client) processSpeedTest(f *ProxyFrame, pipe *mainPipe) {
	st := f.SpeedTestFrame
	if st == nil || !st.Echo {
		return
	}
	elapsed := time.Now().UnixNano() - st.SendTime
	if elapsed <= 0 {
		return
	}
	n := len(st.Payload)
	if n == 0 {
		n = 1
	}
	// Round-trip bytes estimate: send + echo.
	bps := int64(n*2) * int64(time.Second) / elapsed
	atomic.StoreInt64(&pipe.thrBps, bps)
	pipe.noteRTT(time.Duration(elapsed))
	pipe.markActive("probe ok")
	if c.config.ShowPing {
		loggo.Info("speedtest %s thr=%s rtt=%s", pipe.proto, formatBps(bps), time.Duration(elapsed).String())
	}
}

func (c *Client) probePipe(wg *thread.Group, pipe *mainPipe) error {
	inter := c.config.ProbeInter
	if inter <= 0 {
		inter = 5
	}
	size := c.config.ProbeSize
	if size <= 0 {
		size = 64 * 1024
	}
	// Grey pipes probe twice as often.
	ticker := time.NewTicker(time.Duration(inter) * time.Second)
	defer ticker.Stop()

	var seq int64
	for {
		select {
		case <-wg.Done():
			return nil
		case <-ticker.C:
			if !pipe.isEstablished() {
				continue
			}
			seq++
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(seq + int64(i))
			}
			now := time.Now().UnixNano()
			atomic.StoreInt64(&pipe.probeID, seq)
			atomic.StoreInt64(&pipe.probeStart, now)
			f := &ProxyFrame{
				Type: FRAME_TYPE_SPEEDTEST,
				SpeedTestFrame: &SpeedTestFrame{
					Id:       seq,
					SendTime: now,
					Payload:  payload,
					Echo:     false,
				},
			}
			pipe.SendFrame(f)
		}
	}
}

func (c *Client) processSession(wg *thread.Group, sess *ServerConn) error {
	loggo.Info("processSession start")
	recvq := sess.recvq
	for !isExit(wg) {
		// Session ends when torn down.
		c.connMu.Lock()
		alive := c.serverconn == sess
		c.connMu.Unlock()
		if !alive {
			break
		}

		v, closed, ok := recvq.PopWait(time.Second)
		if !ok {
			if closed {
				break
			}
			continue
		}
		f := v.(*ProxyFrame)
		switch f.Type {
		case FRAME_TYPE_DATA:
			c.processData(f, sess)
		case FRAME_TYPE_OPEN:
			c.processOpen(f, sess)
		case FRAME_TYPE_OPENRSP:
			c.processOpenRsp(f, sess)
		case FRAME_TYPE_CLOSE:
			c.processClose(f, sess)
		}
	}
	loggo.Info("processSession end")
	return nil
}

func (c *Client) iniService(wg *thread.Group, serverConn *ServerConn) error {
	services := c.buildLoginServices()
	switch c.clienttype {
	case CLIENT_TYPE_PROXY, CLIENT_TYPE_SOCKS5, CLIENT_TYPE_SS_PROXY, CLIENT_TYPE_HTTP:
		for i, svc := range services {
			var input *Inputer
			var err error
			switch c.clienttype {
			case CLIENT_TYPE_SOCKS5:
				input, err = NewSocks5Inputer(wg, svc.Proxyproto.String(), svc.Fromaddr, c.clienttype, c.config, &serverConn.ProxyConn, i)
			case CLIENT_TYPE_HTTP:
				input, err = NewHttpInputer(wg, svc.Proxyproto.String(), svc.Fromaddr, c.clienttype, c.config, &serverConn.ProxyConn, i)
			default:
				input, err = NewInputer(wg, svc.Proxyproto.String(), svc.Fromaddr, c.clienttype, c.config, &serverConn.ProxyConn, svc.Toaddr, i)
			}
			if err != nil {
				serverConn.closeServices()
				return err
			}
			serverConn.appendInput(input)
			loggo.Info("iniService client input[%d] %s %s -> %s", i, svc.Proxyproto.String(), svc.Fromaddr, svc.Toaddr)
		}
	case CLIENT_TYPE_REVERSE_PROXY, CLIENT_TYPE_REVERSE_SOCKS5, CLIENT_TYPE_REVERSE_HTTP:
		for i, svc := range services {
			output, err := NewOutputer(wg, svc.Proxyproto.String(), c.clienttype, c.config, &serverConn.ProxyConn, i)
			if err != nil {
				serverConn.closeServices()
				return err
			}
			serverConn.appendOutput(output)
			loggo.Info("iniService client output[%d] proto=%s", i, svc.Proxyproto.String())
		}
	default:
		return errors.New("error CLIENT_TYPE " + strconv.Itoa(int(c.clienttype)))
	}
	return nil
}

func (c *Client) processData(f *ProxyFrame, serverconn *ServerConn) {
	id := f.DataFrame.Id
	inputs, outputs := serverconn.serviceSnapshot()
	for _, in := range inputs {
		if in.hasSonny(id) {
			in.processDataFrame(f)
			return
		}
	}
	for _, out := range outputs {
		if out.hasSonny(id) {
			out.processDataFrame(f)
			return
		}
	}
}

func (c *Client) processOpen(f *ProxyFrame, serverconn *ServerConn) {
	_, outputs := serverconn.serviceSnapshot()
	routeOpenToOutput(f, outputs)
}

func (c *Client) processOpenRsp(f *ProxyFrame, serverconn *ServerConn) {
	id := f.OpenRspFrame.Id
	inputs, _ := serverconn.serviceSnapshot()
	for _, in := range inputs {
		if in.hasSonny(id) {
			in.processOpenRspFrame(f)
			return
		}
	}
}

func (c *Client) processClose(f *ProxyFrame, serverconn *ServerConn) {
	id := f.CloseFrame.Id
	inputs, outputs := serverconn.serviceSnapshot()
	for _, in := range inputs {
		if in.hasSonny(id) {
			in.processCloseFrame(f)
			return
		}
	}
	for _, out := range outputs {
		if out.hasSonny(id) {
			out.processCloseFrame(f)
			return
		}
	}
}

func routeOpenToOutput(f *ProxyFrame, outputs []*Outputer) {
	if len(outputs) == 0 {
		return
	}
	idx := int(f.OpenFrame.ServiceIndex)
	if idx < 0 || idx >= len(outputs) {
		loggo.Error("routeOpenToOutput bad service_index=%d outputs=%d", idx, len(outputs))
		return
	}
	outputs[idx].processOpenFrame(f)
}
