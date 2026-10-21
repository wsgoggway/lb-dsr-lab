#!/usr/bin/env bash
# t01-arp.sh - who answers ARP for the VIP?
# From the client VM, arping the VIP: ONLY lb1 (52:54:00:aa:00:11) may answer.
# If a backend MAC (00:21 / 00:22) shows up, the rs arp_ignore sysctls are
# broken and DSR would collapse at L2.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

ssh_to client true || test_fail "cannot ssh to client"

if ! ssh_to client command -v arping >/dev/null 2>&1; then
  test_skip "arping is not installed on client"
fi

OUT=$(ssh_to client "arping -c 3 -I eth0 $LAB_VIP 2>&1 || true")
echo "$OUT" | head -6

# Collect unique MACs (case-normalized) from arping output.
MACS=$(printf '%s\n' "$OUT" | grep -Eio '([0-9a-f]{2}:){5}[0-9a-f]{2}' | tr 'A-F' 'a-f' | sort -u)
echo "ARP responders for $LAB_VIP: ${MACS:-none}"

if ! grep -q '52:54:00:aa:00:11' <<<"$MACS"; then
  test_fail "lb1 MAC (52:54:00:aa:00:11) did not answer ARP for the VIP"
fi
for BAD in 52:54:00:aa:00:21 52:54:00:aa:00:22; do
  if grep -q "$BAD" <<<"$MACS"; then
    test_fail "backend MAC $BAD answered ARP for the VIP (arp_ignore broken)"
  fi
done

test_pass "VIP ARP answered only by lb1 (backends keep the VIP hidden on lo)"
