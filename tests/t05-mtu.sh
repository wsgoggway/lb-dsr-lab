#!/usr/bin/env bash
# t05-mtu.sh - big responses survive the DSR path untouched.
# Downloads a 2 MiB file via the VIP from the client (offloads are off) and
# sends a don't-fragment 1400-byte ping to the VIP.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

ssh_to client true || test_fail "cannot ssh to client"
ssh_to rs1 true || test_fail "cannot ssh to rs1"

SIZE=$(ssh_to client "curl -s -m 30 -o /dev/null -w '%{size_download}' http://$LAB_VIP/big.bin" | tail -n1)
echo "big.bin download size: $SIZE (expect 2097152)"
if [[ $SIZE != 2097152 ]]; then
  test_fail "big.bin download truncated: $SIZE != 2097152"
fi

if ! ssh_to client "ping -M do -s 1400 -c 2 $LAB_VIP" >/dev/null; then
  test_fail "1400-byte don't-fragment ping to the VIP failed"
fi
echo "ping -M do -s 1400 to VIP: ok"

test_pass "2 MiB DSR response intact (2097152 bytes) and DF ping to VIP ok"
