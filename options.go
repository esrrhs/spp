package main

import (
	"fmt"

	"github.com/esrrhs/gohome/network"
)

// startupOptions holds the merged CLI/config values that must be mutually
// consistent before the process starts serving.
type startupOptions struct {
	typ         string
	protos      []string
	proxyproto  []string
	listenaddrs []string
	servers     []string
	fromaddr    []string
	toaddr      []string
}

// validateStartup checks CLI/config consistency and returns the normalized
// transit protos (a client with none given defaults to a single "tcp").
// It performs no I/O so every reject/default branch is unit-testable.
func validateStartup(o startupOptions) ([]string, error) {
	protos := append([]string(nil), o.protos...)

	for _, p := range protos {
		if !network.HasReliableProto(p) {
			return nil, fmt.Errorf("[proto] must be %v", network.SupportReliableProtos())
		}
	}
	for _, p := range o.proxyproto {
		if !network.HasProto(p) {
			return nil, fmt.Errorf("[proxyproto] %v", network.SupportProtos())
		}
	}

	switch o.typ {
	case "proxy_client", "reverse_proxy_client":
		if !(len(o.fromaddr) == len(o.toaddr) && len(o.fromaddr) == len(o.proxyproto)) {
			return nil, fmt.Errorf("[fromaddr] [toaddr] [proxyproto] len must be equal")
		}
		for i := range o.proxyproto {
			if len(o.fromaddr[i]) == 0 || len(o.servers) == 0 || len(o.toaddr[i]) == 0 {
				return nil, fmt.Errorf("[proxy_client] or [reverse_proxy_client] need [server] [fromaddr] [toaddr] [proxyproto]")
			}
		}
		if len(protos) == 0 {
			protos = append(protos, "tcp")
		}

	case "socks5_client", "reverse_socks5_client", "http_client", "reverse_http_client":
		if !(len(o.fromaddr) == len(o.proxyproto)) {
			return nil, fmt.Errorf("[fromaddr] [proxyproto] len must be equal")
		}
		for i := range o.proxyproto {
			if len(o.fromaddr[i]) == 0 || len(o.servers) == 0 {
				return nil, fmt.Errorf("[socks5_client] or [reverse_socks5_client] or [http_client] or [reverse_http_client] need [server] [fromaddr] [proxyproto]")
			}
		}
		if len(protos) == 0 {
			protos = append(protos, "tcp")
		}

	case "server":
		// pairing checked below

	default:
		return nil, fmt.Errorf("[type] must be server/proxy_client/reverse_proxy_client/socks5_client/reverse_socks5_client/http_client/reverse_http_client")
	}

	if o.typ == "server" {
		if len(o.listenaddrs) != len(protos) {
			return nil, fmt.Errorf("[proto] [listen] len must be equal")
		}
	} else {
		if len(protos) != 1 {
			return nil, fmt.Errorf("client takes exactly one [proto]; multi-path is not supported (run multiple clients instead)")
		}
		if len(o.servers) != 1 {
			return nil, fmt.Errorf("client takes exactly one [server]")
		}
	}

	return protos, nil
}
