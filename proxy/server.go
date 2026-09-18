package proxy

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

// ClientConn is the logical per-client session on the server. Multiple underlay
// pipes (tcp/rudp/ricmp/…) attach to one session via LOGIN + CHANNEL_JOIN.
type ClientConn struct {
	ProxyConn

	clienttype CLIENT_TYPE
	name       string // optional client tag; not unique, not used for auth
	clientID   uint64 // server-local session id for tracking

	inputs  []*Inputer
	outputs []*Outputer
	svcMu   sync.Mutex // guards inputs/outputs vs process* readers
	hub     *channelHub
}

func (c *ClientConn) closeServices() {
	c.svcMu.Lock()
	outs := c.outputs
	ins := c.inputs
	c.outputs = nil
	c.inputs = nil
	c.svcMu.Unlock()
	for _, o := range outs {
		o.Close()
	}
	for _, i := range ins {
		i.Close()
	}
}

func (c *ClientConn) serviceSnapshot() (inputs []*Inputer, outputs []*Outputer) {
	c.svcMu.Lock()
	defer c.svcMu.Unlock()
	return c.inputs, c.outputs
}

func (c *ClientConn) appendInput(in *Inputer) {
	c.svcMu.Lock()
	c.inputs = append(c.inputs, in)
	c.svcMu.Unlock()
}

func (c *ClientConn) appendOutput(out *Outputer) {
	c.svcMu.Lock()
	c.outputs = append(c.outputs, out)
	c.svcMu.Unlock()
}

type Server struct {
	config       *Config
	listenaddrs  []string
	listenConns  []network.Conn
	wg           *thread.Group
	clients      sync.Map // key: uint64 clientID -> *ClientConn
	clientNum    int32    // atomic; tracks entries in clients
	nextClientID uint64   // atomic
}

func NewServer(config *Config, proto []string, listenaddrs []string) (*Server, error) {

	if config == nil {
		config = DefaultConfig()
	}
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}

	var listenConns []network.Conn

	for i := range proto {
		conn, err := network.NewConn(proto[i])
		if conn == nil {
			return nil, err
		}

		setCongestion(conn, config)

		listenConn, err := conn.Listen(listenaddrs[i])
		if err != nil {
			return nil, err
		}

		listenConns = append(listenConns, listenConn)
	}

	wg := thread.NewGroup("Server", nil, func() {
		for i := range listenConns {
			loggo.Info("group start exit %s", listenConns[i].Info())
			listenConns[i].Close()
			loggo.Info("group start exit %s", listenConns[i].Info())
		}
	})

	s := &Server{
		config:      config,
		listenaddrs: listenaddrs,
		listenConns: listenConns,
		wg:          wg,
	}

	for i := range proto {
		index := i
		wg.Go("Server listen"+" "+listenaddrs[i], func() error {
			return s.listen(index)
		})
	}

	wg.Go("Client state", func() error {
		return showState(wg)
	})

	return s, nil
}

func (s *Server) Close() {
	s.wg.Stop()
	s.wg.Wait()
}

func (s *Server) listen(index int) error {
	loggo.Info("listen start %d %s", index, s.listenaddrs[index])
	for !isExit(s.wg) {
		conn, err := s.listenConns[index].Accept()
		if err != nil {
			loggo.Info("Server listen Accept fail %s", err)
			continue
		}

		pipe := &mainPipe{
			ProxyConn: ProxyConn{conn: conn},
			proto:     conn.Name(),
			addr:      conn.Info(),
		}
		s.wg.Go("Server servePipe"+" "+conn.Info(), func() error {
			return s.servePipe(pipe)
		})
	}
	loggo.Info("listen end %d %s", index, s.listenaddrs[index])
	return nil
}

func (s *Server) clientSize() int {
	n := atomic.LoadInt32(&s.clientNum)
	if n < 0 {
		return 0
	}
	return int(n)
}

func (s *Server) servePipe(pipe *mainPipe) error {
	loggo.Info("servePipe accept %s", pipe.conn.Info())

	sendq := newPrioQueue(s.config.MainBuffer)
	recvq := newPrioQueue(s.config.MainBuffer)
	pipe.sendq = sendq
	pipe.recvq = recvq
	pipe.setCodec(defaultFrameCodec(s.config))

	var sessionRef atomic.Pointer[ClientConn]

	wg := thread.NewGroup("Server servePipe"+" "+pipe.conn.Info(), s.wg, func() {
		loggo.Info("pipe group exit %s", pipe.conn.Info())
		if pipe.hub != nil {
			pipe.hub.remove(pipe)
		}
		pipe.conn.Close()
		pipe.CloseChannels()
		if sess := sessionRef.Load(); sess != nil {
			s.onPipeGone(sess)
		}
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Server recvFrom"+" "+pipe.conn.Info(), func() error {
		return recvFrom(wg, &pipe.ProxyConn, pipe.conn, s.config.MaxMsgSize)
	})

	wg.Go("Server sendTo"+" "+pipe.conn.Info(), func() error {
		return sendTo(wg, sendq, &pipe.ProxyConn, pipe.conn, s.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})

	wg.Go("Server checkPingActive"+" "+pipe.conn.Info(), func() error {
		err := checkPingActive(wg, &pipe.ProxyConn, s.config.EstablishedTimeout, s.config.PingInter, s.config.PingTimeoutInter, s.config.ShowPing, &pingflag)
		if err != nil {
			pipe.markGray(err.Error())
		}
		return err
	})

	wg.Go("Server checkNeedClose"+" "+pipe.conn.Info(), func() error {
		return checkNeedClose(wg, &pipe.ProxyConn)
	})

	wg.Go("Server processPipe"+" "+pipe.conn.Info(), func() error {
		return s.processPipe(wg, recvq, pipe, &sessionRef, &pongflag, &pongtime)
	})

	if err := s.sendAuthChallenge(pipe); err != nil {
		loggo.Error("servePipe sendAuthChallenge fail %s %s", pipe.conn.Info(), err.Error())
		wg.Stop()
		wg.Wait()
		return err
	}

	wg.Wait()
	loggo.Info("servePipe close %s", pipe.conn.Info())
	return nil
}

func (s *Server) onPipeGone(sess *ClientConn) {
	if sess.hub == nil {
		return
	}
	if sess.hub.liveCount() > 0 {
		return
	}
	loggo.Info("session %d all pipes gone, tear down", sess.clientID)
	sess.closeServices()
	sess.CloseChannels()
	sess.setEstablished(false)
	if _, ok := s.clients.LoadAndDelete(sess.clientID); ok {
		atomic.AddInt32(&s.clientNum, -1)
	}
}

func (s *Server) sendAuthChallenge(pipe *mainPipe) error {
	ch, err := makeAuthChallenge()
	if err != nil {
		return err
	}
	pipe.authChallenge = ch
	f := &ProxyFrame{
		Type:               FRAME_TYPE_AUTH_CHALLENGE,
		AuthChallengeFrame: &AuthChallengeFrame{Challenge: ch},
	}
	pipe.SendFrame(f)
	loggo.Info("sendAuthChallenge to %s", pipe.conn.Info())
	return nil
}

func (s *Server) processPipe(wg *thread.Group, recvq *prioQueue, pipe *mainPipe, sessionRef *atomic.Pointer[ClientConn], pongflag *int32, pongtime *int64) error {
	loggo.Info("processPipe start %s", pipe.conn.Info())

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
		case FRAME_TYPE_LOGIN:
			s.processLogin(f, pipe, sessionRef)

		case FRAME_TYPE_CHANNEL_JOIN:
			s.processChannelJoin(f, pipe, sessionRef)

		case FRAME_TYPE_PING:
			processPing(f, &pipe.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			rtt := processPong(f, &pipe.ProxyConn, s.config.ShowPing)
			pipe.noteRTT(rtt)

		case FRAME_TYPE_SPEEDTEST:
			s.processSpeedTest(f, pipe)

		case FRAME_TYPE_DATA, FRAME_TYPE_OPEN, FRAME_TYPE_OPENRSP, FRAME_TYPE_CLOSE:
			if sess := sessionRef.Load(); sess != nil {
				sess.RecvFrame(f)
			}

		case FRAME_TYPE_AUTH_CHALLENGE:
			loggo.Error("server unexpected AUTH_CHALLENGE from %s", pipe.conn.Info())

		default:
			loggo.Error("processPipe unexpected %s from %s", f.Type.String(), pipe.conn.Info())
		}
	}
	loggo.Info("processPipe end %s", pipe.conn.Info())
	return nil
}

func (s *Server) processSpeedTest(f *ProxyFrame, pipe *mainPipe) {
	st := f.SpeedTestFrame
	if st == nil || st.Echo {
		return
	}
	echo := &ProxyFrame{
		Type: FRAME_TYPE_SPEEDTEST,
		SpeedTestFrame: &SpeedTestFrame{
			Id:       st.Id,
			SendTime: st.SendTime,
			Payload:  st.Payload,
			Echo:     true,
		},
	}
	pipe.SendFrame(echo)
}

func (s *Server) processLogin(f *ProxyFrame, pipe *mainPipe, sessionRef *atomic.Pointer[ClientConn]) {
	loggo.Info("processLogin from %s name=%s", pipe.conn.Info(), f.LoginFrame.Name)

	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_LOGINRSP
	rf.LoginRspFrame = &LoginRspFrame{}

	ch := pipe.authChallenge
	pipe.authChallenge = nil
	authOK := verifyAuthProof(s.config.Key, ch, f.LoginFrame.AuthProof)
	if !authOK {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "auth proof error"
		pipe.SendFrame(rf)
		loggo.Error("processLogin auth proof fail %s", pipe.conn.Info())
		return
	}

	if sessionRef.Load() != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "has established before"
		pipe.SendFrame(rf)
		loggo.Error("processLogin fail has established before %s", pipe.conn.Info())
		return
	}

	if s.clientSize() >= s.config.MaxClient {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "max client"
		pipe.SendFrame(rf)
		loggo.Error("processLogin max client %s", pipe.conn.Info())
		return
	}

	agreeC, agreeE, err := negotiateCodec(f.LoginFrame.CompressType, f.LoginFrame.EncryptType, s.config)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = err.Error()
		pipe.SendFrame(rf)
		loggo.Error("processLogin codec negotiate fail %s %s", pipe.conn.Info(), err.Error())
		return
	}
	codec := defaultFrameCodec(s.config)
	codec.CompressType = agreeC
	codec.EncryptType = agreeE
	pipe.setCodec(codec)

	hub := newChannelHub(s.config)
	sess := &ClientConn{
		ProxyConn: ProxyConn{
			recvq:  newPrioQueue(s.config.MainBuffer),
			router: hub,
		},
		clienttype: f.LoginFrame.Clienttype,
		name:       f.LoginFrame.Name,
		hub:        hub,
	}
	sess.setCodec(codec)
	sess.clientID = atomic.AddUint64(&s.nextClientID, 1)

	err = s.iniService(s.wg, f, sess)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "iniService fail"
		pipe.SendFrame(rf)
		loggo.Error("processLogin iniService fail %s name=%s %s", pipe.conn.Info(), sess.name, err)
		return
	}

	s.clients.Store(sess.clientID, sess)
	atomic.AddInt32(&s.clientNum, 1)
	sessionRef.Store(sess)
	hub.add(pipe)
	pipe.markActive("login")
	pipe.setEstablished(true)
	sess.setEstablished(true)

	s.wg.Go("Server processSession "+strconv.FormatUint(sess.clientID, 10), func() error {
		return s.processSession(s.wg, sess)
	})

	rf.LoginRspFrame.Ret = true
	rf.LoginRspFrame.Msg = "ok"
	rf.LoginRspFrame.CompressType = agreeC
	rf.LoginRspFrame.EncryptType = agreeE
	rf.LoginRspFrame.SessionId = sess.clientID
	pipe.SendFrame(rf)

	loggo.Info("processLogin ok %s name=%s session=%d services=%d compress=%s encrypt=%s",
		pipe.conn.Info(), sess.name, sess.clientID, len(loginServicesOf(f.LoginFrame)),
		compressTypeName(agreeC), encryptTypeName(agreeE))
}

func (s *Server) processChannelJoin(f *ProxyFrame, pipe *mainPipe, sessionRef *atomic.Pointer[ClientConn]) {
	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_CHANNEL_JOIN_RSP
	rf.ChannelJoinRspFrame = &ChannelJoinRspFrame{}

	ch := pipe.authChallenge
	pipe.authChallenge = nil
	authOK := verifyAuthProof(s.config.Key, ch, f.ChannelJoinFrame.AuthProof)
	if !authOK {
		rf.ChannelJoinRspFrame.Ret = false
		rf.ChannelJoinRspFrame.Msg = "auth proof error"
		pipe.SendFrame(rf)
		loggo.Error("processChannelJoin auth fail %s", pipe.conn.Info())
		return
	}

	v, ok := s.clients.Load(f.ChannelJoinFrame.SessionId)
	if !ok {
		rf.ChannelJoinRspFrame.Ret = false
		rf.ChannelJoinRspFrame.Msg = "unknown session"
		pipe.SendFrame(rf)
		loggo.Error("processChannelJoin unknown session %d from %s", f.ChannelJoinFrame.SessionId, pipe.conn.Info())
		return
	}
	sess := v.(*ClientConn)
	if !sess.isEstablished() {
		rf.ChannelJoinRspFrame.Ret = false
		rf.ChannelJoinRspFrame.Msg = "session not ready"
		pipe.SendFrame(rf)
		return
	}

	pipe.setCodec(sess.getCodec())
	sessionRef.Store(sess)
	sess.hub.add(pipe)
	pipe.markActive("join")
	pipe.setEstablished(true)

	rf.ChannelJoinRspFrame.Ret = true
	rf.ChannelJoinRspFrame.Msg = "ok"
	pipe.SendFrame(rf)
	loggo.Info("processChannelJoin ok %s session=%d", pipe.conn.Info(), sess.clientID)
}

func (s *Server) processSession(wg *thread.Group, sess *ClientConn) error {
	loggo.Info("processSession start %d", sess.clientID)
	recvq := sess.recvq
	for !isExit(wg) {
		if !sess.isEstablished() && sess.hub != nil && sess.hub.liveCount() == 0 {
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
			s.processData(f, sess)
		case FRAME_TYPE_OPEN:
			s.processOpen(f, sess)
		case FRAME_TYPE_OPENRSP:
			s.processOpenRsp(f, sess)
		case FRAME_TYPE_CLOSE:
			s.processClose(f, sess)
		}
	}
	loggo.Info("processSession end %d", sess.clientID)
	return nil
}

func loginServicesOf(f *LoginFrame) []*LoginService {
	if len(f.Services) > 0 {
		return f.Services
	}
	return []*LoginService{{
		Proxyproto: f.Proxyproto,
		Fromaddr:   f.Fromaddr,
		Toaddr:     f.Toaddr,
	}}
}

func (s *Server) iniService(wg *thread.Group, f *ProxyFrame, clientConn *ClientConn) error {
	services := loginServicesOf(f.LoginFrame)
	if len(services) == 0 {
		return errors.New("no services in login")
	}

	switch f.LoginFrame.Clienttype {
	case CLIENT_TYPE_PROXY, CLIENT_TYPE_SOCKS5, CLIENT_TYPE_SS_PROXY:
		for i, svc := range services {
			var output *Outputer
			var err error
			if f.LoginFrame.Clienttype == CLIENT_TYPE_SS_PROXY {
				output, err = NewSSOutputer(wg, svc.Proxyproto.String(), f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, i)
			} else {
				output, err = NewOutputer(wg, svc.Proxyproto.String(), f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, i)
			}
			if err != nil {
				clientConn.closeServices()
				return err
			}
			clientConn.appendOutput(output)
			loggo.Info("iniService server output[%d] proto=%s", i, svc.Proxyproto.String())
		}
	case CLIENT_TYPE_REVERSE_PROXY:
		for i, svc := range services {
			input, err := NewInputer(wg, svc.Proxyproto.String(), svc.Fromaddr, f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, svc.Toaddr, i)
			if err != nil {
				clientConn.closeServices()
				return err
			}
			clientConn.appendInput(input)
			loggo.Info("iniService server input[%d] %s %s -> %s", i, svc.Proxyproto.String(), svc.Fromaddr, svc.Toaddr)
		}
	case CLIENT_TYPE_REVERSE_SOCKS5:
		for i, svc := range services {
			input, err := NewSocks5Inputer(wg, svc.Proxyproto.String(), svc.Fromaddr, f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, i)
			if err != nil {
				clientConn.closeServices()
				return err
			}
			clientConn.appendInput(input)
			loggo.Info("iniService server socks5 input[%d] %s %s", i, svc.Proxyproto.String(), svc.Fromaddr)
		}
	default:
		return errors.New("error CLIENT_TYPE " + strconv.Itoa(int(f.LoginFrame.Clienttype)))
	}
	return nil
}

func (s *Server) processData(f *ProxyFrame, clientconn *ClientConn) {
	id := f.DataFrame.Id
	inputs, outputs := clientconn.serviceSnapshot()
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

func (s *Server) processOpenRsp(f *ProxyFrame, clientconn *ClientConn) {
	id := f.OpenRspFrame.Id
	inputs, _ := clientconn.serviceSnapshot()
	for _, in := range inputs {
		if in.hasSonny(id) {
			in.processOpenRspFrame(f)
			return
		}
	}
}

func (s *Server) processOpen(f *ProxyFrame, clientconn *ClientConn) {
	_, outputs := clientconn.serviceSnapshot()
	routeOpenToOutput(f, outputs)
}

func (s *Server) processClose(f *ProxyFrame, clientconn *ClientConn) {
	id := f.CloseFrame.Id
	inputs, outputs := clientconn.serviceSnapshot()
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
