package proxy

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/gohome/thread"
)

// socks5UDPTunnel bridges a single remote destination for a SOCKS5 UDP association session.
type socks5UDPTunnel struct {
	proxyConn  *ProxyConn
	targetAddr string
	dstHost    string
	dstPort    int
	clientAddr atomic.Pointer[net.UDPAddr]
	sendIndex  int32
	recvIndex  int32
}

func getSocks5RelayAddr(listenAddr net.Addr, tcpConn network.Conn) string {
	port := 0
	if udpAddr, ok := listenAddr.(*net.UDPAddr); ok {
		port = udpAddr.Port
	} else {
		_, pStr, err := net.SplitHostPort(listenAddr.String())
		if err == nil {
			port, _ = strconv.Atoi(pStr)
		}
	}

	host := "127.0.0.1"
	if tcpConn != nil {
		info := tcpConn.Info()
		parts := strings.Split(info, "<--")
		if len(parts) > 0 {
			h, _, err := net.SplitHostPort(strings.TrimSpace(parts[0]))
			if err == nil && h != "" {
				ip := net.ParseIP(h)
				if ip != nil && !ip.IsUnspecified() {
					host = h
				}
			}
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (i *Inputer) handleSocks5UDPAssociate(tcpProxyConn *ProxyConn, clientTarget string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	if err != nil {
		return err
	}
	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = network.Sock5SendConnectReply(tcpProxyConn.conn, 0x01, "0.0.0.0:0")
		return err
	}

	relayAddr := getSocks5RelayAddr(udpListener.LocalAddr(), tcpProxyConn.conn)
	if err := network.Sock5SendConnectReply(tcpProxyConn.conn, 0x00, relayAddr); err != nil {
		udpListener.Close()
		return err
	}

	loggo.Info("socks5 UDP ASSOCIATE listening on %s (relay %s) for tcp %s",
		udpListener.LocalAddr().String(), relayAddr, tcpProxyConn.Info())

	udpGroup := thread.NewGroup("Inputer socks5UDPAssociate "+tcpProxyConn.Info(), i.fwg, func() {
		loggo.Info("socks5UDPAssociate exit start %s", tcpProxyConn.Info())
		udpListener.Close()
		loggo.Info("socks5UDPAssociate exit done %s", tcpProxyConn.Info())
	})

	var tunnels sync.Map // targetAddr string -> *socks5UDPTunnel

	// Goroutine 1: Read from local UDP socket, unpack SOCKS5 UDP header, and forward to target tunnel
	udpGroup.Go("socks5UDP localRecv "+tcpProxyConn.Info(), func() error {
		buf := make([]byte, i.config.MaxMsgSize)
		for !udpGroup.IsExit() {
			n, clientAddr, err := udpListener.ReadFromUDP(buf)
			if err != nil {
				if udpGroup.IsExit() {
					return nil
				}
				return err
			}
			if n <= 0 {
				continue
			}

			dstHost, dstPort, payload, err := network.Sock5UnpackUDP(buf[:n])
			if err != nil {
				loggo.Error("socks5UDP unpack error from %s: %v", clientAddr.String(), err)
				continue
			}

			target := net.JoinHostPort(dstHost, strconv.Itoa(dstPort))

			var tun *socks5UDPTunnel
			val, loaded := tunnels.Load(target)
			if loaded {
				tun = val.(*socks5UDPTunnel)
			} else {
				sonny := &ProxyConn{
					id:   common.UniqueId(),
					conn: nil, // remote virtual UDP conn handled via sonny channels
				}
				sonny.setEstablished(true)
				_, exists := i.sonny.LoadOrStore(sonny.id, sonny)
				if exists {
					loggo.Error("socks5UDP sonny ID collision: %s", sonny.id)
					continue
				}
				atomic.AddInt32(&i.sonnyNum, 1)

				sendch := newMsgChannel(i.config.ConnBuffer)
				recvch := newMsgChannel(i.config.ConnBuffer)
				sonny.sendch = sendch
				sonny.recvch = recvch

				tun = &socks5UDPTunnel{
					proxyConn:  sonny,
					targetAddr: target,
					dstHost:    dstHost,
					dstPort:    dstPort,
				}
				tunnels.Store(target, tun)

				// Open remote connection via father SPP tunnel with PROXY_PROTO_UDP
				i.openConnWithProto(sonny, target, PROXY_PROTO_UDP)

				// Manage sonny lifecycle
				sonnyGroup := thread.NewGroup("socks5UDP sonny "+sonny.id, udpGroup, func() {
					loggo.Info("socks5UDP sonny group exit %s %s", sonny.id, target)
					sonny.CloseChannels()
					if _, ok := i.sonny.LoadAndDelete(sonny.id); ok {
						atomic.AddInt32(&i.sonnyNum, -1)
					}
					tunnels.Delete(target)
					closeRemoteConn(sonny, i.father)
				})

				// Routine: Receive packets from SPP remote (via sendch) and send back to client via UDP
				sonnyGroup.Go("socks5UDP toClient "+sonny.id, func() error {
					for !sonnyGroup.IsExit() {
						var ff interface{}
						select {
						case ff = <-sendch.Ch():
						case <-sendch.Done():
							ff = nil
						}
						if ff == nil {
							break
						}
						f := ff.(*ProxyFrame)
						if f.Type == FRAME_TYPE_CLOSE {
							break
						}
						if f.DataFrame == nil || len(f.DataFrame.Data) == 0 {
							continue
						}

						// Validate sequence index
						seq := atomic.AddInt32(&tun.recvIndex, 1)
						expected := seq % MAX_INDEX
						if f.DataFrame.Index != expected {
							loggo.Error("socks5UDP recv index mismatch %s got=%d want=%d", sonny.id, f.DataFrame.Index, expected)
							return errors.New("index error")
						}

						dstClient := tun.clientAddr.Load()
						if dstClient == nil {
							continue
						}

						pkt, err := network.Sock5PackUDP(tun.dstHost, tun.dstPort, f.DataFrame.Data)
						if err != nil {
							loggo.Error("socks5UDP pack error: %v", err)
							continue
						}
						_, _ = udpListener.WriteToUDP(pkt, dstClient)
						atomic.AddInt32(&gState.SendNum, 1)
						atomic.AddInt64(&gState.SendSize, int64(len(pkt)))
					}
					return nil
				})

				// Routine: forward sonny's recvch upstream to father pipe
				sonnyGroup.Go("socks5UDP copySonnyRecv "+sonny.id, func() error {
					return copySonnyRecv(sonnyGroup, recvch, sonny, i.father)
				})

				sonnyGroup.Go("socks5UDP checkNeedClose "+sonny.id, func() error {
					return checkNeedClose(sonnyGroup, sonny)
				})

				sonnyGroup.Go("socks5UDP checkSonnyActive "+sonny.id, func() error {
					return checkSonnyActive(sonnyGroup, sonny, i.config.EstablishedTimeout, i.config.ConnTimeout)
				})
			}

			// Update the latest client UDP address for this destination
			tun.clientAddr.Store(clientAddr)

			// Forward UDP packet to sonny's recvch -> copySonnyRecv -> SPP father tunnel
			seq := atomic.AddInt32(&tun.sendIndex, 1)
			f := &ProxyFrame{
				Type: FRAME_TYPE_DATA,
				DataFrame: &DataFrame{
					Id:    tun.proxyConn.id,
					Data:  make([]byte, len(payload)),
					Index: seq % MAX_INDEX,
				},
			}
			copy(f.DataFrame.Data, payload)
			if loggo.IsDebug() {
				f.DataFrame.Crc = common.GetCrc32(f.DataFrame.Data)
			}

			atomic.AddInt32(&gState.RecvNum, 1)
			atomic.AddInt64(&gState.RecvSize, int64(len(payload)))
			tun.proxyConn.RecvSonnyData(f)
		}
		return nil
	})

	// Wait until client drops TCP connection; once TCP closes, terminate UDP association.
	tcpBuf := make([]byte, 128)
	for {
		_, err := tcpProxyConn.conn.Read(tcpBuf)
		if err != nil {
			break
		}
	}

	udpGroup.Stop()
	udpGroup.Wait()
	return nil
}
