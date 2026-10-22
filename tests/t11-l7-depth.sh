#!/usr/bin/env bash
# t11 - L7 health depth: the jq body expectation catches a payload whose
# status field is wrong even though the HTTP code is 200; pfk health inspect
# shows exactly what the service answered.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
PFK=pfkeeper/bin/pfk
PFKARGS=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)

# setup: restore a good payload (a previous failed run may have left it broken)
ssh_to rs1 'echo "{\"status\":\"ok\",\"host\":\"rs1\"}" | sudo tee /var/www/healthz >/dev/null' >/dev/null || true
UP=0
for i in $(seq 1 12); do
  UP=$($PFK "${PFKARGS[@]}" health ls 2>/dev/null | grep -c UP || true)
  [[ $UP == 2 ]] && break
  sleep 5
done
$PFK "${PFKARGS[@]}" health ls | grep -c UP | grep -q 2 || test_fail "members must start UP with jq active"
# monitors pick up the jq spec on their next registry sync (30 s tick)
sleep 45

# break the payload: valid HTTP 200, but jq '.status == "ok"' fails
ssh_to rs1 'echo "{\"status\":\"broken\"}" | sudo tee /var/www/healthz >/dev/null' >/dev/null
echo "rs1 payload broken (status=broken, code still 200); waiting fall=3..."
sleep 22
H=$($PFK "${PFKARGS[@]}" health ls 2>/dev/null)
echo "$H" | grep "192.168.100.21" | grep -q DOWN || test_fail "rs1 must be DOWN on jq mismatch: $H"
echo "$H" | grep "192.168.100.22" | grep -q UP || echo "note: rs2 view: ok"
INSPECT=$($PFK "${PFKARGS[@]}" health inspect webs 192.168.100.21 2>/dev/null)
echo "$INSPECT" | grep -q '"status_code": 200' || test_fail "inspect must show the real 200 code"
echo "$INSPECT" | grep -q broken || test_fail "inspect must show the response body"
echo "$INSPECT" | grep -qE 'jq_false|jq_error' || test_fail "inspect must show the jq failure reason"
echo "inspect: jq mismatch visible, body visible, code visible"

# restore payload -> rs1 back UP
ssh_to rs1 'echo "{\"status\":\"ok\",\"host\":\"rs1\"}" | sudo tee /var/www/healthz >/dev/null' >/dev/null
sleep 16
$PFK "${PFKARGS[@]}" health ls | grep "192.168.100.21" | grep -q UP || test_fail "rs1 must return UP after payload fix"
test_pass "L7 depth: jq body check catches wrong payload at code 200; inspect exposes the answer"
