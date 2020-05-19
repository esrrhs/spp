package main

import (
	"flag"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"net/http"
	"strconv"
	"time"
)

func main() {

	defer common.CrashLog()

	t := flag.String("type", "", "client or server")
	server := flag.String("s", "", "server addr")
	remote := flag.String("r", "", "remote addr")
	local := flag.String("l", "", "local addr")
	key := flag.Int("key", 0, "key")
	compress := flag.Int("compress", 0, "compress size")
	nolog := flag.Int("nolog", 0, "write log file")
	noprint := flag.Int("noprint", 0, "print stdout")
	loglevel := flag.String("loglevel", "info", "log level")
	profile := flag.Int("profile", 0, "open profile")

	flag.Parse()

	if *t != "client" && *t != "server" {
		flag.Usage()
		return
	}

	if *t == "client" {
		if len(*remote) == 0 || len(*server) == 0 || len(*local) == 0 {
			flag.Usage()
			return
		}
	}

	if *t == "server" {
		if len(*local) == 0 {
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
		Prefix:    "srp",
		MaxDay:    3,
		NoLogFile: *nolog > 0,
		NoPrint:   *noprint > 0,
	})
	loggo.Info("start...")

	if *t == "server" {
		s, err := NewServer(*key, *local)
		if err != nil {
			loggo.Error("NewServer fail: %s", err.Error())
			return
		}
		loggo.Info("Server start")
		err = s.Run()
		if err != nil {
			loggo.Error("Run fail: %s", err.Error())
			return
		}
	} else {
		c, err := NewClient(*key, *server, *local, *remote, *compress)
		if err != nil {
			loggo.Error("NewClient fail: %s", err.Error())
			return
		}
		loggo.Info("Server start")
		err = c.Run()
		if err != nil {
			loggo.Error("Run fail: %s", err.Error())
			return
		}
	}

	if *profile > 0 {
		go http.ListenAndServe("0.0.0.0:"+strconv.Itoa(*profile), nil)
	}

	for {
		time.Sleep(time.Hour)
	}
}
