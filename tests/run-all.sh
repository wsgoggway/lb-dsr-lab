#!/usr/bin/env bash
# Run every lab test in order and print a summary table. Fails if any test
# FAILs; SKIP (exit 2) does not fail the run.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh

TESTS=(t00-smoke t01-arp t02-dsr t03-sticky t04-backend-failure t05-mtu t06-nat-compare t07-carp-failover t13-weighted-balancing t14-weight-health t15-weighted-sticky t16-peer-weight)
declare -A RESULT

for t in "${TESTS[@]}"; do
  printf '== %s ==\n' "$t"
  RC=0
  bash "tests/$t.sh" || RC=$?
  case $RC in
    0) RESULT[$t]=PASS ;;
    2) RESULT[$t]=SKIP ;;
    *) RESULT[$t]=FAIL ;;
  esac
  echo
done

printf '%-22s %s\n' "TEST" "RESULT"
FAILS=0
for t in "${TESTS[@]}"; do
  printf '%-22s %s\n' "$t" "${RESULT[$t]}"
  if [[ ${RESULT[$t]} == FAIL ]]; then FAILS=$((FAILS + 1)); fi
done
echo
if ((FAILS > 0)); then
  echo "$FAILS test(s) FAILED"
  exit 1
fi
echo "all tests passed (skips are not failures)"
