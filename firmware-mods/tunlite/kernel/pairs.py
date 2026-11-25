#!/usr/bin/env python3
# Collect (ref_offset -> our_offset) pairs from aligned differing instructions.
import subprocess, re, sys, difflib, collections
sys.path.insert(0, sys.path[0])
OD = sys.argv[1]
def funcs(p):
    out = subprocess.run([OD, '-d', '--no-show-raw-insn', p], capture_output=True, text=True).stdout
    fs, cur = {}, None
    for l in out.splitlines():
        m = re.match(r'^[0-9a-f]+ <(.+)>:$', l)
        if m: cur = m.group(1); fs[cur] = []; continue
        m = re.match(r'^\s+[0-9a-f]+:\s+(.*)$', l)
        if m and cur:
            i = m.group(1).split(';')[0].split('@')[0].strip()
            i = re.sub(r'\b[0-9a-f]{3,8} <[^>]+>', 'T', i); i = re.sub(r'\.(n|w)\b', '', i)
            fs[cur].append(i)
    return fs
pairs = collections.Counter(); where = {}
for ref, ours in zip(sys.argv[2::2], sys.argv[3::2]):
    R, O = funcs(ref), funcs(ours)
    for n in set(R) & set(O):
        sm = difflib.SequenceMatcher(None, R[n], O[n], autojunk=False)
        for tag, i1, i2, j1, j2 in sm.get_opcodes():
            if tag != 'replace' or (i2 - i1) != (j2 - j1): continue
            for a, b in zip(R[n][i1:i2], O[n][j1:j2]):
                sa, sb = re.sub(r'#-?\d+', '#N', a), re.sub(r'#-?\d+', '#N', b)
                if sa != sb: continue
                na, nb = re.findall(r'#(-?\d+)', a), re.findall(r'#(-?\d+)', b)
                for x, y in zip(na, nb):
                    if x != y and '[' in a: pairs[(int(x), int(y))] += 1; where.setdefault((int(x), int(y)), set()).add(n)
for (x, y), c in sorted(pairs.items(), key=lambda t: t[0][1]):
    print(f'ours {y:5d} -> ref {x:5d}  delta {x-y:+5d}  x{c}  {sorted(where[(x,y)])[:4]}')
