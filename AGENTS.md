# lb-dsr-lab

Libvirt/QEMU lab demonstrating Direct Server Return (DSR) load balancing with
pure pf on FreeBSD 14.5: one load balancer (lb1) forwards HTTP traffic toward
two Debian 13 nginx backends (rs1/rs2) without NAT; the backends answer the
client directly using the VIP on their loopback. A Debian client VM exercises
the VIP. Everything runs on one isolated libvirt network on a single host.

## Stack

- libvirt (`qemu:///system`), q35 guests with UEFI (OVMF), virtio disk/net
- lb1: FreeBSD 14.5 BASIC-CLOUDINIT (ufs qcow2), pure pf - no relayd, no CARP yet
- client / rs1 / rs2: Debian 13 genericcloud + cloud-init (NoCloud seed ISOs)
- Isolated network `net-dsr` (bridge virbr-dsr), 192.168.100.0/24,
  VIP 192.168.100.200/32

## Commands

- `just up` / `just down` / `just wipe` - full lab lifecycle (idempotent)
- `just status` - virsh list + net-dsr status
- `just test-all` - test suite t00..t06 (`just test 02` for a single test)
- `just pf-mode dsr|nat` - switch the lb1 ruleset
- `just bench` - HTTP benchmark against the VIP
- `just ssh lb1|rs1|rs2|client` - SSH as user `lab` (key auto-generated under
  `~/.local/share/lb-dsr-lab/ssh/`)

## Layout

- `libvirt/` - network and domain XML, the source of truth for definitions
- `cloud-init/` - per-VM NoCloud seeds (`user-data`, `meta-data`,
  `network-config`; `@SSH_PUBKEY@` is substituted at seed build time)
- `freebsd/` - pf rulesets (DSR default + NAT comparison) and an rc.conf
  reference snippet
- `linux/` - realserver sysctl and nginx configs (inlined into cloud-init)
- `scripts/` - lab lifecycle, seed ISO build, pf mode switch, bench, helpers
- `tests/` - t00..t06 plus `capture-hints.md`; run via `just test-all`

## Constraints

- All VM state lives in the libvirt `default` pool (volumes `disk-*`,
  `seed-*`). The repo holds definitions only - no images, no seeds, no
  generated state is ever committed.
- Base volumes `freebsd-14.5-cloudinit.qcow2` and
  `debian-13-genericcloud.qcow2` are imported by `scripts/lab-up.sh` from
  `~/.local/share/lb-dsr-lab/images/` - download them yourself; the repo and
  its scripts never download anything.
- Host commands needing sudo (libvirtd setup, tcpdump captures) are run by
  the user, not by the agent.
- pf on FreeBSD 14.x uses the legacy syntax only: no `match`, `rdr-to`,
  `nat-to` keywords. Use `rdr pass` and `pass ... route-to` (see
  `freebsd/pf-dsr.conf`).
- DSR is asymmetric by design: pf sees only one half of each TCP flow, hence
  `keep state (sloppy)` and `sticky-address` on the `route-to` rule, and
  arp_ignore/arp_announce + VIP-on-lo on the backends.

## Known limitation (2026-10-22)

The FreeBSD guest (lb1) currently has NO working outbound internet: pkg and
freebsd-update fail ("Network is unreachable" / resolver failures), while the
Debian guests reach the internet fine through the same NAT network. Root cause
not yet chased down (guest DNS via dnsmasq .1 and connectivity to
pkg.freebsd.org both fail). Consequences:
- doas/sudo CANNOT be pkg-installed on lb1 right now; lab automation uses
  root SSH (ssh_root_lb1, key provisioned by cloud-init) - no package deps.
- pkg repo overrides are already in place on lb1
  (/usr/local/etc/pkg/repos/freebsd.conf, mirror_type NONE) and resolv.conf
  points at 192.168.100.1 - once connectivity is fixed, `pkg install doas`
  + /usr/local/etc/doas.conf (`permit nopass keepenv lab`) gives the idiomatic
  BSD privilege path; note doas is a PACKAGE on FreeBSD (security/doas),
  like sudo it is not in the base system.
