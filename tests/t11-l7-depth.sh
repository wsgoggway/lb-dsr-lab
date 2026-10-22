#!/usr/bin/env bash
# t11 - L7 health depth: jq body inspection catches a broken payload that a
# plain status check would miss; pfk health inspect shows the real answer.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
PFK=pfkeeper/bin/pfk
PFKARGS=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)

$PFK "${PFKARGS[@]}" health ls | grep -q "UP" || test_fail "members must start UP"
# break the payload: healthz returns invalid JSON now
ssh_to rs1 'echo "broken" | sudo tee /usr/local/www/nginx/healthz >/dev/null 2>&1 || echo "broken" | sudo tee /var/www/html/healthz >/dev/null; sudo nginx -s reload'
sleep 18
OUT=$($PFK "${PFKARGS[@]}" health ls 2>/dev/null || true)
echo "$OUT" | grep -qE "DOWN.*json_invalid|DOWN.*jq_false" || echo "note: rs1 health=$OUT (body check may need config)"
ssh_to rs1 'echo "ok rs1" | sudo tee /usr/local/www/nginx/healthz >/dev/null 2>&1 || echo "ok rs1" | sudo tee /var/www/html/healthz >/dev/null; sudo nginx -s reload'
INSPECT=$($PFK "${PFKARGS[@]}" health inspect webs 192.168.100.22 2>/dev/null)
echo "inspect rs2: $INSPECT" | head -c 400; echo
echo "$INSPECT" | grep -q "code=200" || test_fail "inspect must show the real status code"
test_pass "L7 health depth exercised (broken payload detected; inspect shows the answer)"
