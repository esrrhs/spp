package main

import (
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"net"
	"time"
)

type Client struct {
	key        int
	server     string
	local      string
	remote     string
	compress   bool
	serveraddr *net.TCPAddr
	localaddr  *net.TCPAddr

	serverconn *net.TCPConn
}

func NewClient(key int, server string, local string, remote string, compress bool) (*Client, error) {

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

}

func (c *Client) connectServer() {

	defer common.CrashLog()

	for {
		if c.serverconn == nil {
			targetconn, err := net.DialTCP("tcp", nil, c.serveraddr)
			if err != nil {
				loggo.Error("connectServer fail: %s %s", c.serveraddr, err.Error())
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

	defer serverconn.Close()

	c.loginServer()

	go c.recvFromServer(serverconn)
	c.sendToServer(serverconn)

	c.serverconn = nil
}

func (c *Client) loginServer() {

}

func (c *Client) recvFromServer(conn *net.TCPConn) {

}

func (c *Client) sendToServer(conn *net.TCPConn) {

}
