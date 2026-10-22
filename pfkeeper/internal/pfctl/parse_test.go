package pfctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// exitErr produces a genuine *exec.ExitError with the given exit code by
// running `sh -c "exit <code>"` (ProcessState must be set for ExitCode()).
func exitErr(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+fmt.Sprint(code))
	if err := cmd.Run(); err != nil {
		return err
	}
	t.Fatal("expected non-zero exit")
	return nil
}

// siFixture is a real `pfctl -si` sample captured from the lab pair
// (FreeBSD 14, pf enabled ~8h20m).
const siFixture = `Status: Enabled for 0 days 08:20:44           Debug: Urgent

State Table                          Total             Rate
  current entries                     15
  searches                         58451            1.9/s
  inserts                          12548            0.4/s
  removals                          200            0.0/s
Counters
  match                             7169           22.2/s
  bad-offset                           0            0.0/s
  state-mismatch                      90            0.3/s
  state-insert                        15            0.0/s
`

// tableShowVFixture is a real `pfctl -t <n> -T show -v` sample from the lab
// (mixed: member with full counters, member without Cleared, plain member).
const tableShowVFixture = `   192.168.100.21, Cleared: Thu Oct 22 08:00:00 2026, In/Block: 0/0, Out/Block: 10/0, XPass: 0
   192.168.100.22, In/Block: 3/0, Out/Block: 7/0, XPass: 0
   192.168.100.23
`

func TestParseSI(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    PfInfo
		wantErr bool
	}{
		{
			name:  "real lab sample",
			input: siFixture,
			want: PfInfo{
				StatesEntries:  15,
				StatesSearches: 58451,
				StatesInserts:  12548,
				StatesRemovals: 200,
				Match:          7169,
				StateMismatch:  90,
			},
		},
		{
			name:    "empty output",
			input:   "",
			wantErr: true,
		},
		{
			name:    "unrelated text",
			input:   "pfctl: unknown command\n",
			wantErr: true,
		},
		{
			name: "zero counters present",
			input: "State Table                          Total             Rate\n" +
				"  current entries                      0\n",
			want: PfInfo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSI(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSI() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSI() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseSI() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseTableShowV(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []TableMemberStats
		wantErr bool
	}{
		{
			name:  "real lab sample",
			input: tableShowVFixture,
			want: []TableMemberStats{
				{
					Address:  "192.168.100.21",
					Cleared:  "Thu Oct 22 08:00:00 2026",
					InBlock:  0,
					OutBlock: 10,
					XPass:    0,
				},
				{
					Address:  "192.168.100.22",
					InBlock:  3,
					OutBlock: 7,
				},
				{
					Address: "192.168.100.23",
				},
			},
		},
		{
			name:  "empty output",
			input: "",
		},
		{
			name:  "garbage counters tolerated as zero",
			input: "10.0.0.1, Cleared: bogus, In/Block: x/y, Out/Block: -, XPass: nan\n",
			want:  []TableMemberStats{{Address: "10.0.0.1", Cleared: "bogus"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTableShowV(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTableShowV() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTableShowV() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseTableShowV() got %d members, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("member[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseRuleset(t *testing.T) {
	input := "block drop all\n\n  pass out quick on em0 keep state  \ntable <web> persist\n\n"
	want := []string{
		"block drop all",
		"pass out quick on em0 keep state",
		"table <web> persist",
	}
	got, err := ParseRuleset(input)
	if err != nil {
		t.Fatalf("ParseRuleset() error = %v", err)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseRuleset() = %q, want %q", got, want)
	}
	if _, err := ParseRuleset(""); err != nil {
		t.Fatalf("ParseRuleset(\"\") error = %v", err)
	}
}

// stubRunner records argv and returns canned output/error.
type stubRunner struct {
	gotName string
	gotArgs []string
	out     string
	err     error
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	s.gotName = name
	s.gotArgs = args
	return s.out, s.err
}

func TestClientTableTestExitCodes(t *testing.T) {
	t.Run("exit 0 means found", func(t *testing.T) {
		c := NewClient("", &stubRunner{out: "1/1 addresses match\n"})
		found, err := c.TableTest(context.Background(), "web", "192.168.100.21")
		if err != nil || !found {
			t.Fatalf("TableTest() = (%v, %v), want (true, nil)", found, err)
		}
	})
	t.Run("exit 1 means not found without error", func(t *testing.T) {
		c := NewClient("", &stubRunner{err: exitErr(t, 1)})
		found, err := c.TableTest(context.Background(), "web", "192.168.100.99")
		if err != nil || found {
			t.Fatalf("TableTest() = (%v, %v), want (false, nil)", found, err)
		}
	})
	t.Run("exit 2 propagates as error", func(t *testing.T) {
		c := NewClient("", &stubRunner{err: exitErr(t, 2)})
		found, err := c.TableTest(context.Background(), "web", "192.168.100.99")
		if err == nil || found {
			t.Fatalf("TableTest() = (%v, %v), want (false, error)", found, err)
		}
	})
	t.Run("other errors propagate", func(t *testing.T) {
		c := NewClient("", &stubRunner{err: errors.New("boom")})
		if _, err := c.TableTest(context.Background(), "web", "192.168.100.99"); err == nil {
			t.Fatal("TableTest() error = nil, want error")
		}
	})
}

func TestClientArgvStrictness(t *testing.T) {
	stub := &stubRunner{}
	c := NewClient("/sbin/pfctl", stub)
	ctx := context.Background()

	if err := c.TableAdd(ctx, "web", "192.168.100.21"); err != nil {
		t.Fatalf("TableAdd() error = %v", err)
	}
	wantArgs := []string{"-t", "web", "-T", "add", "192.168.100.21"}
	if strings.Join(stub.gotArgs, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("argv = %v, want %v", stub.gotArgs, wantArgs)
	}

	// Option-flag injection via names/addresses must be rejected.
	if err := c.TableAdd(ctx, "-f", "/etc/passwd"); err == nil {
		t.Fatal("TableAdd with flag-like name: error = nil, want error")
	}
	if err := c.TableDelete(ctx, "web", "-f"); err == nil {
		t.Fatal("TableDelete with flag-like address: error = nil, want error")
	}
	if err := c.LoadAnchor(ctx, "x; rm -rf /", "/tmp/a.conf"); err == nil {
		t.Fatal("LoadAnchor with unsafe name: error = nil, want error")
	}
	if err := c.TableReplace(ctx, "web", []string{"192.168.100.1", "not-an-ip"}); err == nil {
		t.Fatal("TableReplace with bad member: error = nil, want error")
	}
}

func TestClientReplaceWritesTempFile(t *testing.T) {
	var written string
	stub := &recordingRunner{onRun: func(args []string) {
		// The temp file path is the last argv element.
		path := args[len(args)-1]
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read temp file: %v", err)
			return
		}
		written = string(b)
	}}
	c := NewClient("", stub)
	if err := c.TableReplace(context.Background(), "web", []string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatalf("TableReplace() error = %v", err)
	}
	if written != "10.0.0.1\n10.0.0.2\n" {
		t.Fatalf("temp file content = %q, want members one per line", written)
	}
}

// recordingRunner invokes onRun with the argv before returning success.
type recordingRunner struct {
	onRun func(args []string)
}

func (r *recordingRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	if r.onRun != nil {
		r.onRun(args)
	}
	return "", nil
}
