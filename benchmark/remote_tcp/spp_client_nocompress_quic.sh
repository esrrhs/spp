#! /bin/bash

SER=`cat server`
../../spp -name "test" -type proxy_client -server $SER:8888 -fromaddr :8855 -toaddr :8844 -proxyproto tcp -proto quic -compress 0 -encrypt=""
