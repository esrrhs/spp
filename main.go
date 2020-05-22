package main

import (
	"flag"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"github.com/esrrhs/go-engine/src/proxy"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func main() {

	defer common.CrashLog()

	t := flag.String("type", "", "type: server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client")
	proto := flag.String("proto", "tcp", "main proto type")
	proxyproto := flag.String("proxyproto", "tcp", "proxy proto type: tcp/udp/rudp/ricmp")
	listenaddr := flag.String("listen", "", "server listen addr")
	name := flag.String("name", "", "client name")
	server := flag.String("server", "", "server addr")
	fromaddr := flag.String("from", "", "from addr")
	toaddr := flag.String("to", "", "to addr")
	key := flag.String("key", "", "verify key")
	encrypt := flag.String("encrypt", "", "encrypt key, empty means off")
	compress := flag.Int("compress", 0, "start compress size, 0 means off")
	nolog := flag.Int("nolog", 0, "write log file")
	noprint := flag.Int("noprint", 0, "print stdout")
	loglevel := flag.String("loglevel", "info", "log level")
	profile := flag.Int("profile", 0, "open profile")

	flag.Parse()

	if *t != "proxy_client" &&
		*t != "reverse_proxy_client" &&
		*t != "socks5_client" &&
		*t != "reverse_socks5_client" &&
		*t != "server" {
		flag.Usage()
		return
	}

	if *t != "proxy_client" &&
		*t != "reverse_proxy_client" {
		if len(*fromaddr) == 0 || len(*server) == 0 || len(*toaddr) == 0 {
			flag.Usage()
			return
		}
	}

	if *t != "socks5_client" &&
		*t != "reverse_socks5_client" {
		if len(*fromaddr) == 0 || len(*server) == 0 {
			flag.Usage()
			return
		}
	}

	if *t == "server" {
		if len(*listenaddr) == 0 {
			flag.Usage()
			return
		}
	}

	level := loggo.LEVEL_INFO
	if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:     level,
		Prefix:    "spp",
		MaxDay:    3,
		NoLogFile: *nolog > 0,
		NoPrint:   *noprint > 0,
	})
	loggo.Info("start...")

	config := proxy.DefaultConfig()
	config.Compress = *compress
	config.Key = *key
	config.Encrypt = *encrypt
	config.Proto = *proto

	if *t == "server" {
		_, err := proxy.NewServer(config, *listenaddr)
		if err != nil {
			loggo.Error("main NewServer fail %s", err.Error())
			return
		}
		loggo.Info("Server start")
	} else {
		clienttypestr := strings.Replace(*t, "_client", "", -1)
		clienttypestr = strings.ToUpper(clienttypestr)
		_, err := proxy.NewClient(config, *server, *name, clienttypestr, *proxyproto, *fromaddr, *toaddr)
		if err != nil {
			loggo.Error("main NewClient fail %s", err.Error())
			return
		}
		loggo.Info("Client start")
	}

	if *profile > 0 {
		go http.ListenAndServe("0.0.0.0:"+strconv.Itoa(*profile), nil)
	}

	for {
		time.Sleep(time.Hour)
	}
}
