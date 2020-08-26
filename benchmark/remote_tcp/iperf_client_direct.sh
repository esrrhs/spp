#! /bin/bash

SER=`cat server`
../iperf -c $SER -p 8844 -f M -F ../smalldata.bin
