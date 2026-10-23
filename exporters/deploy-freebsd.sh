#!/usr/bin/env bash
# Build pf_exporter for FreeBSD/amd64 and install it on lb1+lb2 over root SSH.
# Usage: bash exporters/deploy-freebsd.sh   (from repo root or anywhere)
set -euo pipefail
LAB_ROOT="$(cd "$(dirname "$0")/.." && pwd)"   # lb-dsr-lab repo root
MOD="$LAB_ROOT/exporters/pf-exporter"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes)
KEY="${PF_EXPORTER_KEY:-$HOME/.local/share/lb-dsr-lab/ssh/id_ed25519}"
# Target nodes and SSH key are overridable for real hardware:
#   PF_EXPORTER_NODES="10.0.100.1 10.0.100.2" PF_EXPORTER_KEY=~/.ssh/prod_key bash deploy.sh
NODES=(${PF_EXPORTER_NODES:-192.168.100.11 192.168.100.12})
VERSION="$(git -C "$LAB_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"

echo "== building freebsd/amd64 (version $VERSION)"
mkdir -p "$MOD/bin/freebsd"
(cd "$MOD" && CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 \
  go build -ldflags "-s -w -X main.version=$VERSION" -o bin/freebsd/pf-exporter ./cmd/pf-exporter)

for node in "${NODES[@]}"; do
  echo "== deploying to $node"
  ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" \
    'service pf_exporter stop >/dev/null 2>&1 || true; rm -f /var/run/pf_exporter.pid /var/run/pf_exporter-daemon.pid'
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$MOD/bin/freebsd/pf-exporter" "root@$node:/usr/local/bin/pf_exporter"
  scp -O -q "${SSH_OPTS[@]}" -i "$KEY" "$LAB_ROOT/freebsd/rc.d/pf_exporter" "root@$node:/etc/rc.d/pf_exporter"
  ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" \
    'chmod 555 /etc/rc.d/pf_exporter /usr/local/bin/pf_exporter; sysrc pf_exporter_enable=YES >/dev/null; service pf_exporter start'
  ssh "${SSH_OPTS[@]}" -i "$KEY" "root@$node" 'sleep 1; service pf_exporter status || true'
  echo "   deployed: $node"
done
echo "== done (all nodes)"
