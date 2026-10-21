#!/usr/bin/env bash
# Bring the whole lab up. Idempotent: existing volumes/networks/domains are
# reused, missing pieces are created. Never downloads anything - base images
# must be placed in $LAB_STATE_IMAGES by the user.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
need_virsh
ensure_ssh_key

# ensure_vol_from_file <pool-volume-name> <local-file-name-in-$LAB_STATE_IMAGES>
# Full copy per VM: this libvirt build lacks --backing-format for vol-create-as
# and defaults pool-volume backing to raw, which silently breaks qcow2 backing
# chains - so overlays are deliberately avoided.
ensure_vol_from_file() {
  local name=$1 file="$LAB_STATE_IMAGES/$2"
  if "${VIRSH[@]}" vol-info --pool default "$name" >/dev/null 2>&1; then
    return 0
  fi
  if [[ ! -f $file ]]; then
    die "volume '$name' is neither in pool 'default' nor at $file

Download the images yourself and place them as:
  $LAB_STATE_IMAGES/$BASE_VOL_FREEBSD   (freebsd-14.5-cloudinit.qcow2)
  $LAB_STATE_IMAGES/$BASE_VOL_DEBIAN    (debian-13-genericcloud.qcow2)
This repo never downloads anything."
  fi
  local size
  size=$(stat -Lc%s "$file")
  echo "Importing volume $name from $file"
  "${VIRSH[@]}" vol-create-as default "$name" "$size" --format qcow2 >/dev/null
  "${VIRSH[@]}" vol-upload --pool default "$name" "$file" >/dev/null
}

ensure_vol_from_file "$BASE_VOL_FREEBSD" "$BASE_VOL_FREEBSD"
ensure_vol_from_file "$BASE_VOL_DEBIAN" "$BASE_VOL_DEBIAN"
ensure_vol_from_file disk-client "$BASE_VOL_DEBIAN"
ensure_vol_from_file disk-lb1 "$BASE_VOL_FREEBSD"
ensure_vol_from_file disk-rs1 "$BASE_VOL_DEBIAN"
ensure_vol_from_file disk-rs2 "$BASE_VOL_DEBIAN"

echo "== building seed ISOs =="
bash "$LAB_DIR/scripts/make-seed-iso.sh"

echo "== network net-dsr =="
if ! "${VIRSH[@]}" net-info net-dsr >/dev/null 2>&1; then
  "${VIRSH[@]}" net-define "$LAB_DIR/libvirt/net-dsr.xml" >/dev/null
fi
"${VIRSH[@]}" net-autostart net-dsr >/dev/null
"${VIRSH[@]}" net-start net-dsr >/dev/null 2>&1 || true # already active is fine

echo "== domains =="
for vm in "${VM_NAMES[@]}"; do
  if ! "${VIRSH[@]}" dominfo "$vm" >/dev/null 2>&1; then
    "${VIRSH[@]}" define "$LAB_DIR/libvirt/vm-$vm.xml" >/dev/null
  fi
  "${VIRSH[@]}" start "$vm" >/dev/null 2>&1 || true # already running is fine
done

echo "== waiting for DHCP leases (max 300s) =="
UP=()
REMAINING=("${VM_NAMES[@]}")
DEADLINE=$((SECONDS + 300))
while ((${#REMAINING[@]} > 0)) && ((SECONDS < DEADLINE)); do
  STILL=()
  for vm in "${REMAINING[@]}"; do
    if ping -c1 -W1 "${VM_IP[$vm]}" >/dev/null 2>&1; then
      UP+=("$vm")
      echo "  $vm (${VM_IP[$vm]}) is up"
    else
      STILL+=("$vm")
    fi
  done
  REMAINING=("${STILL[@]}")
  if ((${#REMAINING[@]} > 0)); then sleep 2; fi
done

echo
echo "== lab is up =="
for vm in "${VM_NAMES[@]}"; do
  echo "  $vm: ssh -i $LAB_SSH_KEY lab@${VM_IP[$vm]}"
done
echo "First boot runs cloud-init (Debian VMs install nginx et al.); give it a few"
echo "minutes, then: just test-all"

if ((${#REMAINING[@]} > 0)); then
  echo "WARNING: no DHCP/ping from: ${REMAINING[*]}" >&2
  exit 1
fi
