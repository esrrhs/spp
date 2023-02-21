package proxy

import (
	"encoding/binary"
	"errors"
	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/conn"
	"github.com/esrrhs/gohome/group"
	"github.com/esrrhs/gohome/loggo"
	"github.com/golang/protobuf/proto"
	"io"
	"strconv"
	"sync/atomic"
	"time"
)

type Config struct {
	MaxMsgSize                int    // 消息最大长度
	MainBuffer                int    // 主通道buffer最大长度
	ConnBuffer                int    // 每个conn buffer最大长度
	EstablishedTimeout        int    // 主通道登录超时
	PingInter                 int    // 主通道ping间隔
	PingTimeoutInter          int    // 主通道ping超时间隔
	ConnTimeout               int    // 每个conn的不活跃超时时间
	ConnectTimeout            int    // 每个conn的连接超时
	Key                       string // 连接密码
	Encrypt                   string // 加密密钥
	Compress                  int    // 压缩设置
	ShowPing                  bool   // 是否显示ping
	Username                  string // 登录用户名
	Password                  string // 登录密码
	MaxClient                 int    // 最大客户端数目
	MaxSonny                  int    // 最大连接数目
	MainWriteChannelTimeoutMs int    // 主通道转发消息超时
	Congestion                string // 拥塞算法
}

func DefaultConfig() *Config {
	return &Config{
		MaxMsgSize:                1024 * 1024,
		MainBuffer:                64,
		ConnBuffer:                16,
		EstablishedTimeout:        10,
		PingInter:                 1,
		PingTimeoutInter:          30,
		ConnTimeout:               60,
		ConnectTimeout:            10,
		Key:                       "123456",
		Encrypt:                   "default",
		Compress:                  128,
		ShowPing:                  false,
		Username:                  "",
		Password:                  "",
		MaxClient:                 8,
		MaxSonny:                  128,
		MainWriteChannelTimeoutMs: 1000,
		Congestion:                "bb",
	}
}

type ProxyConn struct {
	conn        conn.Conn
	established bool
	sendch      *common.Channel // *ProxyFrame
	recvch      *common.Channel // *ProxyFrame
	actived     int
	pinged      int
	id          string
	needclose   bool
}

func checkProxyFame(f *ProxyFrame) error {
	switch f.Type {
	case FRAME_TYPE_LOGIN:
		if f.LoginFrame == nil {
			return errors.New("LoginFrame nil")
		}
	case FRAME_TYPE_LOGINRSP:
		if f.LoginRspFrame == nil {
			return errors.New("LoginRspFrame nil")
		}
	case FRAME_TYPE_DATA:
		if f.DataFrame == nil {
			return errors.New("DataFrame nil")
		}
	case FRAME_TYPE_PING:
		if f.PingFrame == nil {
			return errors.New("PingFrame nil")
		}
	case FRAME_TYPE_PONG:
		if f.PongFrame == nil {
			return errors.New("PongFrame nil")
		}
	case FRAME_TYPE_OPEN:
		if f.OpenFrame == nil {
			return errors.New("OpenFrame nil")
		}
	case FRAME_TYPE_OPENRSP:
		if f.OpenRspFrame == nil {
			return errors.New("OpenRspFrame nil")
		}
	case FRAME_TYPE_CLOSE:
		if f.CloseFrame == nil {
			return errors.New("CloseFrame nil")
		}
	default:
		return errors.New("Type error")
	}

	return nil
}

func MarshalSrpFrame(f *ProxyFrame, compress int, encrpyt string) ([]byte, error) {

	err := checkProxyFame(f)
	if err != nil {
		return nil, err
	}

	if f.Type == FRAME_TYPE_DATA && compress > 0 && len(f.DataFrame.Data) > compress && !f.DataFrame.Compress {
		newb := common.CompressData(f.DataFrame.Data)
		if len(newb) < len(f.DataFrame.Data) {
			if loggo.IsDebug() {
				loggo.Debug("MarshalSrpFrame Compress from %d %d", len(f.DataFrame.Data), len(newb))
			}
			atomic.AddInt64(&gState.SendCompSaveSize, int64(len(f.DataFrame.Data)-len(newb)))
			f.DataFrame.Data = newb
			f.DataFrame.Compress = true
		}
	}

	if f.Type == FRAME_TYPE_DATA && encrpyt != "" {
		newb, err := common.Rc4(encrpyt, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("MarshalSrpFrame Rc4 from %s %s", common.GetCrc32(f.DataFrame.Data), common.GetCrc32(newb))
		}
		f.DataFrame.Data = newb
	}

	mb, err := proto.Marshal(f)
	if err != nil {
		return nil, err
	}
	return mb, err
}

func UnmarshalSrpFrame(b []byte, encrpyt string) (*ProxyFrame, error) {

	f := &ProxyFrame{}
	err := proto.Unmarshal(b, f)
	if err != nil {
		return nil, err
	}

	err = checkProxyFame(f)
	if err != nil {
		return nil, err
	}

	if f.Type == FRAME_TYPE_DATA && encrpyt != "" {
		newb, err := common.Rc4(encrpyt, f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("UnmarshalSrpFrame Rc4 from %s %s", common.GetCrc32(f.DataFrame.Data), common.GetCrc32(newb))
		}
		f.DataFrame.Data = newb
	}

	if f.Type == FRAME_TYPE_DATA && f.DataFrame.Compress {
		newb, err := common.DeCompressData(f.DataFrame.Data)
		if err != nil {
			return nil, err
		}
		if loggo.IsDebug() {
			loggo.Debug("UnmarshalSrpFrame Compress from %d %d", len(f.DataFrame.Data), len(newb))
		}
		atomic.AddInt64(&gState.RecvCompSaveSize, int64(len(newb)-len(f.DataFrame.Data)))
		f.DataFrame.Data = newb
		f.DataFrame.Compress = false
	}

	return f, nil
}

const (
	MAX_PROTO_PACK_SIZE = 100
)

func recvFrom(wg *group.Group, recvch *common.Channel, conn conn.Conn, maxmsgsize int, encrypt string) error {

	atomic.AddInt32(&gStateThreadNum.RecvThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.RecvThread, -1)

	loggo.Info("recvFrom start %s", conn.Info())
	bs := make([]byte, 4)
	ds := make([]byte, maxmsgsize+MAX_PROTO_PACK_SIZE)

	for !wg.IsExit() {
		atomic.AddInt32(&gState.RecvFrames, 1)

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start ReadFull len %s", conn.Info())
		}
		_, err := io.ReadFull(conn, bs)
		if err != nil {
			loggo.Info("recvFrom ReadFull fail: %s %s", conn.Info(), err.Error())
			return err
		}

		msglen := binary.LittleEndian.Uint32(bs)
		if msglen > uint32(maxmsgsize)+MAX_PROTO_PACK_SIZE || msglen <= 0 {
			loggo.Error("recvFrom len fail: %s %d", conn.Info(), msglen)
			return errors.New("msg len fail " + strconv.Itoa(int(msglen)))
		}

		gDeadLock.recvTime = time.Now()
		gDeadLock.recving = true

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start ReadFull body %s %d", conn.Info(), msglen)
		}
		_, err = io.ReadFull(conn, ds[0:msglen])
		if err != nil {
			loggo.Info("recvFrom ReadFull fail: %s %s", conn.Info(), err.Error())
			return err
		}

		f, err := UnmarshalSrpFrame(ds[0:msglen], encrypt)
		if err != nil {
			loggo.Error("recvFrom UnmarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if loggo.IsDebug() {
			loggo.Debug("recvFrom start Write %s", conn.Info())
		}
		recvch.Write(f)

		if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
			loggo.Debug("recvFrom %s %s", conn.Info(), f.Type.String())
			if f.Type == FRAME_TYPE_DATA {
				if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
					loggo.Error("recvFrom crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
					return errors.New("conn crc error")
				}
			}
		}

		atomic.AddInt32(&gState.MainRecvNum, 1)
		atomic.AddInt64(&gState.MainRecvSize, int64(msglen)+4)

		gDeadLock.recving = false
	}

	loggo.Info("recvFrom end %s", conn.Info())
	return nil
}

func sendTo(wg *group.Group, sendch *common.Channel, conn conn.Conn, compress int, maxmsgsize int, encrypt string, pingflag *int32, pongflag *int32, pongtime *int64) error {

	atomic.AddInt32(&gStateThreadNum.SendThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.SendThread, -1)

	loggo.Info("sendTo start %s", conn.Info())
	bs := make([]byte, 4)

	for !wg.IsExit() {
		atomic.AddInt32(&gState.SendFrames, 1)

		var f *ProxyFrame
		if *pingflag > 0 {
			*pingflag = 0
			f = &ProxyFrame{}
			f.Type = FRAME_TYPE_PING
			f.PingFrame = &PingFrame{}
			f.PingFrame.Time = time.Now().UnixNano()
		} else if *pongflag > 0 {
			*pongflag = 0
			f = &ProxyFrame{}
			f.Type = FRAME_TYPE_PONG
			f.PongFrame = &PongFrame{}
			f.PongFrame.Time = *pongtime
		} else {
			exit := false
			select {
			case ff := <-sendch.Ch():
				if ff == nil {
					exit = true
					break
				}
				f = ff.(*ProxyFrame)
			case <-time.After(time.Second):
				break
			}
			if f == nil {
				if exit {
					break
				}
				continue
			}
		}
		mb, err := MarshalSrpFrame(f, compress, encrypt)
		if err != nil {
			loggo.Error("sendTo MarshalSrpFrame fail: %s %s", conn.Info(), err.Error())
			return err
		}

		msglen := uint32(len(mb))
		if msglen > uint32(maxmsgsize)+MAX_PROTO_PACK_SIZE || msglen <= 0 {
			loggo.Error("sendTo len fail: %s %d", conn.Info(), msglen)
			return errors.New("msg len fail " + strconv.Itoa(int(msglen)))
		}

		gDeadLock.sendTime = time.Now()
		gDeadLock.sending = true

		if loggo.IsDebug() {
			loggo.Debug("sendTo start Write len %s", conn.Info())
		}
		binary.LittleEndian.PutUint32(bs, msglen)
		_, err = conn.Write(bs)
		if err != nil {
			loggo.Info("sendTo Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if loggo.IsDebug() {
			loggo.Debug("sendTo start Write body %s %d", conn.Info(), msglen)
		}
		n, err := conn.Write(mb)
		if err != nil {
			loggo.Info("sendTo Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if n != len(mb) {
			loggo.Error("sendTo Write len fail: %s %d %d", conn.Info(), n, len(mb))
			return errors.New("len error")
		}

		if f.Type != FRAME_TYPE_PING && f.Type != FRAME_TYPE_PONG && loggo.IsDebug() {
			loggo.Debug("sendTo %s %s", conn.Info(), f.Type.String())
			if f.Type == FRAME_TYPE_DATA {
				if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
					loggo.Error("sendTo crc error %s %s %s %p", conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc, f)
					return errors.New("conn crc error")
				}
			}
		}

		atomic.AddInt32(&gState.MainSendNum, 1)
		atomic.AddInt64(&gState.MainSendSize, int64(msglen)+4)

		gDeadLock.sending = false
	}
	loggo.Info("sendTo end %s", conn.Info())
	return nil
}

const (
	MAX_INDEX = 1024
)

func recvFromSonny(wg *group.Group, recvch *common.Channel, conn conn.Conn, maxmsgsize int) error {

	atomic.AddInt32(&gStateThreadNum.RecvSonnyThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.RecvSonnyThread, -1)

	loggo.Info("recvFromSonny start %s", conn.Info())
	ds := make([]byte, maxmsgsize)

	index := int32(0)
	for !wg.IsExit() {
		atomic.AddInt32(&gState.RecvSonnyFrames, 1)

		msglen, err := conn.Read(ds)
		if err != nil {
			loggo.Info("recvFromSonny Read fail: %s %s", conn.Info(), err.Error())
			if err == io.EOF {
				return nil
			}
			return err
		}

		if msglen <= 0 {
			loggo.Error("recvFromSonny len error: %s %d", conn.Info(), msglen)
			return errors.New("len error " + strconv.Itoa(msglen))
		}

		f := &ProxyFrame{}
		f.Type = FRAME_TYPE_DATA
		f.DataFrame = &DataFrame{}
		f.DataFrame.Data = make([]byte, msglen)
		copy(f.DataFrame.Data, ds[0:msglen])
		f.DataFrame.Compress = false
		if loggo.IsDebug() {
			f.DataFrame.Crc = common.GetCrc32(f.DataFrame.Data)
		}
		index++
		f.DataFrame.Index = index % MAX_INDEX

		recvch.Write(f)

		if loggo.IsDebug() {
			loggo.Debug("recvFromSonny %s %d %s %d %p", conn.Info(), msglen, f.DataFrame.Crc, f.DataFrame.Index, f)
		}

		atomic.AddInt32(&gState.RecvNum, 1)
		atomic.AddInt64(&gState.RecvSize, int64(len(f.DataFrame.Data)))
	}
	loggo.Info("recvFromSonny end %s", conn.Info())
	return nil
}

func sendToSonny(wg *group.Group, sendch *common.Channel, conn conn.Conn, maxmsgsize int) error {

	atomic.AddInt32(&gStateThreadNum.SendSonnyThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.SendSonnyThread, -1)

	loggo.Info("sendToSonny start %s", conn.Info())
	index := int32(0)
	for !wg.IsExit() {
		atomic.AddInt32(&gState.SendSonnyFrames, 1)

		ff := <-sendch.Ch()
		if ff == nil {
			break
		}
		f := ff.(*ProxyFrame)
		if f.Type == FRAME_TYPE_CLOSE {
			loggo.Info("sendToSonny close by remote: %s", conn.Info())
			return errors.New("close by remote")
		}
		if f.DataFrame.Compress {
			loggo.Error("sendToSonny Compress error: %s", conn.Info())
			return errors.New("msg compress error")
		}

		if len(f.DataFrame.Data) <= 0 {
			loggo.Error("sendToSonny len error: %s %d", conn.Info(), len(f.DataFrame.Data))
			return errors.New("len error " + strconv.Itoa(len(f.DataFrame.Data)))
		}

		if len(f.DataFrame.Data) > maxmsgsize {
			loggo.Error("sendToSonny len error: %s %d", conn.Info(), len(f.DataFrame.Data))
			return errors.New("len error " + strconv.Itoa(len(f.DataFrame.Data)))
		}

		if loggo.IsDebug() {
			if f.DataFrame.Crc != common.GetCrc32(f.DataFrame.Data) {
				loggo.Error("sendToSonny crc error: %s %d %s %s", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Crc, common.GetCrc32(f.DataFrame.Data))
				return errors.New("crc error")
			}
		}

		index++
		index = index % MAX_INDEX
		if f.DataFrame.Index != index {
			loggo.Error("sendToSonny index error: %s %d %d %d", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Index, index)
			return errors.New("index error")
		}

		n, err := conn.Write(f.DataFrame.Data)
		if err != nil {
			loggo.Info("sendToSonny Write fail: %s %s", conn.Info(), err.Error())
			return err
		}

		if n != len(f.DataFrame.Data) {
			loggo.Error("sendToSonny Write len fail: %s %d %d", conn.Info(), n, len(f.DataFrame.Data))
			return errors.New("len error")
		}

		if loggo.IsDebug() {
			loggo.Debug("sendToSonny %s %d %s %d", conn.Info(), len(f.DataFrame.Data), f.DataFrame.Crc, f.DataFrame.Index)
		}

		atomic.AddInt32(&gState.SendNum, 1)
		atomic.AddInt64(&gState.SendSize, int64(len(f.DataFrame.Data)))
	}
	loggo.Info("sendToSonny end %s", conn.Info())
	return nil
}

func checkPingActive(wg *group.Group, sendch *common.Channel, recvch *common.Channel, proxyconn *ProxyConn,
	estimeout int, pinginter int, pingintertimeout int, showping bool, pingflag *int32) error {

	atomic.AddInt32(&gStateThreadNum.CheckThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.CheckThread, -1)

	loggo.Info("checkPingActive start %s", proxyconn.conn.Info())

	begin := time.Now()
	for !wg.IsExit() {
		atomic.AddInt32(&gState.CheckFrames, 1)

		if !proxyconn.established {
			if time.Now().Sub(begin) > time.Second*time.Duration(estimeout) {
				loggo.Info("checkPingActive established timeout %s", proxyconn.conn.Info())
				return errors.New("established timeout")
			}
		} else {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	begin = time.Now()
	for !wg.IsExit() {
		atomic.AddInt32(&gState.CheckFrames, 1)

		if time.Now().Sub(begin) > time.Duration(pinginter)*time.Second {
			begin = time.Now()

			if proxyconn.pinged > pingintertimeout {
				loggo.Info("checkPingActive ping pong timeout %s", proxyconn.conn.Info())
				return errors.New("ping pong timeout")
			}

			atomic.AddInt32(pingflag, 1)

			proxyconn.pinged++
			if showping {
				loggo.Info("ping %s", proxyconn.conn.Info())
			}
		}
		time.Sleep(time.Millisecond * 100)
	}

	loggo.Info("checkPingActive end %s", proxyconn.conn.Info())
	return nil
}

func checkNeedClose(wg *group.Group, proxyconn *ProxyConn) error {

	atomic.AddInt32(&gStateThreadNum.CheckThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.CheckThread, -1)

	loggo.Info("checkNeedClose start %s", proxyconn.conn.Info())

	for !wg.IsExit() {
		atomic.AddInt32(&gState.CheckFrames, 1)

		if proxyconn.needclose {
			loggo.Error("checkNeedClose needclose %s", proxyconn.conn.Info())
			return errors.New("needclose")
		}
		time.Sleep(time.Millisecond * 100)
	}

	loggo.Info("checkNeedClose end %s", proxyconn.conn.Info())

	return nil
}

func processPing(f *ProxyFrame, sendch *common.Channel, proxyconn *ProxyConn, pongflag *int32, pongtime *int64) {
	atomic.AddInt32(pongflag, 1)
	*pongtime = f.PingFrame.Time
}

func processPong(f *ProxyFrame, sendch *common.Channel, proxyconn *ProxyConn, showping bool) {
	elapse := time.Duration(time.Now().UnixNano() - f.PongFrame.Time)
	proxyconn.pinged = 0
	if showping {
		loggo.Info("pong %s %s", proxyconn.conn.Info(), elapse.String())
	}
}

func checkSonnyActive(wg *group.Group, proxyconn *ProxyConn, estimeout int, timeout int) error {

	atomic.AddInt32(&gStateThreadNum.CheckThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.CheckThread, -1)

	loggo.Info("checkSonnyActive start %s", proxyconn.conn.Info())

	begin := time.Now()
	for !wg.IsExit() {
		atomic.AddInt32(&gState.CheckFrames, 1)

		if !proxyconn.established {
			if time.Now().Sub(begin) > time.Second*time.Duration(estimeout) {
				loggo.Error("checkSonnyActive established timeout %s", proxyconn.conn.Info())
				return errors.New("established timeout")
			}
		} else {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	begin = time.Now()
	for !wg.IsExit() {
		atomic.AddInt32(&gState.CheckFrames, 1)

		if time.Now().Sub(begin) > time.Second*time.Duration(timeout) {
			if proxyconn.actived == 0 {
				loggo.Error("checkSonnyActive timeout %s", proxyconn.conn.Info())
				return errors.New("conn timeout")
			}
			proxyconn.actived = 0
			begin = time.Now()
		}
		time.Sleep(time.Millisecond * 100)
	}
	loggo.Info("checkSonnyActive end %s", proxyconn.conn.Info())
	return nil
}

func copySonnyRecv(wg *group.Group, recvch *common.Channel, proxyConn *ProxyConn, father *ProxyConn) error {

	atomic.AddInt32(&gStateThreadNum.CopyThread, 1)
	defer atomic.AddInt32(&gStateThreadNum.CopyThread, -1)

	loggo.Info("copySonnyRecv start %s", proxyConn.conn.Info())

	for !wg.IsExit() {
		atomic.AddInt32(&gState.CopyFrames, 1)

		ff := <-recvch.Ch()
		if ff == nil {
			break
		}
		f := ff.(*ProxyFrame)
		if f.Type != FRAME_TYPE_DATA {
			loggo.Error("copySonnyRecv type error %s %d", proxyConn.conn.Info(), f.Type)
			return errors.New("conn type error")
		}
		if f.DataFrame.Compress {
			loggo.Error("copySonnyRecv compress error %s %d", proxyConn.conn.Info(), f.Type)
			return errors.New("conn compress error")
		}
		if loggo.IsDebug() {
			if common.GetCrc32(f.DataFrame.Data) != f.DataFrame.Crc {
				loggo.Error("copySonnyRecv crc error %s %s %s", proxyConn.conn.Info(), common.GetCrc32(f.DataFrame.Data), f.DataFrame.Crc)
				return errors.New("conn crc error")
			}
		}
		f.DataFrame.Id = proxyConn.id
		proxyConn.actived++

		father.sendch.Write(f)

		loggo.Debug("copySonnyRecv %s %d %s %p", proxyConn.id, len(f.DataFrame.Data), f.DataFrame.Crc, f)
	}
	loggo.Info("copySonnyRecv end %s", proxyConn.conn.Info())
	return nil
}

func closeRemoteConn(proxyConn *ProxyConn, father *ProxyConn) {
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_CLOSE
	f.CloseFrame = &CloseFrame{}
	f.CloseFrame.Id = proxyConn.id

	father.sendch.Write(f)
	loggo.Info("closeConn %s", proxyConn.id)
}

type StateThreadNum struct {
	ThreadNum       int32
	RecvThread      int32
	SendThread      int32
	RecvSonnyThread int32
	SendSonnyThread int32
	CopyThread      int32
	CheckThread     int32
}

type State struct {
	RecvFrames      int32
	SendFrames      int32
	RecvSonnyFrames int32
	SendSonnyFrames int32
	CopyFrames      int32
	CheckFrames     int32

	RecvFps      int32
	SendFps      int32
	RecvSonnyFps int32
	SendSonnyFps int32
	CopyFps      int32
	CheckFps     int32

	MainRecvNum  int32
	MainSendNum  int32
	MainRecvSize int64
	MainSendSize int64
	RecvNum      int32
	SendNum      int32
	RecvSize     int64
	SendSize     int64

	RecvCompSaveSize int64
	SendCompSaveSize int64
}

type DeadLock struct {
	sending  bool
	sendTime time.Time
	recving  bool
	recvTime time.Time
}

var gStateThreadNum StateThreadNum
var gState State
var gDeadLock DeadLock

func showState(wg *group.Group) error {
	loggo.Info("showState start ")
	begin := time.Now()
	for !wg.IsExit() {
		dur := time.Now().Sub(begin)
		if dur > time.Minute {
			begin = time.Now()

			dur := int32(dur / time.Second)

			if gStateThreadNum.RecvThread > 0 {
				gState.RecvFps = gState.RecvFrames / gStateThreadNum.RecvThread / dur
			} else {
				gState.RecvFps = 0
			}
			if gStateThreadNum.SendThread > 0 {
				gState.SendFps = gState.SendFrames / gStateThreadNum.SendThread / dur
			} else {
				gState.SendFps = 0
			}
			if gStateThreadNum.RecvSonnyThread > 0 {
				gState.RecvSonnyFps = gState.RecvSonnyFrames / gStateThreadNum.RecvSonnyThread / dur
			} else {
				gState.RecvSonnyFps = 0
			}
			if gStateThreadNum.SendSonnyThread > 0 {
				gState.SendSonnyFps = gState.SendSonnyFrames / gStateThreadNum.SendSonnyThread / dur
			} else {
				gState.SendSonnyFps = 0
			}
			if gStateThreadNum.CopyThread > 0 {
				gState.CopyFps = gState.CopyFrames / gStateThreadNum.CopyThread / dur
			} else {
				gState.CopyFps = 0
			}
			if gStateThreadNum.CheckThread > 0 {
				gState.CheckFps = gState.CheckFrames / gStateThreadNum.CheckThread / dur
			} else {
				gState.CheckFps = 0
			}

			loggo.Info("showState\n%s\n%s", common.StructToTable(&gStateThreadNum), common.StructToTable(&gState))

			gState = State{}
		}
		time.Sleep(time.Second)
	}
	loggo.Info("showState end")
	return nil
}

func checkDeadLock(wg *group.Group) error {
	loggo.Info("checkDeadLock start ")
	begin := time.Now()
	for !wg.IsExit() {
		dur := time.Now().Sub(begin)
		if dur > time.Second {
			begin = time.Now()

			if gDeadLock.sending && time.Now().Sub(gDeadLock.sendTime) > 5*time.Second {
				loggo.Error("send dead lock %v", time.Now().Sub(gDeadLock.sendTime))
			}
			if gDeadLock.recving && time.Now().Sub(gDeadLock.recvTime) > 5*time.Second {
				loggo.Error("recv dead lock")
				loggo.Error("send dead lock %v", time.Now().Sub(gDeadLock.recvTime))
			}
		}
		time.Sleep(time.Millisecond * 300)
	}
	loggo.Info("checkDeadLock end")
	return nil
}

func setCongestion(c conn.Conn, config *Config) {
	if c.Name() == "rudp" {
		cf := c.(*conn.RudpConn).GetConfig()
		cf.Congestion = config.Congestion
		c.(*conn.RudpConn).SetConfig(cf)
	} else if c.Name() == "ricmp" {
		cf := c.(*conn.RicmpConn).GetConfig()
		cf.Congestion = config.Congestion
		c.(*conn.RicmpConn).SetConfig(cf)
	}
}
