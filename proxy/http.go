package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

type bufferedConn struct {
	network.Conn
	br  *bufio.Reader
	buf []byte
}

func newPrefixedConn(c network.Conn, prefix []byte, br *bufio.Reader) network.Conn {
	return &bufferedConn{
		Conn: c,
		buf:  prefix,
		br:   br,
	}
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	if len(b.buf) > 0 {
		n := copy(p, b.buf)
		b.buf = b.buf[n:]
		return n, nil
	}
	if b.br != nil {
		if b.br.Buffered() > 0 {
			return b.br.Read(p)
		}
		return b.Conn.Read(p)
	}
	return b.Conn.Read(p)
}

func checkProxyAuth(authHeader, username, password string) bool {
	if username == "" && password == "" {
		return true
	}
	if authHeader == "" {
		return false
	}
	const prefix = "Basic "
	if len(authHeader) < len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return false
	}
	raw := strings.TrimSpace(authHeader[len(prefix):])
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	expected := username + ":" + password
	return string(decoded) == expected
}

func parseHost(rawHost, defaultPort string) string {
	rawHost = strings.TrimSpace(rawHost)
	if host, port, err := net.SplitHostPort(rawHost); err == nil {
		return net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(rawHost, defaultPort)
}

func NewHttpInputer(wg *thread.Group, proto string, addr string, clienttype CLIENT_TYPE, config *Config, father *ProxyConn, serviceIndex int) (*Inputer, error) {
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

	wg.Go("Inputer listenHttp "+addr, func() error {
		return input.listenHttp()
	})

	loggo.Info("NewHttpInputer ok %s service=%d", addr, serviceIndex)

	return input, nil
}

func (i *Inputer) listenHttp() error {
	loggo.Info("Inputer start listenHttp %s", i.addr)
	for !isExit(i.fwg) {
		conn, err := i.listenconn.Accept()
		if err != nil {
			loggo.Error("Inputer listenHttp accept fail %s %s", i.addr, err.Error())
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
		i.fwg.Go("Inputer processHttpConn "+conn.Info(), func() error {
			atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, 1)
			defer atomic.AddInt32(&gStateThreadNum.InputerSonnyThread, -1)
			return i.processHttpConn(proxyconn)
		})
	}
	loggo.Info("Inputer end listenHttp %s", i.addr)
	return nil
}

func (i *Inputer) processHttpConn(proxyConn *ProxyConn) error {
	loggo.Debug("processHttpConn start %s", proxyConn.conn.Info())

	if proxyConn.conn.Name() != "tcp" {
		loggo.Error("processHttpConn not tcp %s %s", proxyConn.conn.Info(), proxyConn.conn.Name())
		proxyConn.closeConn()
		return errors.New("http proxy not tcp")
	}

	br := bufio.NewReader(proxyConn.conn)

	reqLine, err := br.ReadString('\n')
	if err != nil {
		loggo.Debug("processHttpConn ReadString reqLine fail %s %v", proxyConn.conn.Info(), err)
		proxyConn.closeConn()
		return err
	}

	reqLineTrimmed := strings.TrimRight(reqLine, "\r\n")
	parts := strings.Split(reqLineTrimmed, " ")
	if len(parts) < 3 {
		loggo.Error("processHttpConn invalid reqLine %s: %s", proxyConn.conn.Info(), reqLineTrimmed)
		proxyConn.closeConn()
		return errors.New("invalid http request line")
	}

	method := strings.ToUpper(parts[0])
	rawURI := parts[1]
	protoVer := parts[2]

	var rawHeaders []string
	var hostHeader string
	var proxyAuthHeader string

	for {
		headerLine, err := br.ReadString('\n')
		if err != nil {
			loggo.Error("processHttpConn ReadString header fail %s %v", proxyConn.conn.Info(), err)
			proxyConn.closeConn()
			return err
		}
		trimmed := strings.TrimRight(headerLine, "\r\n")
		if trimmed == "" {
			break
		}

		colonIdx := strings.IndexByte(trimmed, ':')
		if colonIdx > 0 {
			k := strings.TrimSpace(trimmed[:colonIdx])
			v := strings.TrimSpace(trimmed[colonIdx+1:])
			if strings.EqualFold(k, "Host") {
				hostHeader = v
			} else if strings.EqualFold(k, "Proxy-Authorization") {
				proxyAuthHeader = v
				continue
			} else if strings.EqualFold(k, "Proxy-Connection") {
				rawHeaders = append(rawHeaders, "Connection: "+v+"\r\n")
				continue
			}
		}
		rawHeaders = append(rawHeaders, trimmed+"\r\n")
	}

	if !checkProxyAuth(proxyAuthHeader, i.config.Username, i.config.Password) {
		loggo.Warn("processHttpConn proxy auth fail %s", proxyConn.conn.Info())
		const http407 = "HTTP/1.1 407 Proxy Authentication Required\r\n" +
			"Proxy-Authenticate: Basic realm=\"SPP Proxy\"\r\n" +
			"Content-Length: 0\r\n\r\n"
		_, _ = proxyConn.conn.Write([]byte(http407))
		proxyConn.closeConn()
		return nil
	}

	if method == "CONNECT" {
		targetAddr := parseHost(rawURI, "443")
		const resp200 = "HTTP/1.1 200 Connection Established\r\n\r\n"
		if _, err := proxyConn.conn.Write([]byte(resp200)); err != nil {
			loggo.Error("processHttpConn write 200 fail %s %v", proxyConn.conn.Info(), err)
			proxyConn.closeConn()
			return nil
		}

		if br.Buffered() > 0 {
			proxyConn.conn = newPrefixedConn(proxyConn.conn, nil, br)
		}

		loggo.Debug("processHttpConn CONNECT ok %s -> %s", proxyConn.conn.Info(), targetAddr)
		i.fwg.Go("Inputer processProxyConn "+proxyConn.conn.Info(), func() error {
			return i.processProxyConn(proxyConn, targetAddr)
		})
		return nil
	}

	targetAddr := ""
	newURI := rawURI

	if strings.HasPrefix(strings.ToLower(rawURI), "http://") || strings.HasPrefix(strings.ToLower(rawURI), "https://") {
		u, err := url.Parse(rawURI)
		if err == nil && u.Host != "" {
			defaultPort := "80"
			if strings.EqualFold(u.Scheme, "https") {
				defaultPort = "443"
			}
			targetAddr = parseHost(u.Host, defaultPort)
			newURI = u.RequestURI()
			if newURI == "" {
				newURI = "/"
			}
		}
	}

	if targetAddr == "" {
		if hostHeader != "" {
			targetAddr = parseHost(hostHeader, "80")
		} else {
			loggo.Error("processHttpConn missing host %s: %s", proxyConn.conn.Info(), reqLineTrimmed)
			proxyConn.closeConn()
			return errors.New("missing host in http request")
		}
	}

	if hostHeader == "" && targetAddr != "" {
		rawHeaders = append(rawHeaders, "Host: "+targetAddr+"\r\n")
	}

	var prefixBuf bytes.Buffer
	prefixBuf.WriteString(method + " " + newURI + " " + protoVer + "\r\n")
	for _, h := range rawHeaders {
		prefixBuf.WriteString(h)
	}
	prefixBuf.WriteString("\r\n")

	proxyConn.conn = newPrefixedConn(proxyConn.conn, prefixBuf.Bytes(), br)

	loggo.Debug("processHttpConn HTTP ok %s -> %s", proxyConn.conn.Info(), targetAddr)
	i.fwg.Go("Inputer processProxyConn "+proxyConn.conn.Info(), func() error {
		return i.processProxyConn(proxyConn, targetAddr)
	})
	return nil
}
