#!/usr/bin/env bash
# t00-smoke.sh - all four VMs pingable from the host, each answers its
# expected hostname over SSH.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

for vm in "${VM_NAMES[@]}"; do
  if ! ping -c1 -W2 "${VM_IP[$vm]}" >/dev/null 2>&1; then
    test_fail "ping ${VM_IP[$vm]} ($vm) failed from host"
  fi
done
echo "all 4 VM IPs pingable from host"

for vm in "${VM_NAMES[@]}"; do
  if ! HOSTNAME_OUT=$(ssh_to "$vm" hostname); then
    test_fail "cannot ssh to $vm (${VM_IP[$vm]})"
  fi
  if [[ $HOSTNAME_OUT != "$vm" ]]; then
    test_fail "hostname mismatch: $vm returned '$HOSTNAME_OUT'"
  fi
done
echo "ssh ok, hostnames match: ${VM_NAMES[*]}"

test_pass "smoke: 4/4 VMs reachable via icmp+ssh"
