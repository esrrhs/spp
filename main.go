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

type serverAddrs []string

func (f *serverAddrs) String() string {
	return strings.Join(*f, ",")
}

func (f *serverAddrs) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// ConfigFile defines JSON configuration file structure.
type ConfigFile struct {
	Type         string   `json:"type"`
	Proto        []string `json:"proto,omitempty"`
	ProxyProto   []string `json:"proxyproto,omitempty"`
	Listen       []string `json:"listen,omitempty"`
	Name         string   `json:"name,omitempty"`
	Server       string   `json:"server,omitempty"`
	FromAddr     []string `json:"fromaddr,omitempty"`
	ToAddr       []string `json:"toaddr,omitempty"`
	Key          string   `json:"key"`
	Encrypt      *string  `json:"encrypt,omitempty"`
	EncryptType  *string  `json:"encrypttype,omitempty"`
	Compress     *int     `json:"compress,omitempty"`
	CompressType *string  `json:"compresstype,omitempty"`
	NoLog        *int     `json:"nolog,omitempty"`
	NoPrint      *int     `json:"noprint,omitempty"`
	LogLevel     string   `json:"loglevel,omitempty"`
	Profile      *int     `json:"profile,omitempty"`
	Ping         *bool    `json:"ping,omitempty"`
	Username     string   `json:"username,omitempty"`
	Password     string   `json:"password,omitempty"`
	KcpFecData   *int     `json:"kcpfecdata,omitempty"`
	KcpFecParity *int     `json:"kcpfecparity,omitempty"`
	StatusAddr   string   `json:"statusaddr,omitempty"`
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

// sigNotify returns the channel used to await shutdown signals; tests swap it
// to drive graceful shutdown deterministically without real signals.
var sigNotify = func() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	return ch
}

func main() {
	defer common.CrashLog()
	if code := runMain(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

// runMain runs the CLI against the given arguments and returns the process
// exit code (0 on success or clean usage rejection, 2 on flag parse errors,
// 1 on genconfig failure).
func runMain(args []string) int {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version information")
	showVersionShort := fs.Bool("v", false, "print version information")
	configPath := fs.String("config", "", "path to json configuration file")
	genconfig := fs.Bool("genconfig", false, "generate a multi-listener server config plus one client config per mode and exit")
	outdir := fs.String("outdir", ".", "output directory for -genconfig")
	force := fs.Bool("force", false, "overwrite existing files when using -genconfig")

	t := fs.String("type", "", "type: server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client/http_client/reverse_http_client")
	var protos protoFlags
	fs.Var(&protos, "proto", "main proto type (server may repeat; client takes exactly one): "+fmt.Sprintf("%v", network.SupportReliableProtos()))
	var proxyproto proxyprotoFlags
	fs.Var(&proxyproto, "proxyproto", "proxy proto type: "+fmt.Sprintf("%v", network.SupportProtos()))
	var listenaddrs listenAddrs
	fs.Var(&listenaddrs, "listen", "server listen addr (repeat with -proto for multiple listeners)")
	name := fs.String("name", "", "optional client tag for logs; empty is fine")
	var servers serverAddrs
	fs.Var(&servers, "server", "server addr (client takes exactly one)")
	var fromaddr fromFlags
	fs.Var(&fromaddr, "fromaddr", "from addr")
	var toaddr toFlags
	fs.Var(&toaddr, "toaddr", "to addr")
	key := fs.String("key", "", "auth key (required)")
	encrypt := fs.String("encrypt", "", "encrypt key, empty means encryption off")
	encrypttype := fs.String("encrypttype", "chacha20", "encrypt type: none/aes-gcm/chacha20")
	compress := fs.Int("compress", 128, "start compress size, 0 means off")
	compresstype := fs.String("compresstype", "zstd", "compress type: none/zlib/zstd")
	nolog := fs.Int("nolog", 0, "1=disable log file (combine with -noprint 1 for zero-cost log short-circuit)")
	noprint := fs.Int("noprint", 0, "1=disable stdout log (combine with -nolog 1 for zero-cost log short-circuit)")
	loglevel := fs.String("loglevel", "info", "log level")
	profile := fs.Int("profile", 0, "open profile")
	ping := fs.Bool("ping", false, "show ping")
	username := fs.String("username", "", "socks5 username")
	password := fs.String("password", "", "socks5 password")
	kcpfecdata := fs.Int("kcpfecdata", 0, "KCP FEC data shards (0 disables FEC; e.g. 10); client and server must match")
	kcpfecparity := fs.Int("kcpfecparity", 0, "KCP FEC parity shards (0 disables FEC; e.g. 3); client and server must match")
	statusAddr := fs.String("statusaddr", "", "HTTP health/status listen address (e.g. 127.0.0.1:6060), empty disables; serves /healthz and /status")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion || *showVersionShort {
		fmt.Println(version.Full())
		return 0
	}

	if *genconfig {
		if err := generateConfigs(*outdir, *force); err != nil {
			fmt.Fprintf(os.Stderr, "genconfig failed: %v\n", err)
			return 1
		}
		return 0
	}

	// Track which flags were explicitly set via command line
	cliSet := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		cliSet[f.Name] = true
	})

	// Load configuration file if specified
	if *configPath != "" {
		fileCfg, err := loadConfigFile(*configPath)
		if err != nil {
			fmt.Printf("Error loading config file: %v\n\n", err)
			return 0
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
		if len(servers) == 0 && fileCfg.Server != "" {
			servers = []string{fileCfg.Server}
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
		if !cliSet["encrypttype"] && fileCfg.EncryptType != nil {
			*encrypttype = *fileCfg.EncryptType
		}
		if !cliSet["compress"] && fileCfg.Compress != nil {
			*compress = *fileCfg.Compress
		}
		if !cliSet["compresstype"] && fileCfg.CompressType != nil {
			*compresstype = *fileCfg.CompressType
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
		if !cliSet["kcpfecdata"] && fileCfg.KcpFecData != nil {
			*kcpfecdata = *fileCfg.KcpFecData
		}
		if !cliSet["kcpfecparity"] && fileCfg.KcpFecParity != nil {
			*kcpfecparity = *fileCfg.KcpFecParity
		}
		if !cliSet["statusaddr"] && fileCfg.StatusAddr != "" {
			*statusAddr = fileCfg.StatusAddr
		}
	}

	normalizedProtos, verr := validateStartup(startupOptions{
		typ:         *t,
		protos:      protos,
		proxyproto:  proxyproto,
		listenaddrs: listenaddrs,
		servers:     servers,
		fromaddr:    fromaddr,
		toaddr:      toaddr,
	})
	if verr != nil {
		fmt.Println(verr.Error())
		fmt.Println()
		fs.Usage()
		return 0
	}
	protos = protoFlags(normalizedProtos)

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
	config.KcpFecDataShards = *kcpfecdata
	config.KcpFecParityShards = *kcpfecparity

	ct, err := proxy.ParseCompressType(*compresstype)
	if err != nil {
		loggo.Error("invalid -compresstype: %s", err.Error())
		return 0
	}
	et, err := proxy.ParseEncryptType(*encrypttype)
	if err != nil {
		loggo.Error("invalid -encrypttype: %s", err.Error())
		return 0
	}
	config.CompressType = ct
	config.EncryptType = et
	if ct == proxy.CompressUnspecified {
		config.CompressType = proxy.CompressZstd
	}
	if et == proxy.EncryptUnspecified {
		config.EncryptType = proxy.EncryptChaCha20
	}
	if err := proxy.ValidateConfig(config); err != nil {
		loggo.Error("%s", err.Error())
		return 0
	}
	if config.Encrypt == "" {
		loggo.Info("encryption disabled (-encrypt empty)")
	}

	var s *proxy.Server
	var c *proxy.Client

	if *t == "server" {
		var err error
		s, err = proxy.NewServer(config, protos, listenaddrs)
		if err != nil {
			loggo.Error("main NewServer fail %s", err.Error())
			return 0
		}
		loggo.Info("Server started successfully")
	} else {
		clienttypestr := strings.Replace(*t, "_client", "", -1)
		clienttypestr = strings.ToUpper(clienttypestr)
		var err error
		c, err = proxy.NewClient(config, protos[0], servers[0], *name, clienttypestr, proxyproto, fromaddr, toaddr)
		if err != nil {
			loggo.Error("main NewClient fail %s", err.Error())
			return 0
		}
		loggo.Info("Client started successfully")
	}

	if *profile > 0 {
		go http.ListenAndServe(":"+strconv.Itoa(*profile), nil)
	}

	var statusSrv *proxy.StatusServer
	if *statusAddr != "" {
		var collect func() proxy.StatusReport
		if s != nil {
			collect = s.SnapshotStatus
		} else {
			collect = c.SnapshotStatus
		}
		ss, err := proxy.StartStatusServer(*statusAddr, collect)
		if err != nil {
			loggo.Error("main status server fail %s", err.Error())
			if s != nil {
				s.Close()
			}
			if c != nil {
				c.Close()
			}
			return 0
		}
		statusSrv = ss
	}
	sig := <-sigNotify()
	loggo.Info("received signal %v, shutting down...", sig)

	if statusSrv != nil {
		_ = statusSrv.Close()
	}
	if s != nil {
		s.Close()
	}
	if c != nil {
		c.Close()
	}
	loggo.Info("spp exited cleanly")
	return 0
}
