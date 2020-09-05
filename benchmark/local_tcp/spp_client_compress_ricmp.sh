#! /bin/bash

../../spp -name "test" -type proxy_client -server 127.0.0.1 -fromaddr :8855 -toaddr :8844 -proxyproto tcp -proto ricmp
