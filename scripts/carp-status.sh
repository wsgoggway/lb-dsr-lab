#!/usr/bin/env bash
# Show the CARP and pfsync status of both balancers: which node is MASTER,
# the vhid/advskew of the VIP alias, and the pfsync interface state.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

for node in lb1 lb2; do
  case $node in
    lb1) ROOT_SSH=ssh_root_lb1 ;;
    lb2) ROOT_SSH=ssh_root_lb2 ;;
  esac
  echo "== $node =="
  "$ROOT_SSH" 'ifconfig vtnet0 | grep -E "carp:|vhid" ; ifconfig pfsync0 | head -3'
done
