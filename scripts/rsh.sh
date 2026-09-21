#!/bin/bash
# rsh.sh — ejecuta un comando por SSH en el módem Notion 5G (OpenWrt/ASR1901).
# Uso:  scripts/rsh.sh 'comando remoto'
# Vars: NOTION_HOST (default 192.168.1.1), NOTION_USER (root), NOTION_PASS (notion)
#
# El dropbear del equipo es viejo: se habilita ssh-rsa y DH group1/14 explícitamente.
# La clave se pasa con SSH_ASKPASS para no depender de sshpass.

HOST="${NOTION_HOST:-192.168.1.1}"
USER_="${NOTION_USER:-root}"
PASS="${NOTION_PASS:-notion}"

ASK="$(mktemp)"
printf '#!/bin/sh\necho %q\n' "$PASS" > "$ASK"
chmod +x "$ASK"
trap 'rm -f "$ASK"' EXIT

export SSH_ASKPASS="$ASK" SSH_ASKPASS_REQUIRE=force DISPLAY="${DISPLAY:-:0}"
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    -o ConnectTimeout=8 -o PubkeyAuthentication=no \
    -o PreferredAuthentications=password,keyboard-interactive \
    -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa \
    -o KexAlgorithms=+diffie-hellman-group1-sha1,diffie-hellman-group14-sha1 \
    "$USER_@$HOST" "$@"
