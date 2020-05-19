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
	conn    *net.TCPConn
	logined bool
	lastHB  time.Time
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, _ := errgroup.WithContext(ctx)

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

	time.Sleep(time.Second * LOGIN_TIMEOUT)
	if !serverconn.logined {
		return errors.New("login timeout")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(HB_INTER * time.Second):
			f := &SrpFrame{}
			f.Type = SrpFrame_HB
			f.HbFrame = &SrpHBFrame{}
			sendch <- f

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
