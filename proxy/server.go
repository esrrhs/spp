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

// ClientConn is the per-client session on the server. It IS the one accepted
// underlay connection: one client login maps to one conn and one ClientConn.
type ClientConn struct {
	ProxyConn
	bizq *prioQueue // business frames (DATA/OPEN/OPENRSP/CLOSE)

	clienttype CLIENT_TYPE
	name       string // optional client tag; not unique, not used for auth
	clientID   uint64 // server-local session id for tracking

	inputs  []*Inputer
	outputs []*Outputer
	svcMu   sync.Mutex // guards inputs/outputs vs process* readers
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

// deliverBusiness pushes a wire frame into the business queue.
func (c *ClientConn) deliverBusiness(f *ProxyFrame) {
	prio := prioControl
	if f.Type == FRAME_TYPE_DATA {
		prio = prioBulk
	}
	c.bizq.Push(f, prio)
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

		setUnderlayTuning(conn, config)

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
		goSafe(wg, "Server listen"+" "+listenaddrs[i], func() {
			s.listen(index)
		})
	}

	goSafe(wg, "Client state", func() {
		showState(wg)
	})

	return s, nil
}

func (s *Server) Close() {
	s.wg.Stop()
	s.wg.Wait()
}

func (s *Server) listen(index int) {
	loggo.Info("listen start %d %s", index, s.listenaddrs[index])
	for !isExit(s.wg) {
		conn, err := s.listenConns[index].Accept()
		if err != nil {
			loggo.Debug("Server listen Accept fail %s", err)
			if isExit(s.wg) {
				break
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}

		sess := s.newClientConn(conn)
		goSafe(s.wg, "Server serveConn"+" "+conn.Info(), func() {
			s.serveConn(sess)
		})
	}
	loggo.Info("listen end %d %s", index, s.listenaddrs[index])
}

// newClientConn wires a fresh accepted conn into an un-logged-in ClientConn.
func (s *Server) newClientConn(conn network.Conn) *ClientConn {
	sess := &ClientConn{
		ProxyConn: ProxyConn{conn: conn},
		bizq:      newPrioQueue(s.config.MainBuffer),
	}
	sess.sendq = newPrioQueue(s.config.MainBuffer)
	sess.recvq = newPrioQueue(s.config.MainBuffer)
	sess.setCodec(defaultFrameCodec(s.config))
	return sess
}

func (s *Server) clientSize() int {
	n := atomic.LoadInt32(&s.clientNum)
	if n < 0 {
		return 0
	}
	return int(n)
}

func (s *Server) serveConn(sess *ClientConn) {
	loggo.Info("serveConn accept %s", sess.conn.Info())

	wg := thread.NewGroup("Server serveConn"+" "+sess.conn.Info(), s.wg, func() {
		loggo.Info("conn group exit %s", sess.conn.Info())
		// clientID 0 means login never completed: nothing is in the map.
		if _, ok := s.clients.LoadAndDelete(sess.clientID); ok {
			atomic.AddInt32(&s.clientNum, -1)
			sess.closeServices()
		}
		sess.setEstablished(false)
		sess.closeConn()
		sess.bizq.Close()
		sess.CloseChannels()
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Server recvFrom"+" "+sess.conn.Info(), func() error {
		return recvFrom(wg, &sess.ProxyConn, sess.conn, s.config.MaxMsgSize)
	})

	wg.Go("Server sendTo"+" "+sess.conn.Info(), func() error {
		return sendTo(wg, sess.sendq, &sess.ProxyConn, sess.conn, s.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})

	wg.Go("Server checkPingActive"+" "+sess.conn.Info(), func() error {
		authTimeout := s.config.AuthTimeout
		if authTimeout <= 0 {
			authTimeout = s.config.EstablishedTimeout
		}
		return checkPingActive(wg, &sess.ProxyConn, authTimeout, s.config.PingInter, s.config.PingTimeoutInter, s.config.ShowPing, &pingflag)
	})

	wg.Go("Server checkNeedClose"+" "+sess.conn.Info(), func() error {
		return checkNeedClose(wg, &sess.ProxyConn)
	})

	wg.Go("Server processConn"+" "+sess.conn.Info(), func() error {
		return s.processConn(wg, sess, &pongflag, &pongtime)
	})

	wg.Go("Server processSession"+" "+sess.conn.Info(), func() error {
		s.processSession(wg, sess)
		return nil
	})

	if err := s.sendAuthChallenge(sess); err != nil {
		// Runs on the server root group: stop/clean up just this conn's
		// group and return; never propagate the error or the whole server
		// (and every client session) exits.
		loggo.Error("serveConn sendAuthChallenge fail %s %s", sess.conn.Info(), err.Error())
		wg.Stop()
		wg.Wait()
		return
	}

	wg.Wait()
	loggo.Info("serveConn close %s", sess.conn.Info())
}

func (s *Server) sendAuthChallenge(sess *ClientConn) error {
	ch, err := makeAuthChallenge()
	if err != nil {
		return err
	}
	sess.authChallenge = ch
	f := &ProxyFrame{
		Type:               FRAME_TYPE_AUTH_CHALLENGE,
		AuthChallengeFrame: &AuthChallengeFrame{Challenge: ch},
	}
	sess.SendFrame(f)
	loggo.Info("sendAuthChallenge to %s", sess.conn.Info())
	return nil
}

func (s *Server) processConn(wg *thread.Group, sess *ClientConn, pongflag *int32, pongtime *int64) error {
	loggo.Info("processConn start %s", sess.conn.Info())

	for !isExit(wg) {
		v, closed, ok := sess.recvq.PopWait(time.Second)
		if !ok {
			if closed {
				break
			}
			continue
		}
		f := v.(*ProxyFrame)

		switch f.Type {
		case FRAME_TYPE_LOGIN:
			s.processLogin(f, sess)

		case FRAME_TYPE_PING:
			processPing(f, &sess.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			rtt := processPong(f, &sess.ProxyConn, s.config.ShowPing)
			sess.noteRTT(rtt)

		case FRAME_TYPE_DATA, FRAME_TYPE_OPEN, FRAME_TYPE_OPENRSP, FRAME_TYPE_CLOSE:
			if sess.isEstablished() {
				sess.deliverBusiness(f)
			}

		case FRAME_TYPE_AUTH_CHALLENGE:
			loggo.Error("server unexpected AUTH_CHALLENGE from %s", sess.conn.Info())

		default:
			loggo.Error("processConn unexpected %s from %s", f.Type.String(), sess.conn.Info())
		}
	}
	loggo.Info("processConn end %s", sess.conn.Info())
	return nil
}

func (s *Server) processLogin(f *ProxyFrame, sess *ClientConn) {
	loggo.Info("processLogin from %s name=%s", sess.conn.Info(), f.LoginFrame.Name)

	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_LOGINRSP
	rf.LoginRspFrame = &LoginRspFrame{}

	ch := sess.authChallenge
	sess.authChallenge = nil
	authOK := verifyAuthProof(s.config.Key, ch, f.LoginFrame.AuthProof)
	if !authOK {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "auth proof error"
		sess.SendFrame(rf)
		sess.setNeedClose()
		loggo.Error("processLogin auth proof fail %s", sess.conn.Info())
		return
	}

	if sess.isEstablished() {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "has established before"
		sess.SendFrame(rf)
		loggo.Error("processLogin fail has established before %s", sess.conn.Info())
		return
	}

	if s.clientSize() >= s.config.MaxClient {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "max client"
		sess.SendFrame(rf)
		loggo.Error("processLogin max client %s", sess.conn.Info())
		return
	}

	agreeC, agreeE, err := negotiateCodec(f.LoginFrame.CompressType, f.LoginFrame.EncryptType, s.config)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = err.Error()
		sess.SendFrame(rf)
		loggo.Error("processLogin codec negotiate fail %s %s", sess.conn.Info(), err.Error())
		return
	}
	codec := defaultFrameCodec(s.config)
	codec.CompressType = agreeC
	codec.EncryptType = agreeE
	sess.setCodec(codec)

	sess.clienttype = f.LoginFrame.Clienttype
	sess.name = f.LoginFrame.Name
	sess.clientID = atomic.AddUint64(&s.nextClientID, 1)

	if err := s.iniService(s.wg, f, sess); err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "iniService fail"
		sess.SendFrame(rf)
		loggo.Error("processLogin iniService fail %s name=%s %s", sess.conn.Info(), sess.name, err)
		return
	}

	s.clients.Store(sess.clientID, sess)
	atomic.AddInt32(&s.clientNum, 1)
	sess.setEstablished(true)

	rf.LoginRspFrame.Ret = true
	rf.LoginRspFrame.Msg = "ok"
	rf.LoginRspFrame.CompressType = agreeC
	rf.LoginRspFrame.EncryptType = agreeE
	rf.LoginRspFrame.SessionId = sess.clientID
	sess.SendFrame(rf)

	loggo.Info("processLogin ok %s name=%s session=%d services=%d compress=%s encrypt=%s",
		sess.conn.Info(), sess.name, sess.clientID, len(loginServicesOf(f.LoginFrame)),
		compressTypeName(agreeC), encryptTypeName(agreeE))
}

func (s *Server) processSession(wg *thread.Group, sess *ClientConn) {
	loggo.Info("processSession start %d", sess.clientID)
	for !isExit(wg) {
		v, closed, ok := sess.bizq.PopWait(time.Second)
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
	case CLIENT_TYPE_PROXY, CLIENT_TYPE_SOCKS5, CLIENT_TYPE_SS_PROXY, CLIENT_TYPE_HTTP:
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
	case CLIENT_TYPE_REVERSE_HTTP:
		for i, svc := range services {
			input, err := NewHttpInputer(wg, svc.Proxyproto.String(), svc.Fromaddr, f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, i)
			if err != nil {
				clientConn.closeServices()
				return err
			}
			clientConn.appendInput(input)
			loggo.Info("iniService server http input[%d] %s %s", i, svc.Proxyproto.String(), svc.Fromaddr)
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
