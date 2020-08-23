#! /bin/bash

iperf3 -c 127.0.0.1 -p 8844 -f M -F ../data.bin
