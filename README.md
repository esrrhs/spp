# spp

[<img src="https://img.shields.io/github/license/esrrhs/spp">](https://github.com/esrrhs/spp)
[<img src="https://img.shields.io/github/languages/top/esrrhs/spp">](https://github.com/esrrhs/spp)
[![Go Report Card](https://goreportcard.com/badge/github.com/esrrhs/spp)](https://goreportcard.com/report/github.com/esrrhs/spp)
[<img src="https://img.shields.io/github/v/release/esrrhs/spp">](https://github.com/esrrhs/spp/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/spp/total">](https://github.com/esrrhs/spp/releases)
[<img src="https://img.shields.io/docker/pulls/esrrhs/spp">](https://hub.docker.com/repository/docker/esrrhs/spp)
[<img src="https://img.shields.io/github/workflow/status/esrrhs/spp/Go">](https://github.com/esrrhs/spp/actions)

spp是一个简单强大的网络代理工具。

![image](show.png)

# 功能
* 支持的协议：tcp、udp、rudp(可靠udp)、ricmp(可靠icmp)、rhttp(可靠http)、kcp、quic
* 支持的类型：正向代理、反向代理、socks5正向代理、socks5反向代理
* 协议和类型可以自由组合
* 外部代理协议和内部转发协议可以自由组合
* 支持shadowsocks插件，[spp-shadowsocks-plugin](https://github.com/esrrhs/spp-shadowsocks-plugin)，[spp-shadowsocks-plugin-android](https://github.com/esrrhs/spp-shadowsocks-plugin-android)

# 使用
### 服务器
* 启动server，假设服务器ip是www.server.com，监听端口8888
```
# ./spp -type server -proto tcp -listen :8888
```
* 也可以同时监听其他类型的端口与协议
```
# ./spp -type server -proto tcp -listen :8888 -proto rudp -listen :9999 -proto ricmp -listen 0.0.0.0
```
* 也可以使用docker
```
# docker run --name my-server -d --restart=always --network host esrrhs/spp ./spp -proto tcp -listen :8888
```
### 客户端
* 启动tcp正向代理，将www.server.com的8080端口映射到本地8080，这样访问本地的8080就相当于访问到了www.server.com的8080
```
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```
* 启动tcp反向代理，将本地8080映射到www.server.com的8080端口，这样访问www.server.com的8080就相当于访问到了本地的8080
```
# ./spp -name "test" -type reverse_proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```
* 启动tcp正向socks5代理，在本地8080端口开启socks5协议，通过server访问server所在的网络
```
# ./spp -name "test" -type socks5_client -server www.server.com:8888 -fromaddr :8080 -proxyproto tcp
```
* 启动tcp反向socks5代理，在www.server.com的8080端口开启socks5协议，通过client访问client所在的网络
```
# ./spp -name "test" -type reverse_proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```
* 其他代理协议，只需要修改client的proxyproto参数即可，例如
```
代理udp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp

代理rudp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8081 -toaddr :8081 -proxyproto rudp

代理ricmp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

同时代理上述三种
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -fromaddr :8081 -toaddr :8081 -proxyproto rudp -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

```
* client和server之间的内部通信，也可以修改为其他协议，外部协议与内部协议之间自动转换。例如
```
代理tcp，内部用rudp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rudp

代理tcp，内部用ricmp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto ricmp

代理udp，内部用tcp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto tcp

代理udp，内部用kcp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto kcp

代理tcp，内部用quic协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto quic

代理tcp，内部用rhttp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rhttp
```
* 也可以使用docker
```
# docker run --name my-client -d --restart=always --network host esrrhs/spp ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp
```

# 性能测试
* 使用benchmark/local_tcp目录的iperf脚本，在单机测试，在cpu跑满的情况下，测试最大带宽速度。代理协议是tcp，采用各种中转协议转发的结果如下：

|     代理方式   | 速度  | 速度（加密）  | 速度（加密压缩）  |
|--------------|----------|----------|----------|
| 直连 | 3535 MBytes/sec | | |
| tcp转发 | 663 MBytes/sec | 225 MBytes/sec | 23.4 MBytes/sec |
| rudp转发 | 5.15 MBytes/sec | 5.81 MBytes/sec | 5.05 MBytes/sec|
| ricmp转发 | 3.34 MBytes/sec | 3.25 MBytes/sec|3.46 MBytes/sec |
| rhttp转发 | 10.7 MBytes/sec | 10.8 MBytes/sec| 8.73 MBytes/sec|
| kcp转发 | 18.2 MBytes/sec | 18.6 MBytes/sec| 14.7 MBytes/sec|
| quic转发 | 35.5 MBytes/sec | 32.8 MBytes/sec|15.1 MBytes/sec |

* 使用benchmark/remote_tcp目录的iperf脚本，在多机测试，服务器位于腾讯云，客户端位于本地，测试最大带宽速度。代理协议是tcp，采用各种中转协议转发的结果如下：

|     代理方式   | 速度  |速度（加密）  | 速度（加密压缩）  |
|--------------|----------|----------|----------|
| 直连 | 2.74 MBytes/sec | | |
| tcp转发 | 3.81 MBytes/sec |3.90 MBytes/sec | 4.02 MBytes/sec|
| rudp转发 | 3.33 MBytes/sec | 3.41 MBytes/sec| 3.58 MBytes/sec|
| ricmp转发 | 3.21 MBytes/sec | 2.95 MBytes/sec| 3.17 MBytes/sec|
| rhttp转发 | 3.48 MBytes/sec |3.49 MBytes/sec |3.39 MBytes/sec |
| kcp转发 | 3.58 MBytes/sec |3.58 MBytes/sec | 3.75 MBytes/sec |
| quic转发 | 3.85 MBytes/sec | 3.83 MBytes/sec | 3.92 MBytes/sec |


* 注：测试数据是centos.iso，已经被压缩过了，所以压缩转发的效果不明显
* 如果想直接测试下网络的各协议带宽，使用多协议带宽测试工具[connperf](https://github.com/esrrhs/connperf)

