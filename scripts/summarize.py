#!/usr/bin/env python3
"""
summarize.py — resume results/speed.csv (y opcionalmente drive.csv) por etiqueta/operador.

Uso:  python3 scripts/summarize.py            -> tabla por tag+operator+rat+band
      python3 scripts/summarize.py --md       -> misma tabla en Markdown (pegar en el informe)
      python3 scripts/summarize.py --drive    -> resumen de results/drive.csv (cobertura por RAT/banda, RSRP, SINR)
"""
import argparse
import csv
import os
import statistics as stx

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def fnum(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def med(vals):
    vals = [v for v in vals if v is not None]
    return round(stx.median(vals), 1) if vals else None


def load(name):
    p = os.path.join(ROOT, "results", name)
    if not os.path.exists(p):
        raise SystemExit(f"no existe {p}")
    return list(csv.DictReader(open(p)))


def table(rows, cols, md):
    w = [max(len(str(r[i])) for r in [cols] + rows) for i in range(len(cols))]
    line = lambda r: ("| " if md else "") + (" | " if md else "  ").join(str(r[i]).ljust(w[i]) for i in range(len(cols))) + (" |" if md else "")
    print(line(cols))
    print(("|" + "|".join("-" * (x + 2) for x in w) + "|") if md else "-" * (sum(w) + 2 * len(w)))
    for r in rows:
        print(line(r))


def speed(md):
    rows = load("speed.csv")
    groups = {}
    for r in rows:
        k = (r.get("tag", ""), r.get("operator", ""), r.get("rat", ""), r.get("band_lte", ""))
        groups.setdefault(k, []).append(r)
    out = []
    for k, rs in sorted(groups.items()):
        out.append([*k, len(rs),
                    med(fnum(r.get("down_mbps")) for r in rs), med(fnum(r.get("up_mbps")) for r in rs),
                    med(fnum(r.get("ookla_down_mbps")) for r in rs), med(fnum(r.get("ookla_up_mbps")) for r in rs),
                    med(fnum(r.get("rtt_avg")) for r in rs),
                    med(fnum(r.get("rsrp_dbm")) for r in rs), med(fnum(r.get("sinr_db")) for r in rs),
                    sum(1 for r in rs if r.get("via_modem") == "False")])
    table(out, ["tag", "operador", "RAT", "banda", "n", "↓HTTP Mbps", "↑HTTP Mbps", "↓Ookla", "↑Ookla",
                "RTT ms", "RSRP dBm", "SINR dB", "no-via-modem"], md)


def drive(md):
    rows = load("drive.csv")
    groups = {}
    for r in rows:
        k = (r.get("operator", ""), r.get("rat", ""), r.get("band_lte", ""))
        groups.setdefault(k, []).append(r)
    total = len(rows)
    out = []
    for k, rs in sorted(groups.items(), key=lambda kv: -len(kv[1])):
        rsrp = [fnum(r.get("rsrp_dbm")) for r in rs]
        out.append([*k, len(rs), f"{100*len(rs)/total:.0f}%", med(rsrp),
                    sum(1 for v in rsrp if v is not None and v < -100),
                    med(fnum(r.get("sinr_db")) for r in rs), med(fnum(r.get("rtt_avg")) for r in rs),
                    sum(1 for r in rs if fnum(r.get("loss_pct")) not in (None, 0.0)),
                    len({r.get("pci") for r in rs})])
    table(out, ["operador", "RAT", "banda", "muestras", "% tiempo", "RSRP med", "RSRP<-100", "SINR med",
                "RTT med", "muestras c/pérdida", "celdas (PCI)"], md)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--md", action="store_true")
    ap.add_argument("--drive", action="store_true")
    a = ap.parse_args()
    drive(a.md) if a.drive else speed(a.md)
