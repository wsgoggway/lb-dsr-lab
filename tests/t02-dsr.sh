#!/usr/bin/env bash
# t02-dsr.sh - THE core DSR test.
#
# Captures SYN/SYN-ACK traffic on lb1 vtnet0 while curling the VIP, then
# asserts the DSR signature:
#   (a) SYNs to the VIP are seen by lb1 (it forwards them);
#   (b) ZERO SYN-ACKs from the VIP are seen on lb1 - backends answer the
#       client directly, the return path bypasses the load balancer;
#   (c) both backends serve traffic. Curls come from the host AND the client:
#       sticky-address pins one backend per source IP, so a single source can
#       never see both backends (that per-source stickiness is t03's job).
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

served_by() {
  local hdr
  hdr=$(curl -s -m 5 -D - -o /dev/null "http://$LAB_VIP/" \
    | awk 'tolower($1) ~ /^x-served-by:/ {print $2}' || true)
  tr -d '\r' <<<"$hdr"
}
client_served_by() {
  local hdr
  hdr=$(ssh_to client "curl -s -m 5 -D - -o /dev/null http://$LAB_VIP/" \
    | awk 'tolower($1) ~ /^x-served-by:/ {print $2}' || true)
  tr -d '\r' <<<"$hdr"
}

ssh_to lb1 true || test_fail "cannot ssh to lb1"

FILTER='tcp[tcpflags] & (tcp-syn|tcp-ack) == (tcp-syn|tcp-ack)'
# Start tcpdump in the background on lb1 (lab user has passwordless sudo).
ssh_to lb1 "sudo sh -c 'nohup tcpdump -ni vtnet0 -U -c 600 \"$FILTER\" > /tmp/dsr-cap.txt 2>&1 & echo \$! > /tmp/dsr-cap.pid'"
sleep 2

HOST_HITS=()
for _ in $(seq 1 25); do HOST_HITS+=("$(served_by)"); done
CLIENT_HITS=()
for _ in $(seq 1 5); do CLIENT_HITS+=("$(client_served_by)"); done

ssh_to lb1 'sudo kill "$(cat /tmp/dsr-cap.pid)" 2>/dev/null || true'
sleep 1
CAP=$(ssh_to lb1 'cat /tmp/dsr-cap.txt 2>/dev/null || true')

SYN=$(grep -c ' > 192\.168\.100\.200\.80: Flags \[S\],' <<<"$CAP" || true)
SYNACK=$(grep -c 'IP 192\.168\.100\.200\.80 > .*Flags \[S\.\],' <<<"$CAP" || true)
C1=0
C2=0
for b in "${HOST_HITS[@]}" "${CLIENT_HITS[@]}"; do
  case $b in
    rs1) C1=$((C1 + 1)) ;;
    rs2) C2=$((C2 + 1)) ;;
  esac
done

echo "SYNs to VIP on lb1:       $SYN"
echo "SYN-ACKs from VIP on lb1: $SYNACK"
echo "X-Served-By over 30 requests (host+client): rs1=$C1 rs2=$C2"

if ((SYN == 0)); then
  test_fail "no SYNs to $LAB_VIP captured on lb1 vtnet0"
elif ((SYNACK != 0)); then
  test_fail "$SYNACK SYN-ACKs from VIP seen on lb1 - replies transit the LB, this is NOT DSR"
elif ((C1 == 0 || C2 == 0)); then
  test_fail "both backends must serve traffic, got rs1=$C1 rs2=$C2"
fi
test_pass "DSR signature confirmed: lb1 forwards SYNs, never sees SYN-ACKs; rs1=$C1 rs2=$C2"
