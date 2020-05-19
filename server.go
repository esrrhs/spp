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
	pingsend int
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

	loggo.Info("accept new client from %s", clientconn.conn.RemoteAddr().String())

	wg, ctx := errgroup.WithContext(context.Background())

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
		return s.checkActive(ctx, wg, sendch, recvch, clientconn)
	})

	wg.Wait()
	clientconn.conn.Close()
	if clientconn.logined {
		s.clients.Delete(clientconn.remote)
		loggo.Info("close client from %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
	} else {
		loggo.Info("close invalid client from %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
	}
}

func (s *Server) checkActive(ctx context.Context, wg *errgroup.Group, sendch chan<- *SrpFrame, recvch <-chan *SrpFrame, clientconn *ClientConn) error {
	defer common.CrashLog()

	n := 0
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Second):
		n++
		if !clientconn.logined {
			if n > LOGIN_TIMEOUT {
				loggo.Error("checkActive login timeout %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
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
			if clientconn.pingsend > PING_TIMEOUT_INTER {
				loggo.Error("checkActive ping pong timeout %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
				return errors.New("ping pong timeout")
			}

			f := &SrpFrame{}
			f.Type = SrpFrame_PING
			f.PingFrame = &SrpPingFrame{}
			f.PingFrame.Time = time.Now().UnixNano()
			sendch <- f
			clientconn.pingsend++
			loggo.Info("ping %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
		}
	}
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

			case SrpFrame_PING:
				s.processPing(f, sendch, clientconn)

			case SrpFrame_PONG:
				s.processPong(f, sendch, clientconn)
			}
		}
	}

}

func (s *Server) processLogin(f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	loggo.Info("processLogin from %s %s", clientconn.conn.RemoteAddr(), f.LoginFrame.Remote)

	rf := &SrpFrame{}
	rf.Type = SrpFrame_LOGINRSP
	rf.LoginRspFrame = &SrpLoginRspFrame{}

	if clientconn.logined {
		f.LoginRspFrame.Ret = false
		f.LoginRspFrame.Msg = "has login before"
		sendch <- f
		loggo.Error("processLogin fail has login before %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr())
		return
	}

	_, loaded := s.clients.LoadOrStore(f.LoginFrame.Remote, clientconn)
	if loaded {
		f.LoginRspFrame.Ret = false
		f.LoginRspFrame.Msg = "other has login before"
		sendch <- f
		loggo.Error("processLogin fail other has login before %s %s", clientconn.conn.RemoteAddr(), f.LoginFrame.Remote)
		return
	}

	clientconn.compress = int(f.LoginFrame.Compress)
	clientconn.remote = f.LoginFrame.Remote
	clientconn.logined = true

	rf.LoginRspFrame.Ret = true
	rf.LoginRspFrame.Msg = "ok"
	sendch <- rf

	loggo.Info("processLogin ok %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr())
}

func (s *Server) processPing(f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	rf := &SrpFrame{}
	rf.Type = SrpFrame_PONG
	rf.PongFrame = &SrpPongFrame{}
	rf.PongFrame.Time = f.PingFrame.Time
	sendch <- rf
}

func (s *Server) processPong(f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	clientconn.pingsend = 0
	loggo.Info("pong from %s %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String(), elapse.String())
}
