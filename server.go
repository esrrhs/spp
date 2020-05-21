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

type ClientConnSonny struct {
	conn      *net.TCPConn
	id        string
	sendch    chan *SrpFrame
	recvch    chan *SrpFrame
	active    int64
	opened    bool
	needclose bool
}

type ClientConn struct {
	conn        *net.TCPConn
	compress    int
	logined     bool
	remote      string
	pingsend    int
	sendch      chan *SrpFrame
	recvch      chan *SrpFrame
	listenConn  *net.TCPListener
	clientsonny sync.Map
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
	go s.listenClient()
	return nil
}

func (s *Server) listenClient() {

	defer common.CrashLog()

	for {
		conn, err := s.listenConn.Accept()
		if err != nil {
			loggo.Error("listenClient Accept fail: %s %s", s.addr, err.Error())
			continue
		}
		clientconn := &ClientConn{conn: conn.(*net.TCPConn)}
		go s.loopClientConn(clientconn)
	}
}

func (s *Server) loopClientConn(clientconn *ClientConn) {
	defer common.CrashLog()

	loggo.Info("loopClientConn accept new client from %s", clientconn.conn.RemoteAddr().String())

	wg, ctx := errgroup.WithContext(context.Background())

	sendch := make(chan *SrpFrame, MAIN_BUFFER)
	recvch := make(chan *SrpFrame, MAIN_BUFFER)

	clientconn.sendch = sendch
	clientconn.recvch = recvch

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
		clientconn.listenConn.Close()
		loggo.Info("loopClientConn close client from %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
	} else {
		loggo.Info("loopClientConn close invalid client from %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String())
	}
	close(sendch)
	close(recvch)
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
				s.processLogin(ctx, f, sendch, clientconn)

			case SrpFrame_PING:
				s.processPing(ctx, f, sendch, clientconn)

			case SrpFrame_PONG:
				s.processPong(ctx, f, sendch, clientconn)

			case SrpFrame_DATA:
				s.processData(ctx, f, sendch, clientconn)

			case SrpFrame_OPENRSP:
				s.processOpenRsp(ctx, f, sendch, clientconn)
			}
		}
	}

}

func (s *Server) processLogin(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	loggo.Info("processLogin from %s %s", clientconn.conn.RemoteAddr(), f.LoginFrame.Remote)

	rf := &SrpFrame{}
	rf.Type = SrpFrame_LOGINRSP
	rf.LoginRspFrame = &SrpLoginRspFrame{}

	if clientconn.logined {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "has login before"
		sendch <- rf
		loggo.Error("processLogin fail has login before %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr())
		return
	}

	_, loaded := s.clients.LoadOrStore(f.LoginFrame.Remote, clientconn)
	if loaded {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "other has login before"
		sendch <- rf
		loggo.Error("processLogin fail other has login before %s %s", clientconn.conn.RemoteAddr(), f.LoginFrame.Remote)
		return
	}

	addr, err := net.ResolveTCPAddr("tcp", f.LoginFrame.Remote)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "ResolveTCPAddr fail"
		sendch <- rf
		loggo.Error("processLogin ResolveTCPAddr fail %s %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr(), err.Error())
		return
	}

	listenConn, err := net.ListenTCP("tcp", addr)
	if err != nil {
		rf.LoginRspFrame.Ret = false
		rf.LoginRspFrame.Msg = "ResolveTCPAddr fail"
		sendch <- rf
		loggo.Error("processLogin ListenTCP fail %s %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr(), err.Error())
		return
	}

	clientconn.compress = int(f.LoginFrame.Compress)
	clientconn.remote = f.LoginFrame.Remote
	clientconn.logined = true
	clientconn.listenConn = listenConn

	go s.listenClientConn(ctx, clientconn)

	rf.LoginRspFrame.Ret = true
	rf.LoginRspFrame.Msg = "ok"
	sendch <- rf

	loggo.Info("processLogin ok %s %s", f.LoginFrame.Remote, clientconn.conn.RemoteAddr())
}

func (s *Server) processPing(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	rf := &SrpFrame{}
	rf.Type = SrpFrame_PONG
	rf.PongFrame = &SrpPongFrame{}
	rf.PongFrame.Time = f.PingFrame.Time
	sendch <- rf
}

func (s *Server) processPong(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	clientconn.pingsend = 0
	loggo.Info("pong from %s %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String(), elapse.String())
}

func (s *Server) processData(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {
	id := f.DataFrame.Id
	v, ok := clientconn.clientsonny.Load(id)
	if !ok {
		return
	}
	clientconnsonny := v.(*ClientConnSonny)
	clientconnsonny.sendch <- f
	clientconnsonny.active++
}

func (s *Server) processOpenRsp(ctx context.Context, f *SrpFrame, sendch chan<- *SrpFrame, clientconn *ClientConn) {

	id := f.OpenRspFrame.Id

	loggo.Info("open rsp from %s %s %s", clientconn.remote, clientconn.conn.RemoteAddr().String(), id, f.OpenRspFrame.Msg)

	v, ok := clientconn.clientsonny.Load(id)
	if !ok {
		return
	}
	clientconnsonny := v.(*ClientConnSonny)

	if f.OpenRspFrame.Ret {
		clientconnsonny.opened = true
	} else {
		clientconnsonny.needclose = true
	}
}

func (s *Server) listenClientConn(ctx context.Context, clientconn *ClientConn) {

	defer common.CrashLog()

	for {
		conn, err := clientconn.listenConn.Accept()
		if err != nil {
			loggo.Error("listenClientConn Accept fail: %s %s", s.addr, err.Error())
			continue
		}
		clientconnsonny := &ClientConnSonny{conn: conn.(*net.TCPConn)}
		go s.loopClientConnSonny(ctx, clientconn, clientconnsonny)
	}

}

func (s *Server) loopClientConnSonny(fctx context.Context, clientconn *ClientConn, clientconnsonny *ClientConnSonny) {

	defer common.CrashLog()

	clientconnsonny.id = common.UniqueId()

	loggo.Info("loopClientConnSonny %s accept new conn %s coming from %s", clientconn.remote, clientconnsonny.id, clientconnsonny.conn.RemoteAddr().String())

	sendch := make(chan *SrpFrame, CONN_BUFFER)
	recvch := make(chan *SrpFrame, CONN_BUFFER)

	clientconnsonny.sendch = sendch
	clientconnsonny.recvch = recvch

	_, loaded := clientconn.clientsonny.LoadOrStore(clientconnsonny.id, clientconnsonny)
	if loaded {
		loggo.Error("loopClientConnSonny LoadOrStore fail %s %s", clientconn.remote, clientconnsonny.id)
		clientconnsonny.conn.Close()
		return
	}

	wg, ctx := errgroup.WithContext(fctx)

	s.openClientConnSonny(ctx, clientconn, clientconnsonny)

	wg.Go(func() error {
		return recvFrom(ctx, wg, recvch, clientconnsonny.conn)
	})

	wg.Go(func() error {
		return sendTo(ctx, wg, sendch, clientconnsonny.conn, clientconn.compress)
	})

	wg.Go(func() error {
		return s.transferClientConnSonnyRecv(ctx, wg, recvch, clientconn, clientconnsonny)
	})

	wg.Go(func() error {
		return s.checkClientConnSonnyActive(ctx, wg, clientconn, clientconnsonny)
	})

	wg.Go(func() error {
		return s.checkClientConnSonnyNeedClose(ctx, wg, clientconn, clientconnsonny)
	})

	wg.Wait()

	loggo.Info("loopClientConnSonny %s close conn %s coming from %s", clientconn.remote, clientconnsonny.id, clientconnsonny.conn.RemoteAddr().String())
	clientconn.clientsonny.Delete(clientconnsonny.id)
	clientconnsonny.conn.Close()
	close(sendch)
	close(recvch)
}

func (s *Server) openClientConnSonny(ctx context.Context, clientconn *ClientConn, clientconnsonny *ClientConnSonny) {
	f := &SrpFrame{}
	f.Type = SrpFrame_OPEN
	f.OpenFrame = &SrpOpenFrame{}
	f.OpenFrame.Id = clientconnsonny.id

	clientconn.sendch <- f
}

func (s *Server) transferClientConnSonnyRecv(ctx context.Context, wg *errgroup.Group, recvch <-chan *SrpFrame, clientconn *ClientConn, clientconnsonny *ClientConnSonny) error {
	defer common.CrashLog()

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-recvch:
			clientconn.sendch <- f
			clientconnsonny.active++
		}
	}
}

func (s *Server) checkClientConnSonnyActive(ctx context.Context, wg *errgroup.Group, clientconn *ClientConn, clientconnsonny *ClientConnSonny) error {

	n := 0
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Second):
		n++
		if !clientconnsonny.opened {
			if n > CONNNECT_TIMEOUT {
				loggo.Error("checkClientConnSonnyActive open timeout %s %s", clientconn.remote, clientconnsonny.id, clientconnsonny.conn.RemoteAddr().String())
				return errors.New("open timeout")
			}
		} else {
			break
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(CONN_TIMEOUT * time.Second):
			if clientconnsonny.active == 0 {
				loggo.Error("checkClientConnSonnyActive timeout %s %s %s", clientconn.remote, clientconnsonny.id, clientconnsonny.conn.RemoteAddr().String())
				return errors.New("conn timeout")
			}
			clientconnsonny.active = 0
		}
	}
}

func (s *Server) checkClientConnSonnyNeedClose(ctx context.Context, wg *errgroup.Group, clientconn *ClientConn, clientconnsonny *ClientConnSonny) error {

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
			if clientconnsonny.needclose {
				loggo.Error("checkClientConnSonnyNeedClose needclose %s %s %s", clientconn.remote, clientconnsonny.id, clientconnsonny.conn.RemoteAddr().String())
				return errors.New("needclose")
			}
		}
	}
}
