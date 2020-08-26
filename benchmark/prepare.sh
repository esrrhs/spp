#! /bin/bash


if test -f "data.bin"; then
	echo "data.bin exist"
else
	rm CentOS-8.2.2004-x86_64-minimal.iso -rf
	wget http://mirrors.cqu.edu.cn/CentOS/8.2.2004/isos/x86_64/CentOS-8.2.2004-x86_64-minimal.iso
	mv CentOS-8.2.2004-x86_64-minimal.iso data.bin
fi

if test -f "smalldata.bin"; then
	echo "smalldata.bin exist"
else
	rm CentOS-8.2.2004-x86_64-boot.iso -rf
	wget http://mirrors.cqu.edu.cn/CentOS/8.2.2004/isos/x86_64/CentOS-8.2.2004-x86_64-boot.iso
	mv CentOS-8.2.2004-x86_64-boot.iso smalldata.bin
fi

rm ./iperf
sudo wget -O ./iperf https://iperf.fr/download/ubuntu/iperf_2.0.9
chmod a+x ./iperf
