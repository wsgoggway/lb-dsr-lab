#!/usr/bin/env bash
# Wait until SSH works against a lab VM: wait-ssh.sh <ip> [timeout-seconds=120]
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
IP=${1:?usage: wait-ssh.sh <ip> [timeout-sec]}
TIMEOUT=${2:-120}
ensure_ssh_key

DEADLINE=$((SECONDS + TIMEOUT))
echo "waiting for ssh lab@$IP (max ${TIMEOUT}s)"
while ! ssh "${SSH_OPTS[@]}" -o ConnectTimeout=3 "lab@$IP" true 2>/dev/null; do
  ((SECONDS < DEADLINE)) || die "timeout waiting for ssh lab@$IP"
  sleep 3
done
echo "ssh lab@$IP is ready"
