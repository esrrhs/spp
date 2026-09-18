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

type ClientConn struct {
	ProxyConn

	proxyproto PROXY_PROTO
	clienttype CLIENT_TYPE
	fromaddr   string
	toaddr     string
	name       string // optional client tag; not unique, not used for auth
	clientID   uint64 // server-local session id for tracking

	authChallenge []byte

	input  *Inputer
	output *Outputer
}

type Server struct {
	config      *Config
	listenaddrs []string
	listenConns []network.Conn
	wg          *thread.Group
	clients     sync.Map // key: uint64 clientID -> *ClientConn
	clientNum   int32    // atomic; tracks entries in clients
	nextClientID uint64  // atomic
}

func NewServer(config *Config, proto []string, listenaddrs []string) (*Server, error) {

	if config == nil {
		config = DefaultConfig()
	}

	var listenConns []network.Conn

	for i, _ := range proto {
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
		for i, _ := range listenConns {
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

	for i, _ := range proto {
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

		size := s.clientSize()
		if size >= s.config.MaxClient {
			loggo.Info("Server listen max client %s %d", conn.Info(), size)
			conn.Close()
			continue
		}

		clientconn := &ClientConn{ProxyConn: ProxyConn{conn: conn}}
		s.wg.Go("Server serveClient"+" "+conn.Info(), func() error {
			return s.serveClient(clientconn)
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

func (s *Server) serveClient(clientconn *ClientConn) error {

	loggo.Info("serveClient accept new client %s", clientconn.conn.Info())

	sendq := newPrioQueue(s.config.MainBuffer)
	recvq := newPrioQueue(s.config.MainBuffer)

	clientconn.sendq = sendq
	clientconn.recvq = recvq
	clientconn.setCodec(defaultFrameCodec(s.config))

	wg := thread.NewGroup("Server serveClient"+" "+clientconn.conn.Info(), s.wg, func() {
		loggo.Info("group start exit %s", clientconn.conn.Info())
		if clientconn.input != nil {
			clientconn.input.Close()
		}
		if clientconn.output != nil {
			clientconn.output.Close()
		}
		clientconn.conn.Close()
		clientconn.CloseChannels()
		loggo.Info("group end exit %s", clientconn.conn.Info())
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Server recvFrom"+" "+clientconn.conn.Info(), func() error {
		return recvFrom(wg, &clientconn.ProxyConn, clientconn.conn, s.config.MaxMsgSize)
	})

	wg.Go("Server sendTo"+" "+clientconn.conn.Info(), func() error {
		return sendTo(wg, sendq, &clientconn.ProxyConn, clientconn.conn, s.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})

	wg.Go("Server checkPingActive"+" "+clientconn.conn.Info(), func() error {
		return checkPingActive(wg, &clientconn.ProxyConn, s.config.EstablishedTimeout, s.config.PingInter, s.config.PingTimeoutInter, s.config.ShowPing, &pingflag)
	})

	wg.Go("Server checkNeedClose"+" "+clientconn.conn.Info(), func() error {
		return checkNeedClose(wg, &clientconn.ProxyConn)
	})

	wg.Go("Server process"+" "+clientconn.conn.Info(), func() error {
		return s.process(wg, recvq, clientconn, &pongflag, &pongtime)
	})

	// Issue auth challenge after IO loops are running (encrypted with PSK if AEAD).
	if err := s.sendAuthChallenge(clientconn); err != nil {
		loggo.Error("serveClient sendAuthChallenge fail %s %s", clientconn.conn.Info(), err.Error())
		wg.Stop()
		wg.Wait()
		return err
	}

	wg.Wait()
	if clientconn.isEstablished() {
		if _, ok := s.clients.LoadAndDelete(clientconn.clientID); ok {
			atomic.AddInt32(&s.clientNum, -1)
		}
	}

	loggo.Info("serveClient close client %s", clientconn.conn.Info())

	return nil
}

func (s *Server) process(wg *thread.Group, recvq *prioQueue, clientconn *ClientConn, pongflag *int32, pongtime *int64) error {

	loggo.Info("process start %s", clientconn.conn.Info())

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
			s.processLogin(wg, f, clientconn)

		case FRAME_TYPE_PING:
			processPing(f, &clientconn.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			processPong(f, &clientconn.ProxyConn, s.config.ShowPing)

		case FRAME_TYPE_DATA:
			s.processData(f, clientconn)

		case FRAME_TYPE_OPEN:
			s.processOpen(f, clientconn)

		case FRAME_TYPE_OPENRSP:
			s.processOpenRsp(f, clientconn)

		case FRAME_TYPE_CLOSE:
			s.processClose(f, clientconn)

		case FRAME_TYPE_AUTH_CHALLENGE:
			loggo.Error("server unexpected AUTH_CHALLENGE from %s", clientconn.conn.Info())
		}
	}
	loggo.Info("process end %s", clientconn.conn.Info())
	return nil
}

func (s *Server) sendAuthChallenge(clientconn *ClientConn) error {
	ch, err := makeAuthChallenge()
	if err != nil {
		return err
	}
	clientconn.authChallenge = ch
	f := &ProxyFrame{
		Type:               FRAME_TYPE_AUTH_CHALLENGE,
		AuthChallengeFrame: &AuthChallengeFrame{Challenge: ch},
	}
	clientconn.SendFrame(f)
	loggo.Info("sendAuthChallenge to %s", clientconn.conn.Info())
	return nil
}

func (s *Server) processLogin(wg *thread.Group, f *ProxyFrame, clientconn *ClientConn) {
	loggo.Info("processLogin from %s name=%s", clientconn.conn.Info(), f.LoginFrame.Name)

	clientconn.proxyproto = f.LoginFrame.Proxyproto
	clientconn.clienttype = f.LoginFrame.Clienttype
	clientconn.fromaddr = f.LoginFrame.Fromaddr
	clientconn.toaddr = f.LoginFrame.Toaddr
	clientconn.name = f.LoginFrame.Name

	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_LOGINRSP
	rf.LoginRspFrame = &LoginRspFrame{}

	authOK := verifyAuthProof(s.config.Key, clientconn.authChallenge, f.LoginFrame.AuthProof)
	if !authOK {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "auth proof error"
		clientconn.SendFrame(rf)
		loggo.Error("processLogin auth proof fail %s", clientconn.conn.Info())
		return
	}
	clientconn.authChallenge = nil

	agreeC, agreeE, err := negotiateCodec(f.LoginFrame.CompressType, f.LoginFrame.EncryptType, s.config)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = err.Error()
		clientconn.SendFrame(rf)
		loggo.Error("processLogin codec negotiate fail %s %s", clientconn.conn.Info(), err.Error())
		return
	}
	codec := defaultFrameCodec(s.config)
	codec.CompressType = agreeC
	codec.EncryptType = agreeE
	clientconn.setCodec(codec)

	if clientconn.isEstablished() {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "has established before"
		clientconn.SendFrame(rf)
		loggo.Error("processLogin fail has established before %s", clientconn.conn.Info())
		return
	}

	clientconn.clientID = atomic.AddUint64(&s.nextClientID, 1)
	s.clients.Store(clientconn.clientID, clientconn)
	atomic.AddInt32(&s.clientNum, 1)

	err = s.iniService(wg, f, clientconn)
	if err != nil {
		if _, ok := s.clients.LoadAndDelete(clientconn.clientID); ok {
			atomic.AddInt32(&s.clientNum, -1)
		}
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "iniService fail"
		clientconn.SendFrame(rf)
		loggo.Error("processLogin iniService fail %s name=%s %s", clientconn.conn.Info(), clientconn.name, err)
		return
	}

	clientconn.setEstablished(true)

	rf.LoginRspFrame.Ret = true
	rf.LoginRspFrame.Msg = "ok"
	rf.LoginRspFrame.CompressType = agreeC
	rf.LoginRspFrame.EncryptType = agreeE
	clientconn.SendFrame(rf)

	loggo.Info("processLogin ok %s name=%s compress=%s encrypt=%s",
		clientconn.conn.Info(), clientconn.name,
		compressTypeName(agreeC), encryptTypeName(agreeE))
}

func (s *Server) iniService(wg *thread.Group, f *ProxyFrame, clientConn *ClientConn) error {
	switch f.LoginFrame.Clienttype {
	case CLIENT_TYPE_PROXY:
		output, err := NewOutputer(wg, f.LoginFrame.Proxyproto.String(), f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn)
		if err != nil {
			return err
		}
		clientConn.output = output
	case CLIENT_TYPE_REVERSE_PROXY:
		input, err := NewInputer(wg, f.LoginFrame.Proxyproto.String(), f.LoginFrame.Fromaddr, f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn, clientConn.toaddr)
		if err != nil {
			return err
		}
		clientConn.input = input
	case CLIENT_TYPE_SOCKS5:
		output, err := NewOutputer(wg, f.LoginFrame.Proxyproto.String(), f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn)
		if err != nil {
			return err
		}
		clientConn.output = output
	case CLIENT_TYPE_REVERSE_SOCKS5:
		input, err := NewSocks5Inputer(wg, f.LoginFrame.Proxyproto.String(), f.LoginFrame.Fromaddr, f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn)
		if err != nil {
			return err
		}
		clientConn.input = input
	case CLIENT_TYPE_SS_PROXY:
		output, err := NewSSOutputer(wg, f.LoginFrame.Proxyproto.String(), f.LoginFrame.Clienttype, s.config, &clientConn.ProxyConn)
		if err != nil {
			return err
		}
		clientConn.output = output
	default:
		return errors.New("error CLIENT_TYPE " + strconv.Itoa(int(f.LoginFrame.Clienttype)))
	}
	return nil
}

func (s *Server) processData(f *ProxyFrame, clientconn *ClientConn) {
	if clientconn.input != nil {
		clientconn.input.processDataFrame(f)
	} else if clientconn.output != nil {
		clientconn.output.processDataFrame(f)
	}
}

func (s *Server) processOpenRsp(f *ProxyFrame, clientconn *ClientConn) {
	if clientconn.input != nil {
		clientconn.input.processOpenRspFrame(f)
	}
}

func (c *Server) processOpen(f *ProxyFrame, clientconn *ClientConn) {
	if clientconn.output != nil {
		clientconn.output.processOpenFrame(f)
	}
}

func (c *Server) processClose(f *ProxyFrame, clientconn *ClientConn) {
	if clientconn.input != nil {
		clientconn.input.processCloseFrame(f)
	} else if clientconn.output != nil {
		clientconn.output.processCloseFrame(f)
	}
}
