#!/usr/bin/env python3
"""Compare per-function disassembly of a reference .ko vs our .o (same compiler).
Normalizes addresses/branch targets; reports identical functions and first diffs."""
import subprocess, re, sys, difflib
OBJDUMP = sys.argv[1]
def funcs(path):
    out = subprocess.run([OBJDUMP, '-d', '--no-show-raw-insn', path], capture_output=True, text=True).stdout
    fs, cur = {}, None
    for line in out.splitlines():
        m = re.match(r'^[0-9a-f]+ <(.+)>:$', line)
        if m: cur = m.group(1); fs[cur] = []; continue
        m = re.match(r'^\s+[0-9a-f]+:\s+(.*)$', line)
        if m and cur:
            ins = m.group(1).split(';')[0].split('@')[0].strip()
            ins = re.sub(r'\b[0-9a-f]{3,8} <[^>]+>', 'T', ins)   # branch targets
            ins = re.sub(r'<[^>]+>', '', ins)
            ins = re.sub(r'\.(n|w)\b', '', ins)
            fs[cur].append(ins)
    return fs
ref, ours = funcs(sys.argv[2]), funcs(sys.argv[3])
same = diff = 0
for name in sorted(set(ref) & set(ours)):
    a, b = ref[name], ours[name]
    if a == b: same += 1; continue
    diff += 1
    d = [l for l in difflib.unified_diff(a, b, lineterm='', n=0) if not l.startswith(('---', '+++', '@@'))]
    print(f'  {name}: {len(a)} vs {len(b)} insns, {len(d)} diff lines; e.g. {d[:4]}')
print(f'{sys.argv[2].split("/")[-1]}: identical={same} different={diff} only_ref={len(set(ref)-set(ours))} only_ours={len(set(ours)-set(ref))}')
