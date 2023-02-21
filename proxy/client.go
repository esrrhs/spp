package proxy

import (
	"errors"
	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/conn"
	"github.com/esrrhs/gohome/group"
	"github.com/esrrhs/gohome/loggo"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type ServerConn struct {
	ProxyConn
	output *Outputer
	input  *Inputer
}

type Client struct {
	config     *Config
	server     string
	name       string
	clienttype CLIENT_TYPE
	proxyproto []PROXY_PROTO
	fromaddr   []string
	toaddr     []string
	serverconn []*ServerConn
	wg         *group.Group
}

func NewClient(config *Config, serverproto string, server string, name string, clienttypestr string, proxyprotostr []string, fromaddr []string, toaddr []string) (*Client, error) {

	if config == nil {
		config = DefaultConfig()
	}

	cn, err := conn.NewConn(serverproto)
	if cn == nil {
		return nil, err
	}

	setCongestion(cn, config)

	clienttypestr = strings.ToUpper(clienttypestr)
	clienttype, ok := CLIENT_TYPE_value[clienttypestr]
	if !ok {
		return nil, errors.New("no CLIENT_TYPE " + clienttypestr)
	}

	var proxyproto []PROXY_PROTO
	for i, _ := range proxyprotostr {
		p, ok := PROXY_PROTO_value[strings.ToUpper(proxyprotostr[i])]
		if !ok {
			return nil, errors.New("no PROXY_PROTO " + proxyprotostr[i])
		}
		proxyproto = append(proxyproto, PROXY_PROTO(p))
	}

	wg := group.NewGroup("Clent"+" "+clienttypestr, nil, nil)

	c := &Client{
		config:     config,
		server:     server,
		name:       name,
		clienttype: CLIENT_TYPE(clienttype),
		proxyproto: proxyproto,
		fromaddr:   fromaddr,
		toaddr:     toaddr,
		serverconn: make([]*ServerConn, len(proxyprotostr)),
		wg:         wg,
	}

	wg.Go("Client state"+" "+clienttypestr, func() error {
		return showState(wg)
	})

	wg.Go("Client check deadlock"+" "+clienttypestr, func() error {
		return checkDeadLock(wg)
	})

	for i, _ := range proxyprotostr {
		index := i
		toaddrstr := ""
		if len(toaddr) > 0 {
			toaddrstr = toaddr[i]
		}
		wg.Go("Client connect"+" "+fromaddr[i]+" "+toaddrstr, func() error {
			atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
			defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
			return c.connect(index, cn)
		})
	}

	return c, nil
}

func (c *Client) Close() {
	c.wg.Stop()
	c.wg.Wait()
}

func (c *Client) connect(index int, conn conn.Conn) error {
	loggo.Info("connect start %d %s", index, c.server)

	for !c.wg.IsExit() {
		if c.serverconn[index] == nil {
			targetconn, err := conn.Dial(c.server)
			if err != nil {
				loggo.Error("connect Dial fail: %s %s", c.server, err.Error())
				time.Sleep(time.Second)
				continue
			}
			c.serverconn[index] = &ServerConn{ProxyConn: ProxyConn{conn: targetconn}}
			c.wg.Go("Client useServer"+" "+targetconn.Info(), func() error {
				atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
				defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
				return c.useServer(index, c.serverconn[index])
			})
		} else {
			time.Sleep(time.Second)
		}
	}
	loggo.Info("connect end %s", c.server)
	return nil
}

func (c *Client) useServer(index int, serverconn *ServerConn) error {

	loggo.Info("useServer %s", serverconn.conn.Info())

	sendch := common.NewChannel(c.config.MainBuffer)
	recvch := common.NewChannel(c.config.MainBuffer)

	serverconn.sendch = sendch
	serverconn.recvch = recvch

	wg := group.NewGroup("Client useServer"+" "+serverconn.conn.Info(), c.wg, func() {
		loggo.Info("group start exit %s", serverconn.conn.Info())
		serverconn.conn.Close()
		sendch.Close()
		recvch.Close()
		if serverconn.output != nil {
			serverconn.output.Close()
		}
		if serverconn.input != nil {
			serverconn.input.Close()
		}
		loggo.Info("group end exit %s", serverconn.conn.Info())
	})

	c.login(index, sendch)

	var pingflag int32
	var pongflag int32
	var pongtime int64

	wg.Go("Client recvFrom"+" "+serverconn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return recvFrom(wg, recvch, serverconn.conn, c.config.MaxMsgSize, c.config.Encrypt)
	})

	wg.Go("Client sendTo"+" "+serverconn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return sendTo(wg, sendch, serverconn.conn, c.config.Compress, c.config.MaxMsgSize, c.config.Encrypt, &pingflag, &pongflag, &pongtime)
	})

	wg.Go("Client checkPingActive"+" "+serverconn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return checkPingActive(wg, sendch, recvch, &serverconn.ProxyConn, c.config.EstablishedTimeout, c.config.PingInter, c.config.PingTimeoutInter, c.config.ShowPing, &pingflag)
	})

	wg.Go("Client checkNeedClose"+" "+serverconn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return checkNeedClose(wg, &serverconn.ProxyConn)
	})

	wg.Go("Client process"+" "+serverconn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return c.process(wg, index, sendch, recvch, serverconn, &pongflag, &pongtime)
	})

	wg.Wait()
	c.serverconn[index] = nil
	loggo.Info("useServer close %s %s", c.server, serverconn.conn.Info())

	return nil
}

func (c *Client) login(index int, sendch *common.Channel) {
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_LOGIN
	f.LoginFrame = &LoginFrame{}
	f.LoginFrame.Proxyproto = c.proxyproto[index]
	f.LoginFrame.Clienttype = c.clienttype
	f.LoginFrame.Fromaddr = c.fromaddr[index]
	if len(c.toaddr) > 0 {
		f.LoginFrame.Toaddr = c.toaddr[index]
	}
	f.LoginFrame.Name = c.name + "_" + strconv.Itoa(index)
	f.LoginFrame.Key = c.config.Key

	sendch.Write(f)

	loggo.Info("start login %d %s %s", index, c.server, f.LoginFrame.String())
}

func (c *Client) process(wg *group.Group, index int, sendch *common.Channel, recvch *common.Channel, serverconn *ServerConn, pongflag *int32, pongtime *int64) error {

	loggo.Info("process start %s", serverconn.conn.Info())

	for !wg.IsExit() {

		ff := <-recvch.Ch()
		if ff == nil {
			break
		}
		f := ff.(*ProxyFrame)
		switch f.Type {
		case FRAME_TYPE_LOGINRSP:
			c.processLoginRsp(wg, index, f, sendch, serverconn)

		case FRAME_TYPE_PING:
			processPing(f, sendch, &serverconn.ProxyConn, pongflag, pongtime)

		case FRAME_TYPE_PONG:
			processPong(f, sendch, &serverconn.ProxyConn, c.config.ShowPing)

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

func (c *Client) processLoginRsp(wg *group.Group, index int, f *ProxyFrame, sendch *common.Channel, serverconn *ServerConn) {
	if !f.LoginRspFrame.Ret {
		serverconn.needclose = true
		loggo.Error("processLoginRsp fail %s %s", c.server, f.LoginRspFrame.Msg)
		return
	}

	loggo.Info("processLoginRsp ok %s", c.server)

	err := c.iniService(wg, index, serverconn)
	if err != nil {
		loggo.Error("processLoginRsp iniService fail %s %s", c.server, err)
		return
	}

	serverconn.established = true
}

func (c *Client) iniService(wg *group.Group, index int, serverConn *ServerConn) error {
	switch c.clienttype {
	case CLIENT_TYPE_PROXY:
		input, err := NewInputer(wg, c.proxyproto[index].String(), c.fromaddr[index], c.clienttype, c.config, &serverConn.ProxyConn, c.toaddr[index])
		if err != nil {
			return err
		}
		serverConn.input = input
	case CLIENT_TYPE_REVERSE_PROXY:
		output, err := NewOutputer(wg, c.proxyproto[index].String(), c.clienttype, c.config, &serverConn.ProxyConn)
		if err != nil {
			return err
		}
		serverConn.output = output
	case CLIENT_TYPE_SOCKS5:
		input, err := NewSocks5Inputer(wg, c.proxyproto[index].String(), c.fromaddr[index], c.clienttype, c.config, &serverConn.ProxyConn)
		if err != nil {
			return err
		}
		serverConn.input = input
	case CLIENT_TYPE_REVERSE_SOCKS5:
		output, err := NewOutputer(wg, c.proxyproto[index].String(), c.clienttype, c.config, &serverConn.ProxyConn)
		if err != nil {
			return err
		}
		serverConn.output = output
	case CLIENT_TYPE_SS_PROXY:
		input, err := NewInputer(wg, c.proxyproto[index].String(), c.fromaddr[index], c.clienttype, c.config, &serverConn.ProxyConn, c.toaddr[index])
		if err != nil {
			return err
		}
		serverConn.input = input
	default:
		return errors.New("error CLIENT_TYPE " + strconv.Itoa(int(c.clienttype)))
	}
	return nil
}

func (c *Client) processData(f *ProxyFrame, serverconn *ServerConn) {
	if serverconn.input != nil {
		serverconn.input.processDataFrame(f)
	} else if serverconn.output != nil {
		serverconn.output.processDataFrame(f)
	}
}

func (c *Client) processOpen(f *ProxyFrame, serverconn *ServerConn) {
	if serverconn.output != nil {
		serverconn.output.processOpenFrame(f)
	}
}

func (c *Client) processOpenRsp(f *ProxyFrame, serverconn *ServerConn) {
	if serverconn.input != nil {
		serverconn.input.processOpenRspFrame(f)
	}
}

func (c *Client) processClose(f *ProxyFrame, serverconn *ServerConn) {
	if serverconn.input != nil {
		serverconn.input.processCloseFrame(f)
	} else if serverconn.output != nil {
		serverconn.output.processCloseFrame(f)
	}
}
