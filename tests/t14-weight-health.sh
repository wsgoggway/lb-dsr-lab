#!/usr/bin/env bash
# t14 - health intersect weight (docs/weighted-balancing.md): stopping nginx
# on rs1 must re-render the probability chain WITHOUT rs1 on BOTH balancers
# (identical anchors, and-consensus), sending 100% of traffic to rs2; bringing
# nginx back must restore the weighted split. Weights are 5:3 during the test.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
CERTS=(-cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
LB1=(-addr 192.168.100.11:8443 "${CERTS[@]}")

# measure <n> — "<rs1> <rs2> <no-answer>" counts of n fresh connections.
measure() {
  ssh_to client "for i in \$(seq $1); do curl -s --no-keepalive -m 5 -o /dev/null -w '%header{x-served-by}\n' http://192.168.100.200/; done" \
    | awk '{v=$1; sub(/^rs/, "", v); if (v != 1 && v != 2) v = 0; c[v]++} END {print c[1] + 0, c[2] + 0, c[0] + 0}'
}

anchor() { ssh_root_lb1 "pfctl -a pfkeeper/web -sr" 2>/dev/null; }
anchor2() { ssh_root_lb2 "pfctl -a pfkeeper/web -sr" 2>/dev/null; }

# setup: rs1 nginx running, both members UP, weighted 5:3 (62.5% rule live)
ssh_to rs1 'sudo systemctl start nginx' >/dev/null || true
for i in $(seq 1 12); do
  UP=$($BIN "${LB1[@]}" health ls 2>/dev/null | grep -c "UP" || true)
  [[ $UP == 2 ]] && break
  sleep 5
done
((UP == 2)) || test_fail "members did not reach UP within warm-up (UP=$UP)"
$BIN "${LB1[@]}" weight set webs 192.168.100.21 5 -r "t14" >/dev/null
$BIN "${LB1[@]}" weight set webs 192.168.100.22 3 -r "t14" >/dev/null
for i in $(seq 1 15); do
  anchor | grep -q "probability 62.5%" && break
  sleep 3
done
anchor | grep -q "probability 62.5%" || test_fail "62.5% chain never rendered before the failure test"

# fail rs1: after fall (3 x 5s) + peer consensus + re-render the chain must
# contain ONLY rs2 (unconditional catch-all) on both nodes
ssh_to rs1 'sudo systemctl stop nginx' >/dev/null
echo "rs1 nginx stopped; waiting for fall (3x5s) and re-render..."
OK=""
for i in $(seq 1 20); do
  sleep 5
  if [[ $(anchor) == *192.168.100.22* && $(anchor) != *192.168.100.21* \
    && $(anchor2) == *192.168.100.22* && $(anchor2) != *192.168.100.21* ]]; then OK=1; break; fi
done
[[ $OK ]] || test_fail "anchors did not converge to rs2-only: lb1=[$(anchor)] lb2=[$(anchor2)]"
if [[ $(anchor) != $(anchor2) ]]; then test_fail "rs2-only anchors differ between lb1 and lb2"; fi
echo "rs2-only anchors identical on both nodes: $(anchor | head -1)"
read -r R1 R2 R0 <<<"$(measure 50)"
echo "rs1 down -> rs1=$R1 rs2=$R2 miss=$R0 (of 50)"
((R1 == 0 && R2 == 50 && R0 == 0)) || test_fail "expected 100% rs2 with rs1 down, got rs1=$R1 rs2=$R2 miss=$R0"

# heal: nginx back -> rise (2 x 5s) -> weighted chain returns on both nodes
ssh_to rs1 'sudo systemctl start nginx' >/dev/null
echo "rs1 nginx restored; waiting for rise and re-render..."
OK=""
for i in $(seq 1 20); do
  sleep 5
  [[ $(anchor) == *"probability 62.5%"* && $(anchor2) == *"probability 62.5%"* ]] && OK=1 && break
done
[[ $OK ]] || test_fail "62.5% chain never returned after rs1 recovery: lb1=[$(anchor)]"
if [[ $(anchor) != $(anchor2) ]]; then test_fail "post-heal anchors differ between lb1 and lb2"; fi
read -r H1 H2 H0 <<<"$(measure 200)"
echo "rs1 back -> rs1=$H1 rs2=$H2 miss=$H0 (of 200, target ~62.5/37.5, binomial +-8%)"
((H0 == 0)) || test_fail "$H0 unanswered connections after recovery"
[[ $H1 -ge 105 && $H1 -le 145 ]] || test_fail "post-recovery split far from 5:3: rs1=$H1/200"

test_pass "health x weight: rs1 down -> 100% rs2 ($R2/$R2) with identical anchors; recovery -> $H1/$H2 of 200"
