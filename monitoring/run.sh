#!/usr/bin/env bash
# lb-dsr-lab monitoring stack: Prometheus + Grafana on the host (host networking).
# Idempotent: re-running replaces the lab-prometheus / lab-grafana containers.
#
# Overrides (optional, via environment):
#   PROM_IMAGE    default prom/prometheus:v3.15.1
#   GRAFANA_IMAGE default grafana/grafana:11.6.7
#   PROM_PORT     default 9090
#   GRAFANA_PORT  default 3000 (set 3300 if 3000 is taken on the host)
set -euo pipefail

PROM_IMAGE="${PROM_IMAGE:-prom/prometheus:v3.15.1}"
GRAFANA_IMAGE="${GRAFANA_IMAGE:-grafana/grafana:11.6.7}"
PROM_PORT="${PROM_PORT:-9090}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Pull from Docker Hub, fall back to mirror.gcr.io if the Hub is unreachable.
resolve_image() {
    local img="$1"
    if docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img" >/dev/null 2>&1; then
        echo "$img"
        return 0
    fi
    local alt="mirror.gcr.io/$img"
    echo "INFO: cannot pull $img from Docker Hub, trying $alt" >&2
    docker pull "$alt" >/dev/null 2>&1 || {
        echo "ERROR: cannot pull $img nor $alt" >&2
        return 1
    }
    echo "$alt"
}

# Replace the container only if it already exists.
replace_container() {
    local name="$1"
    if docker ps -a --format '{{.Names}}' | grep -qx "$name"; then
        docker rm -f "$name" >/dev/null
    fi
}

PROM_IMG="$(resolve_image "$PROM_IMAGE")"
GRAF_IMG="$(resolve_image "$GRAFANA_IMAGE")"

replace_container lab-prometheus
docker run -d --name lab-prometheus --network host --restart unless-stopped \
    -v "$DIR/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
    "$PROM_IMG" \
    --config.file=/etc/prometheus/prometheus.yml \
    --storage.tsdb.path=/prometheus \
    --web.listen-address=":$PROM_PORT"

replace_container lab-grafana
docker run -d --name lab-grafana --network host --restart unless-stopped \
    -e GF_SERVER_HTTP_PORT="$GRAFANA_PORT" \
    -e GF_AUTH_ANONYMOUS_ENABLED=true \
    -e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer \
    -e GF_SECURITY_ADMIN_USER=admin \
    -e GF_SECURITY_ADMIN_PASSWORD=admin \
    -v "$DIR/grafana/provisioning:/etc/grafana/provisioning:ro" \
    -v "$DIR/grafana/dashboards:/var/lib/grafana/dashboards:ro" \
    "$GRAF_IMG"

echo
echo "Prometheus: http://127.0.0.1:$PROM_PORT  (targets: /targets)"
echo "Grafana:    http://127.0.0.1:$GRAFANA_PORT  (admin/admin, anonymous Viewer enabled)"
echo "Stop:       docker rm -f lab-prometheus lab-grafana"
