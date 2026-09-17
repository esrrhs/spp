package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/spp/proxy"
	"github.com/esrrhs/spp/version"
)

type fromFlags []string

func (f *fromFlags) String() string {
	return strings.Join(*f, ",")
}

func (f *fromFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type toFlags []string

func (f *toFlags) String() string {
	return strings.Join(*f, ",")
}

func (f *toFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type proxyprotoFlags []string

func (f *proxyprotoFlags) String() string {
	if len(*f) == 0 {
		return "tcp"
	}
	return strings.Join(*f, ",")
}

func (f *proxyprotoFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type protoFlags []string

func (f *protoFlags) String() string {
	if len(*f) == 0 {
		return "tcp"
	}
	return strings.Join(*f, ",")
}

func (f *protoFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type listenAddrs []string

func (f *listenAddrs) String() string {
	return strings.Join(*f, ",")
}

func (f *listenAddrs) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// ConfigFile defines JSON configuration file structure.
type ConfigFile struct {
	Type        string   `json:"type"`
	Proto       []string `json:"proto"`
	ProxyProto  []string `json:"proxyproto"`
	Listen      []string `json:"listen"`
	Name        string   `json:"name"`
	Server      string   `json:"server"`
	FromAddr    []string `json:"fromaddr"`
	ToAddr      []string `json:"toaddr"`
	Key         string   `json:"key"`
	Encrypt     *string  `json:"encrypt"`
	Compress    *int     `json:"compress"`
	NoLog       *int     `json:"nolog"`
	NoPrint     *int     `json:"noprint"`
	LogLevel    string   `json:"loglevel"`
	Profile     *int     `json:"profile"`
	Ping        *bool    `json:"ping"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	MaxClient   *int     `json:"maxclient"`
	MaxConn     *int     `json:"maxconn"`
}

func loadConfigFile(filePath string) (*ConfigFile, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var cfg ConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid json config file %s: %w", filePath, err)
	}
	return &cfg, nil
}

func main() {
	defer common.CrashLog()

	showVersion := flag.Bool("version", false, "print version information")
	showVersionShort := flag.Bool("v", false, "print version information")
	configPath := flag.String("config", "", "path to json configuration file")

	t := flag.String("type", "", "type: server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client")
	var protos protoFlags
	flag.Var(&protos, "proto", "main proto type: "+fmt.Sprintf("%v", network.SupportReliableProtos()))
	var proxyproto proxyprotoFlags
	flag.Var(&proxyproto, "proxyproto", "proxy proto type: "+fmt.Sprintf("%v", network.SupportProtos()))
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
	maxclient := flag.Int("maxclient", 10000, "max client connection")
	maxconn := flag.Int("maxconn", 10240, "max connection")

	flag.Parse()

	if *showVersion || *showVersionShort {
		fmt.Println(version.Full())
		return
	}

	// Track which flags were explicitly set via command line
	cliSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		cliSet[f.Name] = true
	})

	// Load configuration file if specified
	if *configPath != "" {
		fileCfg, err := loadConfigFile(*configPath)
		if err != nil {
			fmt.Printf("Error loading config file: %v\n\n", err)
			return
		}

		if !cliSet["type"] && fileCfg.Type != "" {
			*t = fileCfg.Type
		}
		if len(protos) == 0 && len(fileCfg.Proto) > 0 {
			protos = fileCfg.Proto
		}
		if len(proxyproto) == 0 && len(fileCfg.ProxyProto) > 0 {
			proxyproto = fileCfg.ProxyProto
		}
		if len(listenaddrs) == 0 && len(fileCfg.Listen) > 0 {
			listenaddrs = fileCfg.Listen
		}
		if !cliSet["name"] && fileCfg.Name != "" {
			*name = fileCfg.Name
		}
		if !cliSet["server"] && fileCfg.Server != "" {
			*server = fileCfg.Server
		}
		if len(fromaddr) == 0 && len(fileCfg.FromAddr) > 0 {
			fromaddr = fileCfg.FromAddr
		}
		if len(toaddr) == 0 && len(fileCfg.ToAddr) > 0 {
			toaddr = fileCfg.ToAddr
		}
		if !cliSet["key"] && fileCfg.Key != "" {
			*key = fileCfg.Key
		}
		if !cliSet["encrypt"] && fileCfg.Encrypt != nil {
			*encrypt = *fileCfg.Encrypt
		}
		if !cliSet["compress"] && fileCfg.Compress != nil {
			*compress = *fileCfg.Compress
		}
		if !cliSet["nolog"] && fileCfg.NoLog != nil {
			*nolog = *fileCfg.NoLog
		}
		if !cliSet["noprint"] && fileCfg.NoPrint != nil {
			*noprint = *fileCfg.NoPrint
		}
		if !cliSet["loglevel"] && fileCfg.LogLevel != "" {
			*loglevel = fileCfg.LogLevel
		}
		if !cliSet["profile"] && fileCfg.Profile != nil {
			*profile = *fileCfg.Profile
		}
		if !cliSet["ping"] && fileCfg.Ping != nil {
			*ping = *fileCfg.Ping
		}
		if !cliSet["username"] && fileCfg.Username != "" {
			*username = fileCfg.Username
		}
		if !cliSet["password"] && fileCfg.Password != "" {
			*password = fileCfg.Password
		}
		if !cliSet["maxclient"] && fileCfg.MaxClient != nil {
			*maxclient = *fileCfg.MaxClient
		}
		if !cliSet["maxconn"] && fileCfg.MaxConn != nil {
			*maxconn = *fileCfg.MaxConn
		}
	}

	for _, p := range protos {
		if !network.HasReliableProto(p) {
			fmt.Println("[proto] must be " + fmt.Sprintf("%v", network.SupportReliableProtos()) + "\n")
			flag.Usage()
			return
		}
	}

	for _, p := range proxyproto {
		if !network.HasProto(p) {
			fmt.Println("[proxyproto] " + fmt.Sprintf("%v", network.SupportProtos()))
			fmt.Println()
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
		if !(len(fromaddr) == len(toaddr) && len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [toaddr] [proxyproto] len must be equal")
			fmt.Println()
			flag.Usage()
			return
		}

		for i := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 || len(toaddr[i]) == 0 {
				fmt.Println("[proxy_client] or [reverse_proxy_client] need [server] [fromaddr] [toaddr] [proxyproto]")
				fmt.Println()
				flag.Usage()
				return
			}
		}

		if len(protos) == 0 {
			protos = append(protos, "tcp")
		}
	}

	if *t == "socks5_client" ||
		*t == "reverse_socks5_client" {
		if !(len(fromaddr) == len(proxyproto)) {
			fmt.Println("[fromaddr] [proxyproto] len must be equal")
			fmt.Println()
			flag.Usage()
			return
		}

		for i := range proxyproto {
			if len(fromaddr[i]) == 0 || len(*server) == 0 {
				fmt.Println("[socks5_client] or [reverse_socks5_client] need [server] [fromaddr] [proxyproto]")
				fmt.Println()
				flag.Usage()
				return
			}
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
	loggo.Info("start %s...", version.Short())

	config := proxy.DefaultConfig()
	config.Compress = *compress
	config.Key = *key
	config.Encrypt = *encrypt
	config.ShowPing = *ping
	config.Username = *username
	config.Password = *password
	config.MaxClient = *maxclient
	config.MaxSonny = *maxconn

	var s *proxy.Server
	var c *proxy.Client

	if *t == "server" {
		var err error
		s, err = proxy.NewServer(config, protos, listenaddrs)
		if err != nil {
			loggo.Error("main NewServer fail %s", err.Error())
			return
		}
		loggo.Info("Server started successfully")
	} else {
		clienttypestr := strings.Replace(*t, "_client", "", -1)
		clienttypestr = strings.ToUpper(clienttypestr)
		var err error
		c, err = proxy.NewClient(config, protos[0], *server, *name, clienttypestr, proxyproto, fromaddr, toaddr)
		if err != nil {
			loggo.Error("main NewClient fail %s", err.Error())
			return
		}
		loggo.Info("Client started successfully")
	}

	if *profile > 0 {
		go http.ListenAndServe("0.0.0.0:"+strconv.Itoa(*profile), nil)
	}

	// Wait for OS termination signal to gracefully shut down
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	loggo.Info("received signal %v, shutting down...", sig)

	if s != nil {
		s.Close()
	}
	if c != nil {
		c.Close()
	}
	loggo.Info("spp exited cleanly")
}
