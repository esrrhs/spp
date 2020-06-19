package main

import (
	"flag"
	"fmt"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"github.com/esrrhs/go-engine/src/proxy"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type fromFlags []string

func (f *fromFlags) String() string {
	return ""
}

func (f *fromFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type toFlags []string

func (f *toFlags) String() string {
	return ""
}

func (f *toFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type proxyprotoFlags []string

func (f *proxyprotoFlags) String() string {
	return "tcp"
}

func (f *proxyprotoFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {

	defer common.CrashLog()

	t := flag.String("type", "", "type: server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client")
	proto := flag.String("proto", "tcp", "main proto type: tcp/rudp/ricmp")
	var proxyproto proxyprotoFlags
	flag.Var(&proxyproto, "proxyproto", "proxy proto type: tcp/udp/rudp/ricmp")
	listenaddr := flag.String("listen", "", "server listen addr")
	name := flag.String("name", "client", "client name")
	server := flag.String("server", "", "server addr")
	var fromaddr fromFlags
	flag.Var(&fromaddr, "fromaddr", "from addr")
	var toaddr toFlags
	flag.Var(&toaddr, "toaddr", "to addr")
	key := flag.String("key", "123456", "verify key")
	encrypt := flag.String("encrypt", "default", "encrypt key, empty means off")
	compress := flag.Int("compress", 128, "start compress size, 0 means off")
	nolog := flag.Int("nolog", 0, "write log file")
	noprint := flag.Int("noprint", 0, "print stdout")
	loglevel := flag.String("loglevel", "info", "log level")
	profile := flag.Int("profile", 0, "open profile")
	ping := flag.Bool("ping", false, "show ping")
	username := flag.String("username", "", "socks5 username")
	password := flag.String("password", "", "socks5 password")
	maxclient := flag.Int("maxclient", 8, "max client connection")
	maxconn := flag.Int("maxconn", 128, "max connection")

	flag.Parse()

	if *proto != "tcp" &&
		*proto != "rudp" &&
		*proto != "ricmp" {
		fmt.Println("[proto] must be tcp/rudp/ricmp\n")
		flag.Usage()
		return
	}

	for _, p := range proxyproto {
		if p != "tcp" &&
			p != "udp" &&
			p != "rudp" &&
			p != "ricmp" {
			fmt.Println("[proxyproto] tcp/udp/rudp/ricmp\n")
			flag.Usage()
			return
		}
	}

	if *t != "proxy_client" &&
		*t != "reverse_proxy_client" &&
		*t != "socks5_client" &&
		*t != "reverse_socks5_client" &&
		*t != "server" {
		fmt.Println("[type] must be server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client\n")
		flag.Usage()
		return
	}

	if *t == "proxy_client" ||
		*t == "reverse_proxy_client" {
		for i, _ := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 || len(toaddr[i]) == 0 {
				fmt.Println("[proxy_client] or [reverse_proxy_client] need [server] [fromaddr] [toaddr] [proxyproto]\n")
				flag.Usage()
				return
			}
		}

		if !(len(fromaddr) == len(toaddr) && len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [toaddr] [proxyproto] len must be equal\n")
			flag.Usage()
			return
		}
	}

	if *t == "socks5_client" ||
		*t == "reverse_socks5_client" {
		for i, _ := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 {
				fmt.Println("[socks5_client] or [reverse_socks5_client] need [server] [fromaddr] [proxyproto]\n")
				flag.Usage()
				return
			}
		}

		if !(len(fromaddr) == len(toaddr) && len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [proxyproto] len must be equal\n")
			flag.Usage()
			return
		}
	}

	if *t == "server" {
		if len(*listenaddr) == 0 {
			fmt.Println("[server] need [listen]\n")
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
	config.ShowPing = *ping
	config.Username = *username
	config.Password = *password
	config.MaxClient = *maxclient
	config.MaxSonny = *maxconn

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
		_, err := proxy.NewClient(config, *server, *name, clienttypestr, proxyproto, fromaddr, toaddr)
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
