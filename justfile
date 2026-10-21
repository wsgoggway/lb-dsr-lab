# lb-dsr-lab task runner. Default recipe prints the list.
set shell := ["bash", "-c"]

default: list

list:
    @just --list

# Define + start the network and all four VMs (idempotent).
up:
    bash scripts/lab-up.sh

# Graceful shutdown of all VMs.
down:
    bash scripts/lab-down.sh

# Full cleanup: VMs, disks, seeds, network. Base image volumes are kept.
wipe:
    bash scripts/lab-down.sh --wipe

# VM and network status.
status:
    virsh -c qemu:///system list --all
    @virsh -c qemu:///system net-info net-dsr || true

# Capture on the lab bridge (sudo). Segment arg is informational for now.
capture seg="host":
    @echo "hint: sudo pacman -S --needed tcpdump wireshark-cli"
    @echo "segment: {{seg}} (host capture uses virbr-dsr; see tests/capture-hints.md)"
    sudo tcpdump -ni virbr-dsr

# SSH into a VM by name: just ssh lb1
ssh vm:
    bash -c 'source scripts/lab-env.sh && exec ssh "${SSH_OPTS[@]}" lab@"${VM_IP[{{vm}}]}"'

# Run a single test by number: just test 02
test t:
    bash "tests/t{{t}}.sh"

# Run the whole test suite t00..t06.
test-all:
    bash tests/run-all.sh

# Switch lb1 pf ruleset: just pf-mode dsr | just pf-mode nat
pf-mode mode:
    bash scripts/pf-mode.sh {{mode}}

# Quick HTTP benchmark against the VIP.
bench:
    bash scripts/bench.sh
