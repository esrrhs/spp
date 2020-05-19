package main

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"github.com/golang/protobuf/proto"
	"golang.org/x/sync/errgroup"
	"io"
	"net"
	"strconv"
)

const (
	MAX_MSG_SIZE = 1024 * 1024
)

func MarshalSrpFrame(f *SrpFrame, compress int) ([]byte, error) {

	if f.Type == SrpFrame_DATA && compress > 0 && len(f.DataFrame.Data) > compress {
		newb := common.CompressData(f.DataFrame.Data)
		if len(newb) < len(f.DataFrame.Data) {
			f.DataFrame.Data = newb
			f.DataFrame.Compress = true
		}
	}

	mb, err := proto.Marshal(f)
	if err != nil {
		return nil, err
	}
	return mb, err
}

func recvFrom(ctx context.Context, wg *errgroup.Group, recvch chan<- *SrpFrame, conn *net.TCPConn) error {
	defer common.CrashLog()

	bs := make([]byte, 4)
	ds := make([]byte, MAX_MSG_SIZE)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			_, err := io.ReadFull(conn, bs)
			if err != nil {
				loggo.Error("recvFrom ReadFull fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}

			len := binary.LittleEndian.Uint32(bs)
			if len > MAX_MSG_SIZE {
				loggo.Error("recvFrom len fail: %s %d", conn.RemoteAddr().String(), len)
				return errors.New("msg len fail " + strconv.Itoa(int(len)))
			}

			_, err = io.ReadFull(conn, ds[0:len])
			if err != nil {
				loggo.Error("recvFrom ReadFull fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}

			f := &SrpFrame{}
			err = proto.Unmarshal(ds[0:len], f)
			if err != nil {
				loggo.Error("recvFrom Unmarshal fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}

			recvch <- f
		}
	}
}

func sendTo(ctx context.Context, wg *errgroup.Group, sendch <-chan *SrpFrame, conn *net.TCPConn, compress int) error {
	defer common.CrashLog()

	bs := make([]byte, 4)

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-sendch:
			mb, err := MarshalSrpFrame(f, compress)
			if err != nil {
				loggo.Error("sendTo MarshalSrpFrame fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}

			len := uint32(len(mb))
			binary.LittleEndian.PutUint32(bs, len)
			_, err = conn.Write(bs)
			if err != nil {
				loggo.Error("sendTo Write fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}

			_, err = conn.Write(mb)
			if err != nil {
				loggo.Error("sendTo Write fail: %s %s", conn.RemoteAddr().String(), err.Error())
				return err
			}
		}
	}
}
