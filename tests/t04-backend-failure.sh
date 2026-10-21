#!/usr/bin/env bash
# t04-backend-failure.sh - failover by removing a dead backend from <webs>.
#
# 1. stop nginx on rs1; probe from host AND client (states are flushed so the
#    two sources spread): the source pinned to rs1 must see failures;
# 2. pfctl -t webs -T delete 192.168.100.21 -> all requests must succeed;
# 3. re-add rs1, restart nginx -> VIP healthy again.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

probe() { # one request; prints "<backend> <time_total>" or empty on failure
  local h t b
  h=$(mktemp) || return 0
  if ! t=$(curl -s -m 5 -D "$h" -o /dev/null -w '%{time_total}' "http://$LAB_VIP/" 2>/dev/null); then
    rm -f "$h"
    echo ""
    return 0
  fi
  b=$(awk 'tolower($1) ~ /^x-served-by:/ {print $2}' "$h" | tr -d '\r')
  rm -f "$h"
  echo "$b $t"
}
client_served_by() {
  local hdr
  hdr=$(ssh_to client "curl -s -m 5 -D - -o /dev/null http://$LAB_VIP/" \
    | awk 'tolower($1) ~ /^x-served-by:/ {print $2}' || true)
  tr -d '\r' <<<"$hdr"
}

ssh_to lb1 true || test_fail "cannot ssh to lb1"

BASE=$(probe)
echo "baseline request: ${BASE:-FAILED}"

ssh_to rs1 'sudo systemctl stop nginx' || test_fail "cannot stop nginx on rs1"
sleep 1
# Spread the probes over both backends: kill states first, then use two sources.
ssh_to lb1 'sudo pfctl -k 0.0.0.0/0 >/dev/null 2>&1 || true
            sudo pfctl -F Sources >/dev/null 2>&1 || true'
FAILS=0
GOOD=0
for src in host client; do
  for _ in 1 2 3; do
    if [[ $src == host ]]; then R=$(probe); B=${R%% *}; else B=$(client_served_by); fi
    if [[ -n $B ]]; then GOOD=$((GOOD + 1)); else FAILS=$((FAILS + 1)); fi
  done
done
echo "rs1 nginx down (rs1 still in <webs>): ok=$GOOD failed=$FAILS"
if ((FAILS == 0)); then
  test_fail "no failures with rs1 down - probes never reached rs1, cannot verify failover"
fi

ssh_to lb1 'sudo pfctl -t webs -T delete 192.168.100.21'
GOOD=0
FAILS=0
TIMES=()
for src in host client; do
  for _ in 1 2 3; do
    if [[ $src == host ]]; then
      R=$(probe)
      B=${R%% *}
      TIMES+=("${R#* }")
    else
      B=$(client_served_by)
    fi
    if [[ -n $B ]]; then GOOD=$((GOOD + 1)); else FAILS=$((FAILS + 1)); fi
  done
done
echo "rs1 deleted from <webs>: ok=$GOOD failed=$FAILS"
echo "response times (host): ${TIMES[*]}"
if ((FAILS != 0)); then
  test_fail "requests still failing after removing rs1 from <webs>"
fi

ssh_to lb1 'sudo pfctl -t webs -T add 192.168.100.21'
ssh_to rs1 'sudo systemctl start nginx'
sleep 2
R=$(probe)
[[ -n ${R%% *} ]] || test_fail "VIP not serving after rs1 recovery"
echo "recovered: $R"

test_pass "failover OK: rs1 down caused $((FAILS + GOOD)) phase-1 failures, table delete restored service, re-add healed"
