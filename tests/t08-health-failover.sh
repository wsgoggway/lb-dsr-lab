#!/usr/bin/env bash
# t08 - health-driven failover: stopping nginx on rs1 must remove it from the
# pf tables on BOTH balancers automatically (consensus and), traffic stays
# 100% on rs2 without any manual pfctl; bringing nginx back restores the member.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
PFK=pfkeeper/bin/pfk
PFKARGS=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
table_members() { ssh_root_lb1 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' '; }

# setup: ensure rs1 nginx is running (a previous test may have stopped it)
ssh_to rs1 'sudo systemctl start nginx' >/dev/null || true
# warm-up: wait until health probers promote both members (rise=2 x interval)
UP=0
for i in $(seq 1 12); do
  UP=$($PFK "${PFKARGS[@]}" health ls 2>/dev/null | grep -c "UP" || true)
  [[ $UP == 2 ]] && break
  sleep 5
done
[[ $UP == 2 ]] || test_fail "members did not reach UP within warm-up (UP=$UP)"
# converge: tables fill after peer-view propagation (up to ~30 s)
CONV=""
for i in $(seq 1 12); do
  CONV=$(table_members)
  [[ $CONV == *192.168.100.21* ]] && break
  sleep 5
done
[[ $CONV == *192.168.100.21* ]] || test_fail "rs1 never converged into the table (last: $CONV)"
echo "initial: $(table_members)"

ssh_to rs1 'sudo systemctl stop nginx' >/dev/null
echo "rs1 nginx stopped; waiting for probers (fall=3 x 5s)..."
sleep 40
M1=$(table_members); M2=$(ssh_root_lb2 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' ')
echo "lb1 table: $M1"; echo "lb2 table: $M2"
[[ $M1 != *192.168.100.21* && $M1 == *192.168.100.22* ]] || test_fail "rs1 not removed from lb1 table: $M1"
[[ $M2 != *192.168.100.21* && $M2 == *192.168.100.22* ]] || test_fail "rs1 not removed from lb2 table: $M2"
for i in 1 2 3 4 5; do curl -sS -m 3 -o /dev/null "http://192.168.100.200/" || test_fail "VIP failed with only rs2 alive"; done

ssh_to rs1 'sudo systemctl start nginx' >/dev/null
echo "rs1 nginx restored; waiting for rise=2..."
sleep 15
M1=$(table_members); M2=$(ssh_root_lb2 'pfctl -t webs -T show' 2>/dev/null | sort | tr '\n' ' ')
[[ $M1 == *192.168.100.21* ]] || test_fail "rs1 not restored on lb1: $M1"
[[ $M2 == *192.168.100.21* ]] || test_fail "rs1 not restored on lb2: $M2"
curl -sS -m 3 "http://192.168.100.200/healthz" | grep -q ok || test_fail "healthz broken"
test_pass "health-driven failover: rs1 auto-removed from both tables and restored"
