# SPP Usage Guide

This document provides a comprehensive guide on configuring, running, and deploying SPP (Simple and Powerful Proxy).

---

## Table of Contents

- [Overview](#overview)
- [Quick Start](#quick-start)
  - [Server](#server)
  - [Client](#client)
- [Proxy Modes](#proxy-modes)
  - [1. Forward Proxy](#1-forward-proxy)
  - [2. Reverse Proxy](#2-reverse-proxy)
  - [3. SOCKS5 Forward Proxy](#3-socks5-forward-proxy)
  - [4. SOCKS5 Reverse Proxy](#4-socks5-reverse-proxy)
  - [5. HTTP/HTTPS Forward Proxy](#5-httphttps-forward-proxy)
  - [6. HTTP/HTTPS Reverse Proxy](#6-httphttps-reverse-proxy)
  - [7. Shadowsocks Plugin](#7-shadowsocks-plugin)
- [Protocol Multiplexing and Conversion](#protocol-multiplexing-and-conversion)
- [Configuration File](#configuration-file)
  - [Configuration File Schema](#configuration-file-schema)
  - [Server Example](#server-example)
  - [Client Example](#client-example)
- [Command Line Reference](#command-line-reference)
- [Docker Deployment](#docker-deployment)
- [Graceful Shutdown](#graceful-shutdown)
- [IPv6 Support](#ipv6-support)

---

## Overview

SPP is designed to route and forward network traffic across diverse network environments and protocol boundaries. It supports:
- **Proxy Protocols**: TCP, UDP
- **Transit Protocols**: TCP, UDP, RUDP (Reliable UDP), RICMP (Reliable ICMP), RHTTP (Reliable HTTP), KCP, QUIC
- **Proxy Types**: Forward Proxy, Reverse Proxy, SOCKS5 Forward Proxy, SOCKS5 Reverse Proxy, HTTP/HTTPS Forward Proxy, HTTP/HTTPS Reverse Proxy, Shadowsocks plugin mode

---

## Quick Start

### Server

Start a basic TCP server listening on port `8888`:

```bash
./spp -type server -proto tcp -listen :8888
```

You can listen on multiple ports with different protocols simultaneously:

```bash
./spp -type server -proto tcp -listen :8888 -proto rudp -listen :9999 -proto ricmp -listen 0.0.0.0
```

Client can attach **all** of those underlays into one logical session. Traffic is sent on the highest-throughput active path; unhealthy paths are greyed out, probed with `SPEEDTEST`, and re-enabled when they recover:

```bash
./spp -type proxy_client \
  -proto tcp -server www.server.com:8888 \
  -proto rudp -server www.server.com:9999 \
  -proto ricmp -server www.server.com \
  -fromaddr :8080 -toaddr :8080 -proxyproto tcp \
  -key 'your-auth-key' -encrypt 'your-encrypt-key'
```

A single `-server` may be repeated for every `-proto` when the address is the same.

### Client

Map the remote server's port `8080` to local port `8080` over TCP:

```bash
./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```

---

## Proxy Modes

### 1. Forward Proxy

Maps a local port to a remote destination through the SPP server. Accessing the local port reaches the target via the server.

* **TCP Forward Proxy**:
  ```bash
  ./spp -name "tcp_proxy" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
  ```

* **UDP Forward Proxy**:
  ```bash
  ./spp -name "udp_proxy" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp
  ```

* **Multiple Ports Simultaneously**:
  ```bash
  ./spp -name "multi" -type proxy_client -server www.server.com:8888 \
    -fromaddr :8080 -toaddr :8080 -proxyproto tcp \
    -fromaddr :8081 -toaddr :8081 -proxyproto udp
  ```

### 2. Reverse Proxy

Exposes a service running locally to the outside world through the SPP server. Visitors accessing the server's port reach the local service.

```bash
./spp -name "reverse_tcp" -type reverse_proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```

### 3. SOCKS5 Forward Proxy

Starts a SOCKS5 proxy server on the local machine on port `8080`. Both TCP (`CONNECT`) and UDP (`UDP ASSOCIATE`) traffic sent to this SOCKS5 proxy are automatically forwarded through the SPP server.

```bash
./spp -name "socks5" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

With username and password authentication:

```bash
./spp -name "socks5_auth" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp -username myuser -password mypass
```

### 4. SOCKS5 Reverse Proxy

Opens a SOCKS5 proxy server on the remote SPP server's port `8080`. Both TCP (`CONNECT`) and UDP (`UDP ASSOCIATE`) traffic sent to the remote server's SOCKS5 port are proxied through the client network.

```bash
./spp -name "rev_socks5" -type reverse_socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

### 5. HTTP/HTTPS Forward Proxy

Starts an HTTP/HTTPS proxy server on the local machine on port `8080`. Supports standard HTTP methods (`GET`, `POST`, etc.) and HTTPS tunneling (`CONNECT`).

```bash
./spp -name "http" -type http_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

With optional Basic authentication (returns `407 Proxy Authentication Required` if unauthenticated):

```bash
./spp -name "http_auth" -type http_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp -username myuser -password mypass
```

### 6. HTTP/HTTPS Reverse Proxy

Opens an HTTP/HTTPS proxy server on the remote SPP server's port `8080`. Traffic sent to the remote server's HTTP proxy port is proxied through the client network.

```bash
./spp -name "rev_http" -type reverse_http_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

With optional Basic authentication:

```bash
./spp -name "rev_http_auth" -type reverse_http_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp -username myuser -password mypass
```

### 7. Shadowsocks Plugin

SPP can function as a SIP003 plugin for Shadowsocks:
- [spp-shadowsocks-plugin](https://github.com/esrrhs/spp-shadowsocks-plugin)
- [spp-shadowsocks-plugin-android](https://github.com/esrrhs/spp-shadowsocks-plugin-android)

---

## Protocol Multiplexing and Conversion

External proxy protocols and internal transit protocols can be converted automatically:

* **Proxy TCP traffic using internal RUDP transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rudp
  ```

* **Proxy TCP traffic using internal RICMP transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto ricmp
  ```

* **Proxy UDP traffic using internal TCP transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto tcp
  ```

* **Proxy UDP traffic using internal KCP transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto kcp
  ```

* **Proxy TCP traffic using internal QUIC transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto quic
  ```

* **Proxy TCP traffic using internal RHTTP transit:**
  ```bash
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rhttp
  ```

  RHTTP reuses one pooled HTTP connection per logical tunnel (HTTP/1.1
  keep-alive; HTTP/2 over TLS for `https://` server addresses). Prefix the
  server address with `h2c://` to use cleartext HTTP/2 with prior knowledge
  (the rhttp server accepts both HTTP/1.1 and h2c on the same port), e.g.
  `-server h2c://www.server.com:8888`.

* **Proxy UDP traffic using KCP transit with forward error correction:**
  ```bash
  # Weak-network option: 10 data + 3 parity shards (~30% redundancy) lets
  # the receiver recover up to 3 lost packets per FEC group without waiting
  # for a retransmit. MUST be configured identically on client and server;
  # FEC-enabled endpoints cannot interop with FEC-less ones. Default is off.
  ./spp -type server -proto kcp -listen :8888 -key <key> -kcpfecdata 10 -kcpfecparity 3
  ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto kcp -key <key> -kcpfecdata 10 -kcpfecparity 3
  ```

---

## Configuration File

SPP supports JSON configuration files via `-config <path>`. Command-line arguments can override settings in the configuration file.

### Generate configs

```bash
./spp -genconfig
```

Writes these files (shared random auth/encrypt keys):

| File | Role |
| :--- | :--- |
| `config_server.json` | Server listening on **all** main channels (`tcp`/`rudp`/`ricmp`/`kcp`/`quic`/`rhttp`) |
| `config_proxy_client.json` | Forward proxy (`:8080` → `:8080`) |
| `config_reverse_proxy_client.json` | Reverse proxy |
| `config_socks5_client.json` | SOCKS5 on `:1080` |
| `config_reverse_socks5_client.json` | Reverse SOCKS5 on `:1080` |
| `config_http_client.json` | HTTP/HTTPS proxy on `:8081` |
| `config_reverse_http_client.json` | Reverse HTTP/HTTPS proxy on `:8081` |

Client configs dial the same full set of underlay addresses. Defaults: AEAD `chacha20`, compression `zstd` / threshold `128`.

```bash
./spp -genconfig -outdir ./conf    # output directory
./spp -genconfig -force            # overwrite existing files
```

Then:

```bash
./spp -config config_server.json
./spp -config config_proxy_client.json   # or reverse / socks5 / reverse_socks5 / http / reverse_http
```

### Configuration File Schema

| Field | Type | Description |
| :--- | :--- | :--- |
| `type` | string | `server`, `proxy_client`, `reverse_proxy_client`, `socks5_client`, `reverse_socks5_client`, `http_client`, `reverse_http_client` |
| `proto` | array of string | Internal transit protocols (e.g. `["tcp"]`, `["kcp"]`, `["quic"]`) |
| `proxyproto` | array of string | Proxy protocols (e.g. `["tcp"]`, `["udp"]`) |
| `listen` | array of string | Server listening addresses (e.g. `[":8888"]`) |
| `server` | string | Remote server address (e.g. `127.0.0.1:8888`) |
| `name` | string | Client identifier |
| `fromaddr` | array of string | Source addresses to bind / listen |
| `toaddr` | array of string | Destination target addresses |
| `key` | string | Authentication key (must match on client and server) |
| `encrypt` | string | Encryption key (empty disables encryption; default is enabled) |
| `compress` | integer | Threshold size in bytes to trigger compression (0 disables) |
| `loglevel` | string | `debug`, `info`, `warn`, `error` |
| `nolog` | integer | `1` to disable writing log files, `0` to keep |
| `noprint` | integer | `1` to suppress stdout logs, `0` to print |
| `maxclient` | integer | Maximum concurrent client connections |
| `maxconn` | integer | Maximum sub-connections |
| `kcpfecdata` | integer | KCP FEC data shards (e.g. `10`); `0` disables FEC. Must match on both ends |
| `kcpfecparity` | integer | KCP FEC parity shards (e.g. `3`); `0` disables FEC. `kcpfecdata` + `kcpfecparity` must not exceed 256 |
| `statusaddr` | string | HTTP health/status listen address (e.g. `127.0.0.1:6060`); omit/empty disables |

### Server Example

Save as `config_server.json`:

```json
{
  "type": "server",
  "proto": ["tcp"],
  "listen": [":8888"],
  "key": "replace-with-auth-key",
  "encrypt": "replace-with-encrypt-key",
  "compress": 128,
  "loglevel": "info"
}
```

Run:
```bash
./spp -config config_server.json
```

### Client Example

Save as `config_client.json`:

```json
{
  "name": "my_client",
  "type": "proxy_client",
  "server": "127.0.0.1:8888",
  "proto": ["tcp"],
  "proxyproto": ["tcp"],
  "fromaddr": [":8080"],
  "toaddr": [":8080"],
  "key": "replace-with-auth-key",
  "encrypt": "replace-with-encrypt-key",
  "compress": 128,
  "loglevel": "info"
}
```

Run:
```bash
./spp -config config_client.json
```

---

## Command Line Reference

```text
Usage of spp:
  -type string
        Role type: server, proxy_client, reverse_proxy_client, socks5_client, reverse_socks5_client, http_client, reverse_http_client
  -config string
        Path to json configuration file
  -proto value
        Main transit protocol: [tcp udp rudp ricmp rhttp kcp quic]
  -proxyproto value
        Proxy protocol: [tcp udp]
  -listen value
        Server listen address (e.g. :8888)
  -server string
        Target server address (e.g. 1.2.3.4:8888)
  -fromaddr value
        Source address
  -toaddr value
        Destination target address
  -key string
        Authentication key (required; no default)
  -encrypt string
        Encryption key (empty disables encryption; no default)
  -encrypttype string
        Encryption type: none/aes-gcm/chacha20 (default "chacha20")
  -compress int
        Minimum payload size in bytes to compress (default 128, 0 disables)
  -loglevel string
        Log level: debug, info, warn, error (default "info")
  -nolog int
        Disable writing log file (1=disabled, 0=enabled)
  -noprint int
        Disable stdout printing (1=disabled, 0=enabled)
  -username string
        SOCKS5 username authentication
  -password string
        SOCKS5 password authentication
  -profile int
        Enable pprof profiling on specified port
  -ping
        Log periodic ping latency
  -statusaddr
        HTTP health/status listen address (e.g. 127.0.0.1:6060); empty
        disables. Serves GET /healthz (liveness) and GET /status (JSON:
        pipe state/RTT/throughput, services, sonny counts, byte counters).
        Bind to loopback unless external access is secured separately.
  -version, -v
        Print version and build details
```

`/status` example:

```bash
$ curl -s 127.0.0.1:6060/status | python3 -m json.tool
{
    "role": "server",
    "version": "0.14.1",
    "uptimeSec": 128,
    "goroutines": 37,
    "established": false,
    "clients": 1,
    "sonny": 2,
    "pipes": [
        {"proto": "tcp", "addr": "127.0.0.1:9000<--tcp-->...", "state": "active", "rttMs": 3, "thrBps": 1048576}
    ],
    "services": [{"index": 0, "kind": "output", "proto": "tcp", "sonny": 2}],
    "counters": {"MainRecvSize": 512000, "SendCompSaveSize": 81920}
}
```

---

## Docker Deployment

### Server

```bash
docker run -d --name spp-server \
  --restart always \
  --network host \
  esrrhs/spp ./spp -type server -proto tcp -listen :8888
```

### Client

```bash
docker run -d --name spp-client \
  --restart always \
  --network host \
  esrrhs/spp ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```

---

## Graceful Shutdown

SPP handles OS termination signals (`SIGINT`, `SIGTERM`, `Ctrl+C`). Upon receiving a signal:
1. Stop accepting new connections.
2. Gracefully terminate active sub-connections and close listeners.
3. Notify the remote server/client to release allocated resources.
4. Exit cleanly with code `0`.

---

## IPv6 Support

SPP works over IPv6 for all address-bearing options (`-listen`, `-server`,
`-fromaddr`, `-toaddr`), JSON config fields, and proxy destinations.

* **Bracket IPv6 literals with a port**: `[2001:db8::1]:8888`, `[::1]:1080`.
  Brackets are required so the colons inside an IPv6 address are not confused
  with the port separator.
* **Wildcard listeners are dual-stack**: a bare port (`:8888`) or `[::]:8888`
  accepts both IPv4 and IPv6 clients on platforms that support dual-stack
  sockets. Use `0.0.0.0:8888` / `[::1]:8888` to restrict to a single family.
* **SOCKS5**: CONNECT and UDP ASSOCIATE accept and return RFC 1928
  `ATYP=IP6` (type `4`) addresses. The UDP relay socket is automatically
  opened in the same address family as the client's TCP control connection,
  and the relay address returned to the client is reachable over that family.
* **HTTP proxy**: `CONNECT [host]:port` and absolute-form requests with
  bracketed IPv6 targets are supported.
* **Transit protocols**: all of `tcp`, `rudp`, `kcp`, `quic`, `rhttp`, and
  `ricmp` run over both IPv4 and IPv6. Because ICMP carries no port, `ricmp`
  addresses are host-only: on the client use a bare IPv6 literal without
  brackets (`-server 2001:db8::1`, or a link-local form such as
  `fe80::1%eth0`). On the server, `-listen 0.0.0.0` / `-listen ::` opens both
  an `ip4:icmp` and an `ip6:icmp` socket, `-listen ::1` restricts to IPv6, and
  `-listen 127.0.0.1` restricts to IPv4. Like IPv4, ICMPv6 raw sockets require
  root / `CAP_NET_RAW`; if one family cannot be opened, the other still
  listens.

Examples:

```bash
# dual-stack server
./spp -type server -proto tcp -listen :8888

# SOCKS5 client reaching an IPv6 server, exposing the proxy on IPv6 loopback
./spp -type socks5_client -proto tcp -server '[2001:db8::1]:8888' \
  -fromaddr '[::1]:1080' -proxyproto tcp -key 'your-auth-key'

# use it with an IPv6-aware client
curl -x 'socks5h://[::1]:1080' 'http://[2001:db8::2]:80/'
```
