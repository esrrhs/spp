package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

var (
	errAddrType      = errors.New("socks addr type not supported")
	errVer           = errors.New("socks version not supported")
	errMethod        = errors.New("socks only support 1 method now")
	errAuthExtraData = errors.New("socks authentication get extra data")
	errReqExtraData  = errors.New("socks request get extra data")
	errCmd           = errors.New("socks command not supported")
)

const (
	socksCmdConnect = 1
	NoAuth          = uint8(0)
	userAuthVersion = uint8(1)
	UserPassAuth    = uint8(2)
	authSuccess     = uint8(0)
	authFailure     = uint8(1)
)

func Sock5HandshakeBy(conn io.ReadWriter, username string, password string) (err error) {
	const (
		idVer     = 0
		idNmethod = 1
	)
	// version identification and method selection message in theory can have
	// at most 256 methods, plus version and nmethod field in total 258 bytes
	// the current rfc defines only 3 authentication methods (plus 2 reserved),
	// so it won't be such long in practice

	buf := make([]byte, 258)

	var n int
	// make sure we get the nmethod field
	if n, err = io.ReadAtLeast(conn, buf, idNmethod+1); err != nil {
		return err
	}
	if buf[idVer] != socksVer5 {
		return errVer
	}
	nmethod := int(buf[idNmethod])
	msgLen := nmethod + 2
	if n == msgLen { // handshake done, common case
		// do nothing, jump directly to send confirmation
	} else if n < msgLen { // has more methods to read, rare case
		if _, err = io.ReadFull(conn, buf[n:msgLen]); err != nil {
			return err
		}
	} else { // error, should not get extra data
		return errAuthExtraData
	}

	if username == "" && password == "" {
		// send confirmation: version 5, no authentication required
		_, err = conn.Write([]byte{socksVer5, NoAuth})
	} else {
		// Tell the client to use user/pass auth
		if _, err := conn.Write([]byte{socksVer5, UserPassAuth}); err != nil {
			return err
		}

		// Get the version and username length
		header := []byte{0, 0}
		if _, err := io.ReadAtLeast(conn, header, 2); err != nil {
			return err
		}

		// Ensure we are compatible
		if header[0] != userAuthVersion {
			return fmt.Errorf("Unsupported auth version: %v", header[0])
		}

		// Get the user name
		userLen := int(header[1])
		user := make([]byte, userLen)
		if _, err := io.ReadAtLeast(conn, user, userLen); err != nil {
			return err
		}

		// Get the password length
		if _, err := conn.Read(header[:1]); err != nil {
			return err
		}

		// Get the password
		passLen := int(header[0])
		pass := make([]byte, passLen)
		if _, err := io.ReadAtLeast(conn, pass, passLen); err != nil {
			return err
		}

		// Verify the password
		if username == string(user) && password == string(pass) {
			if _, err := conn.Write([]byte{userAuthVersion, authSuccess}); err != nil {
				return err
			}
		} else {
			if _, err := conn.Write([]byte{userAuthVersion, authFailure}); err != nil {
				return err
			}
		}
	}
	return
}

func Sock5GetRequest(conn io.ReadWriter) (rawaddr []byte, host string, err error) {
	const (
		idVer   = 0
		idCmd   = 1
		idType  = 3 // address type index
		idIP0   = 4 // ip address start index
		idDmLen = 4 // domain address length index
		idDm0   = 5 // domain address start index

		typeIPv4 = 1 // type is ipv4 address
		typeDm   = 3 // type is domain address
		typeIPv6 = 4 // type is ipv6 address

		lenIPv4   = 3 + 1 + net.IPv4len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv4 + 2port
		lenIPv6   = 3 + 1 + net.IPv6len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv6 + 2port
		lenDmBase = 3 + 1 + 1 + 2           // 3 + 1addrType + 1addrLen + 2port, plus addrLen
	)
	// refer to getRequest in server.go for why set buffer size to 263
	buf := make([]byte, 263)
	var n int
	// read till we get possible domain length field
	if n, err = io.ReadAtLeast(conn, buf, idDmLen+1); err != nil {
		return
	}
	// check version and cmd
	if buf[idVer] != socksVer5 {
		err = errVer
		return
	}
	if buf[idCmd] != socksCmdConnect {
		err = errCmd
		return
	}

	reqLen := -1
	switch buf[idType] {
	case typeIPv4:
		reqLen = lenIPv4
	case typeIPv6:
		reqLen = lenIPv6
	case typeDm:
		reqLen = int(buf[idDmLen]) + lenDmBase
	default:
		err = errAddrType
		return
	}

	if n == reqLen {
		// common case, do nothing
	} else if n < reqLen { // rare case
		if _, err = io.ReadFull(conn, buf[n:reqLen]); err != nil {
			return
		}
	} else {
		err = errReqExtraData
		return
	}

	rawaddr = buf[idType:reqLen]

	switch buf[idType] {
	case typeIPv4:
		host = net.IP(buf[idIP0 : idIP0+net.IPv4len]).String()
	case typeIPv6:
		host = net.IP(buf[idIP0 : idIP0+net.IPv6len]).String()
	case typeDm:
		host = string(buf[idDm0 : idDm0+buf[idDmLen]])
	}
	port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))

	return
}
