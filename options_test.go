package main

import (
	"strings"
	"testing"
)

func TestValidateStartup(t *testing.T) {
	ok := func(o startupOptions, wantProtos []string) {
		t.Helper()
		got, err := validateStartup(o)
		if err != nil {
			t.Fatalf("unexpected error for %+v: %v", o, err)
		}
		if len(got) != len(wantProtos) {
			t.Fatalf("protos=%v want %v", got, wantProtos)
		}
		for i := range wantProtos {
			if got[i] != wantProtos[i] {
				t.Fatalf("protos=%v want %v", got, wantProtos)
			}
		}
		// Must not mutate/mutate-share the caller slice when defaulting.
	}
	wantErr := func(o startupOptions, substr string) {
		t.Helper()
		got, err := validateStartup(o)
		if err == nil {
			t.Fatalf("expected error containing %q, got protos=%v", substr, got)
		}
		if !strings.Contains(err.Error(), substr) {
			t.Fatalf("error %q does not contain %q", err.Error(), substr)
		}
	}

	// ---- valid cases ----
	ok(startupOptions{typ: "server", protos: []string{"tcp"}, listenaddrs: []string{":1"}}, []string{"tcp"})
	ok(startupOptions{ // server may listen on several protos
		typ:         "server",
		protos:      []string{"tcp", "rudp"},
		listenaddrs: []string{":1", ":2"},
	}, []string{"tcp", "rudp"})
	ok(startupOptions{ // forward client defaults to tcp when proto omitted
		typ:        "proxy_client",
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":8080"},
		toaddr:     []string{":9090"},
	}, []string{"tcp"})
	ok(startupOptions{ // explicit single proto stays as given
		typ:        "socks5_client",
		protos:     []string{"kcp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1080"},
	}, []string{"kcp"})
	ok(startupOptions{ // http client has no toaddr
		typ:        "http_client",
		protos:     []string{"quic"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":8081"},
	}, []string{"quic"})
	ok(startupOptions{ // multiple services on the one connection
		typ:        "proxy_client",
		protos:     []string{"tcp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp", "tcp"},
		fromaddr:   []string{":1", ":2"},
		toaddr:     []string{":3", ":4"},
	}, []string{"tcp"})

	// ---- invalid type / proto ----
	wantErr(startupOptions{typ: "bogus", protos: []string{"tcp"}, servers: []string{"h:1"}}, "[type]")
	wantErr(startupOptions{typ: "server", protos: []string{"carrier-pigeon"}, listenaddrs: []string{":1"}}, "[proto]")
	wantErr(startupOptions{typ: "proxy_client", protos: []string{"tcp"}, servers: []string{"h:1"}, proxyproto: []string{"sctp"}, fromaddr: []string{":1"}, toaddr: []string{":2"}}, "[proxyproto]")

	// ---- server pairing ----
	wantErr(startupOptions{typ: "server", protos: []string{"tcp", "rudp"}, listenaddrs: []string{":1"}}, "[proto] [listen]")
	wantErr(startupOptions{typ: "server", protos: []string{"tcp"}, listenaddrs: []string{":1", ":2"}}, "[proto] [listen]")

	// ---- client must use exactly one underlay ----
	wantErr(startupOptions{
		typ:        "proxy_client",
		protos:     []string{"tcp", "rudp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1"},
		toaddr:     []string{":2"},
	}, "exactly one [proto]")
	wantErr(startupOptions{
		typ:        "proxy_client",
		protos:     []string{"tcp"},
		servers:    []string{"h:1", "h:2"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1"},
		toaddr:     []string{":2"},
	}, "exactly one [server]")
	wantErr(startupOptions{ // no server at all is caught by the type-level required-field check
		typ:        "socks5_client",
		protos:     []string{"tcp"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1080"},
	}, "need [server]")

	// ---- service array consistency ----
	wantErr(startupOptions{ // fromaddr/toaddr/proxyproto length mismatch
		typ:        "proxy_client",
		protos:     []string{"tcp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1"},
		toaddr:     []string{":2", ":3"},
	}, "len must be equal")
	wantErr(startupOptions{ // empty fromaddr entry
		typ:        "proxy_client",
		protos:     []string{"tcp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{""},
		toaddr:     []string{":2"},
	}, "need [server]")
	wantErr(startupOptions{ // socks5 fromaddr/proxyproto mismatch
		typ:        "socks5_client",
		protos:     []string{"tcp"},
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp", "tcp"},
		fromaddr:   []string{":1"},
	}, "len must be equal")
}

func TestValidateStartup_DoesNotMutateInput(t *testing.T) {
	in := startupOptions{
		typ:        "proxy_client",
		servers:    []string{"h:1"},
		proxyproto: []string{"tcp"},
		fromaddr:   []string{":1"},
		toaddr:     []string{":2"},
	}
	if _, err := validateStartup(in); err != nil {
		t.Fatal(err)
	}
	if len(in.protos) != 0 {
		t.Fatalf("input protos mutated: %v", in.protos)
	}
}
