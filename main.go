package main

import (
	"flag"
	"fmt"
	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/conn"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/spp/proxy"
	"net/http"
	_ "net/http/pprof"
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

type protoFlags []string

func (f *protoFlags) String() string {
	return "tcp"
}

func (f *protoFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type listenAddrs []string

func (f *listenAddrs) String() string {
	return ""
}

func (f *listenAddrs) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {

	defer common.CrashLog()

	t := flag.String("type", "", "type: server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client")
	var protos protoFlags
	flag.Var(&protos, "proto", "main proto type: "+fmt.Sprintf("%v", conn.SupportReliableProtos()))
	var proxyproto proxyprotoFlags
	flag.Var(&proxyproto, "proxyproto", "proxy proto type: "+fmt.Sprintf("%v", conn.SupportProtos()))
	var listenaddrs listenAddrs
	flag.Var(&listenaddrs, "listen", "server listen addr")
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

	for _, p := range protos {
		if !conn.HasReliableProto(p) {
			fmt.Println("[proto] must be " + fmt.Sprintf("%v", conn.SupportReliableProtos()) + "\n")
			flag.Usage()
			return
		}
	}

	for _, p := range proxyproto {
		if !conn.HasProto(p) {
			fmt.Println("[proxyproto] " + fmt.Sprintf("%v", conn.SupportProtos()) + "\n")
			flag.Usage()
			return
		}
	}

	if *t != "proxy_client" &&
		*t != "reverse_proxy_client" &&
		*t != "socks5_client" &&
		*t != "reverse_socks5_client" &&
		*t != "server" {
		fmt.Println("[type] must be server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client")
		fmt.Println()
		flag.Usage()
		return
	}

	if *t == "proxy_client" ||
		*t == "reverse_proxy_client" {
		for i, _ := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 || len(toaddr[i]) == 0 {
				fmt.Println("[proxy_client] or [reverse_proxy_client] need [server] [fromaddr] [toaddr] [proxyproto]")
				fmt.Println()
				flag.Usage()
				return
			}
		}

		if !(len(fromaddr) == len(toaddr) && len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [toaddr] [proxyproto] len must be equal")
			fmt.Println()
			flag.Usage()
			return
		}

		if len(protos) == 0 {
			protos = append(protos, "tcp")
		}
	}

	if *t == "socks5_client" ||
		*t == "reverse_socks5_client" {
		for i, _ := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 {
				fmt.Println("[socks5_client] or [reverse_socks5_client] need [server] [fromaddr] [proxyproto]")
				fmt.Println()
				flag.Usage()
				return
			}
		}

		if !(len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [proxyproto] len must be equal")
			fmt.Println()
			flag.Usage()
			return
		}

		if len(protos) == 0 {
			protos = append(protos, "tcp")
		}
	}

	if *t == "server" {
		if len(listenaddrs) != len(protos) {
			fmt.Println("[proto] [listen] len must be equal")
			fmt.Println()
			flag.Usage()
			return
		}
	}

	logprefix := "server"
	if *t != "server" {
		logprefix = "client"
	}

	level := loggo.LEVEL_INFO
	if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:     level,
		Prefix:    "spp" + logprefix,
		MaxDay:    3,
		NoLogFile: *nolog > 0,
		NoPrint:   *noprint > 0,
	})
	loggo.Info("start...")

	config := proxy.DefaultConfig()
	config.Compress = *compress
	config.Key = *key
	config.Encrypt = *encrypt
	config.ShowPing = *ping
	config.Username = *username
	config.Password = *password
	config.MaxClient = *maxclient
	config.MaxSonny = *maxconn

	if *t == "server" {
		_, err := proxy.NewServer(config, protos, listenaddrs)
		if err != nil {
			loggo.Error("main NewServer fail %s", err.Error())
			return
		}
		loggo.Info("Server start")
	} else {
		clienttypestr := strings.Replace(*t, "_client", "", -1)
		clienttypestr = strings.ToUpper(clienttypestr)
		_, err := proxy.NewClient(config, protos[0], *server, *name, clienttypestr, proxyproto, fromaddr, toaddr)
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
