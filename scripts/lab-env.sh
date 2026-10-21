#!/usr/bin/env bash
# Shared environment for lb-dsr-lab scripts and tests. Source this, do not
# execute it.
set -euo pipefail

LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB_STATE="${LAB_STATE:-$HOME/.local/share/lb-dsr-lab}"
LAB_STATE_IMAGES="$LAB_STATE/images"
LAB_SSH_KEY="$LAB_STATE/ssh/id_ed25519"
LAB_SSH_PUB="$LAB_SSH_KEY.pub"
LAB_VIP="192.168.100.200"
VIRSH=(virsh -c qemu:///system)

# Lab inventory: names are the libvirt domain names and the indexes of the
# maps below. NIC is virtio -> vtnet0 inside FreeBSD, eth0/ens*/enp* on Debian.
VM_NAMES=(client lb1 rs1 rs2)
declare -Ar VM_IP=(
  [client]=192.168.100.10
  [lb1]=192.168.100.11
  [rs1]=192.168.100.21
  [rs2]=192.168.100.22
)
declare -Ar VM_MAC=(
  [client]=52:54:00:aa:00:10
  [lb1]=52:54:00:aa:00:11
  [rs1]=52:54:00:aa:00:21
  [rs2]=52:54:00:aa:00:22
)

# Base image volumes in the libvirt default pool. Imported by lab-up.sh from
# $LAB_STATE_IMAGES if missing - this repo never downloads anything.
BASE_VOL_FREEBSD=freebsd-14.5-cloudinit.qcow2
BASE_VOL_DEBIAN=debian-13-genericcloud.qcow2
OVERLAY_SIZE_GB=12

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i "$LAB_SSH_KEY")

die() { echo "ERROR: $*" >&2; exit 1; }

need_virsh() {
  if ! "${VIRSH[@]}" uri >/dev/null 2>&1; then
    cat >&2 <<'EOF'
Cannot talk to qemu:///system. On the host run (user commands, then re-login):
  sudo pacman -S --needed libvirt qemu-full
  sudo systemctl enable --now libvirtd.service
  sudo usermod -aG libvirt "$USER"
EOF
    exit 1
  fi
}

ensure_ssh_key() {
  if [[ ! -f "$LAB_SSH_KEY" ]]; then
    mkdir -p "$(dirname "$LAB_SSH_KEY")"
    ssh-keygen -q -t ed25519 -N '' -f "$LAB_SSH_KEY" -C lb-dsr-lab
    echo "Generated lab SSH key: $LAB_SSH_KEY"
  fi
}

# ssh_to <vm-name> [command...] - SSH into a lab VM as user lab.
ssh_to() {
  local vm=$1
  shift
  ssh "${SSH_OPTS[@]}" "lab@${VM_IP[$vm]}" "$@"
}

# Test verdict helpers shared by tests/t*.sh (exit 2 == SKIP).
test_pass() { echo "PASS: $*"; exit 0; }
test_fail() { echo "FAIL: $*"; exit 1; }
test_skip() { echo "SKIP: $*"; exit 2; }
