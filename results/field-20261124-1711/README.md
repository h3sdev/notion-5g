# Prueba de campo 4G → 5G → 4G (2026-11-24, Claro, netparam = 64)

Grabado con `scripts/field/` (ratwatch.sh en el módem, pc_watch.sh en el PC). Resumen: `python3 scripts/field/analyze.py results/field-20261124-1711`.

| Momento | Reglas v4/v6 | TTL ping | Notas |
|---|---|---|---|
| 4G B28 → 5G-NSA B7 + n78 (17:14:14) | 3/3 sin interrupción | 64 | misma IP, sin recarga de firewall |
| 5G-NSA, 3 min 08 s, 6 cambios de celda | 3/3 | 64 (12/12) | mejor NR: n78 PCI 66, RSRP −78, SINR 12 |
| 5G → 4G (17:17) | 3/3 | 64 al volver | 25 s sin internet, pasó por sys_mode=1 (probablemente 3G) |
| Reinicio ~17:17:55 | vuelven 3/3 | 64 | IP nueva; causa: subvoltaje (PMIC UV_VSYS1/2=1, WD=0) — ver `modem/persistent-logfile.txt` |
| 17:19–18:49, oficina, solo 4G B28 | 3/3 todo el tiempo | 64 | 6,4 % de pings perdidos, SINR mediano 0 dB |

- `modem/boot-<epoch>/`: samples.csv (cada ~4 s), events.log (cambios), syslog.log (logread -f), syslog-before.log.
- `pc_ping.csv`: ping del PC al módem y a 8.8.8.8 (TTL 64 = salió por el módem; 118 = por Wi-Fi).
- Se perdieron ~16 s del módem entre la última copia (17:17:39) y el reinicio.
- Pendiente: repetir 5G → 4G con alimentación firme (cargador de pared o powerbank ≥ 2 A) para confirmar la causa del reinicio.
