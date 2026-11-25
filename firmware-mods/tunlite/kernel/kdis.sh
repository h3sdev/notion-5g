#!/bin/bash
# kdis.sh <function>  -> disassemble a vendor-kernel function (Thumb-2) using device kallsyms
K=${REF:-$HOME/notion-build/ref}/kallsyms.txt
a=$(awk -v f="$1" '$3==f && ($2=="T"||$2=="t") {print $1; exit}' $K)
[ -z "$a" ] && { echo "no symbol $1"; exit 1; }
n=$(awk -v a="$a" '{print $1}' $K | sort -u | awk -v a="$a" '$1>a {print; exit}')
/home/ubudev/notion-build/openwrt-toolchain-24.10.8-sunxi-cortexa7_gcc-13.3.0_musl_eabi.Linux-x86_64/toolchain-arm_cortex-a7+neon-vfpv4_gcc-13.3.0_musl_eabi/bin/arm-openwrt-linux-muslgnueabi-objdump -D -M force-thumb --no-show-raw-insn --start-address=0x$a --stop-address=0x$n ${REF:-$HOME/notion-build/ref}/vmlinux.elf | tail -n +8 | cut -f2-
