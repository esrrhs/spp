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

type ClientConn struct {
	conn     *net.TCPConn
	compress int
	logined  bool
	remote   string
}

type Server struct {
	key        int
	addr       *net.TCPAddr
	listenConn *net.TCPListener
	clients    sync.Map
}

func NewServer(key int, local string) (*Server, error) {

	localaddr, err := net.ResolveTCPAddr("tcp", local)
	if err != nil {
		return nil, err
	}

	listenConn, err := net.ListenTCP("tcp", localaddr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		key:        key,
		addr:       localaddr,
		listenConn: listenConn,
	}
	return s, nil
}

func (s *Server) Run() error {
	go s.serveClient()
	return nil
}

func (s *Server) serveClient() {

	defer common.CrashLog()

	for {
		conn, err := s.listenConn.Accept()
		if err != nil {
			loggo.Error("serveClient Accept fail: %s %s", s.addr, err.Error())
			continue
		}
		clientconn := &ClientConn{conn: conn.(*net.TCPConn)}
		go s.loopClient(clientconn)
	}
}

func (s *Server) loopClient(clientconn *ClientConn) {
	defer common.CrashLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg, _ := errgroup.WithContext(ctx)

	sendch := make(chan *SrpFrame, 1024)
	recvch := make(chan *SrpFrame, 1024)

	wg.Go(func() error {
		return recvFrom(ctx, wg, recvch, clientconn.conn)
	})

	wg.Go(func() error {
		return sendTo(ctx, wg, sendch, clientconn.conn, clientconn.compress)
	})

	wg.Go(func() error {
		return s.process(ctx, wg, sendch, recvch, clientconn)
	})

	wg.Go(func() error {
		time.Sleep(time.Second * LOGIN_TIMEOUT)
		if !clientconn.logined {
			return errors.New("login timeout")
		}
		return nil
	})

	wg.Wait()
	clientconn.conn.Close()
}

func (s *Server) process(ctx context.Context, wg *errgroup.Group, sendch chan<- *SrpFrame, recvch <-chan *SrpFrame, clientconn *ClientConn) error {
	defer common.CrashLog()

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-recvch:
			switch f.Type {
			case SrpFrame_LOGIN:
				s.processLogin(f, sendch, clientconn)
			}
		}
	}

}

func (s *Server) processLogin(f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	rf := &SrpFrame{}
	f.Type = SrpFrame_LOGINRSP
	rf.LoginRspFrame = &SrpLoginRspFrame{}

	if clientconn.logined {
		f.LoginRspFrame.Ret = false
		f.LoginRspFrame.Msg = "has login before"
		sendch <- f
		return
	}

	_, loaded := s.clients.LoadOrStore(f.LoginFrame.Remote, clientconn)
	if loaded {
		f.LoginRspFrame.Ret = false
		f.LoginRspFrame.Msg = "other has login before"
		sendch <- f
		return
	}

	clientconn.compress = int(f.LoginFrame.Compress)
	clientconn.remote = f.LoginFrame.Remote
	clientconn.logined = true

	f.LoginRspFrame.Ret = true
	f.LoginRspFrame.Msg = "ok"
	sendch <- f
	return
}
