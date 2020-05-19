package main

import (
	"context"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"golang.org/x/sync/errgroup"
	"net"
	"time"
)

type Client struct {
	key        int
	server     string
	local      string
	remote     string
	compress   int
	serveraddr *net.TCPAddr
	localaddr  *net.TCPAddr

	serverconn *net.TCPConn
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
				loggo.Error("connectServer DialTCP fail: %s %s", c.serveraddr, err.Error())
				time.Sleep(time.Second)
				continue
			}
			c.serverconn = targetconn
			go c.loopServer(targetconn)
		} else {
			time.Sleep(time.Second)
		}
	}

}

func (c *Client) loopServer(serverconn *net.TCPConn) {
	defer common.CrashLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, _ := errgroup.WithContext(ctx)

	sendch := make(chan *SrpFrame, 1024)
	recvch := make(chan *SrpFrame, 1024)

	c.loginServer(sendch)

	wg.Go(func() error {
		return recvFrom(ctx, wg, recvch, serverconn)
	})

	wg.Go(func() error {
		return sendTo(ctx, wg, sendch, serverconn, c.compress)
	})

	wg.Wait()
	serverconn.Close()
	c.serverconn = nil
}

func (c *Client) loginServer(sendch chan<- *SrpFrame) {
	f := &SrpFrame{}
	f.LoginFrame = &SrpLoginFrame{}
	f.LoginFrame.Remote = c.remote
	f.LoginFrame.Compress = int32(c.compress)

	sendch <- f
}
