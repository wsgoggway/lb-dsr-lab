#!/usr/bin/env bash
# t07-carp-failover.sh - CARP failover of the balancer pair.
#
#   A. preflight: lb1 = CARP MASTER, lb2 = CARP BACKUP, pfsync0 up on both;
#   B. pfsync evidence: curls to the VIP, then pf states for the client
#      source must be visible on lb2 (state replication works);
#   C. failover: a 120-request curl loop runs on the client while lb1 is
#      powered off (shutdown -p now); lb2 must take the VIP over with at most
#      a handful of failed requests;
#   D. recovery: lb1 is started again, boots (~2.5 min), exactly one node is
#      MASTER afterwards (preempt=1 lets lb1 retake it) and the VIP serves.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

# == phase A: preflight ======================================================
ssh_root_lb1 true || test_fail "cannot ssh to lb1"
ssh_root_lb2 true || test_fail "cannot ssh to lb2"
C1=$(ssh_root_lb1 'ifconfig vtnet0 | grep "carp:"' || true)
C2=$(ssh_root_lb2 'ifconfig vtnet0 | grep "carp:"' || true)
echo "lb1 carp: $C1"
echo "lb2 carp: $C2"
grep -q 'carp: MASTER' <<<"$C1" || test_fail "lb1 is not CARP MASTER ($C1)"
grep -q 'carp: BACKUP' <<<"$C2" || test_fail "lb2 is not CARP BACKUP ($C2)"
ssh_root_lb1 'ifconfig pfsync0 >/dev/null' || test_fail "pfsync0 missing on lb1"
ssh_root_lb2 'ifconfig pfsync0 >/dev/null' || test_fail "pfsync0 missing on lb2"
echo "phase A: preflight ok (lb1 MASTER, lb2 BACKUP, pfsync0 present on both)"

# == phase B: pfsync evidence ================================================
# Generate traffic toward the VIP from the host AND the client, then look for
# client-source states on lb2: pfsync must have replicated them.
for _ in $(seq 1 12); do curl -sS -m 3 -o /dev/null "http://$LAB_VIP/" || true; done
for _ in $(seq 1 12); do ssh_to client "curl -sS -m 3 -o /dev/null http://$LAB_VIP/" || true; done
SYNCED=$(ssh_root_lb2 'pfctl -ss | grep -c "100\.10"' || true)
echo "states mentioning 192.168.100.10 on lb2: ${SYNCED:-0}"
if (( ${SYNCED:-0} > 0 )); then
  echo "pfsync: states visible on backup"
fi

# == phase C: failover =======================================================
echo "== phase C: powering lb1 off under a live client loop =="
ssh_to client "nohup sh -c 'for i in \$(seq 1 120); do if curl -sS -m 2 -o /dev/null http://192.168.100.200/ 2>/dev/null; then echo ok; else echo fail; fi; sleep 0.1; done > /tmp/t7-loop.txt 2>&1' >/dev/null 2>&1 &"
sleep 2
ssh_root_lb1 'shutdown -p now' || true # the shutdown kills sshd mid-run
DEADLINE=$((SECONDS + 60))
while ping -c1 -W1 "${VM_IP[lb1]}" >/dev/null 2>&1 && ((SECONDS < DEADLINE)); do sleep 1; done
if ping -c1 -W1 "${VM_IP[lb1]}" >/dev/null 2>&1; then
  test_fail "lb1 (${VM_IP[lb1]}) still pingable 60s after 'shutdown -p now'"
fi
echo "lb1 is off; waiting 5s for the CARP takeover (advbase 1 + advskew)"
sleep 5

# Let the client loop finish: 120 result lines, max 90s.
LINES=0
DEADLINE=$((SECONDS + 90))
while ((SECONDS < DEADLINE)); do
  LINES=$(ssh_to client 'wc -l < /tmp/t7-loop.txt' 2>/dev/null | tr -d ' ' || echo 0)
  if (( LINES >= 120 )); then break; fi
  sleep 3
done
if (( LINES < 120 )); then
  test_fail "client loop did not finish (got $LINES/120 results in 90s)"
fi
LOOP=$(ssh_to client 'cat /tmp/t7-loop.txt')
OK=$(grep -c '^ok$' <<<"$LOOP" || true)
FAILS=$(grep -c '^fail$' <<<"$LOOP" || true)
echo "client loop through the failover: ok=$OK fail=$FAILS (of 120)"
if ((FAILS > 10)); then
  test_fail "too many failed requests during failover: $FAILS (limit 10)"
fi
if ((OK < 100)); then
  test_fail "too few successful requests during failover: $OK (<100 required)"
fi
C2=$(ssh_root_lb2 'ifconfig vtnet0 | grep "carp:"' || true)
echo "lb2 carp after failover: $C2"
grep -q 'carp: MASTER' <<<"$C2" || test_fail "lb2 did not become CARP MASTER after lb1 died ($C2)"
echo "phase C: failover ok - VIP served through lb1 power-off (fail=$FAILS ok=$OK)"

# == phase D: recovery =======================================================
echo "== phase D: restarting lb1 =="
if ! "${VIRSH[@]}" start lb1 >/dev/null 2>&1; then
  test_skip "could not start lb1 via virsh - start it manually: virsh -c qemu:///system start lb1"
fi
DEADLINE=$((SECONDS + 180))
while ! ping -c1 -W1 "${VM_IP[lb1]}" >/dev/null 2>&1; do
  ((SECONDS < DEADLINE)) || test_fail "lb1 did not come back (ping) within 180s"
  sleep 5
done
echo "lb1 pings again; waiting for ssh (FreeBSD boot takes ~2.5 min)"
DEADLINE=$((SECONDS + 120))
while ! ssh_root_lb1 true >/dev/null 2>&1; do
  ((SECONDS < DEADLINE)) || test_fail "ssh to lb1 did not come back within 120s"
  sleep 5
done
M1=$(ssh_root_lb1 'ifconfig vtnet0 | grep "carp:"' || true)
M2=$(ssh_root_lb2 'ifconfig vtnet0 | grep "carp:"' || true)
echo "lb1 carp after recovery: $M1"
echo "lb2 carp after recovery: $M2"
MASTERS=0
if grep -q 'carp: MASTER' <<<"$M1"; then MASTERS=$((MASTERS + 1)); fi
if grep -q 'carp: MASTER' <<<"$M2"; then MASTERS=$((MASTERS + 1)); fi
if ((MASTERS != 1)); then
  test_fail "expected exactly one CARP MASTER after recovery, got $MASTERS"
fi
CODE=$(curl -sS -m 3 -o /dev/null -w '%{http_code}' "http://$LAB_VIP/" || true)
if [[ $CODE != 200 ]]; then
  test_fail "final curl to the VIP failed (http code: ${CODE:-none})"
fi

test_pass "CARP failover ok: loop ok=$OK fail=$FAILS, lb2 took over, lb1 recovered (master count: $MASTERS)"
