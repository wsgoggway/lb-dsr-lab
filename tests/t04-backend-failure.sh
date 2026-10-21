#!/usr/bin/env bash
# t04-backend-failure.sh - failover by removing a dead backend from <webs>.
#
# 1. with nginx UP, flush source tracking and probe until some source is
#    pinned to rs1 (round-robin assigns per fresh binding, so this may take
#    a couple of tries);
# 2. stop nginx on rs1: the pinned source must see failures;
# 3. pfctl -t webs -T delete 192.168.100.21 + flush: ALL requests succeed;
# 4. re-add rs1, restart nginx -> VIP healthy again.
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
flush_states() {
  # double -k kills states by src AND dst endpoint; single -k leaves the
  # reverse-half states that keep the sticky binding alive
  ssh_root_lb1 'pfctl -k 0.0.0.0/0 -k 0.0.0.0/0 >/dev/null 2>&1 || true
              pfctl -F Sources >/dev/null 2>&1 || true'
}

ssh_to lb1 true || test_fail "cannot ssh to lb1"

BASE=$(probe)
echo "baseline request: ${BASE:-FAILED}"

# Phase 0: pin some source to rs1 while it is still healthy.
PINNED=''
for _ in 1 2 3 4 5 6; do
  flush_states
  sleep 1
  if [[ $(probe) == rs1* ]]; then PINNED=host; break; fi
  if [[ $(client_served_by) == rs1 ]]; then PINNED=client; break; fi
done
[[ -n $PINNED ]] || test_fail "could not pin any source to rs1 in 6 tries"
echo "pinned source: $PINNED -> rs1"

# Phase 1: rs1 dies; the pinned source must fail.
ssh_to rs1 'sudo systemctl stop nginx' || test_fail "cannot stop nginx on rs1"
sleep 1
FAILS=0
GOOD=0
for _ in 1 2 3; do
  if [[ $PINNED == host ]]; then R=$(probe); B=${R%% *}; else B=$(client_served_by); fi
  if [[ -n $B ]]; then GOOD=$((GOOD + 1)); else FAILS=$((FAILS + 1)); fi
done
echo "rs1 nginx down (rs1 still in <webs>): ok=$GOOD failed=$FAILS (pinned: $PINNED)"
if ((FAILS == 0)); then
  test_fail "no failures with rs1 down - pinned source never reached rs1"
fi

# Phase 2: remove rs1 from the table -> everything must succeed via rs2.
ssh_root_lb1 'pfctl -t webs -T delete 192.168.100.21
            pfctl -k 0.0.0.0/0 -k 0.0.0.0/0 >/dev/null 2>&1 || true
            pfctl -F Sources >/dev/null 2>&1 || true'
sleep 1
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

# Phase 3: heal.
ssh_root_lb1 'pfctl -t webs -T add 192.168.100.21'
ssh_to rs1 'sudo systemctl start nginx'
sleep 2
R=$(probe)
[[ -n ${R%% *} ]] || test_fail "VIP not serving after rs1 recovery"
echo "recovered: $R"

test_pass "failover OK: pinned $PINNED saw rs1 failure, table delete restored service, re-add healed"
