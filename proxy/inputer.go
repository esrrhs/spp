package proxy

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

type Inputer struct {
	clienttype   CLIENT_TYPE
	config       *Config
	proto        string
	addr         string
	father       *ProxyConn
	fwg          *thread.Group
	serviceIndex int32

	listenconn network.Conn
	sonny      sync.Map
	sonnyNum   int32 // atomic; tracks entries in sonny
}

func NewInputer(wg *thread.Group, proto string, addr string, clienttype CLIENT_TYPE, config *Config, father *ProxyConn, targetAddr string, serviceIndex int) (*Inputer, error) {
	conn, err := network.NewConn(proto)
	if conn == nil {
		return nil, err
	}

	listenconn, err := conn.Listen(addr)
	if err != nil {
		return nil, err
	}

	input := &Inputer{
		clienttype:   clienttype,
		config:       config,
		proto:        proto,
		addr:         addr,
		father:       father,
		fwg:          wg,
		serviceIndex: int32(serviceIndex),
		listenconn:   listenconn,
	}

	wg.Go("Inputer listen"+" "+targetAddr, func() error {
		return input.listen(targetAddr)
	})

	loggo.Info("NewInputer ok %s service=%d", addr, serviceIndex)

	return input, nil
}

func NewSocks5Inputer(wg *thread.Group, proto string, addr string, clienttype CLIENT_TYPE, config *Config, father *ProxyConn, serviceIndex int) (*Inputer, error) {
	conn, err := network.NewConn(proto)
	if conn == nil {
		return nil, err
	}

	listenconn, err := conn.Listen(addr)
	if err != nil {
		return nil, err
	}

	input := &Inputer{
		clienttype:   clienttype,
		config:       config,
		proto:        proto,
		addr:         addr,
		father:       father,
		fwg:          wg,
		serviceIndex: int32(serviceIndex),
		listenconn:   listenconn,
	}

	wg.Go("Inputer listenSocks5"+" "+addr, func() error {
		return input.listenSocks5()
	})

	loggo.Info("NewInputer ok %s service=%d", addr, serviceIndex)

	return input, nil
}

func (i *Inputer) Close() {
	// Signal sonny to exit. Close TCP accepted conns here; UDP accepted conns are
	// owned by the listener and closed inside listenconn.Close (avoid double Close).
	i.sonny.Range(func(key, value interface{}) bool {
		s := value.(*ProxyConn)
		s.setNeedClose()
		if s.conn != nil && s.conn.Name() != "udp" {
			s.closeConn()
		}
		return true
	})
	// Close listener asynchronously so UdpConn's internal Group.Stop is not nested
	// inside an outer Group.exit callback (avoids gohome Group.isexit races).
	listen := i.listenconn
	go listen.Close()
}

func (i *Inputer) processDataFrame(f *ProxyFrame) {
	id := f.DataFrame.Id
	v, ok := i.sonny.Load(id)
	if !ok {
		loggo.Debug("Inputer processDataFrame no sonnny %s %d", id, len(f.DataFrame.Data))
		return
	}
	sonny := v.(*ProxyConn)
	if !sonny.SendSonnyData(f, i.config.MainWriteChannelTimeoutMs) {
		sonny.setNeedClose()
		loggo.Error("Inputer processDataFrame timeout sonnny %s %d", f.DataFrame.Id, len(f.DataFrame.Data))
	}
	atomic.AddInt32(&sonny.actived, 1)
	loggo.Debug("Inputer processDataFrame %s %d", f.DataFrame.Id, len(f.DataFrame.Data))
}

func (i *Inputer) processCloseFrame(f *ProxyFrame) {
	id := f.CloseFrame.Id
	v, ok := i.sonny.Load(id)
	if !ok {
		loggo.Info("Inputer processCloseFrame no sonnny %s", f.CloseFrame.Id)
		return
	}

	sonny := v.(*ProxyConn)
	sonny.SendSonnyClose(f)
}

func (i *Inputer) processOpenRspFrame(f *ProxyFrame) {
	id := f.OpenRspFrame.Id
	v, ok := i.sonny.Load(id)
	if !ok {
		loggo.Info("Inputer processOpenRspFrame no sonnny %s", id)
		return
	}
	sonny := v.(*ProxyConn)
	if f.OpenRspFrame.Ret {
		sonny.setEstablished(true)
		loggo.Info("Inputer processOpenRspFrame ok %s %s", id, sonny.conn.Info())
	} else {
		sonny.setNeedClose()
		loggo.Info("Inputer processOpenRspFrame fail %s %s", id, sonny.conn.Info())
	}
}

func (i *Inputer) listen(targetAddr string) error {

	loggo.Info("Inputer start listen %s %s", i.addr, targetAddr)

	for !isExit(i.fwg) {
		conn, err := i.listenconn.Accept()
		if err != nil {
			// Avoid busy-loop+log CPU spikes when Accept fails (e.g. EMFILE).
			loggo.Debug("Inputer listen Accept fail %s", err)
			if isExit(i.fwg) {
				break
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}

		size := i.sonnySize()
		if size >= i.config.MaxSonny {
			loggo.Info("Inputer listen max sonny %s %d", conn.Info(), size)
			conn.Close()
			continue
		}

		proxyconn := &ProxyConn{conn: conn}
		i.fwg.Go("Inputer processProxyConn"+" "+targetAddr, func() error {
			atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, 1)
			defer atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, -1)
			return i.processProxyConn(proxyconn, targetAddr)
		})
	}
	loggo.Info("Inputer end listen %s", i.addr)
	return nil
}

func (i *Inputer) listenSocks5() error {

	loggo.Info("Inputer start listenSocks5 %s", i.addr)

	for !isExit(i.fwg) {
		conn, err := i.listenconn.Accept()
		if err != nil {
			// Avoid busy-loop+log CPU spikes when Accept fails (e.g. EMFILE from CLOSE-WAIT pileup).
			loggo.Debug("Inputer listenSocks5 Accept fail %s", err)
			if isExit(i.fwg) {
				break
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}

		size := i.sonnySize()
		if size >= i.config.MaxSonny {
			loggo.Info("Inputer listen max sonny %s %d", conn.Info(), size)
			conn.Close()
			continue
		}

		proxyconn := &ProxyConn{conn: conn}
		i.fwg.Go("Inputer processSocks5Conn"+" "+conn.Info(), func() error {
			atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, 1)
			defer atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, -1)
			return i.processSocks5Conn(proxyconn)
		})
	}
	loggo.Info("Inputer end listenSocks5 %s", i.addr)
	return nil
}

func (i *Inputer) processSocks5Conn(proxyConn *ProxyConn) error {

	loggo.Debug("processSocks5Conn start %s", proxyConn.conn.Info())

	wg := thread.NewGroup("Inputer processSocks5Conn"+" "+proxyConn.conn.Info(), i.fwg, func() {
		loggo.Debug("group start exit %s", proxyConn.conn.Info())
		proxyConn.closeConn()
		loggo.Debug("group end exit %s", proxyConn.conn.Info())
	})

	targetAddr := ""
	wg.Go("Inputer socks5"+" "+proxyConn.conn.Info(), func() error {
		if proxyConn.conn.Name() != "tcp" {
			loggo.Error("processSocks5Conn no tcp %s %s", proxyConn.conn.Info(), proxyConn.conn.Name())
			return errors.New("socks5 not tcp")
		}

		var err error = nil
		if err = network.Sock5HandshakeBy(proxyConn.conn, i.config.Username, i.config.Password); err != nil {
			loggo.Error("processSocks5Conn Sock5HandshakeBy %s %s", proxyConn.conn.Info(), err)
			return err
		}
		_, addr, err := network.Sock5GetRequest(proxyConn.conn)
		if err != nil {
			loggo.Error("processSocks5Conn Sock5GetRequest %s %s", proxyConn.conn.Info(), err)
			return err
		}
		// Sending connection established message immediately to client.
		// This some round trip time for creating socks connection with the client.
		// But if connection failed, the client will get connection reset error.
		_, err = proxyConn.conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
		if err != nil {
			loggo.Error("processSocks5Conn Write %s %s", proxyConn.conn.Info(), err)
			return err
		}

		targetAddr = addr
		return nil
	})

	err := wg.Wait()
	if err != nil {
		return nil
	}

	loggo.Debug("processSocks5Conn ok %s %s", proxyConn.conn.Info(), targetAddr)

	i.fwg.Go("Inputer processProxyConn"+" "+proxyConn.conn.Info(), func() error {
		return i.processProxyConn(proxyConn, targetAddr)
	})

	return nil
}

func (i *Inputer) processProxyConn(proxyConn *ProxyConn, targetAddr string) error {

	proxyConn.id = common.UniqueId()

	loggo.Info("Inputer processProxyConn start %s %s %s", proxyConn.id, proxyConn.conn.Info(), targetAddr)

	_, loaded := i.sonny.LoadOrStore(proxyConn.id, proxyConn)
	if loaded {
		loggo.Error("Inputer processProxyConn LoadOrStore fail %s", proxyConn.id)
		proxyConn.conn.Close()
		return nil
	}
	atomic.AddInt32(&i.sonnyNum, 1)

	sendch := newMsgChannel(i.config.ConnBuffer)
	recvch := newMsgChannel(i.config.ConnBuffer)

	proxyConn.sendch = sendch
	proxyConn.recvch = recvch

	wg := thread.NewGroup("Inputer processProxyConn"+" "+proxyConn.conn.Info(), i.fwg, func() {
		loggo.Info("group start exit %s", proxyConn.conn.Info())
		// UDP sonny is closed by the listener; TCP sonny closed here / Inputer.Close.
		if proxyConn.conn != nil && proxyConn.conn.Name() != "udp" {
			proxyConn.closeConn()
		}
		proxyConn.CloseChannels()
		loggo.Info("group end exit %s", proxyConn.conn.Info())
	})

	i.openConn(proxyConn, targetAddr)

	wg.Go("Inputer recvFromSonny"+" "+proxyConn.conn.Info(), func() error {
		return recvFromSonny(wg, proxyConn, proxyConn.conn, i.config.MaxMsgSize)
	})

	wg.Go("Inputer sendToSonny"+" "+proxyConn.conn.Info(), func() error {
		return sendToSonny(wg, sendch, proxyConn.conn, i.config.MaxMsgSize)
	})

	wg.Go("Inputer checkSonnyActive"+" "+proxyConn.conn.Info(), func() error {
		return checkSonnyActive(wg, proxyConn, i.config.EstablishedTimeout, i.config.ConnTimeout)
	})

	wg.Go("Inputer checkNeedClose"+" "+proxyConn.conn.Info(), func() error {
		return checkNeedClose(wg, proxyConn)
	})

	wg.Go("Inputer copySonnyRecv"+" "+proxyConn.conn.Info(), func() error {
		return copySonnyRecv(wg, recvch, proxyConn, i.father)
	})

	wg.Wait()
	// Belt-and-suspenders: if all workers returned nil without Group.exit,
	// still close the local socket (closeOnce makes this safe with exitfunc).
	if proxyConn.conn != nil && proxyConn.conn.Name() != "udp" {
		proxyConn.closeConn()
	}
	if _, ok := i.sonny.LoadAndDelete(proxyConn.id); ok {
		atomic.AddInt32(&i.sonnyNum, -1)
	}

	closeRemoteConn(proxyConn, i.father)

	loggo.Info("Inputer processProxyConn end %s %s %s", proxyConn.id, proxyConn.conn.Info(), targetAddr)

	return nil
}

func (i *Inputer) hasSonny(id string) bool {
	_, ok := i.sonny.Load(id)
	return ok
}

func (i *Inputer) openConn(proxyConn *ProxyConn, targetAddr string) {
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_OPEN
	f.OpenFrame = &OpenConnFrame{}
	f.OpenFrame.Id = proxyConn.id
	f.OpenFrame.Toaddr = targetAddr
	f.OpenFrame.ServiceIndex = i.serviceIndex
	if p, ok := PROXY_PROTO_value[strings.ToUpper(i.proto)]; ok {
		f.OpenFrame.Proxyproto = PROXY_PROTO(p)
	}

	i.father.SendFrame(f)
	loggo.Info("Inputer openConn %s %s proto=%s service=%d", proxyConn.id, targetAddr, i.proto, i.serviceIndex)
}

func (i *Inputer) sonnySize() int {
	n := atomic.LoadInt32(&i.sonnyNum)
	if n < 0 {
		return 0
	}
	return int(n)
}
