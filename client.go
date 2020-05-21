package main

import (
	"context"
	"errors"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"golang.org/x/sync/errgroup"
	"net"
	"sync"
	"time"
)

type ServerConnSonny struct {
	conn   *net.TCPConn
	id     string
	sendch chan *SrpFrame
	recvch chan *SrpFrame
	active int64
}

type ServerConn struct {
	conn        *net.TCPConn
	logined     bool
	lastHB      time.Time
	pingsend    int
	sendch      chan *SrpFrame
	recvch      chan *SrpFrame
	serversonny sync.Map
}

type Client struct {
	key       int
	server    string
	local     string
	remote    string
	compress  int
	localaddr *net.TCPAddr

	serverconn *ServerConn
}

func NewClient(key int, server string, local string, remote string, compress int) (*Client, error) {

	_, err := net.ResolveTCPAddr("tcp", server)
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
		key:       key,
		server:    server,
		local:     local,
		remote:    remote,
		compress:  compress,
		localaddr: localaddr,
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

			serveraddr, err := net.ResolveTCPAddr("tcp", c.server)
			if err != nil {
				loggo.Error("connectServer ResolveTCPAddr fail: %s %s %s", c.server, c.remote, err.Error())
				time.Sleep(time.Second)
				continue
			}

			targetconn, err := net.DialTCP("tcp", nil, serveraddr)
			if err != nil {
				loggo.Error("connectServer DialTCP fail: %s %s %s", c.server, c.remote, err.Error())
				time.Sleep(time.Second)
				continue
			}
			c.serverconn = &ServerConn{conn: targetconn}
			go c.loopServerConn(c.serverconn)
		} else {
			time.Sleep(time.Second)
		}
	}

}

func (c *Client) loopServerConn(serverconn *ServerConn) {
	defer common.CrashLog()

	loggo.Info("loopServerConn new server conn from %s", serverconn.conn.RemoteAddr().String())

	wg, ctx := errgroup.WithContext(context.Background())

	sendch := make(chan *SrpFrame, MAIN_BUFFER)
	recvch := make(chan *SrpFrame, MAIN_BUFFER)

	serverconn.sendch = sendch
	serverconn.recvch = recvch

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
		loggo.Info("loopServerConn close server conn from %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String())
	} else {
		loggo.Info("loopServerConn close invalid server conn from %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String())
	}
}

func (c *Client) loginServer(sendch chan<- *SrpFrame) {
	f := &SrpFrame{}
	f.Type = SrpFrame_LOGIN
	f.LoginFrame = &SrpLoginFrame{}
	f.LoginFrame.Remote = c.remote
	f.LoginFrame.Compress = int32(c.compress)

	sendch <- f

	loggo.Info("start login %s %s", c.server, c.remote)
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
				loggo.Error("checkActive login timeout %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String())
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
				loggo.Error("checkActive ping pong timeout %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String())
				return errors.New("ping pong timeout")
			}

			f := &SrpFrame{}
			f.Type = SrpFrame_PING
			f.PingFrame = &SrpPingFrame{}
			f.PingFrame.Time = time.Now().UnixNano()
			sendch <- f
			serverconn.pingsend++
			loggo.Info("ping %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String())
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
				c.processLoginRsp(ctx, f, sendch, serverconn)

			case SrpFrame_PING:
				c.processPing(ctx, f, sendch, serverconn)

			case SrpFrame_PONG:
				c.processPong(ctx, f, sendch, serverconn)

			case SrpFrame_OPEN:
				c.processOpen(ctx, f, sendch, serverconn)
			}
		}
	}

}

func (c *Client) processLoginRsp(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	if f.LoginRspFrame.Ret {
		loggo.Info("login rsp ok %s %s", c.server, c.remote)
		serverconn.logined = true
	} else {
		loggo.Error("login rsp fail %s %s %s", c.server, c.remote, f.LoginRspFrame.Msg)
	}
}

func (c *Client) processPing(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	rf := &SrpFrame{}
	rf.Type = SrpFrame_PONG
	rf.PongFrame = &SrpPongFrame{}
	rf.PongFrame.Time = f.PingFrame.Time
	sendch <- rf
}

func (c *Client) processPong(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	serverconn.pingsend = 0
	loggo.Info("pong from %s %s %s %s", c.server, c.remote, serverconn.conn.RemoteAddr().String(), elapse.String())
}

func (c *Client) processOpen(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, serverconn *ServerConn) {
	id := f.OpenFrame.Id
	loggo.Info("remote %s %s open new conn %s", c.server, c.remote, id)
	go c.connectServerSonny(ctx, sendch, serverconn, id)
}

func (c *Client) connectServerSonny(ctx context.Context, sendch chan<- *SrpFrame, serverconn *ServerConn, id string) {

	defer common.CrashLog()

	loggo.Info("connectServerSonny %s %s connect local conn %s %s", c.server, c.remote, id, c.local)

	rf := &SrpFrame{}
	rf.Type = SrpFrame_OPENRSP
	rf.OpenRspFrame = &SrpOpenRspFrame{}
	rf.OpenRspFrame.Id = id

	localaddr, err := net.ResolveTCPAddr("tcp", c.local)
	if err != nil {
		rf.OpenRspFrame.Ret = false
		rf.OpenRspFrame.Msg = "ResolveTCPAddr fail"
		sendch <- rf
		loggo.Error("connectServerSonny ResolveTCPAddr %s %s %s %s", c.server, c.remote, id, err.Error())
		return
	}

	targetconn, err := net.DialTCP("tcp", nil, localaddr)
	if err != nil {
		rf.OpenRspFrame.Ret = false
		rf.OpenRspFrame.Msg = "DialTCP fail"
		sendch <- rf
		loggo.Error("connectServerSonny DialTCP fail: %s %s %s", c.server, c.remote, err.Error())
		return
	}

	serverconnsonny := &ServerConnSonny{conn: targetconn, id: id}
	_, loaded := serverconn.serversonny.LoadOrStore(serverconnsonny.id, serverconnsonny)
	if loaded {
		serverconnsonny.conn.Close()
		rf.OpenRspFrame.Ret = false
		rf.OpenRspFrame.Msg = "LoadOrStore fail"
		sendch <- rf
		loggo.Error("connectServerSonny LoadOrStore fail %s %s", c.server, c.remote, serverconnsonny.id)
		return
	}

	rf.OpenRspFrame.Ret = true
	rf.OpenRspFrame.Msg = "ok"
	sendch <- rf

	go c.loopServerConnSonny(ctx, c.serverconn, serverconnsonny)
}

func (c *Client) loopServerConnSonny(fctx context.Context, serverconn *ServerConn, serverconnsonny *ServerConnSonny) {
	defer common.CrashLog()

	loggo.Info("loopServerConnSonny local conn %s", serverconnsonny.conn.RemoteAddr().String())

	wg, ctx := errgroup.WithContext(fctx)

	sendch := make(chan *SrpFrame, CONN_BUFFER)
	recvch := make(chan *SrpFrame, CONN_BUFFER)

	serverconnsonny.sendch = sendch
	serverconnsonny.recvch = recvch

	wg.Go(func() error {
		return recvFrom(ctx, wg, recvch, serverconnsonny.conn)
	})

	wg.Go(func() error {
		return sendTo(ctx, wg, sendch, serverconnsonny.conn, c.compress)
	})

	wg.Go(func() error {
		return c.transferServerConnSonnyRecv(ctx, wg, recvch, serverconn, serverconnsonny)
	})

	wg.Go(func() error {
		return c.checkActive(ctx, wg, sendch, recvch, serverconn)
	})

	wg.Wait()
	serverconnsonny.conn.Close()
	c.closeClientConnSonny(ctx, serverconn, serverconnsonny)
}

func (c *Client) closeClientConnSonny(ctx context.Context, serverconn *ServerConn, serverconnsonny *ServerConnSonny) {
	f := &SrpFrame{}
	f.Type = SrpFrame_CLOSE
	f.CloseFrame = &SrpCloseFrame{}
	f.CloseFrame.Id = serverconnsonny.id

	serverconn.sendch <- f
}

func (c *Client) transferServerConnSonnyRecv(ctx context.Context, group *errgroup.Group, recvch <-chan *SrpFrame, serverconn *ServerConn, serverconnsonny *ServerConnSonny) error {

}
