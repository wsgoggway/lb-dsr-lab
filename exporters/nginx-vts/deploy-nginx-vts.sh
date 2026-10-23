#!/usr/bin/env bash
# deploy-nginx-vts.sh - full nginx observability stack on the DSR lab
# Debian backends (rs1/rs2): prometheus-node-exporter, nginx-module-vts
# (dynamic module built against the exact installed nginx), and
# nginx-vts-exporter (cross-built on the host).
#
# Pinned versions (deployed 2026-10-23/24, verified live 2026-10-22 report):
#   nginx                      1.26.3  (Debian 13 apt, `nginx -V`)
#   nginx-module-vts           v0.2.7  (github.com/vozlt/nginx-module-vts,
#                                       commit 38b8612527ecb003ea15cefe934f302cf1a7c27e)
#   nginx-vts-exporter (sysulq) c12edaab1412e04005642fe7d58411409fc41d18 (master)
#   prometheus-node-exporter   1.9.0-1+b4 (Debian apt)
#
# Layout after deploy:
#   :9100 node_exporter  :8081 nginx vts status (JSON, LAN-only)
#   :9913 nginx-vts-exporter -> Prometheus at http://<rs1|rs2>:9913/metrics
#
# Usage: scripts run from the repo; deploy to both backends by default.
#   ./deploy-nginx-vts.sh [VM...]        # VM names per lab-env.sh, default: rs1 rs2
#
# Safety: /etc/nginx is backed up to /etc/nginx.bak-pre-vts (first run only);
# after any nginx reload the pfkeeper healthz probe is verified and the
# config is rolled back if the probe breaks.
set -euo pipefail

LAB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../scripts/lab-env.sh
source "$LAB_ROOT/scripts/lab-env.sh"

NGINX_VER=1.26.3
VTS_TAG=v0.2.7
VTS_COMMIT=38b8612527ecb003ea15cefe934f302cf1a7c27e
EXPORTER_REPO=https://github.com/sysulq/nginx-vts-exporter
EXPORTER_COMMIT=c12edaab1412e04005642fe7d58411409fc41d18
EXPORTER_DIR="${TMPDIR:-/tmp}/nginx-vts-exporter"
HOST_EXPORTER_BIN="$EXPORTER_DIR/nginx-vts-exporter"
NGINX_DL="https://nginx.org/download/nginx-${NGINX_VER}.tar.gz"

if [[ $# -gt 0 ]]; then VMS=("$@"); else VMS=(rs1 rs2); fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. Host-side: cross-build nginx-vts-exporter for the Debian guests.
# ---------------------------------------------------------------------------
build_exporter() {
    command -v go >/dev/null || die "go not found on host (mise: mise use go)"
    if [[ ! -d "$EXPORTER_DIR/.git" ]]; then
        log "Cloning $EXPORTER_REPO"
        git clone "$EXPORTER_REPO" "$EXPORTER_DIR"
    fi
    git -C "$EXPORTER_DIR" fetch --quiet origin 2>/dev/null || true
    git -C "$EXPORTER_DIR" checkout --quiet "$EXPORTER_COMMIT"
    log "Cross-building nginx-vts-exporter @ $(git -C "$EXPORTER_DIR" rev-parse --short HEAD)"
    (cd "$EXPORTER_DIR" && \
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -o nginx-vts-exporter .)
}

# ---------------------------------------------------------------------------
# 2. Remote part (runs on a Debian guest as root via sudo bash -s).
#    Reads NGINX_VER / VTS_TAG from the environment.
# ---------------------------------------------------------------------------
read -r -d '' REMOTE_SCRIPT <<'REMOTE' || true
set -euo pipefail
die() { echo "REMOTE ERROR: $*" >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
log_r() { echo "  [remote] $*"; }

# -- node_exporter -----------------------------------------------------------
apt-get update -qq
apt-get install -y -qq prometheus-node-exporter \
    build-essential libpcre2-dev zlib1g-dev libssl-dev \
    libxml2-dev libxslt1-dev git curl ca-certificates >/dev/null
apt-get install -y -qq libgeoip-dev >/dev/null 2>&1 || \
    log_r "libgeoip-dev unavailable (ok if nginx -V has no geoip)"
systemctl enable --now prometheus-node-exporter >/dev/null 2>&1 || true
ss -tln | grep -q :9100 || die "node_exporter is not listening on :9100"

# -- nginx-module-vts (dynamic) ----------------------------------------------
SO=/usr/lib/nginx/modules/ngx_http_vhost_traffic_status_module.so
if [[ ! -f $SO ]]; then
    log_r "building nginx-module-vts $VTS_TAG against nginx $NGINX_VER"
    NGINX_VER="$NGINX_VER" VTS_TAG="$VTS_TAG" bash -s <<'BUILD'
set -euo pipefail
die() { echo "BUILD ERROR: $*" >&2; exit 1; }
command -v nginx >/dev/null || die "nginx not installed"
WORK=/tmp/vts-build
rm -rf "$WORK"; mkdir -p "$WORK"; cd "$WORK"
curl -fsSL "https://nginx.org/download/nginx-${NGINX_VER}.tar.gz" -o nginx.tar.gz
tar xzf nginx.tar.gz
git clone --depth 1 --branch "$VTS_TAG" \
    https://github.com/vozlt/nginx-module-vts 2>/dev/null
CONFIG_ARGS="$(nginx -V 2>&1 | sed -n 's/^configure arguments: //p')"
[[ -n $CONFIG_ARGS ]] || die "cannot read configure arguments from nginx -V"
cd "nginx-${NGINX_VER}"
# shellcheck disable=SC2086 - eval restores the quoted configure args
eval "set -- $CONFIG_ARGS"
./configure "$@" --add-dynamic-module="$WORK/nginx-module-vts" >/tmp/vts-configure.log
make modules >/tmp/vts-make.log 2>&1 || { tail -20 /tmp/vts-make.log; die "make modules failed"; }
install -m 0644 objs/ngx_http_vhost_traffic_status_module.so /usr/lib/nginx/modules/
BUILD
else
    log_r "vts module already installed: $SO"
fi
[[ -f $SO ]] || die "vts module missing after build step"

# -- nginx configs (backup once, then install) --------------------------------
if [[ ! -d /etc/nginx.bak-pre-vts ]]; then
    cp -a /etc/nginx /etc/nginx.bak-pre-vts
    log_r "backup saved to /etc/nginx.bak-pre-vts"
fi
install -m 0644 /tmp/vts-90.conf       /etc/nginx/modules-enabled/90-vts.conf
install -m 0644 /tmp/vts-zone.conf     /etc/nginx/conf.d/00-vts-zone.conf
install -m 0644 /tmp/vts-status.conf   /etc/nginx/conf.d/vts-status.conf
rm -f /tmp/vts-90.conf /tmp/vts-zone.conf /tmp/vts-status.conf

if ! nginx -t; then
    log_r "nginx -t FAILED - rolling back to /etc/nginx.bak-pre-vts"
    rm -rf /etc/nginx
    cp -a /etc/nginx.bak-pre-vts /etc/nginx
    nginx -t || die "rollback still broken, manual fix needed"
fi
systemctl reload nginx

# -- nginx-vts-exporter --------------------------------------------------------
install -m 0755 /tmp/nginx-vts-exporter-bin /usr/local/bin/nginx-vts-exporter
rm -f /tmp/nginx-vts-exporter-bin
install -m 0644 /tmp/nginx-vts-exporter.service /etc/systemd/system/nginx-vts-exporter.service
rm -f /tmp/nginx-vts-exporter.service
systemctl daemon-reload
systemctl enable --now nginx-vts-exporter >/dev/null 2>&1 || true
sleep 1
systemctl is-active --quiet nginx-vts-exporter || die "nginx-vts-exporter not active"
REMOTE

# ---------------------------------------------------------------------------
# 3. Deploy per VM.
# ---------------------------------------------------------------------------
deploy_vm() {
    local vm=$1 ip=${VM_IP[$1]}
    log "[$vm] node_exporter + vts module + nginx configs + exporter"
    # stage remote files
    scp -q "${SSH_OPTS[@]}" \
        "$HERE/nginx/90-vts.conf"      "lab@$ip:/tmp/vts-90.conf"
    scp -q "${SSH_OPTS[@]}" \
        "$HERE/nginx/00-vts-zone.conf" "lab@$ip:/tmp/vts-zone.conf"
    scp -q "${SSH_OPTS[@]}" \
        "$HERE/nginx/vts-status.conf"  "lab@$ip:/tmp/vts-status.conf"
    scp -q "${SSH_OPTS[@]}" \
        "$HOST_EXPORTER_BIN"           "lab@$ip:/tmp/nginx-vts-exporter-bin"
    scp -q "${SSH_OPTS[@]}" \
        "$HERE/nginx-vts-exporter.service" "lab@$ip:/tmp/nginx-vts-exporter.service"

    ssh_to "$vm" \
        "sudo -n NGINX_VER=$NGINX_VER VTS_TAG=$VTS_TAG bash -s" <<<"$REMOTE_SCRIPT"

    # pfkeeper healthz probe is sacred: verify right after the nginx reload.
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://$ip/healthz" || true)"
    if [[ $code != 200 ]]; then
        log "[$vm] healthz probe returned $code - ROLLING BACK nginx config"
        ssh_to "$vm" "sudo -n bash -c 'rm -rf /etc/nginx && cp -a /etc/nginx.bak-pre-vts /etc/nginx && nginx -t && systemctl reload nginx'"
        die "[$vm] rolled back; pfkeeper probe was $code"
    fi
    local bad
    bad="$(curl -s "http://${VM_IP[lb1]}:9100/metrics" \
        | grep "^pfkeeper_member_health{address=\"$ip\"" \
        | awk '{print $NF}' | grep -vx 1 || true)"
    if [[ -n $bad ]]; then
        log "[$vm] pfkeeper marks member DOWN - ROLLING BACK nginx config"
        ssh_to "$vm" "sudo -n bash -c 'rm -rf /etc/nginx && cp -a /etc/nginx.bak-pre-vts /etc/nginx && nginx -t && systemctl reload nginx'"
        die "[$vm] rolled back; member_health != 1"
    fi
}

build_exporter
for vm in "${VMS[@]}"; do
    deploy_vm "$vm"
done

# ---------------------------------------------------------------------------
# 4. Live verification from the host.
# ---------------------------------------------------------------------------
log "verification"
for vm in "${VMS[@]}"; do
    ip=${VM_IP[$vm]}
    n_exp="$(curl -s "http://$ip:9913/metrics" | grep -c '^nginx_vts_' || true)"
    n_node="$(curl -s "http://$ip:9100/metrics" | grep -c '^node_' || true)"
    n_exp=${n_exp:-0}; n_node=${n_node:-0}
    vts_json="$(curl -s "http://$ip:8081/status/format/json" | head -c 1)"
    echo "  $vm ($ip): nginx_vts_ metrics=$n_exp node_ metrics=$n_node vts_json_first_char=${vts_json:-NONE}"
    [[ $n_exp -gt 100 && $n_node -gt 100 && $vts_json == "{" ]] \
        || die "$vm verification failed"
done
log "OK: nginx observability stack deployed to: ${VMS[*]}"
