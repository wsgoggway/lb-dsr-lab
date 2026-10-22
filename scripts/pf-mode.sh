#!/usr/bin/env bash
# Switch BOTH balancers of the CARP pair between the DSR and NAT pf rulesets.
#
# The pair must always run the SAME ruleset: states are replicated via
# pfsync, and a failover onto a node with a different ruleset would change
# DSR behavior mid-flight.
#
# NAT additionally forces rs1/rs2 to send replies via the active balancer
# (host routes for the client and the host bridge): the whole lab is one L2
# segment with the host bridge as default gw, and rdr translation only
# applies to traffic that returns through pf. DSR mode removes those routes
# so backends answer the client directly - that is the whole point of DSR.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
MODE=${1:-}
case $MODE in
  dsr | nat) ;;
  *) die "usage: pf-mode.sh dsr|nat" ;;
esac

for node in lb1 lb2; do
  case $node in
    lb1) ROOT_SSH=ssh_root_lb1 ;;
    lb2) ROOT_SSH=ssh_root_lb2 ;;
  esac
  scp -q "${SSH_OPTS[@]}" "$LAB_DIR/freebsd/pf-$MODE.conf" "root@${VM_IP[$node]}:/tmp/pf.conf"
  # Validate before installing so a bad ruleset cannot leave pf broken.
  if "$ROOT_SSH" 'pfctl -n -f /tmp/pf.conf && cp /tmp/pf.conf /etc/pf.conf && pfctl -f /etc/pf.conf && echo ok'; then
    echo "$node: pf mode $MODE applied"
  else
    die "$node: failed to apply the $MODE ruleset"
  fi
done

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
echo "pf mode: $MODE (applied to lb1+lb2)"
