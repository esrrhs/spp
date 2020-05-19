package main

import (
	"context"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"golang.org/x/sync/errgroup"
	"net"
	"sync"
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
	lock       sync.Mutex
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

	wg.Wait()
	clientconn.conn.Close()
}

func (s *Server) process(ctx context.Context, group *errgroup.Group, sendch chan<- *SrpFrame, recvch <-chan *SrpFrame, clientconn *ClientConn) error {
	defer common.CrashLog()

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-recvch:
			switch f.Type {
			case SrpFrame_LOGIN:
				s.loginRspClient(f, sendch, clientconn)
			}
		}
	}

}

func (s *Server) loginRspClient(f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	rf := &SrpFrame{}
	rf.LoginRspFrame = &SrpLoginRspFrame{}

	if clientconn.logined {
		f.LoginRspFrame.Ret = false
		f.LoginRspFrame.Msg = "has login before"
		sendch <- f
		return
	}

	s.lock.Lock()
	defer s.lock.Unlock()

}
