package main

import (
	"context"
	"errors"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"golang.org/x/sync/errgroup"
	"net"
	"time"
)

type ServerConn struct {
	conn     *net.TCPConn
	logined  bool
	lastHB   time.Time
	pingsend int
}

type Client struct {
	key        int
	server     string
	local      string
	remote     string
	compress   int
	serveraddr *net.TCPAddr
	localaddr  *net.TCPAddr

	serverconn *ServerConn
}

func NewClient(key int, server string, local string, remote string, compress int) (*Client, error) {

	serveraddr, err := net.ResolveTCPAddr("tcp", server)
	if err != nil {
		return nil, err
	}

	localaddr, err := net.ResolveTCPAddr("tcp", local)
	if err != nil {
		return nil, err
	}

	_, err = net.ResolveTCPAddr("tcp", remote)
	if err != nil {
		return nil, err
	}

	c := &Client{
		key:        key,
		server:     server,
		local:      local,
		remote:     remote,
		compress:   compress,
		serveraddr: serveraddr,
		localaddr:  localaddr,
	}

	return c, nil
}

func (c *Client) Run() error {

	go c.connectServer()

	return nil
}

func (c *Client) connectServer() {

	defer common.CrashLog()

	for {
		if c.serverconn == nil {
			targetconn, err := net.DialTCP("tcp", nil, c.serveraddr)
			if err != nil {
				loggo.Error("connectServer DialTCP fail: %s %s", c.server, err.Error())
				time.Sleep(time.Second)
				continue
			}
			c.serverconn = &ServerConn{conn: targetconn}
			go c.loopServer(c.serverconn)
		} else {
			time.Sleep(time.Second)
		}
	}

}

func (c *Client) loopServer(serverconn *ServerConn) {
	defer common.CrashLog()

	loggo.Info("new server conn from %s", serverconn.conn.RemoteAddr().String())

	wg, ctx := errgroup.WithContext(context.Background())

	sendch := make(chan *SrpFrame, 1024)
	recvch := make(chan *SrpFrame, 1024)

	c.loginServer(sendch)

	wg.Go(func() error {
		return recvFrom(ctx, wg, recvch, serverconn.conn)
	})

	wg.Go(func() error {
		return sendTo(ctx, wg, sendch, serverconn.conn, c.compress)
	})

	wg.Go(func() error {
		return c.process(ctx, wg, sendch, recvch, serverconn)
	})

	wg.Go(func() error {
		return c.checkActive(ctx, wg, sendch, recvch, serverconn)
	})

	wg.Wait()
	serverconn.conn.Close()
	c.serverconn = nil
	if serverconn.logined {
		loggo.Info("close server conn from %s %s", c.server, serverconn.conn.RemoteAddr().String())
	} else {
		loggo.Info("close invalid server conn from %s %s", c.server, serverconn.conn.RemoteAddr().String())
	}
}

func (c *Client) loginServer(sendch chan<- *SrpFrame) {
	f := &SrpFrame{}
	f.Type = SrpFrame_LOGIN
	f.LoginFrame = &SrpLoginFrame{}
	f.LoginFrame.Remote = c.remote
	f.LoginFrame.Compress = int32(c.compress)

	sendch <- f

	loggo.Info("start login %s", c.server)
}

func (c *Client) checkActive(ctx context.Context, wg *errgroup.Group, sendch chan<- *SrpFrame, recvch <-chan *SrpFrame, serverconn *ServerConn) error {
	defer common.CrashLog()

	n := 0
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Second):
		n++
		if !serverconn.logined {
			if n > LOGIN_TIMEOUT {
				loggo.Error("checkActive login timeout %s %s", c.server, serverconn.conn.RemoteAddr().String())
				return errors.New("login timeout")
			}
		} else {
			break
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(PING_INTER * time.Second):
			if serverconn.pingsend > PING_TIMEOUT_INTER {
				loggo.Error("checkActive ping pong timeout %s %s", c.server, serverconn.conn.RemoteAddr().String())
				return errors.New("ping pong timeout")
			}

			f := &SrpFrame{}
			f.Type = SrpFrame_PING
			f.PingFrame = &SrpPingFrame{}
			f.PingFrame.Time = time.Now().UnixNano()
			sendch <- f
			serverconn.pingsend++
			loggo.Info("ping %s %s", c.server, serverconn.conn.RemoteAddr().String())
		}
	}
}

func (c *Client) process(ctx context.Context, wg *errgroup.Group, sendch chan<- *SrpFrame, recvch <-chan *SrpFrame, serverconn *ServerConn) error {
	defer common.CrashLog()

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-recvch:
			switch f.Type {
			case SrpFrame_LOGINRSP:
				c.processLoginRsp(f, sendch, serverconn)

			case SrpFrame_PING:
				c.processPing(f, sendch, serverconn)

			case SrpFrame_PONG:
				c.processPong(f, sendch, serverconn)
			}
		}
	}

}

func (c *Client) processLoginRsp(f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	if f.LoginRspFrame.Ret {
		loggo.Info("login rsp ok %s", c.server)
		serverconn.logined = true
	} else {
		loggo.Error("login rsp fail %s %s", c.server, f.LoginRspFrame.Msg)
	}
}

func (c *Client) processPing(f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	rf := &SrpFrame{}
	rf.Type = SrpFrame_PONG
	rf.PongFrame = &SrpPongFrame{}
	rf.PongFrame.Time = f.PingFrame.Time
	sendch <- rf
}

func (c *Client) processPong(f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	serverconn.pingsend = 0
	loggo.Info("pong from %s %s %s", c.server, serverconn.conn.RemoteAddr().String(), elapse.String())
}
