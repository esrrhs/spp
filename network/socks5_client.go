package network

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	socksVer5          = 5
	socks5AuthNone     = 0
	socks5UserPassAuth = 2
	socks5Connect      = 1
	Socks5AtypIP4      = 1
	Socks5AtypDomain   = 3
	Socks5AtypIP6      = 4
)

var socks5Errors = []string{
	"",
	"general failure",
	"connection forbidden",
	"network unreachable",
	"host unreachable",
	"connection refused",
	"TTL expired",
	"command not supported",
	"address type not supported",
}

func Sock5Handshake(conn *net.TCPConn, timeoutms int, username string, password string) (err error) {

	if username == "" && password == "" {
		buf := make([]byte, 0)
		buf = append(buf, socksVer5)
		buf = append(buf, 1)
		buf = append(buf, socks5AuthNone)

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := conn.Write(buf); err != nil {
			return errors.New("proxy: failed to write greeting to SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return errors.New("proxy: failed to read greeting from SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}
		if buf[0] != 5 {
			return errors.New("proxy: SOCKS5 proxy at " + conn.RemoteAddr().String() + " has unexpected version " + strconv.Itoa(int(buf[0])))
		}
		if buf[1] != socks5AuthNone {
			return errors.New("proxy: SOCKS5 proxy at " + conn.RemoteAddr().String() + " requires authentication")
		}

		if timeoutms > 0 {
			conn.SetDeadline(time.Time{})
		}

		return nil
	} else {
		buf := make([]byte, 0)
		buf = append(buf, socksVer5)
		buf = append(buf, 1)
		buf = append(buf, socks5UserPassAuth)

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := conn.Write(buf); err != nil {
			return errors.New("proxy: failed to write greeting to SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return errors.New("proxy: failed to read greeting from SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}
		if buf[0] != 5 {
			return errors.New("proxy: SOCKS5 proxy at " + conn.RemoteAddr().String() + " has unexpected version " + strconv.Itoa(int(buf[0])))
		}
		if buf[1] != socks5UserPassAuth {
			return errors.New("proxy: SOCKS5 proxy at " + conn.RemoteAddr().String() + " not requires authentication")
		}

		userLen := len(username)
		passLen := len(password)
		socket5Authentication := make([]byte, 0, 3+userLen+passLen)
		socket5Authentication = append(socket5Authentication, 0x01, byte(userLen))
		socket5Authentication = append(socket5Authentication, []byte(username)...)
		socket5Authentication = append(socket5Authentication, byte(passLen))
		socket5Authentication = append(socket5Authentication, []byte(password)...)

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := conn.Write(socket5Authentication); err != nil {
			return errors.New("proxy: failed to read send Authentication from SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}

		if timeoutms > 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
		}
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return errors.New("proxy: failed to read Authentication from SOCKS5 proxy at " + conn.RemoteAddr().String() + ": " + err.Error())
		}
		if bytes.Equal(buf, []byte{0x01, 0x00}) != true {
			return errors.New("proxy: SOCKS5 proxy at " + conn.RemoteAddr().String() + " fail authentication")
		}

		if timeoutms > 0 {
			conn.SetDeadline(time.Time{})
		}

		return nil
	}
}

func Sock5SetRequest(conn *net.TCPConn, host string, port int, timeoutms int) (err error) {

	buf := make([]byte, 0)

	buf = buf[:0]
	buf = append(buf, socksVer5, socks5Connect, 0 /* reserved */)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf = append(buf, Socks5AtypIP4)
			ip = ip4
		} else {
			buf = append(buf, Socks5AtypIP6)
		}
		buf = append(buf, ip...)
	} else {
		if len(host) > 255 {
			err = errors.New("proxy: destination hostname too long: " + host)
			return
		}
		buf = append(buf, Socks5AtypDomain)
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = append(buf, byte(port>>8), byte(port))

	if timeoutms > 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
	}
	if _, err = conn.Write(buf); err != nil {
		return errors.New("proxy: failed to write connect request to SOCKS5 proxy: " + err.Error())
	}

	if timeoutms > 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
	}
	if _, err = io.ReadFull(conn, buf[:4]); err != nil {
		return errors.New("proxy: failed to read connect reply from SOCKS5 proxy: " + err.Error())
	}

	failure := "unknown error"
	if int(buf[1]) < len(socks5Errors) {
		failure = socks5Errors[buf[1]]
	}

	if len(failure) > 0 {
		err = errors.New("proxy: SOCKS5 proxy failed to connect: " + failure)
		return
	}

	if timeoutms > 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
	}
	hostType := buf[3]
	_, err = readSocksHost(conn, hostType)
	if err != nil {
		return fmt.Errorf("proxy: invalid request: fail to read dst host: %s", err)
	}

	if timeoutms > 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeoutms) * time.Millisecond))
	}
	_, err = readSocksPort(conn)
	if err != nil {
		return fmt.Errorf("proxy: invalid request: fail to read dst port: %s", err)
	}

	if timeoutms > 0 {
		conn.SetDeadline(time.Time{})
	}

	return nil
}

func ntohs(data [2]byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])<<0
}

func readSocksIPv4Host(r io.Reader) (host string, err error) {
	var buf [4]byte
	_, err = io.ReadFull(r, buf[:])
	if err != nil {
		return
	}

	var ip net.IP = buf[:]
	host = ip.String()
	return
}

func readSocksIPv6Host(r io.Reader) (host string, err error) {
	var buf [16]byte
	_, err = io.ReadFull(r, buf[:])
	if err != nil {
		return
	}

	var ip net.IP = buf[:]
	host = ip.String()
	return
}

func readSocksDomainHost(r io.Reader) (host string, err error) {
	var buf [0x200]byte
	_, err = r.Read(buf[0:1])
	if err != nil {
		return
	}
	length := buf[0]
	_, err = io.ReadFull(r, buf[1:1+length])
	if err != nil {
		return
	}
	host = string(buf[1 : 1+length])
	return
}

func readSocksHost(r io.Reader, hostType byte) (string, error) {
	switch hostType {
	case Socks5AtypIP4:
		return readSocksIPv4Host(r)
	case Socks5AtypIP6:
		return readSocksIPv6Host(r)
	case Socks5AtypDomain:
		return readSocksDomainHost(r)
	default:
		return string(""), fmt.Errorf("Unknown address type 0x%02x ", hostType)
	}
}

func readSocksPort(r io.Reader) (port uint16, err error) {
	var buf [2]byte
	_, err = io.ReadFull(r, buf[:])
	if err != nil {
		return
	}

	port = ntohs(buf)
	return
}
