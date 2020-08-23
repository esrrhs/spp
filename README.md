# spp
spp是一个简单强大的网络代理工具。

# 功能
* 支持的协议：tcp、udp、可靠udp、可靠icmp（TODO）
* 支持的类型：正向代理、反向代理、socks5正向代理、socks5反向代理
* 协议和类型可以自由组合
* 外部代理协议和内部转发协议可以自由组合

# 使用
### 服务器
* 启动server，假设服务器ip是www.server.com，监听端口8888
```
# ./spp -type server -listen :8888
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

代理可靠udp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8081 -toaddr :8081 -proxyproto rudp

代理可靠icmp
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

同时代理上述三种
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -fromaddr :8081 -toaddr :8081 -proxyproto rudp -fromaddr :8082 -toaddr :8082 -proxyproto ricmp

```
* client和server之间的内部通信，也可以修改为其他协议，外部协议与内部协议之间自动转换。例如
```
代理tcp，内部用可靠udp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto rudp

代理tcp，内部用可靠icmp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto tcp -proto ricmp

代理udp，内部用tcp协议转发
# ./spp -name "test" -type proxy_client -server www.server.com:8888 -fromaddr :8080 -toaddr :8080 -proxyproto udp -proto tcp
```

# 性能测试
使用benchmark目录的iperf脚本在单机测试，cpu跑满，带宽数据如下
|     代理协议         | 直连     | tcp转发 | tcp转发（加密）  |tcp转发（加密压缩）  | rudp转发 | rudp转发（加密）  |rudp转发（加密压缩）  |
|--------------|----------|------------|------------|------------|------------|------------|------------|
| TCP | 2019 MBytes/sec | 1186 MBytes/sec   | 201 MBytes/sec | 25.9 MBytes/sec    |  10.5 MBytes/sec  |  10.1 MBytes/sec  |  11.1 MBytes/sec |

性能损耗点主要是打解包、gc、加密压缩算法导致。对于限制带宽的场景，可以牺牲cpu换取更高使用带宽。rudp因为协议的关系，极限值在10MB。

