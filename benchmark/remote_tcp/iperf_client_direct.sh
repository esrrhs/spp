#! /bin/bash

SER=`cat server`
iperf3 -c $SER -p 8844 -f M -F ../smalldata.bin
