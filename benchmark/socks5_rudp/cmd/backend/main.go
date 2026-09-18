package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen addr")
	mode := flag.String("mode", "sink", "sink|source|echo")
	bs := flag.Int("bs", 64*1024, "chunk size for source")
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
