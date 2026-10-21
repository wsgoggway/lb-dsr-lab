#!/usr/bin/env bash
# Quick HTTP benchmark against the VIP from the host, then lb1 pf state info.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
URL="${LAB_VIP_URL:-http://192.168.100.200/}"

echo "== benchmark $URL (30s, 10 concurrent) =="
if command -v hey >/dev/null 2>&1; then
  hey -z 30s -c 10 "$URL"
elif command -v ab >/dev/null 2>&1; then
  ab -n 20000 -c 10 "$URL"
else
  die "install 'hey' (pacman -S hey) or 'apache2-utils' (ab) on the host"
fi

echo "== lb1 pf state info =="
ssh_to lb1 'sudo pfctl -si'
