package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

func fillPattern(b []byte, seed byte) {
	for i := range b {
		b[i] = byte(i) + seed
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen addr")
	mode := flag.String("mode", "sink", "sink|source|echo|page")
	bs := flag.Int("bs", 64*1024, "chunk size for source")
	pageSize := flag.Int("pagesize", 100*1024, "page body size for page mode")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		panic(err)
	}
	fmt.Println("listening", *addr, "mode", *mode)

	var bytes int64
	go func() {
		for range time.Tick(2 * time.Second) {
			fmt.Printf("backend bytes=%.1fMB\n", float64(atomic.LoadInt64(&bytes))/1e6)
		}
	}()

	payload := make([]byte, *bs)
	fillPattern(payload, 0)

	pageBody := make([]byte, *pageSize)
	fillPattern(pageBody, 0x5A)
	pageSum := sha256.Sum256(pageBody)

	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(conn net.Conn) {
			defer conn.Close()
			buf := make([]byte, *bs)
			switch *mode {
			case "source":
				for {
					n, err := conn.Write(payload)
					if n > 0 {
						atomic.AddInt64(&bytes, int64(n))
					}
					if err != nil {
						return
					}
				}
			case "echo":
				n, err := io.Copy(conn, conn)
				atomic.AddInt64(&bytes, n)
				_ = err
			case "page":
				// req: 4B len + body; rsp: 4B len + 32B sha256 + body
				var hdr [4]byte
				if _, err := io.ReadFull(conn, hdr[:]); err != nil {
					return
				}
				reqLen := binary.BigEndian.Uint32(hdr[:])
				if reqLen > 1<<20 {
					return
				}
				req := make([]byte, reqLen)
				if _, err := io.ReadFull(conn, req); err != nil {
					return
				}
				atomic.AddInt64(&bytes, int64(4+reqLen))

				binary.BigEndian.PutUint32(hdr[:], uint32(len(pageBody)))
				if _, err := conn.Write(hdr[:]); err != nil {
					return
				}
				if _, err := conn.Write(pageSum[:]); err != nil {
					return
				}
				n, err := conn.Write(pageBody)
				atomic.AddInt64(&bytes, int64(4+32+n))
				_ = err
			default:
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						atomic.AddInt64(&bytes, int64(n))
					}
					if err != nil {
						return
					}
				}
			}
		}(c)
	}
}
