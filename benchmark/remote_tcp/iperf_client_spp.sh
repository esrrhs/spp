#! /bin/bash

SER=`cat server`
iperf3 -c $SER -p 8855 -f M  -F ../data.bin
