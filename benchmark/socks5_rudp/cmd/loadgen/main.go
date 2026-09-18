package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func socks5Dial(proxy, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxy, timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 auth rejected: %v", resp)
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		conn.Close()
		return nil, fmt.Errorf("need ipv4 target: %s", host)
	}
	var port uint16
	fmt.Sscanf(portStr, "%d", &port)

	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ip...)
	pb := make([]byte, 2)
	binary.BigEndian.PutUint16(pb, port)
	req = append(req, pb...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	cres := make([]byte, 10)
	if _, err := io.ReadFull(conn, cres); err != nil {
		conn.Close()
		return nil, err
	}
	if cres[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed: %d", cres[1])
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

func fillPattern(b []byte, seed byte) {
	for i := range b {
		b[i] = byte(i) + seed
	}
}

func main() {
	proxy := flag.String("proxy", "127.0.0.1:1080", "socks5 proxy")
	target := flag.String("target", "127.0.0.1:9999", "backend target")
	conns := flag.Int("c", 32, "concurrent workers")
	duration := flag.Duration("d", 30*time.Second, "test duration")
	chunk := flag.Int("bs", 32*1024, "write chunk size (upload/download) or short echo size")
	reqSize := flag.Int("req", 512, "page-mode request body size")
	pageSize := flag.Int("pagesize", 100*1024, "page-mode expected response body size")
	mode := flag.String("mode", "upload", "upload|download|bidi|short|page")
	flag.Parse()

	payload := make([]byte, *chunk)
	fillPattern(payload, 0)
	if *mode == "short" && *chunk > 8*1024 {
		payload = payload[:4*1024]
	}

	pageReq := make([]byte, *reqSize)
	fillPattern(pageReq, 0xA1)
	wantPage := make([]byte, *pageSize)
	fillPattern(wantPage, 0x5A)
	wantSum := sha256.Sum256(wantPage)

	var totalUp, totalDown, opens, errs, bad, latSumNs, latCount int64
	deadline := time.Now().Add(*duration)
	var wg sync.WaitGroup

	for i := 0; i < *conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 64*1024)
			for time.Now().Before(deadline) {
				remain := time.Until(deadline)
				if remain < 200*time.Millisecond {
					return
				}
				start := time.Now()
				c, err := socks5Dial(*proxy, *target, 5*time.Second)
				if err != nil {
					atomic.AddInt64(&errs, 1)
					time.Sleep(20 * time.Millisecond)
					continue
				}

				switch *mode {
				case "short":
					c.SetDeadline(time.Now().Add(5 * time.Second))
					n, err := c.Write(payload)
					if n > 0 {
						atomic.AddInt64(&totalUp, int64(n))
					}
					if err != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					got := make([]byte, len(payload))
					rn, rerr := io.ReadFull(c, got)
					if rn > 0 {
						atomic.AddInt64(&totalDown, int64(rn))
					}
					if rerr != nil {
						atomic.AddInt64(&errs, 1)
					} else if !bytes.Equal(got, payload) {
						atomic.AddInt64(&bad, 1)
					}
					c.Close()
					atomic.AddInt64(&opens, 1)
					atomic.AddInt64(&latSumNs, time.Since(start).Nanoseconds())
					atomic.AddInt64(&latCount, 1)

				case "page":
					// Webpage-like: small request, larger response; verify length+sha256+bytes.
					c.SetDeadline(time.Now().Add(10 * time.Second))
					var hdr [4]byte
					binary.BigEndian.PutUint32(hdr[:], uint32(len(pageReq)))
					if _, err := c.Write(hdr[:]); err != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					wn, err := c.Write(pageReq)
					atomic.AddInt64(&totalUp, int64(4+wn))
					if err != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					if _, err := io.ReadFull(c, hdr[:]); err != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					gotLen := binary.BigEndian.Uint32(hdr[:])
					var sum [32]byte
					if _, err := io.ReadFull(c, sum[:]); err != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					if int(gotLen) != len(wantPage) {
						atomic.AddInt64(&bad, 1)
						io.Copy(io.Discard, c)
						c.Close()
						continue
					}
					got := make([]byte, gotLen)
					rn, rerr := io.ReadFull(c, got)
					atomic.AddInt64(&totalDown, int64(4+32+rn))
					if rerr != nil {
						atomic.AddInt64(&errs, 1)
						c.Close()
						continue
					}
					gotSum := sha256.Sum256(got)
					if sum != wantSum || gotSum != wantSum || !bytes.Equal(got, wantPage) {
						atomic.AddInt64(&bad, 1)
					}
					c.Close()
					atomic.AddInt64(&opens, 1)
					atomic.AddInt64(&latSumNs, time.Since(start).Nanoseconds())
					atomic.AddInt64(&latCount, 1)

				case "download":
					c.SetDeadline(deadline)
					for time.Now().Before(deadline) {
						n, err := c.Read(buf)
						if n > 0 {
							atomic.AddInt64(&totalDown, int64(n))
						}
						if err != nil {
							break
						}
					}
					c.Close()
					atomic.AddInt64(&opens, 1)

				case "bidi":
					c.SetDeadline(deadline)
					done := make(chan struct{})
					go func() {
						defer close(done)
						for {
							n, err := c.Read(buf)
							if n > 0 {
								atomic.AddInt64(&totalDown, int64(n))
							}
							if err != nil {
								return
							}
						}
					}()
					for time.Now().Before(deadline) {
						n, err := c.Write(payload)
						if n > 0 {
							atomic.AddInt64(&totalUp, int64(n))
						}
						if err != nil {
							break
						}
					}
					c.Close()
					<-done
					atomic.AddInt64(&opens, 1)

				default: // upload
					c.SetDeadline(deadline)
					for time.Now().Before(deadline) {
						n, err := c.Write(payload)
						if n > 0 {
							atomic.AddInt64(&totalUp, int64(n))
						}
						if err != nil {
							break
						}
					}
					c.Close()
					atomic.AddInt64(&opens, 1)
				}
			}
		}()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	startAll := time.Now()
	go func() {
		var lastUp, lastDown, lastOpens int64
		lastT := startAll
		for range ticker.C {
			up := atomic.LoadInt64(&totalUp)
			down := atomic.LoadInt64(&totalDown)
			op := atomic.LoadInt64(&opens)
			now := time.Now()
			dt := now.Sub(lastT).Seconds()
			avgLat := 0.0
			if n := atomic.LoadInt64(&latCount); n > 0 {
				avgLat = float64(atomic.LoadInt64(&latSumNs)) / float64(n) / 1e6
			}
			fmt.Printf("t=%.0fs opens/s=%.0f total_opens=%d up=%.2fMB/s down=%.2fMB/s errs=%d bad=%d avg_lat=%.1fms\n",
				now.Sub(startAll).Seconds(),
				float64(op-lastOpens)/dt,
				op,
				float64(up-lastUp)/dt/1e6,
				float64(down-lastDown)/dt/1e6,
				atomic.LoadInt64(&errs),
				atomic.LoadInt64(&bad),
				avgLat,
			)
			lastUp, lastDown, lastOpens, lastT = up, down, op, now
		}
	}()

	wg.Wait()
	elapsed := time.Since(startAll).Seconds()
	op := atomic.LoadInt64(&opens)
	avgLat := 0.0
	if n := atomic.LoadInt64(&latCount); n > 0 {
		avgLat = float64(atomic.LoadInt64(&latSumNs)) / float64(n) / 1e6
	}
	up := atomic.LoadInt64(&totalUp)
	down := atomic.LoadInt64(&totalDown)
	fmt.Printf("DONE elapsed=%.1fs total_opens=%d opens/s=%.0f avg_up=%.2fMB/s avg_down=%.2fMB/s errs=%d bad=%d avg_lat=%.1fms\n",
		elapsed, op, float64(op)/elapsed,
		float64(up)/elapsed/1e6,
		float64(down)/elapsed/1e6,
		atomic.LoadInt64(&errs), atomic.LoadInt64(&bad), avgLat)
	if op > 0 {
		fmt.Printf("INTEGRITY opens=%d bad=%d per_open_up=%.0fB per_open_down=%.0fB\n",
			op, atomic.LoadInt64(&bad), float64(up)/float64(op), float64(down)/float64(op))
	}
}
