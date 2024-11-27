package export

import (
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/spp/proxy"
	"strings"
)

func NewClient(ty string, proto string, server string, name string, proxyproto string, fromaddr string, toaddr string) {
	level := loggo.LEVEL_INFO
	loggo.Ini(loggo.Config{
		Level:     level,
		Prefix:    "spp-android",
		MaxDay:    3,
		NoLogFile: true,
		NoPrint:   false,
	})
	loggo.Info("start...")

	config := proxy.DefaultConfig()

	proxyprotos := strings.Split(proxyproto, ",")
	fromaddrs := strings.Split(fromaddr, ",")
	toaddrs := strings.Split(toaddr, ",")

	clienttypestr := strings.Replace(ty, "_client", "", -1)
	clienttypestr = strings.ToUpper(clienttypestr)
	_, err := proxy.NewClient(config, proto, server, name, clienttypestr, proxyprotos, fromaddrs, toaddrs)
	if err != nil {
		loggo.Error("main NewClient fail %s", err.Error())
		return
	}
	loggo.Info("Client start")
}
