#!/usr/bin/env bash
# t03-sticky.sh - sticky-address is per SOURCE IP.
# States are flushed first so both sources get a fresh round-robin decision.
# Expected: the host lands on one backend for all 40 requests, the client on
# the other for all 10 - each source is pinned, the two sources are spread.
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

# Best effort: drop all states and sticky (source tracking) entries so the
# round-robin pointer decides freshly for both sources.
ssh_root_lb1 'pfctl -k 0.0.0.0/0 >/dev/null 2>&1 || true
            sudo pfctl -F Sources >/dev/null 2>&1 || sudo pfctl -F source-tracking >/dev/null 2>&1 || true'

HOST=()
for _ in $(seq 1 40); do HOST+=("$(served_by)"); done
CLIENT=()
for _ in $(seq 1 10); do CLIENT+=("$(client_served_by)"); done

UNIQ_HOST=$(printf '%s\n' "${HOST[@]}" | sort -u | grep -v '^$' || true)
UNIQ_CLIENT=$(printf '%s\n' "${CLIENT[@]}" | sort -u | grep -v '^$' || true)
HB=$(head -n1 <<<"$UNIQ_HOST")
CB=$(head -n1 <<<"$UNIQ_CLIENT")
echo "host   (40 reqs) saw: ${UNIQ_HOST//$'\n'/, }"
echo "client (10 reqs) saw: ${UNIQ_CLIENT//$'\n'/, }"

if [[ $(wc -l <<<"$UNIQ_HOST") != 1 ]]; then
  test_fail "host requests hit multiple backends ($UNIQ_HOST) - sticky-address is not pinning per source"
fi
if [[ $(wc -l <<<"$UNIQ_CLIENT") != 1 ]]; then
  test_fail "client requests hit multiple backends ($UNIQ_CLIENT) - sticky-address is not pinning per source"
fi

if [[ $HB == "$CB" ]]; then
  test_fail "both sources stuck to $HB; round-robin should have spread host/client across rs1/rs2"
fi

test_pass "sticky per source: host -> $HB (40/40), client -> $CB (10/10)"
