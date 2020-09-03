#! /bin/bash

../iperf -c 127.0.0.1 -p 8855 -f M  -F ../smalldata.bin -t 60
