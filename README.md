# spp

[<img src="https://img.shields.io/github/license/esrrhs/spp">](https://github.com/esrrhs/spp)
[<img src="https://img.shields.io/github/languages/top/esrrhs/spp">](https://github.com/esrrhs/spp)
[![Go Report Card](https://goreportcard.com/badge/github.com/esrrhs/spp)](https://goreportcard.com/report/github.com/esrrhs/spp)
[<img src="https://img.shields.io/github/v/release/esrrhs/spp">](https://github.com/esrrhs/spp/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/spp/total">](https://github.com/esrrhs/spp/releases)
[<img src="https://img.shields.io/docker/pulls/esrrhs/spp">](https://hub.docker.com/repository/docker/esrrhs/spp)
[<img src="https://img.shields.io/github/actions/workflow/status/esrrhs/spp/go.yml?branch=master">](https://github.com/esrrhs/spp/actions)

SPP is a simple and powerful proxy

## Note: This tool is only to be used for study and research, do not use it for illegal purposes

![image](show.png)

# Features
* Supported protocol: TCP, UDP, RUDP (Reliable UDP), RICMP (Reliable ICMP), RHTTP (Reliable HTTP), KCP, Quic
* Support type: forward proxy, reverse agent, SOCKS5 forward agent, SOCKS5 reverse agent
* Agreement and type can be freely combined
* External agent agreement and internal forwarding protocols can freely combine
* Support Shadowsocks plug-in, [spp-shadowsocks-plugin](https://github.com/esrrhs/spp-shadowsocks-plugin)，[spp-shadowsocks-plugin-android](https://github.com/esrrhs/spp-shadowsocks-plugin-android)

# Instructions
### Server
* Start Server, assume that the server IP is www.server.com, listening port 8888
```
# ./spp -type server -proto tcp -listen :8888
```
* You can also listen simultaneously with other types of ports and protocols.
```
# ./spp -type server -proto tcp -listen :8888 -proto rudp -listen :9999 -proto ricmp -listen 0.0.0.0
```
* Can also use Docker
```
# docker run --name my-server -d --restart=always --network host esrrhs/spp ./spp -proto tcp -listen :8888
```
### Client
* Start TCP forward proxy, map the 8080 port of www.server.com to the local 8080 so that access to local 8080 is equivalent to accessing www.server.com 8080
```
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```
* Start the TCP reverse agent, map the local 8080 to the 8080 port of www.server.com, which visit www.server.com 8080 is equivalent to accessing the local 8080
```
# ./spp -name "test" -type reverse_proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```
* Start TCP Positive Socks5 Agent, open the SOCKS5 protocol in the local 8080 port, access the network in Server through Server
```
# ./spp -name "test" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```
* Start TCP Reverse Socks5 Agent, open the Socks5 protocol at www.server.com's 8080 port, access the network in the client through the Client
```
# ./spp -name "test" -type reverse_socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```
* Other proxy protocols, only need to modify the proxyProto parameters of the client, for example

```
Proxy UDP
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp

Proxy rudp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8081 -toaddr :8081 -proxyproto rudp

Proxy ricmp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

At the same time, the above three
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -fromaddr :8081 -toaddr :8081 -proxyproto rudp -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

```
* Internal communication between Client and Server, can also be modified to other protocols, automatic conversion between external protocols and internal protocols. E.g

```
Proxy TCP, internal RUDP protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rudp

Proxy TCP, internal RICMP protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto ricmp

Agent UDP, internal TCP protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto tcp

Agent UDP, internal KCP protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto kcp

Proxy TCP, internal Quic protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto quic

Proxy TCP, internal RHTTP protocol forwarding
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rhttp
```
* Can also use Docker

```
# docker run --name my-client -d --restart=always --network host esrrhs/spp ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```

# Performance Testing
* Test the maximum bandwidth speed in the case where the IPERF script using the Benchmark / local_tcp directory is tested in the CPU. The proxy protocol is TCP, and the results of various transit protocols are used as follows:

|     Agent | Speed | Speed (Ency) | Speed (Encryption Compression)
|--------------|----------|----------|----------|
| Direct connection | 3535 MBytes/sec | | |
| tcp forwarding  | 663 MBytes/sec | 225 MBytes/sec | 23.4 MBytes/sec |
| rudp forwarding  | 5.15 MBytes/sec | 5.81 MBytes/sec | 5.05 MBytes/sec|
| ricmp forwarding  | 3.34 MBytes/sec | 3.25 MBytes/sec|3.46 MBytes/sec |
| rhttp forwarding  | 10.7 MBytes/sec | 10.8 MBytes/sec| 8.73 MBytes/sec|
| kcp forwarding  | 18.2 MBytes/sec | 18.6 MBytes/sec| 14.7 MBytes/sec|
| quic forwarding  | 35.5 MBytes/sec | 32.8 MBytes/sec|15.1 MBytes/sec |

* Using the IPERF script of the Benchmark / Remote_TCP directory, in the multi-machine test, the server is located in Tencent Cloud, the client is located locally, and the maximum bandwidth speed is tested. The proxy protocol is TCP, and the results of various transit protocols are used as follows:

|     Agent | Speed | Speed (Ency) | Speed (Encryption Compression)
|--------------|----------|----------|----------|
| Direct connection | 2.74 MBytes/sec | | |
| tcp forwarding | 3.81 MBytes/sec |3.90 MBytes/sec | 4.02 MBytes/sec|
| rudp forwarding | 3.33 MBytes/sec | 3.41 MBytes/sec| 3.58 MBytes/sec|
| ricmp forwarding | 3.21 MBytes/sec | 2.95 MBytes/sec| 3.17 MBytes/sec|
| rhttp forwarding | 3.48 MBytes/sec |3.49 MBytes/sec |3.39 MBytes/sec |
| kcp forwarding | 3.58 MBytes/sec |3.58 MBytes/sec | 3.75 MBytes/sec |
| quic forwarding | 3.85 MBytes/sec | 3.83 MBytes/sec | 3.92 MBytes/sec |


* Note: The test data is Centos.ISO, which has been compressed, so the effect of compression forwarding is not obvious.
* If you want to directly test each protocol bandwidth of the network, use multi-protocol bandwidth test tools [connperf](https://github.com/esrrhs/connperf)


## Thanks for free JetBrains Open Source license

<img src="https://resources.jetbrains.com/storage/products/company/brand/logos/GoLand.png" height="200"/></a>

