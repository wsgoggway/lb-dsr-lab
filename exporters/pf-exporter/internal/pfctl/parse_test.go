package pfctl

import (
	"os"
	"testing"
)

// fixtureDir points at the module-root testdata shared with the collector
// package tests: one canonical set of captured pfctl outputs.
const fixtureDir = "../../testdata"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(fixtureDir + "/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

func TestParseSI_FreeBSD(t *testing.T) {
	t.Parallel()

	stats, err := ParseSI(readFixture(t, "pfctl_si_freebsd.txt"))
	if err != nil {
		t.Fatalf("ParseSI: %v", err)
	}
	if stats.States != 20 {
		t.Errorf("States = %d, want 20", stats.States)
	}

	// State Table section counters.
	wantStateTable := map[string]uint64{
		"searches": 1541595,
		"inserts":  75707,
		"removals": 75687,
	}
	got := stateTableMap(t, stats)
	for label, want := range wantStateTable {
		if have := got[label]; have != want {
			t.Errorf("StateTable[%q] = %d, want %d", label, have, want)
		}
	}
	if len(stats.StateTable) != len(wantStateTable) {
		t.Errorf("len(StateTable) = %d, want %d", len(stats.StateTable), len(wantStateTable))
	}

	// Counters section: spot-check the non-zero lines and the tail of the file.
	wantCounters := map[string]uint64{
		"match":          116280,
		"ip-option":      4,
		"state-mismatch": 9,
		"map-failed":     0,
	}
	counters := countersMap(t, stats)
	for label, want := range wantCounters {
		if have := counters[label]; have != want {
			t.Errorf("Counters[%q] = %d, want %d", label, have, want)
		}
	}
	if len(stats.Counters) < 15 {
		t.Errorf("len(Counters) = %d, want >= 15 (full FreeBSD counter block)", len(stats.Counters))
	}
}

func TestParseSI_OpenBSDStyleFixture(t *testing.T) {
	t.Parallel()

	stats, err := ParseSI(readFixture(t, "pfctl_si.txt"))
	if err != nil {
		t.Fatalf("ParseSI: %v", err)
	}
	if stats.States != 12 {
		t.Errorf("States = %d, want 12", stats.States)
	}
	got := stateTableMap(t, stats)
	if have := got["searches"]; have != 1495870 {
		t.Errorf("StateTable[searches] = %d, want 1495870", have)
	}
	counters := countersMap(t, stats)
	if have := counters["match"]; have != 114830 {
		t.Errorf("Counters[match] = %d, want 114830", have)
	}
	if have := counters["state-mismatch"]; have != 9 {
		t.Errorf("Counters[state-mismatch] = %d, want 9", have)
	}
}

func TestParseSI_NoStateSection(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"empty", "banner only"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := ""
			if name == "banner only" {
				in = "Status: Enabled for 1 days 0:00:00           Debug: Urgent\n"
			}
			if _, err := ParseSI(in); err == nil {
				t.Error("ParseSI: expected an error without a State Table section")
			}
		})
	}
}

func TestParseSI_PreambleNumbersIgnored(t *testing.T) {
	t.Parallel()

	// "Enabled for 1 days" must not leak in as a counter of the State Table
	// section: everything before the header is dropped.
	in := "Status: Enabled for 1 days 12:13:06           Debug: Urgent\n" +
		"State Table                          Total             Rate\n" +
		"  current entries                        3               \n" +
		"  searches                             100             1.0/s\n"
	stats, err := ParseSI(in)
	if err != nil {
		t.Fatalf("ParseSI: %v", err)
	}
	if stats.States != 3 {
		t.Errorf("States = %d, want 3", stats.States)
	}
	if len(stats.StateTable) != 1 || stats.StateTable[0].Label != "searches" {
		t.Errorf("StateTable = %+v, want only searches=100", stats.StateTable)
	}
}

func TestParseInterfaces_FreeBSD(t *testing.T) {
	t.Parallel()

	ifaces := ParseInterfaces(readFixture(t, "pfctl_sI_freebsd.txt"))
	wantNames := []struct {
		name string
		skip bool
	}{
		{"all", false}, {"lo", false}, {"lo0", true}, {"pfsync", false},
		{"pfsync0", false}, {"vtnet0", false}, {"vtnet1", false},
	}
	if len(ifaces) != len(wantNames) {
		t.Fatalf("got %d interfaces, want %d", len(ifaces), len(wantNames))
	}
	for i, w := range wantNames {
		if ifaces[i].Name != w.name {
			t.Errorf("ifaces[%d].Name = %q, want %q", i, ifaces[i].Name, w.name)
		}
		if ifaces[i].Skip != w.skip {
			t.Errorf("ifaces[%d] (%s).Skip = %v, want %v", i, w.name, ifaces[i].Skip, w.skip)
		}
		if len(ifaces[i].Packets) != 0 {
			t.Errorf("ifaces[%d] (%s): FreeBSD output must carry no verbose packets, got %d",
				i, w.name, len(ifaces[i].Packets))
		}
	}
}

func TestParseInterfaces_OpenBSDStyleVerbose(t *testing.T) {
	t.Parallel()

	ifaces := ParseInterfaces(readFixture(t, "pfctl_siv.txt"))
	if len(ifaces) < 2 {
		t.Fatalf("got %d interfaces, want >= 2", len(ifaces))
	}
	all := ifaces[0]
	if all.Name != "all" {
		t.Fatalf("first interface = %q, want %q", all.Name, "all")
	}
	if len(all.Packets) != 8 {
		t.Fatalf("interface %q: got %d packet counter lines, want 8 (In/Out x 4/6 x Pass/Block)",
			all.Name, len(all.Packets))
	}
	// The first counter line is In4/Pass: [ Packets: 0 Bytes: 0 ].
	first := all.Packets[0]
	if first.Direction != "in" || first.Family != "4" || first.Action != "pass" {
		t.Errorf("first packets entry = %+v, want in/4/pass", first)
	}
	if first.Packets != 0 || first.Bytes != 0 {
		t.Errorf("first packets entry values = %d/%d, want 0/0", first.Packets, first.Bytes)
	}
}

func TestParseInterfaces_MalformedVerboseLinesIgnored(t *testing.T) {
	t.Parallel()

	in := "vtnet0\n\tCleared:     some time\n\tIn4/Pass:    [ Packets: not-a-number ]\n" +
		"\tIn4/Block:   [ Packets: 7 Bytes: 42 ]\n"
	ifaces := ParseInterfaces(in)
	if len(ifaces) != 1 {
		t.Fatalf("got %d interfaces, want 1", len(ifaces))
	}
	packets := ifaces[0].Packets
	if len(packets) != 1 {
		t.Fatalf("got %d packet lines, want 1 (malformed lines ignored)", len(packets))
	}
	if packets[0].Action != "block" || packets[0].Packets != 7 || packets[0].Bytes != 42 {
		t.Errorf("packets[0] = %+v, want block 7/42", packets[0])
	}
}

func TestParseTables(t *testing.T) {
	t.Parallel()

	tables := ParseTables(readFixture(t, "pfctl_st_freebsd.txt"))
	if len(tables) != 1 || tables[0] != "webs" {
		t.Errorf("ParseTables = %v, want [webs]", tables)
	}
	if tables := ParseTables("a\n\nb\n"); len(tables) != 2 {
		t.Errorf("ParseTables(a,,b) = %v, want [a b]", tables)
	}
}

func TestParseTableShow(t *testing.T) {
	t.Parallel()

	const in = "192.168.100.21\n192.168.100.22\n\n"
	if n := ParseTableShow(in); n != 2 {
		t.Errorf("ParseTableShow = %d, want 2", n)
	}
	if n := ParseTableShow(""); n != 0 {
		t.Errorf("ParseTableShow(empty) = %d, want 0", n)
	}
}

func stateTableMap(t *testing.T, stats Stats) map[string]uint64 {
	t.Helper()
	m := make(map[string]uint64, len(stats.StateTable))
	for _, c := range stats.StateTable {
		m[c.Label] = c.Value
	}
	return m
}

func countersMap(t *testing.T, stats Stats) map[string]uint64 {
	t.Helper()
	m := make(map[string]uint64, len(stats.Counters))
	for _, c := range stats.Counters {
		m[c.Label] = c.Value
	}
	return m
}
