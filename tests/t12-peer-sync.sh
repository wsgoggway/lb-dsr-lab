#!/usr/bin/env bash
# t12 - peer sync: a write through the BACKUP is committed and applied on the
# MASTER within the lag window; peer stays reachable, no divergence.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
LB1=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
LB2=(-addr 192.168.100.12:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)

$BIN "${LB2[@]}" tables set webs 192.168.100.21 192.168.100.22 >/dev/null || test_fail "write via BACKUP failed"
sleep 8
H1=$(ssh_root_lb1 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' ')
H2=$(ssh_root_lb2 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' ')
echo "lb1: $H1"; echo "lb2: $H2"
[[ $H1 == $H2 ]] || test_fail "tables diverged: lb1=[$H1] lb2=[$H2]"
PEER=$($BIN "${LB1[@]}" status peer 2>/dev/null)
echo "$PEER" | grep -q "reachable=true" || test_fail "peer unreachable: $PEER"
echo "$PEER" | grep -q "diverged=false" || test_fail "divergence flag set: $PEER"
test_pass "peer sync: write via BACKUP converged on MASTER, no divergence"
