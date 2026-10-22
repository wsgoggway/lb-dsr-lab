package pfctl

import "testing"

// carpFixture is a real-shaped `ifconfig vtnet0` excerpt from the lab pair.
const carpFixtureMaster = `vtnet0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 10 mtu 1500
	description: net-sync
	options=4c07bb
	ether 00:11:32:aa:bb:cc
	inet6 fe80::211:32ff:feaa:bbcc%vtnet0 prefixlen 64 scopeid 0x1
	inet 192.168.100.11 netmask 0xffffff00 broadcast 192.168.100.255 vhid 1
	inet 192.168.100.10 netmask 0xffffffff vhid 1
	media: Ethernet 10Gbase-T <full-duplex>
	status: active
	nd6 options=21<PERFORMNUD,AUTO_LINKLOCAL>
	carp: MASTER vhid 1 advbase 1 advskew 0
`

const carpFixtureBackup = `vtnet0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 10 mtu 1500
	ether 00:11:32:dd:ee:ff
	inet 192.168.100.12 netmask 0xffffff00 broadcast 192.168.100.255 vhid 1
	carp: BACKUP vhid 1 advbase 1 advskew 100
`

func TestParseCarp(t *testing.T) {
	tests := []struct {
		name   string
		iface  string
		output string
		want   CarpState
	}{
		{"master line", "vtnet0", carpFixtureMaster, CarpState{Role: "MASTER", Vhid: "1", Interface: "vtnet0"}},
		{"backup line", "vtnet0", carpFixtureBackup, CarpState{Role: "BACKUP", Vhid: "1", Interface: "vtnet0"}},
		{"no carp line -> INIT", "em0", "em0: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0\n	status: active\n", CarpState{Role: "INIT", Interface: "em0"}},
		{"lowercase role normalised", "em0", "em0: flags=1\ncarp: master vhid 7 advbase 1 advskew 0\n", CarpState{Role: "MASTER", Vhid: "7", Interface: "em0"}},
		{"empty output", "em0", "", CarpState{Role: "INIT", Interface: "em0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCarp(tt.iface, tt.output)
			if got != tt.want {
				t.Fatalf("ParseCarp() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
