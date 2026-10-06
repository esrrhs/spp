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

// ServerConn is the logical client↔server session. It IS the single underlay
// connection: business Inputer/Outputer services attach directly to it and all
// frames travel over this one conn. Reconnecting creates a fresh ServerConn.
type ServerConn struct {
	ProxyConn
	bizq *prioQueue // business frames (DATA/OPEN/OPENRSP/CLOSE)

	outputs []*Outputer
	inputs  []*Inputer
	svcMu   sync.Mutex // guards inputs/outputs vs process* readers
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

// deliverBusiness pushes a wire frame into the business queue. Control frames
// keep priority; DATA stays FIFO bulk.
func (s *ServerConn) deliverBusiness(f *ProxyFrame) {
	prio := prioControl
	if f.Type == FRAME_TYPE_DATA {
		prio = prioBulk
	}
	s.bizq.Push(f, prio)
}

type Client struct {
	config     *Config
	proto      string
	server     string
	name       string
	clienttype CLIENT_TYPE
	proxyproto []PROXY_PROTO
	fromaddr   []string
	toaddr     []string
	serverconn *ServerConn
	connMu     sync.Mutex
	wg         *thread.Group
}

// NewClient dials exactly one underlay (proto, server). Multi-path redundancy,
// if ever needed, belongs to a layer above (e.g. run multiple clients).
func NewClient(config *Config, serverproto string, server string, name string, clienttypestr string, proxyprotostr []string, fromaddr []string, toaddr []string) (*Client, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	if serverproto == "" {
		return nil, errors.New("no server proto")
	}
	if server == "" {
		return nil, errors.New("no server addr")
	}

	cn, err := network.NewConn(serverproto)
	if cn == nil {
		return nil, err
	}
	setUnderlayTuning(cn, config)
	cn.Close()

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
		config:     config,
		proto:      serverproto,
		server:     server,
		name:       name,
		clienttype: CLIENT_TYPE(clienttype),
		proxyproto: proxyproto,
		fromaddr:   fromaddr,
		toaddr:     toaddr,
		wg:         wg,
	}

	goSafe(wg, "Client state"+" "+clienttypestr, func() {
		showState(wg)
	})

	goSafe(wg, "Client connect", func() {
		c.connect()
	})

	return c, nil
}

func (c *Client) Close() {
	c.wg.Stop()
	c.wg.Wait()
}

func (c *Client) connect() {
	loggo.Info("connect start proto=%s addr=%s", c.proto, c.server)

	retry := time.NewTicker(time.Second)
	defer retry.Stop()

	for {
		if isExit(c.wg) {
			loggo.Info("connect end")
			return
		}
		c.runOnce()
		select {
		case <-c.wg.Done():
			loggo.Info("connect end")
			return
		case <-retry.C:
		}
	}
}

// runOnce dials the server and blocks serving the single connection until it
// tears down. A failed dial or a dead conn simply returns for reconnect.
func (c *Client) runOnce() {
	dialer, err := network.NewConn(c.proto)
	if dialer == nil {
		loggo.Error("connect NewConn fail: %s %v", c.proto, err)
		return
	}
	setUnderlayTuning(dialer, c.config)
	conn, err := dialWithTimeout(dialer, c.server, c.config.ConnectTimeout)
	if err != nil {
		dialer.Close()
		loggo.Error("connect Dial fail: %s %s %s", c.proto, c.server, err.Error())
		return
	}

	sess := &ServerConn{
		ProxyConn: ProxyConn{conn: conn},
		bizq:      newPrioQueue(c.config.MainBuffer),
	}
	sess.sendq = newPrioQueue(c.config.MainBuffer)
	sess.recvq = newPrioQueue(c.config.MainBuffer)
	sess.setCodec(defaultFrameCodec(c.config))

	c.connMu.Lock()
	c.serverconn = sess
	c.connMu.Unlock()

	wg := thread.NewGroup("Client main "+c.proto+" "+c.server, c.wg, func() {
		loggo.Info("main conn group exit %s %s", c.proto, c.server)
		sess.closeServices()
		sess.closeConn()
		sess.bizq.Close()
		sess.CloseChannels()
		sess.setEstablished(false)
		c.connMu.Lock()
		if c.serverconn == sess {
			c.serverconn = nil
		}
		c.connMu.Unlock()
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Client recvFrom "+c.proto, func() error {
		return recvFrom(wg, &sess.ProxyConn, conn, c.config.MaxMsgSize)
	})
	wg.Go("Client sendTo "+c.proto, func() error {
		return sendTo(wg, sess.sendq, &sess.ProxyConn, conn, c.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})
	// kcp/quic Accept only completes after the client writes application data.
	// Without a nudge, server Accept and client waiting for AUTH_CHALLENGE deadlock.
	atomic.StoreInt32(&pingflag, 1)
	wg.Go("Client checkPingActive "+c.proto, func() error {
		authTimeout := c.config.AuthTimeout
		if authTimeout <= 0 {
			authTimeout = c.config.EstablishedTimeout
		}
		return checkPingActive(wg, &sess.ProxyConn, authTimeout, c.config.PingInter, c.config.PingTimeoutInter, c.config.ShowPing, &pingflag)
	})
	wg.Go("Client checkNeedClose "+c.proto, func() error {
		return checkNeedClose(wg, &sess.ProxyConn)
	})
	wg.Go("Client processConn "+c.proto, func() error {
		return c.processConn(wg, sess, &pongflag, &pongtime)
	})
	wg.Go("Client processSession", func() error {
		c.processSession(wg, sess)
		return nil
	})

	wg.Wait()
	loggo.Info("runOnce close %s %s", c.proto, c.server)
}

func (c *Client) currentSession() *ServerConn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.serverconn
}

func (c *Client) processConn(wg *thread.Group, sess *ServerConn, pongflag *int32, pongtime *int64) error {
	loggo.Info("processConn start %s %s", c.proto, c.server)

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
		case FRAME_TYPE_AUTH_CHALLENGE:
			c.sendLogin(sess, f.AuthChallengeFrame.Challenge)

		case FRAME_TYPE_LOGINRSP:
			c.processLoginRsp(wg, f, sess)

		case FRAME_TYPE_PING:
			processPing(f, &sess.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			rtt := processPong(f, &sess.ProxyConn, c.config.ShowPing)
			sess.noteRTT(rtt)

		case FRAME_TYPE_DATA, FRAME_TYPE_OPEN, FRAME_TYPE_OPENRSP, FRAME_TYPE_CLOSE:
			sess.deliverBusiness(f)

		default:
			loggo.Error("processConn unexpected %s on %s", f.Type.String(), c.proto)
		}
	}
	loggo.Info("processConn end %s %s", c.proto, c.server)
	return nil
}

func (c *Client) sendLogin(sess *ServerConn, challenge []byte) {
	codec := sess.getCodec()
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

	sess.SendFrame(f)
	loggo.Info("start login via %s %s name=%s services=%d (hmac)",
		c.proto, c.server, f.LoginFrame.Name, len(services))
}

func (c *Client) processLoginRsp(wg *thread.Group, f *ProxyFrame, sess *ServerConn) {
	if !f.LoginRspFrame.Ret {
		sess.setNeedClose()
		loggo.Error("processLoginRsp fail %s: %s", c.proto, f.LoginRspFrame.Msg)
		return
	}
	if sess.isEstablished() {
		return
	}

	codec := sess.getCodec()
	if f.LoginRspFrame.CompressType != CompressUnspecified {
		codec.CompressType = f.LoginRspFrame.CompressType
	}
	if f.LoginRspFrame.EncryptType != EncryptUnspecified {
		codec.EncryptType = f.LoginRspFrame.EncryptType
	}
	sess.setCodec(codec)

	if err := c.iniService(wg, sess); err != nil {
		sess.setNeedClose()
		loggo.Error("processLoginRsp iniService fail %s", err)
		return
	}
	sess.setEstablished(true)

	loggo.Info("processLoginRsp ok session=%d compress=%s encrypt=%s",
		f.LoginRspFrame.SessionId, compressTypeName(codec.CompressType), encryptTypeName(codec.EncryptType))
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

func (c *Client) processSession(wg *thread.Group, sess *ServerConn) {
	loggo.Info("processSession start")
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
