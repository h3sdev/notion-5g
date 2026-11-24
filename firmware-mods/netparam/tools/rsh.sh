#!/bin/bash
# rsh.sh - run a command on the device over SSH (used by install.sh and the tests).
#   NOTION_PASS=<root password> tools/rsh.sh 'command'
# Env: NOTION_HOST (default 192.168.1.1), NOTION_USER (default root), NOTION_PASS (required).
#
# The device's dropbear is old: ssh-rsa and DH group1/group14 have to be enabled explicitly.
# The password is passed through SSH_ASKPASS, so sshpass is not needed.
HOST="${NOTION_HOST:-192.168.1.1}"
USER_="${NOTION_USER:-root}"
PASS="${NOTION_PASS:?set NOTION_PASS to the device root password}"

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
