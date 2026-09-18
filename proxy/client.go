package proxy

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

type ServerConn struct {
	ProxyConn
	outputs []*Outputer
	inputs  []*Inputer
}

func (s *ServerConn) closeServices() {
	for _, o := range s.outputs {
		o.Close()
	}
	for _, i := range s.inputs {
		i.Close()
	}
	s.outputs = nil
	s.inputs = nil
}

type Client struct {
	config      *Config
	server      string
	serverproto string
	name        string
	clienttype  CLIENT_TYPE
	proxyproto  []PROXY_PROTO
	fromaddr    []string
	toaddr      []string
	serverconn  *ServerConn
	connMu      sync.RWMutex
	wg          *thread.Group
}

func NewClient(config *Config, serverproto string, server string, name string, clienttypestr string, proxyprotostr []string, fromaddr []string, toaddr []string) (*Client, error) {

	if config == nil {
		config = DefaultConfig()
	}
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}

	cn, err := network.NewConn(serverproto)
	if cn == nil {
		return nil, err
	}
	setCongestion(cn, config)
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
		config:      config,
		server:      server,
		serverproto: serverproto,
		name:        name,
		clienttype:  CLIENT_TYPE(clienttype),
		proxyproto:  proxyproto,
		fromaddr:    fromaddr,
		toaddr:      toaddr,
		wg:          wg,
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
	loggo.Info("connect start %s", c.server)

	checkTicker := time.NewTicker(time.Second)
	defer checkTicker.Stop()

	exit := false
	for !exit {
		select {
		case <-c.wg.Done():
			exit = true
		case <-checkTicker.C:
			c.connMu.RLock()
			sconn := c.serverconn
			c.connMu.RUnlock()
			if sconn == nil {
				dialer, err := network.NewConn(c.serverproto)
				if dialer == nil {
					loggo.Error("connect NewConn fail: %s %v", c.server, err)
					break
				}
				setCongestion(dialer, c.config)
				targetconn, err := dialWithTimeout(dialer, c.server, c.config.ConnectTimeout)
				if err != nil {
					dialer.Close()
					loggo.Error("connect Dial fail: %s %s", c.server, err.Error())
					break
				}
				newConn := &ServerConn{ProxyConn: ProxyConn{conn: targetconn}}
				c.connMu.Lock()
				c.serverconn = newConn
				c.connMu.Unlock()
				c.wg.Go("Client useServer"+" "+targetconn.Info(), func() error {
					return c.useServer(newConn)
				})
			}
		}
	}

	loggo.Info("connect end %s", c.server)
	return nil
}

func (c *Client) useServer(serverconn *ServerConn) error {
	loggo.Info("useServer %s", serverconn.conn.Info())

	sendq := newPrioQueue(c.config.MainBuffer)
	recvq := newPrioQueue(c.config.MainBuffer)

	serverconn.sendq = sendq
	serverconn.recvq = recvq
	serverconn.setCodec(defaultFrameCodec(c.config))

	wg := thread.NewGroup("Client useServer"+" "+serverconn.conn.Info(), c.wg, func() {
		loggo.Info("group start exit %s", serverconn.conn.Info())
		serverconn.closeServices()
		serverconn.conn.Close()
		serverconn.CloseChannels()
		loggo.Info("group end exit %s", serverconn.conn.Info())
	})

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Client recvFrom"+" "+serverconn.conn.Info(), func() error {
		return recvFrom(wg, &serverconn.ProxyConn, serverconn.conn, c.config.MaxMsgSize)
	})

	wg.Go("Client sendTo"+" "+serverconn.conn.Info(), func() error {
		return sendTo(wg, sendq, &serverconn.ProxyConn, serverconn.conn, c.config.MaxMsgSize, &pingflag, &pongflag, &pongtime)
	})

	wg.Go("Client checkPingActive"+" "+serverconn.conn.Info(), func() error {
		return checkPingActive(wg, &serverconn.ProxyConn, c.config.EstablishedTimeout, c.config.PingInter, c.config.PingTimeoutInter, c.config.ShowPing, &pingflag)
	})

	wg.Go("Client checkNeedClose"+" "+serverconn.conn.Info(), func() error {
		return checkNeedClose(wg, &serverconn.ProxyConn)
	})

	wg.Go("Client process"+" "+serverconn.conn.Info(), func() error {
		return c.process(wg, recvq, serverconn, &pongflag, &pongtime)
	})

	wg.Wait()
	c.connMu.Lock()
	if c.serverconn == serverconn {
		c.serverconn = nil
	}
	c.connMu.Unlock()
	loggo.Info("useServer close %s %s", c.server, serverconn.conn.Info())

	return nil
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

func (c *Client) loginWithChallenge(serverconn *ServerConn, challenge []byte) {
	codec := serverconn.getCodec()
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

	serverconn.SendFrame(f)

	loggo.Info("start login %s name=%s services=%d compress=%s encrypt=%s (hmac)",
		c.server, f.LoginFrame.Name, len(services),
		compressTypeName(codec.CompressType), encryptTypeName(codec.EncryptType))
}

func (c *Client) process(wg *thread.Group, recvq *prioQueue, serverconn *ServerConn, pongflag *int32, pongtime *int64) error {
	loggo.Info("process start %s", serverconn.conn.Info())

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
			c.loginWithChallenge(serverconn, f.AuthChallengeFrame.Challenge)

		case FRAME_TYPE_LOGINRSP:
			c.processLoginRsp(wg, f, serverconn)

		case FRAME_TYPE_PING:
			processPing(f, &serverconn.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			processPong(f, &serverconn.ProxyConn, c.config.ShowPing)

		case FRAME_TYPE_DATA:
			c.processData(f, serverconn)

		case FRAME_TYPE_OPEN:
			c.processOpen(f, serverconn)

		case FRAME_TYPE_OPENRSP:
			c.processOpenRsp(f, serverconn)

		case FRAME_TYPE_CLOSE:
			c.processClose(f, serverconn)
		}
	}
	loggo.Info("process end %s", serverconn.conn.Info())
	return nil
}

func (c *Client) processLoginRsp(wg *thread.Group, f *ProxyFrame, serverconn *ServerConn) {
	if !f.LoginRspFrame.Ret {
		serverconn.setNeedClose()
		loggo.Error("processLoginRsp fail %s %s", c.server, f.LoginRspFrame.Msg)
		return
	}

	codec := serverconn.getCodec()
	if f.LoginRspFrame.CompressType != CompressUnspecified {
		codec.CompressType = f.LoginRspFrame.CompressType
	}
	if f.LoginRspFrame.EncryptType != EncryptUnspecified {
		codec.EncryptType = f.LoginRspFrame.EncryptType
	}
	serverconn.setCodec(codec)

	loggo.Info("processLoginRsp ok %s compress=%s encrypt=%s",
		c.server, compressTypeName(codec.CompressType), encryptTypeName(codec.EncryptType))

	err := c.iniService(wg, serverconn)
	if err != nil {
		serverconn.setNeedClose()
		loggo.Error("processLoginRsp iniService fail %s %s", c.server, err)
		return
	}

	serverconn.setEstablished(true)
}

func (c *Client) iniService(wg *thread.Group, serverConn *ServerConn) error {
	services := c.buildLoginServices()
	switch c.clienttype {
	case CLIENT_TYPE_PROXY, CLIENT_TYPE_SOCKS5, CLIENT_TYPE_SS_PROXY:
		for i, svc := range services {
			var input *Inputer
			var err error
			switch c.clienttype {
			case CLIENT_TYPE_SOCKS5:
				input, err = NewSocks5Inputer(wg, svc.Proxyproto.String(), svc.Fromaddr, c.clienttype, c.config, &serverConn.ProxyConn, i)
			default:
				input, err = NewInputer(wg, svc.Proxyproto.String(), svc.Fromaddr, c.clienttype, c.config, &serverConn.ProxyConn, svc.Toaddr, i)
			}
			if err != nil {
				serverConn.closeServices()
				return err
			}
			serverConn.inputs = append(serverConn.inputs, input)
			loggo.Info("iniService client input[%d] %s %s -> %s", i, svc.Proxyproto.String(), svc.Fromaddr, svc.Toaddr)
		}
	case CLIENT_TYPE_REVERSE_PROXY, CLIENT_TYPE_REVERSE_SOCKS5:
		for i, svc := range services {
			output, err := NewOutputer(wg, svc.Proxyproto.String(), c.clienttype, c.config, &serverConn.ProxyConn, i)
			if err != nil {
				serverConn.closeServices()
				return err
			}
			serverConn.outputs = append(serverConn.outputs, output)
			loggo.Info("iniService client output[%d] proto=%s", i, svc.Proxyproto.String())
		}
	default:
		return errors.New("error CLIENT_TYPE " + strconv.Itoa(int(c.clienttype)))
	}
	return nil
}

func (c *Client) processData(f *ProxyFrame, serverconn *ServerConn) {
	id := f.DataFrame.Id
	for _, in := range serverconn.inputs {
		if in.hasSonny(id) {
			in.processDataFrame(f)
			return
		}
	}
	for _, out := range serverconn.outputs {
		if out.hasSonny(id) {
			out.processDataFrame(f)
			return
		}
	}
}

func (c *Client) processOpen(f *ProxyFrame, serverconn *ServerConn) {
	routeOpenToOutput(f, serverconn.outputs)
}

func (c *Client) processOpenRsp(f *ProxyFrame, serverconn *ServerConn) {
	id := f.OpenRspFrame.Id
	for _, in := range serverconn.inputs {
		if in.hasSonny(id) {
			in.processOpenRspFrame(f)
			return
		}
	}
}

func (c *Client) processClose(f *ProxyFrame, serverconn *ServerConn) {
	id := f.CloseFrame.Id
	for _, in := range serverconn.inputs {
		if in.hasSonny(id) {
			in.processCloseFrame(f)
			return
		}
	}
	for _, out := range serverconn.outputs {
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
