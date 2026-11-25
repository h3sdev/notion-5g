#!/usr/bin/env python3
"""Apply the ASR1901 stock-kernel structure layout to a Linux 5.4.288 tree that already has the OpenWrt 21.02
generic 5.4 patches. Each change was measured against the stock modules / kernel image (see ../BUILD.md)."""
import sys, pathlib
k = pathlib.Path(sys.argv[1])
edits = [
    ('include/linux/skbuff.h',
     '\t__u32\t\t\theaders_end[0];\n',
     '\t__u32\t\t\theaders_end[0];\n\t/* ASR1901 vendor kernel: 20 bytes of vendor fields here (offsets measured against the stock modules) */\n\t__u8\t\t\tasr_vendor_pad[20];\n'),
    ('include/linux/module.h',
     'struct module_kobject {\n\tstruct kobject kobj;\n\tstruct module *mod;\n',
     'struct module_kobject {\n\tstruct kobject kobj;\n\tstruct module *mod;\n\t/* ASR1901 vendor kernel: 36 extra bytes before drivers_dir (measured in module_add_driver) */\n\tchar asr_vendor_pad[36];\n'),
    ('include/linux/module.h',
     '\tatomic_t refcnt;\n',
     '\tatomic_t refcnt;\n\t/* ASR1901 vendor kernel: sizeof(struct module) is 512 (.gnu.linkonce.this_module of stock modules) */\n\tchar asr_vendor_tail[64];\n'),
    ('arch/arm/include/asm/module.h',
     '\tstruct unwind_table *unwind[ARM_SEC_MAX];\n',
     '\t/* ASR1901 vendor kernel: 3 unwind slots (backported ARM_SEC rework), measured in module_frob_arch_sections */\n\tstruct unwind_table *unwind[3];\n'),
]
for rel, old, new in edits:
    p = k / rel; s = p.read_text()
    if new in s: continue
    if s.count(old) < 1: sys.exit(f'{rel}: anchor not found')
    p.write_text(s.replace(old, new, 1)); print('patched', rel)
