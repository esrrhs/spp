package proxy

import (
	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/conn"
	"github.com/esrrhs/gohome/group"
	"github.com/esrrhs/gohome/loggo"
	"os"
	"sync"
	"sync/atomic"
)

type Outputer struct {
	clienttype CLIENT_TYPE
	config     *Config
	proto      string
	father     *ProxyConn
	fwg        *group.Group

	conn  conn.Conn
	sonny sync.Map

	ss bool
}

func NewOutputer(wg *group.Group, proto string, clienttype CLIENT_TYPE, config *Config, father *ProxyConn) (*Outputer, error) {
	conn, err := conn.NewConn(proto)
	if conn == nil {
		return nil, err
	}

	output := &Outputer{
		clienttype: clienttype,
		config:     config,
		conn:       conn,
		proto:      proto,
		father:     father,
		fwg:        wg,
	}

	loggo.Info("NewOutputer ok %s", proto)

	return output, nil
}

func NewSSOutputer(wg *group.Group, proto string, clienttype CLIENT_TYPE, config *Config, father *ProxyConn) (*Outputer, error) {
	conn, err := conn.NewConn(proto)
	if conn == nil {
		return nil, err
	}

	output := &Outputer{
		clienttype: clienttype,
		config:     config,
		conn:       conn,
		proto:      proto,
		father:     father,
		fwg:        wg,
		ss:         true,
	}

	loggo.Info("NewSSOutputer ok %s", proto)

	return output, nil
}

func (o *Outputer) Close() {
	o.conn.Close()
}

func (o *Outputer) processDataFrame(f *ProxyFrame) {
	id := f.DataFrame.Id
	v, ok := o.sonny.Load(id)
	if !ok {
		loggo.Debug("Outputer processDataFrame no sonnny %s %d", f.DataFrame.Id, len(f.DataFrame.Data))
		return
	}
	sonny := v.(*ProxyConn)
	if !sonny.sendch.WriteTimeout(f, o.config.MainWriteChannelTimeoutMs) {
		sonny.needclose = true
		loggo.Error("Outputer processDataFrame timeout sonnny %s %d", f.DataFrame.Id, len(f.DataFrame.Data))
	}
	sonny.actived++
	loggo.Debug("Outputer processDataFrame %s %d", f.DataFrame.Id, len(f.DataFrame.Data))
}

func (o *Outputer) processCloseFrame(f *ProxyFrame) {
	id := f.CloseFrame.Id
	v, ok := o.sonny.Load(id)
	if !ok {
		loggo.Info("Outputer processCloseFrame no sonnny %s", f.CloseFrame.Id)
		return
	}

	sonny := v.(*ProxyConn)
	sonny.sendch.Write(f)
}

func (o *Outputer) open(proxyconn *ProxyConn, targetAddr string) bool {

	id := proxyconn.id

	loggo.Info("Outputer open start %s %s", id, targetAddr)

	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_OPENRSP
	rf.OpenRspFrame = &OpenConnRspFrame{}
	rf.OpenRspFrame.Id = id

	c, err := conn.NewConn(o.conn.Name())
	if err != nil {
		rf.OpenRspFrame.Ret = false
		rf.OpenRspFrame.Msg = "NewConn fail " + targetAddr
		o.father.sendch.Write(rf)
		loggo.Error("Outputer open NewConn fail %s %s", targetAddr, err.Error())
		return false
	}

	wg := group.NewGroup("Outputer open"+" "+targetAddr, o.fwg, func() {
		loggo.Info("group start exit %s", c.Info())
		c.Close()
		loggo.Info("group end exit %s", c.Info())
	})

	var conn conn.Conn
	wg.Go("Outputer Dial"+" "+targetAddr, func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		cc, err := c.Dial(targetAddr)
		if err != nil {
			return err
		}
		conn = cc
		return nil
	})

	err = wg.Wait()
	if err != nil {
		rf.OpenRspFrame.Ret = false
		rf.OpenRspFrame.Msg = "Dial fail " + targetAddr
		o.father.sendch.Write(rf)
		loggo.Error("Outputer open Dial fail %s %s", targetAddr, err.Error())
		return false
	}

	loggo.Info("Outputer open Dial ok %s %s", id, targetAddr)

	proxyconn.conn = conn

	rf.OpenRspFrame.Ret = true
	rf.OpenRspFrame.Msg = "ok"
	o.father.sendch.Write(rf)

	return true
}

func (o *Outputer) processOpenFrame(f *ProxyFrame) {

	id := f.OpenFrame.Id
	targetAddr := f.OpenFrame.Toaddr

	rf := &ProxyFrame{}
	rf.Type = FRAME_TYPE_OPENRSP
	rf.OpenRspFrame = &OpenConnRspFrame{}
	rf.OpenRspFrame.Id = id
	rf.OpenRspFrame.Ret = false

	if o.ss {
		ss_local_host := os.Getenv("SS_LOCAL_HOST")
		ss_local_port := os.Getenv("SS_LOCAL_PORT")
		if len(ss_local_host) <= 0 || len(ss_local_port) <= 0 {
			rf.OpenRspFrame.Msg = "ss no env"
			o.father.sendch.Write(rf)
			loggo.Info("Outputer ss no env %s %s", ss_local_host, ss_local_port)
			return
		}
		targetAddr = ss_local_host + ":" + ss_local_port
	}

	size := o.sonnySize()
	if size >= o.config.MaxSonny {
		rf.OpenRspFrame.Msg = "max sonny"
		o.father.sendch.Write(rf)
		loggo.Info("Outputer listen max sonny %s %d", id, size)
		return
	}

	proxyconn := &ProxyConn{id: id, conn: nil, established: true}
	_, loaded := o.sonny.LoadOrStore(proxyconn.id, proxyconn)
	if loaded {
		rf.OpenRspFrame.Msg = "Conn id fail"
		o.father.sendch.Write(rf)
		loggo.Error("Outputer processOpenFrame LoadOrStore fail %s %s", targetAddr, id)
		return
	}

	sendch := common.NewChannel(o.config.ConnBuffer)
	recvch := common.NewChannel(o.config.ConnBuffer)

	proxyconn.sendch = sendch
	proxyconn.recvch = recvch

	o.fwg.Go("Outputer processProxyConn"+" "+targetAddr, func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return o.processProxyConn(proxyconn, targetAddr)
	})
}

func (o *Outputer) processProxyConn(proxyConn *ProxyConn, targetAddr string) error {

	loggo.Info("Outputer processProxyConn start %s %s", proxyConn.id, targetAddr)

	sendch := proxyConn.sendch
	recvch := proxyConn.recvch

	if !o.open(proxyConn, targetAddr) {
		sendch.Close()
		recvch.Close()
		return nil
	}

	loggo.Info("Outputer processProxyConn open ok %s %s", proxyConn.id, proxyConn.conn.Info())

	wg := group.NewGroup("Outputer processProxyConn"+" "+proxyConn.conn.Info(), o.fwg, func() {
		loggo.Info("group start exit %s", proxyConn.conn.Info())
		proxyConn.conn.Close()
		sendch.Close()
		recvch.Close()
		loggo.Info("group end exit %s", proxyConn.conn.Info())
	})

	wg.Go("Outputer recvFromSonny"+" "+proxyConn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return recvFromSonny(wg, recvch, proxyConn.conn, o.config.MaxMsgSize)
	})

	wg.Go("Outputer sendToSonny"+" "+proxyConn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return sendToSonny(wg, sendch, proxyConn.conn, o.config.MaxMsgSize)
	})

	wg.Go("Outputer checkSonnyActive"+" "+proxyConn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return checkSonnyActive(wg, proxyConn, o.config.EstablishedTimeout, o.config.ConnTimeout)
	})

	wg.Go("Outputer checkNeedClose"+" "+proxyConn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return checkNeedClose(wg, proxyConn)
	})

	wg.Go("Outputer copySonnyRecv"+" "+proxyConn.conn.Info(), func() error {
		atomic.AddInt32(&gStateThreadNum.ThreadNum, 1)
		defer atomic.AddInt32(&gStateThreadNum.ThreadNum, -1)
		return copySonnyRecv(wg, recvch, proxyConn, o.father)
	})

	wg.Wait()
	o.sonny.Delete(proxyConn.id)

	closeRemoteConn(proxyConn, o.father)

	loggo.Info("Outputer processProxyConn end %s %s", proxyConn.id, proxyConn.conn.Info())

	return nil
}

func (o *Outputer) sonnySize() int {
	size := 0
	o.sonny.Range(func(key, value interface{}) bool {
		size++
		return true
	})
	return size
}
