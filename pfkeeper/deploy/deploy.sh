#!/usr/bin/env bash
# Build pfkeeper for FreeBSD/amd64 and install it on lb1+lb2 over root SSH.
# Usage: bash pfkeeper/deploy/deploy.sh   (from repo root or anywhere)
set -euo pipefail
LAB_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"   # lb-dsr-lab repo root
PFK="$LAB_ROOT/pfkeeper"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes)
KEY="$HOME/.local/share/lb-dsr-lab/ssh/id_ed25519"
NODES=(192.168.100.11 192.168.100.12)

echo "== building freebsd/amd64"
make -C "$PFK" build-freebsd

for node in "${NODES[@]}"; do
  echo "== deploying to $node"
  ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" 'mkdir -p /usr/local/bin /usr/local/etc/pfkeeper /var/db/pfkeeper /var/log'
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$PFK/bin/freebsd/pfkeeper" "root@$node:/usr/local/bin/pfkeeper"
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$PFK/bin/freebsd/pfk" "root@$node:/usr/local/bin/pfk"
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$PFK/rc.d/pfkeeper" "root@$node:/etc/rc.d/pfkeeper"
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$PFK/deploy/newsyslog.conf.d/pfkeeper.conf" "root@$node:/usr/local/etc/newsyslog.conf.d/" 2>/dev/null || \
    ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" 'mkdir -p /usr/local/etc/newsyslog.conf.d'
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$PFK/deploy/newsyslog.conf.d/pfkeeper.conf" "root@$node:/usr/local/etc/newsyslog.conf.d/pfkeeper.conf"
  ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" 'chmod 555 /etc/rc.d/pfkeeper /usr/local/bin/pfkeeper /usr/local/bin/pfk; sysrc pfkeeper_enable=YES >/dev/null; service pfkeeper restart >/dev/null 2>&1 || service pfkeeper start'
  echo "   deployed: $node"
done
echo "== done (both nodes)"
