#!/usr/bin/env bash
# t09 - config drift detection and heal: a manual pfctl table change is
# detected via the API (Diff) and healed by Admin.Sync.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
PFK=pfkeeper/bin/pfk
PFKARGS=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
PFK2=("${PFKARGS[@]}"); PFK2[0]=192.168.100.12:8443

ssh_root_lb2 'pfctl -t webs -T add 192.168.100.99' >/dev/null
sleep 2
D=$($PFK "${PFK2[@]}" rules diff 2>/dev/null || true)
echo "diff on lb2: $D"
echo "$D" | grep -q 192.168.100.99 || test_fail "drift (extra 192.168.100.99) not detected on lb2"
$PFK "${PFK2[@]}" admin sync >/dev/null || test_fail "admin sync failed"
sleep 2
$PFK "${PFK2[@]}" rules diff 2>/dev/null | grep -q 192.168.100.99 && test_fail "drift still present after sync"
test_pass "drift detected via Diff and healed by Admin.Sync (lb2)"
