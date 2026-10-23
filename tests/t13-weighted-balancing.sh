#!/usr/bin/env bash
# t13 - weighted distribution (docs/weighted-balancing.md): weights 5:3 must
# split ~62.5/37.5 and a LIVE weight change to 7:1 must move the split to
# ~87.5/12.5 without any pf.conf edit (the chain is rendered by pfkeeper from
# the git state into the pfkeeper/web anchor). Tolerance ±3% of the target
# share (1000 connections, binomial sigma ~1.5%).
#
# NOTE on round-robin mode: with the legacy RR rule a single source CANNOT
# show the split (sticky-address pins per source, see t03); weighted chains
# decide per connection, so one client source is enough here.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lab-env.sh
BIN=pfkeeper/bin/pfk
CERTS=(-cert pfkeeper/certs/client-admin.crt -key pfkeeper/certs/client-admin.key -ca pfkeeper/certs/ca.crt)
LB1=(-addr 192.168.100.11:8443 "${CERTS[@]}")

# measure <n> — "<rs1> <rs2> <no-answer>" counts of n fresh connections from client.
measure() {
  ssh_to client "for i in \$(seq $1); do curl -s --no-keepalive -m 5 -o /dev/null -w '%header{x-served-by}\n' http://192.168.100.200/; done" \
    | awk '{v=$1; sub(/^rs/, "", v); if (v != 1 && v != 2) v = 0; c[v]++} END {print c[1] + 0, c[2] + 0, c[0] + 0}'
}

set_weights() { # <w1> <w2> via MASTER gRPC (commit + apply + peer push)
  $BIN "${LB1[@]}" weight set webs 192.168.100.21 "$1" -r "t13" >/dev/null
  $BIN "${LB1[@]}" weight set webs 192.168.100.22 "$2" -r "t13" >/dev/null
}

wait_anchor() { # <pattern> — wait until the live lb1 anchor matches (max 45s)
  for _ in $(seq 1 15); do
    ssh_root_lb1 "pfctl -a pfkeeper/web -sr" 2>/dev/null | grep -q "$1" && return 0
    sleep 3
  done
  return 1
}

in_range() { [[ $1 -ge $2 && $1 -le $3 ]]; }

# setup: ensure services/web.yaml exists in the state and is weighted (a
# previous run may have left round-robin); the state lives in the bare repo
# on the client VM, MASTER is the only writer and picks it up on sync.
NEED_SYNC=0
if ! $BIN "${LB1[@]}" service show web >/dev/null 2>&1; then
  ssh_to client "rm -rf /tmp/pfk-st-t13 && git clone -q /var/db/pfkeeper-state.git /tmp/pfk-st-t13 && cd /tmp/pfk-st-t13 && git config user.name qa-test && git config user.email qa-test@pfkeeper && mkdir -p services && printf 'service: web\ntable: webs\nmode: weighted\nlisten:\n    iface: vtnet0\n    vip: 192.168.100.200\n    port: 80\n    proto: tcp\nsticky: false\n' > services/web.yaml && git add -A && git commit -q -m 'test: add web service weighted (t13)' && git push -q origin main && rm -rf /tmp/pfk-st-t13"
  NEED_SYNC=1
elif [[ $($BIN "${LB1[@]}" service show web 2>/dev/null | grep -o 'mode=[a-z-]*') != mode=weighted ]]; then
  ssh_to client "rm -rf /tmp/pfk-st-t13 && git clone -q /var/db/pfkeeper-state.git /tmp/pfk-st-t13 && cd /tmp/pfk-st-t13 && git config user.name qa-test && git config user.email qa-test@pfkeeper && sed -i 's/^mode: .*/mode: weighted/' services/web.yaml && git add -A && git commit -q -m 'test: mode=weighted (t13)' && git push -q origin main && rm -rf /tmp/pfk-st-t13"
  NEED_SYNC=1
fi
if ((NEED_SYNC)); then $BIN "${LB1[@]}" admin sync >/dev/null; fi
wait_anchor "probability" || test_fail "weighted chain never rendered"

# phase 1: 5:3 -> target 62.5% rs1 (±3% of 1000 => 595..655)
set_weights 5 3
wait_anchor "probability 62.5%" || test_fail "anchor never showed 62.5% (weights 5:3)"
read -r A1 A2 A0 <<<"$(measure 1000)"
echo "5:3 -> rs1=$A1 rs2=$A2 miss=$A0 (target 625/375 +-3%: 595..655 / 345..405)"
in_range "$A1" 595 655 || test_fail "rs1 share out of tolerance at 5:3: $A1/1000 (need 595..655)"
in_range "$A2" 345 405 || test_fail "rs2 share out of tolerance at 5:3: $A2/1000 (need 345..405)"
((A0 == 0)) || test_fail "$A0 connections got no answer at 5:3 (probability leak past catch-all?)"

# phase 2: live change to 7:1 -> target 87.5% (845..905), no pf.conf touched
set_weights 7 1
wait_anchor "probability 87.5%" || test_fail "anchor never showed 87.5% (weights 7:1)"
read -r B1 B2 B0 <<<"$(measure 1000)"
echo "7:1 -> rs1=$B1 rs2=$B2 miss=$B0 (target 875/125 +-3%: 845..905 / 95..155)"
in_range "$B1" 845 905 || test_fail "rs1 share out of tolerance at 7:1: $B1/1000 (need 845..905)"
in_range "$B2" 95 155 || test_fail "rs2 share out of tolerance at 7:1: $B2/1000 (need 95..155)"
((B0 == 0)) || test_fail "$B0 connections got no answer at 7:1"

# observability: pfkeeper_service_probability follows the live render (the
# GitWatcher republishes the gauge on its tick; allow ~40s after the change)
P=""
for _ in $(seq 1 10); do
  P=$(curl -s -m 5 http://192.168.100.11:9100/metrics | grep '^pfkeeper_service_probability' | awk '{print $1 "=" $2}' | sort | tr '\n' ' ')
  [[ $P == *87.5* ]] && break
  sleep 4
done
echo "metrics: $P"
[[ $P == *192.168.100.21*87.5* && $P == *192.168.100.22*12.5* ]] \
  || test_fail "service_probability does not reflect 7:1: $P"

# restore the stand baseline (weighted, 5:3)
set_weights 5 3
test_pass "weighted: 5:3 -> $A1/$A2, live 7:1 -> $B1/$B2, gauge follows the render"
