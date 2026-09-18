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
  - [5. Shadowsocks Plugin](#5-shadowsocks-plugin)
- [Protocol Multiplexing and Conversion](#protocol-multiplexing-and-conversion)
- [Configuration File](#configuration-file)
  - [Configuration File Schema](#configuration-file-schema)
  - [Server Example](#server-example)
  - [Client Example](#client-example)
- [Command Line Reference](#command-line-reference)
- [Docker Deployment](#docker-deployment)
- [Graceful Shutdown](#graceful-shutdown)

---

## Overview

SPP is designed to route and forward network traffic across diverse network environments and protocol boundaries. It supports:
- **Proxy Protocols**: TCP, UDP
- **Transit Protocols**: TCP, UDP, RUDP (Reliable UDP), RICMP (Reliable ICMP), RHTTP (Reliable HTTP), KCP, QUIC
- **Proxy Types**: Forward Proxy, Reverse Proxy, SOCKS5 Forward Proxy, SOCKS5 Reverse Proxy, Shadowsocks plugin mode

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

Starts a SOCKS5 proxy server on the local machine on port `8080`. All traffic sent to this SOCKS5 proxy is forwarded through the SPP server.

```bash
./spp -name "socks5" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

With username and password authentication:

```bash
./spp -name "socks5_auth" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp -username myuser -password mypass
```

### 4. SOCKS5 Reverse Proxy

Opens a SOCKS5 proxy server on the remote SPP server's port `8080`. Traffic sent to the remote server's SOCKS5 port is proxied through the client network.

```bash
./spp -name "rev_socks5" -type reverse_socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```

### 5. Shadowsocks Plugin

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

---

## Configuration File

SPP supports JSON configuration files via `-config <path>`. Command-line arguments can override settings in the configuration file.

### Configuration File Schema

| Field | Type | Description |
| :--- | :--- | :--- |
| `type` | string | `server`, `proxy_client`, `reverse_proxy_client`, `socks5_client`, `reverse_socks5_client` |
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
        Role type: server, proxy_client, reverse_proxy_client, socks5_client, reverse_socks5_client
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
  -version, -v
        Print version and build details
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
