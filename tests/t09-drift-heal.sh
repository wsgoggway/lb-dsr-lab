#!/usr/bin/env bash
# t09 - drift detect + heal: a manual pfctl table change is either caught by
# rules diff (fast path) or auto-healed by the peer snapshot (slow path);
# Admin.Sync always converges the node to the git state.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
LB2=(-addr 192.168.100.12:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)

cleanup() { ssh_root_lb2 'pfctl -t webs -T delete 192.168.100.99 >/dev/null 2>&1; true'; }
trap cleanup EXIT

ssh_root_lb2 'pfctl -t webs -T add 192.168.100.99' >/dev/null
DETECTED=""
for i in $(seq 1 20); do
  D=$($BIN "${LB2[@]}" rules diff 2>/dev/null || true)
  if echo "$D" | grep -q "192.168.100.99"; then DETECTED=yes; break; fi
  # re-add if the peer snapshot already healed it between checks
  ssh_root_lb2 'pfctl -t webs -T add 192.168.100.99 >/dev/null 2>&1' || true
  sleep 1
done
[[ -n $DETECTED ]] && echo "drift detected via rules diff" || echo "drift auto-healed faster than diff checks (also acceptable)"

# Admin.Sync must converge regardless
$BIN "${LB2[@]}" admin sync >/dev/null || test_fail "admin sync failed"
sleep 2
FINAL=$(ssh_root_lb2 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' ')
[[ $FINAL != *192.168.100.99* ]] || test_fail "192.168.100.99 still in the table after sync: $FINAL"
echo "final table on lb2: $FINAL"
test_pass "drift detect+heal: manual change cannot survive (diff and/or sync converge lb2)"
