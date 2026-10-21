# Capture hints

Where to look at packets, and what each spot proves. All host-side captures
need sudo (`sudo pacman -S --needed tcpdump wireshark-cli`).

## On the host

- Whole lab bridge: `sudo tcpdump -ni virbr-dsr`
- One VM only (find its tap device):
  `virsh -c qemu:///system domiflist lb1` prints the vnetX for each VM, then
  `sudo tcpdump -ni vnetX`

## Inside lb1 (ssh as root@lb1 - the FreeBSD image has no sudo)

- Forward/return path around pf:
  `tcpdump -ni vtnet0 'tcp[tcpflags] & (tcp-syn|tcp-ack) == (tcp-syn|tcp-ack)'`
- pf decisions: `ifconfig pflog0 up && tcpdump -ni pflog0`
  (add a `log` action to rules first if nothing shows up)

## On a realserver (rs1/rs2)

- Return path check: `tcpdump -ni eth0 host 192.168.100.10`
  (client connections arrive here directly in DSR mode)
- conntrack (only meaningful in NAT mode): `conntrack -E`
- ARP guard: `ip addr show lo` - the VIP /32 must be on lo, never on eth0

## What SYNs / SYN-ACKs prove where

| Observation on lb1 vtnet0              | DSR mode | NAT mode |
| -------------------------------------- | -------- | -------- |
| SYN, dst 192.168.100.200:80            | present  | present  |
| SYN-ACK, src 192.168.100.200:80        | never    | present  |
| pf states for :80                      | sloppy half-flows only | full flows |

- The DSR signature is the ABSENCE of SYN-ACKs from the VIP on lb1: pf
  forwarded the SYN, the backend answered the client directly.
  tests/t02-dsr.sh asserts exactly this.
- In NAT mode rdr rewrites `src backend -> VIP` on the way out, so lb1 sees
  both halves of every flow and the SYN-ACK appears on vtnet0.
  tests/t06-nat-compare.sh flips the ruleset to demonstrate it.
- On the client side the reply always carries src=VIP; what differs is the
  source MAC: the backend's own MAC in DSR mode, lb1's MAC in NAT mode.
