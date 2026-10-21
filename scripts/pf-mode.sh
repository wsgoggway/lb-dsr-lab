#!/usr/bin/env bash
# Switch lb1 between the DSR and NAT pf rulesets.
#
# NAT additionally forces rs1/rs2 to send replies via lb1 (host routes for the
# client and the host bridge): the whole lab is one L2 segment with the host
# bridge as default gw, and rdr translation only applies to traffic that
# returns through pf. DSR mode removes those routes so backends answer the
# client directly - that is the whole point of DSR.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
MODE=${1:-}
case $MODE in
  dsr | nat) ;;
  *) die "usage: pf-mode.sh dsr|nat" ;;
esac

scp -q "${SSH_OPTS[@]}" "$LAB_DIR/freebsd/pf-$MODE.conf" "lab@${VM_IP[lb1]}:/tmp/pf.conf"
# Validate before installing so a bad ruleset cannot leave pf broken.
ssh_to lb1 'sudo pfctl -n -f /tmp/pf.conf \
  && sudo cp /tmp/pf.conf /etc/pf.conf \
  && sudo pfctl -f /etc/pf.conf \
  && echo "== active rules ==" && sudo pfctl -sr'

if [[ $MODE == nat ]]; then
  for rs in rs1 rs2; do
    ssh_to "$rs" 'sudo ip route replace 192.168.100.1/32 via 192.168.100.11 dev eth0
                  sudo ip route replace 192.168.100.10/32 via 192.168.100.11 dev eth0'
  done
else
  for rs in rs1 rs2; do
    ssh_to "$rs" 'sudo ip route del 192.168.100.1/32 2>/dev/null || true
                  sudo ip route del 192.168.100.10/32 2>/dev/null || true'
  done
fi
echo "pf mode: $MODE"
