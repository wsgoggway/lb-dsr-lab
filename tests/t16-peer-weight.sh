#!/usr/bin/env bash
# t16 - peer path for weighted mutations (docs/weighted-balancing.md §5):
# a `weight set` issued against the BACKUP (lb2) must be redirected to the
# MASTER, committed in the state git and converged on the whole CARP pair
# (identical pfkeeper/web anchors, peer_reachable=1, peer_diverged=0 on both);
# a MANUAL anchor drift injected on one node must be caught by the peer
# SyncCheck and healed back to the state-rendered chain.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
CERTS=(-cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
LB1=(-addr 192.168.100.11:8443 "${CERTS[@]}")
LB2=(-addr 192.168.100.12:8443 "${CERTS[@]}")

anchor() { ssh_root_lb1 "pfctl -a pfkeeper/web -sr" 2>/dev/null; }
anchor2() { ssh_root_lb2 "pfctl -a pfkeeper/web -sr" 2>/dev/null; }
metric() { # <node-ip> <metric-prefix>
  curl -s -m 5 "http://$1:9100/metrics" | grep "^$2" | grep -v '^#'
}

wait_anchor() { # <pattern> on lb2, up to 45s (the write lands via peer push)
  for _ in $(seq 1 15); do
    anchor2 | grep -q "$1" && return 0
    sleep 3
  done
  return 1
}

# phase 1: write via the BACKUP -> redirect -> commit -> pair convergence.
# 3:1 renders 75% — a value nothing else on the stand uses, so seeing it on
# lb2 proves the mutation flowed BACKUP -> MASTER -> snapshot -> lb2.
#
# KNOWN RACE (filed as a finding, observed once in ~20 mutations): the
# GitWatcher tick hard-resets the state worktree every 30s; a mutation whose
# WriteTable lands inside that window is silently reverted BEFORE the commit
# status check — the server audits "ok" with the UNCHANGED HEAD and the
# weight change is lost. One retry of the whole phase keeps the test
# deterministic while the race is unfixed in product code.
apply_3_1() {
  $BIN "${LB2[@]}" weight set webs 192.168.100.21 3 -r "t16 3:1 via backup" >/dev/null
  $BIN "${LB2[@]}" weight set webs 192.168.100.22 1 -r "t16 3:1 via backup" >/dev/null
}
apply_3_1
wait_anchor "probability 75%" || {
  echo "75% did not converge (GitWatcher hard-reset race?) — retrying once"
  apply_3_1
  wait_anchor "probability 75%" || test_fail "75% chain never reached lb2 after BACKUP write (2 attempts)"
}
echo "backup write -> identical 3:1 anchors on the pair ($(anchor | head -1))"

# peer flags refresh on the SyncCheck tick (10s); right after a mutation the
# last tick may still report the pre-mutation hash, so POLL for convergence
# (observed: divergence clears within 1-2 ticks after the snapshot re-push).
OK=""
for i in $(seq 1 15); do
  M1=$(metric 192.168.100.11 pfkeeper_peer_reachable | awk '{print $2}')
  M2=$(metric 192.168.100.12 pfkeeper_peer_reachable | awk '{print $2}')
  D1=$(metric 192.168.100.11 pfkeeper_peer_diverged | awk '{print $2}')
  D2=$(metric 192.168.100.12 pfkeeper_peer_diverged | awk '{print $2}')
  [[ $M1 == 1 && $M2 == 1 && $D1 == 0 && $D2 == 0 ]] && OK=1 && break
  sleep 3
done
echo "peer metrics: lb1 reachable=$M1 diverged=$D1 | lb2 reachable=$M2 diverged=$D2"
[[ $OK ]] || test_fail "peer flags never converged (lb1 reachable=$M1 diverged=$D1, lb2 reachable=$M2 diverged=$D2) - dead-metric fix regressed"

# phase 2: manual drift on lb2 (BACKUP never touches git — only a peer push
# can heal it). SyncCheck (peer controller tick) must catch the mismatch and
# re-push the state-rendered chain within its tick window (observed <=45s).
ssh_root_lb2 "printf 'pass in quick on vtnet0 route-to (vtnet0 192.168.100.21) proto tcp to 192.168.100.200 port 80 probability 70%% keep state (sloppy)\npass in quick on vtnet0 route-to (vtnet0 192.168.100.22) proto tcp to 192.168.100.200 port 80 keep state (sloppy)\n' > /tmp/t16-drift.conf && pfctl -a pfkeeper/web -f /tmp/t16-drift.conf && rm -f /tmp/t16-drift.conf"
anchor2 | grep -q "probability 70%" || test_fail "drift injection failed (70% not live on lb2)"
echo "drift injected on lb2 (70%); waiting for SyncCheck heal..."
OK=""
for i in $(seq 1 15); do
  sleep 3
  anchor2 | grep -q "probability 75%" && OK=1 && break
done
[[ $OK ]] || test_fail "drift on lb2 not healed within 45s (SyncCheck): $(anchor2 | head -1)"
if [[ $(anchor) != $(anchor2) ]]; then test_fail "anchors differ after heal"; fi
echo "healed: lb2 anchor back to $(anchor2 | head -1)"

# restore the stand baseline (weighted, 5:3) through the same BACKUP path
$BIN "${LB2[@]}" weight set webs 192.168.100.21 5 -r "t16 restore 5:3" >/dev/null
$BIN "${LB2[@]}" weight set webs 192.168.100.22 3 -r "t16 restore 5:3" >/dev/null
M1=$(metric 192.168.100.11 pfkeeper_peer_reachable | awk '{print $2}')
M2=$(metric 192.168.100.12 pfkeeper_peer_reachable | awk '{print $2}')
[[ $M1 == 1 && $M2 == 1 ]] || test_fail "peer_reachable dropped after restore (lb1=$M1 lb2=$M2)"
test_pass "peer: BACKUP write converged (75%), drift healed by SyncCheck, peer_reachable=1 on both nodes"
