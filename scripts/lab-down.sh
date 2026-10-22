#!/usr/bin/env bash
# Shut the lab down (graceful, with a destroy fallback after 60s).
#   --wipe        also removes domains, disk-*/seed-* volumes and net-dsr
#   --wipe-bases  like --wipe and additionally removes the base image volumes
# Base volumes are NEVER touched without --wipe-bases.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
MODE=${1:-}
need_virsh

vm_running() { "${VIRSH[@]}" domstate "$1" 2>/dev/null | grep -q running; }

for vm in "${VM_NAMES[@]}"; do
  "${VIRSH[@]}" dominfo "$vm" >/dev/null 2>&1 || continue
  if vm_running "$vm"; then
    echo "shutdown $vm"
    "${VIRSH[@]}" shutdown "$vm" >/dev/null || true
    DEADLINE=$((SECONDS + 60))
    while vm_running "$vm" && ((SECONDS < DEADLINE)); do sleep 2; done
    if vm_running "$vm"; then
      echo "  $vm still running after 60s, destroying"
      "${VIRSH[@]}" destroy "$vm" >/dev/null || true
    fi
  fi
done

if [[ $MODE == --wipe || $MODE == --wipe-bases ]]; then
  echo "== wipe =="
  for vm in "${VM_NAMES[@]}"; do
    "${VIRSH[@]}" destroy "$vm" >/dev/null 2>&1 || true
    # --nvram: efi domains keep an auto-created NVRAM file that must go too.
    "${VIRSH[@]}" undefine "$vm" --managed-save --nvram >/dev/null 2>&1 ||
      "${VIRSH[@]}" undefine "$vm" >/dev/null 2>&1 || true
    "${VIRSH[@]}" vol-delete --pool default "disk-$vm" >/dev/null 2>&1 || true
    "${VIRSH[@]}" vol-delete --pool default "seed-$vm" >/dev/null 2>&1 || true
  done
  "${VIRSH[@]}" net-destroy net-dsr >/dev/null 2>&1 || true
  "${VIRSH[@]}" net-undefine net-dsr >/dev/null 2>&1 || true
  "${VIRSH[@]}" net-destroy net-sync >/dev/null 2>&1 || true
  "${VIRSH[@]}" net-undefine net-sync >/dev/null 2>&1 || true
  echo "domains, disks, seeds, net-dsr and net-sync removed"
fi

if [[ $MODE == --wipe-bases ]]; then
  "${VIRSH[@]}" vol-delete --pool default "$BASE_VOL_FREEBSD" >/dev/null 2>&1 || true
  "${VIRSH[@]}" vol-delete --pool default "$BASE_VOL_DEBIAN" >/dev/null 2>&1 || true
  echo "base volumes removed"
fi

echo "lab down"
