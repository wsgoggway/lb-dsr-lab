#!/usr/bin/env bash
# Build NoCloud seed ISOs (volid cidata) for every VM and upload them into the
# libvirt default pool as raw volumes seed-<vm>. Idempotent: an existing seed
# volume is deleted and re-created.
set -euo pipefail
source "$(dirname "$0")/lab-env.sh"
need_virsh
ensure_ssh_key

PUBKEY=$(cat "$LAB_SSH_PUB")

# genisoimage on Debian-land, mkisofs (cdrtools) or xorriso on Arch.
ISO_TOOL=''
for t in genisoimage mkisofs xorriso; do
  if command -v "$t" >/dev/null 2>&1; then ISO_TOOL=$t; break; fi
done
[[ -n $ISO_TOOL ]] || die "no ISO tool found; install cdrtools (mkisofs) or xorriso"
case $ISO_TOOL in
  xorriso) ISO_CMD=(xorriso -as mkisofs) ;;
  *) ISO_CMD=("$ISO_TOOL") ;;
esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

for vm in "${VM_NAMES[@]}"; do
  SRC="$LAB_DIR/cloud-init/$vm"
  [[ -d $SRC ]] || die "missing cloud-init/$vm"
  mkdir "$WORK/$vm"
  cp "$SRC/user-data" "$SRC/meta-data" "$SRC/network-config" "$WORK/$vm/"
  sed -i "s|@SSH_PUBKEY@|$PUBKEY|" "$WORK/$vm/user-data"
  (cd "$WORK/$vm" && "${ISO_CMD[@]}" -quiet -output seed.iso -volid cidata -joliet -rock \
    user-data meta-data network-config)

  # Capacity = actual ISO size: a fixed 1M could truncate larger seeds.
  SIZE=$(stat -Lc%s "$WORK/$vm/seed.iso")
  if "${VIRSH[@]}" vol-info --pool default "seed-$vm" >/dev/null 2>&1; then
    "${VIRSH[@]}" vol-delete --pool default "seed-$vm" >/dev/null
  fi
  "${VIRSH[@]}" vol-create-as default "seed-$vm" "$SIZE" --format raw >/dev/null
  "${VIRSH[@]}" vol-upload --pool default "seed-$vm" "$WORK/$vm/seed.iso" >/dev/null
  echo "seed-$vm: uploaded ($SIZE bytes)"
done
