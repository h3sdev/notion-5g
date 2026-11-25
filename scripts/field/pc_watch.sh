#!/bin/bash
# pc_watch.sh - PC side of the field test (runs in WSL, keeps going if the Claude session drops).
# Every 2 s: ping the modem LAN and 8.8.8.8 from the PC (traffic that crosses the modem).
# Every 30 s: copy /tmp/ratwatch/* from the modem into results/field-<stamp>/modem/ (survives a modem reboot
# except for the last 30 s).
#   run:   NOTION_PASS=... nohup scripts/field/pc_watch.sh > /dev/null 2>&1 &
#   stop:  pkill -f pc_watch.sh
cd "$(dirname "$0")/../.." || exit 1
OUT=${OUT:-results/field-$(date +%Y%m%d-%H%M)}
mkdir -p "$OUT/modem"
echo "$OUT" > results/field-latest
echo "ts,lan_ms,inet_ttl,inet_ms" > "$OUT/pc_ping.csv"
n=0
while :; do
	ts=$(date +%s)
	lan=$(ping -c1 -W1 192.168.1.1 2>/dev/null | sed -n 's/.*time=\([0-9.]*\).*/\1/p')
	inet=$(ping -c1 -W2 8.8.8.8 2>/dev/null | sed -n 's/.*ttl=\([0-9]*\) time=\([0-9.]*\).*/\1,\2/p')
	echo "$ts,$lan,${inet:-,}" >> "$OUT/pc_ping.csv"
	if [ $((n % 15)) = 0 ] && [ -n "$lan" ]; then
		up=$(scripts/rsh.sh 'cut -d" " -f1 /proc/uptime' 2>/dev/null)
		if [ -n "$up" ]; then
			echo "$ts,$up" >> "$OUT/modem_uptime.csv"
			boot=$(( (ts - ${up%.*}) / 60 * 60 ))   # one folder per modem boot: a reboot never overwrites data
			mkdir -p "$OUT/modem/boot-$boot"
			scripts/rsh.sh 'cd /tmp/ratwatch && tar cf - samples.csv events.log syslog.log syslog-before.log 2>/dev/null' \
				2>/dev/null | tar xf - -C "$OUT/modem/boot-$boot" 2>/dev/null
		fi
	fi
	n=$((n + 1))
	sleep 2
done
