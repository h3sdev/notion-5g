#!/bin/sh
# ratwatch.sh - field recorder that runs ON the modem (busybox ash), independent of any PC/SSH session.
# Logs radio state, TX power, SoC temperature, raw PMIC ADC and netparam rules every INTERVAL s to /tmp/ratwatch/samples.csv, every RAT/cell/IP change
# to events.log, and syslog (logread -f) to syslog.log. Stops by itself after MAXSEC.
#   start:  scripts/rsh.sh 'sh -s start' < scripts/field/ratwatch.sh
#   stop:   scripts/rsh.sh 'sh -s stop'  < scripts/field/ratwatch.sh
# Writes only to /tmp (RAM). Installs nothing persistent.
D=/tmp/ratwatch
INTERVAL=${INTERVAL:-3}
MAXSEC=${MAXSEC:-7200}

case "$1" in
stop)
	[ -f $D/pids ] && kill $(cat $D/pids) 2>/dev/null
	rm -f $D/pids $D/logread.pid; echo "stopped"; exit 0 ;;
status)
	[ -f $D/pids ] && echo "running: $(cat $D/pids)" || echo "not running"
	wc -l $D/samples.csv $D/events.log 2>/dev/null; tail -3 $D/events.log 2>/dev/null; exit 0 ;;
start) ;;
*) echo "usage: start|stop|status"; exit 1 ;;
esac

[ -f $D/pids ] && { echo "already running: $(cat $D/pids)"; exit 1; }
mkdir -p $D

loop() {
	j() { echo "$ENG" | jsonfilter -e "$1" 2>/dev/null; }
	[ -s $D/samples.csv ] || echo "ts,uptime,sys_mode,data_mode,reg,lte_band,lte_pci,lte_rsrp,lte_sinr,rrc,nr_band,nr_pci,nr_arfcn,nr_rsrp,nr_sinr,ip,np_enabled,np_value,rules4,rules6,pkts4,pkts6,ping_ttl,ping_ms,tx_lte,tx_nr,temp_c,adc1,adc2" > $D/samples.csv
	prev=""; t0=$(date +%s)
	while [ $(( $(date +%s) - t0 )) -lt "$MAXSEC" ]; do
		ts=$(date +%s); up=$(cut -d' ' -f1 /proc/uptime)
		ENG=$(ubus call cm get_eng_info 2>/dev/null)
		LC=$(ubus call cm get_link_context 2>/dev/null)
		sm=$(echo "$LC" | jsonfilter -e '@.celluar_basic_info.sys_mode'); dm=$(echo "$LC" | jsonfilter -e '@.celluar_basic_info.data_mode')
		rg=$(echo "$LC" | jsonfilter -e '@.celluar_basic_info.RegStatus')
		lb=$(j @.eng.lte.band); lp=$(j @.eng.lte.phy_cell_id); lr=$(j @.eng.lte.rsrp); ls=$(j @.eng.lte.sinr); rrc=$(j @.eng.lte.rrc_state)
		nb=$(j @.eng.nr.band); np=$(j @.eng.nr.phy_cell_id); na=$(j @.eng.nr.dl_nrafcn); nr=$(j @.eng.nr.rsrp); ns=$(j @.eng.nr.sinr)
		ip=$(ip -4 -o addr show 2>/dev/null | awk '/ccinet/ {print $4; exit}')
		en=$(uci -q get netparam.main.enabled); v=$(uci -q get netparam.main.value)
		r4=$(iptables -t mangle -S 2>/dev/null | grep -c -- '--comment netparam ')
		r6=$(ip6tables -t mangle -S 2>/dev/null | grep -c -- '--comment netparam ')
		p4=$(iptables -t mangle -L POSTROUTING -v -n -x 2>/dev/null | awk '/netparam/ {print $1; exit}')
		p6=$(ip6tables -t mangle -L POSTROUTING -v -n -x 2>/dev/null | awk '/netparam/ {print $1; exit}')
		tl=$(j @.eng.lte.tx_power); tn=$(j @.eng.nr.tx_power)
		tc=$(awk '{printf "%.1f", $1/1000}' /sys/class/hwmon/hwmon0/temp1_input 2>/dev/null)
		ad=$(awk '/^adc1:/ {a=$2+0} /^adc2:/ {b=$2+0} END {print a","b}' /sys/kernel/debug/auxadc 2>/dev/null); [ -n "$ad" ] || ad=","
		pg=$(ping -c 1 -W 2 8.8.8.8 2>/dev/null | sed -n 's/.*ttl=\([0-9]*\) time=\([0-9.]*\).*/\1,\2/p'); [ -n "$pg" ] || pg=","
		echo "$ts,$up,$sm,$dm,$rg,$lb,$lp,$lr,$ls,$rrc,$nb,$np,$na,$nr,$ns,$ip,$en,$v,$r4,$r6,$p4,$p6,$pg,$tl,$tn,$tc,$ad" >> $D/samples.csv
		key="sys=$sm data=$dm lte=B$lb/$lp nr=${nb:+n$nb/$np} ip=$ip rules=$r4/$r6"
		if [ "$key" != "$prev" ]; then
			echo "$(date '+%F %T') up=$up $key (prev: ${prev:-none}) nr_rsrp=$nr lte_rsrp=$lr" >> $D/events.log
			prev=$key
		fi
		sleep "$INTERVAL"
	done
	echo "$(date '+%F %T') MAXSEC reached, stopping" >> $D/events.log
	kill $(cat $D/logread.pid 2>/dev/null) 2>/dev/null; rm -f $D/pids $D/logread.pid
}

logread > $D/syslog-before.log 2>/dev/null
( trap '' HUP; exec logread -f >> $D/syslog.log 2>/dev/null ) </dev/null >/dev/null 2>&1 &
lp=$!; echo $lp > $D/logread.pid
( trap '' HUP; loop ) </dev/null >/dev/null 2>&1 &
echo "$! $lp" > $D/pids
echo "$(date '+%F %T') START interval=${INTERVAL}s maxsec=$MAXSEC last_powerdown=[$(tr '\n' ' ' < /sys/kernel/debug/pm800_powerdown_log 2>/dev/null)] powerup=[$(tr '\n' ' ' < /sys/kernel/debug/pm800_powerup_log 2>/dev/null)]" >> $D/events.log
echo "started: $(cat $D/pids)"
