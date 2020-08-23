#! /bin/bash

iperf3 -c 127.0.0.1 -p 8855 -f M  -F ../data.bin
