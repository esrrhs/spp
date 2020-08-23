#! /bin/bash

rm CentOS-8.2.2004-x86_64-minimal.iso -rf
wget http://mirrors.cqu.edu.cn/CentOS/8.2.2004/isos/x86_64/CentOS-8.2.2004-x86_64-minimal.iso
mv CentOS-8.2.2004-x86_64-minimal.iso data.bin

rm CentOS-8.2.2004-x86_64-boot.iso -rf
wget http://mirrors.cqu.edu.cn/CentOS/8.2.2004/isos/x86_64/CentOS-8.2.2004-x86_64-boot.iso
mv CentOS-8.2.2004-x86_64-boot.iso smalldata.bin
