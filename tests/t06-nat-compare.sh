#!/usr/bin/env bash
# t06-nat-compare.sh - proves the DSR signature by its absence.
# Switches lb1 to the NAT ruleset (scripts/pf-mode.sh nat also routes backend
# replies through lb1), captures the same SYN/SYN-ACK traffic as t02 and
# asserts the OPPOSITE: SYN-ACKs from the VIP ARE on lb1 vtnet0, because pf
# rewrites src backend -> VIP on the way out. Then switches back to DSR.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

served_by() {
  local hdr
  hdr=$(curl -s -m 5 -D - -o /dev/null "http://$LAB_VIP/" \
    | awk 'tolower($1) ~ /^x-served-by:/ {print $2}' || true)
  tr -d '\r' <<<"$hdr"
}

ssh_to lb1 true || test_fail "cannot ssh to lb1"

echo "== switching lb1 to NAT mode =="
bash "$LAB_DIR/scripts/pf-mode.sh" nat
sleep 1

FILTER='tcp[tcpflags] & (tcp-syn|tcp-ack) == (tcp-syn|tcp-ack)'
ssh_root_lb1 "sh -c 'nohup tcpdump -ni vtnet0 -U -c 600 \"$FILTER\" > /tmp/dsr-cap.txt 2>&1 & echo \$! > /tmp/dsr-cap.pid'"
sleep 2

for _ in $(seq 1 10); do served_by >/dev/null; done

ssh_root_lb1 'kill "$(cat /tmp/dsr-cap.pid)" 2>/dev/null || true'
sleep 1
CAP=$(ssh_to lb1 'cat /tmp/dsr-cap.txt 2>/dev/null || true')
STATES=$(ssh_root_lb1 'pfctl -ss 2>/dev/null | grep -c tcp' || true)

SYN=$(grep -c ' > 192\.168\.100\.200\.80: Flags \[S\],' <<<"$CAP" || true)
SYNACK=$(grep -c 'IP 192\.168\.100\.200\.80 > .*Flags \[S\.\],' <<<"$CAP" || true)
echo "SYNs to VIP on lb1:       $SYN"
echo "SYN-ACKs from VIP on lb1: $SYNACK (DSR mode: 0, NAT mode: >0)"
echo "pf states through lb1:    $STATES"

if ((SYNACK == 0)); then
  bash "$LAB_DIR/scripts/pf-mode.sh" dsr >/dev/null 2>&1 || true
  test_fail "no SYN-ACKs from VIP captured - NAT mode is not in effect"
fi
CODE=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$LAB_VIP/" || true)
if [[ $CODE != 200 ]]; then
  bash "$LAB_DIR/scripts/pf-mode.sh" dsr >/dev/null 2>&1 || true
  test_fail "VIP not serving in NAT mode (http code: $CODE)"
fi
if ((STATES == 0)); then
  echo "warning: pfctl -ss showed no TCP states (unexpected in NAT mode)"
fi

echo "== switching back to DSR mode =="
bash "$LAB_DIR/scripts/pf-mode.sh" dsr
sleep 1
[[ -n $(served_by) ]] || test_fail "VIP not serving after switching back to DSR"

echo "conclusion: NAT mirrors every SYN-ACK through lb1 (stateful, symmetric path;"
echo "pf sees both flow halves, $STATES states). DSR never sees return packets -"
echo "that asymmetry is exactly what t02 asserted."

test_pass "NAT comparison done: SYN-ACKs from VIP present on lb1 ($SYNACK), then restored DSR"
