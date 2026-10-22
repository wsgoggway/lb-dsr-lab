#!/usr/bin/env bash
# t10 - mTLS and RBAC: no cert -> refused; viewer cannot mutate; operator can.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
NOAUTH=(-addr 192.168.100.11:8443)
VIEWER=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-viewer.crt -key pfkeeper/certs/client-viewer.key -ca pfkeeper/certs/ca.crt)
OPERATOR=(-addr 192.168.100.11:8443 -cert pfkeeper/certs/client-operator.crt -key pfkeeper/certs/client-operator.key -ca pfkeeper/certs/ca.crt)

if $BIN "${NOAUTH[@]}" tables ls >/dev/null 2>&1; then test_fail "unauthenticated request must be refused"; fi
echo "no-cert: refused (ok)"
if $BIN "${VIEWER[@]}" tables set webs 192.168.100.21 192.168.100.22 >/dev/null 2>&1; then test_fail "viewer must not mutate"; fi
echo "viewer mutation: denied (ok)"
$BIN "${VIEWER[@]}" tables ls >/dev/null || test_fail "viewer must be able to read"
$BIN "${OPERATOR[@]}" tables ls >/dev/null || test_fail "operator must be able to read"
test_pass "mTLS+RBAC: no-cert refused, viewer read-only, operator reads"
