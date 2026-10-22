package pfctl

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// DefaultBin is the canonical location of pfctl on FreeBSD.
const DefaultBin = "/sbin/pfctl"

// tableNameRe restricts table and anchor names passed to pfctl. A name is a
// distinct argv element (no shell involved), but a leading "-" would still be
// parsed by pfctl as an option flag, so names are anchored to safe charset.
var tableNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)

// Client is a thin typed wrapper around the pfctl binary.
//
// Concurrency: mutating methods (TableReplace/TableAdd/TableDelete, Load*,
// Flush*, KillStatesAll) do NOT acquire any lock here — pfctl operations on the
// same ruleset must be serialised by a higher-level owner (the reconciler),
// which also needs to interleave reads and writes atomically. Read methods are
// safe to call concurrently.
type Client struct {
	bin    string
	runner Runner

	availOnce sync.Once
	availOK   bool
	availErr  error
}

// NewClient returns a Client running bin (DefaultBin when empty) through
// runner (ExecRunner when nil).
func NewClient(bin string, runner Runner) *Client {
	if bin == "" {
		bin = DefaultBin
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Client{bin: bin, runner: runner}
}

// Available reports whether the pfctl binary exists and is executable.
// The check is performed once per Client and cached (result and error).
func (c *Client) Available() (bool, error) {
	c.availOnce.Do(func() {
		if strings.ContainsRune(c.bin, os.PathSeparator) {
			info, err := os.Stat(c.bin)
			c.availOK = err == nil && !info.IsDir() && info.Mode()&0o111 != 0
			if err != nil {
				c.availErr = fmt.Errorf("pfctl binary %q: %w", c.bin, err)
			}
			return
		}
		path, err := exec.LookPath(c.bin)
		c.availOK = err == nil
		if err != nil {
			c.availErr = fmt.Errorf("pfctl lookup %q: %w", c.bin, err)
		} else {
			c.bin = path
		}
	})
	return c.availOK, c.availErr
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	return c.runner.Run(ctx, c.bin, args...)
}

// checkName validates a table/anchor name before it reaches the argv.
func checkName(kind, name string) error {
	if !tableNameRe.MatchString(name) {
		return fmt.Errorf("invalid pf %s name %q", kind, name)
	}
	return nil
}

// checkAddr validates a member address (plain IP or CIDR, either family) and
// rejects anything that could be mistaken for a pfctl option flag.
func checkAddr(addr string) error {
	if strings.HasPrefix(addr, "-") {
		return fmt.Errorf("invalid address %q: must not start with '-'", addr)
	}
	if strings.Contains(addr, "/") {
		if _, _, err := net.ParseCIDR(addr); err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", addr, err)
		}
		return nil
	}
	if net.ParseIP(addr) == nil {
		return fmt.Errorf("invalid IP %q", addr)
	}
	return nil
}

// TableShow returns the current members of table name (`pfctl -t <n> -T show`).
func (c *Client) TableShow(ctx context.Context, name string) ([]string, error) {
	if err := checkName("table", name); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "-t", name, "-T", "show")
	if err != nil {
		return nil, fmt.Errorf("table show %q: %w", name, err)
	}
	return splitLines(out), nil
}

// TableReplace atomically replaces the members of table name with members,
// using a temp file (`pfctl -t <n> -T replace -f <tmpfile>`).
func (c *Client) TableReplace(ctx context.Context, name string, members []string) error {
	if err := checkName("table", name); err != nil {
		return err
	}
	for _, m := range members {
		if err := checkAddr(m); err != nil {
			return fmt.Errorf("table %q: %w", name, err)
		}
	}
	tmp, err := os.CreateTemp("", "pfkeeper-table-*.txt")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
	if _, err := tmp.WriteString(strings.Join(members, "\n") + "\n"); err != nil {
		tmp.Close() //nolint:errcheck // best-effort
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if _, err := c.run(ctx, "-t", name, "-T", "replace", "-f", tmpName); err != nil {
		return fmt.Errorf("table replace %q: %w", name, err)
	}
	return nil
}

// TableAdd adds addr to table name (`pfctl -t <n> -T add <addr>`).
func (c *Client) TableAdd(ctx context.Context, name, addr string) error {
	if err := checkName("table", name); err != nil {
		return err
	}
	if err := checkAddr(addr); err != nil {
		return err
	}
	if _, err := c.run(ctx, "-t", name, "-T", "add", addr); err != nil {
		return fmt.Errorf("table add %q %q: %w", name, addr, err)
	}
	return nil
}

// TableDelete removes addr from table name (`pfctl -t <n> -T delete <addr>`).
func (c *Client) TableDelete(ctx context.Context, name, addr string) error {
	if err := checkName("table", name); err != nil {
		return err
	}
	if err := checkAddr(addr); err != nil {
		return err
	}
	if _, err := c.run(ctx, "-t", name, "-T", "delete", addr); err != nil {
		return fmt.Errorf("table delete %q %q: %w", name, addr, err)
	}
	return nil
}

// TableTest reports whether addr is a member of table name
// (`pfctl -t <n> -T test <addr>`). pfctl exit code 1 means "not found" and is
// translated to (false, nil); any other error is propagated.
func (c *Client) TableTest(ctx context.Context, name, addr string) (bool, error) {
	if err := checkName("table", name); err != nil {
		return false, err
	}
	if err := checkAddr(addr); err != nil {
		return false, err
	}
	_, err := c.run(ctx, "-t", name, "-T", "test", addr)
	if err == nil {
		return true, nil
	}
	if code, ok := ExitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("table test %q %q: %w", name, addr, err)
}

// TableStats returns per-member traffic counters of table name
// (`pfctl -t <n> -T show -v`).
func (c *Client) TableStats(ctx context.Context, name string) ([]TableMemberStats, error) {
	if err := checkName("table", name); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "-t", name, "-T", "show", "-v")
	if err != nil {
		return nil, fmt.Errorf("table stats %q: %w", name, err)
	}
	return ParseTableShowV(out)
}

// Stats parses `pfctl -si` output (state table and counters).
func (c *Client) Stats(ctx context.Context) (PfInfo, error) {
	out, err := c.run(ctx, "-si")
	if err != nil {
		return PfInfo{}, fmt.Errorf("pf stats: %w", err)
	}
	return ParseSI(out)
}

// RulesetShow returns the main ruleset lines (`pfctl -sr`).
func (c *Client) RulesetShow(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "-sr")
	if err != nil {
		return nil, fmt.Errorf("ruleset show: %w", err)
	}
	return splitLines(out), nil
}

// AnchorsShow returns the ruleset lines of anchor name (`pfctl -a <n> -sr`).
func (c *Client) AnchorsShow(ctx context.Context, name string) ([]string, error) {
	if err := checkName("anchor", name); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "-a", name, "-sr")
	if err != nil {
		return nil, fmt.Errorf("anchor show %q: %w", name, err)
	}
	return splitLines(out), nil
}

// ValidateMain syntax-checks path without applying it (`pfctl -n -f <path>`).
func (c *Client) ValidateMain(ctx context.Context, path string) error {
	if _, err := c.run(ctx, "-n", "-f", path); err != nil {
		return fmt.Errorf("validate main conf %q: %w", path, err)
	}
	return nil
}

// ValidateAnchor syntax-checks path as the content of anchor name
// (`pfctl -a <n> -n -f <path>`).
func (c *Client) ValidateAnchor(ctx context.Context, name, path string) error {
	if err := checkName("anchor", name); err != nil {
		return err
	}
	if _, err := c.run(ctx, "-a", name, "-n", "-f", path); err != nil {
		return fmt.Errorf("validate anchor %q %q: %w", name, path, err)
	}
	return nil
}

// LoadAnchor loads path into anchor name (`pfctl -a <n> -f <path>`).
// Mutating: the caller (reconciler) is responsible for serialisation.
func (c *Client) LoadAnchor(ctx context.Context, name, path string) error {
	if err := checkName("anchor", name); err != nil {
		return err
	}
	if _, err := c.run(ctx, "-a", name, "-f", path); err != nil {
		return fmt.Errorf("load anchor %q %q: %w", name, path, err)
	}
	return nil
}

// LoadMainConf loads path as the main ruleset (`pfctl -f <path>`).
// Mutating: the caller (reconciler) is responsible for serialisation.
func (c *Client) LoadMainConf(ctx context.Context, path string) error {
	if _, err := c.run(ctx, "-f", path); err != nil {
		return fmt.Errorf("load main conf %q: %w", path, err)
	}
	return nil
}

// FlushStates drops all states (`pfctl -F states`).
// Mutating: the caller (reconciler) is responsible for serialisation.
func (c *Client) FlushStates(ctx context.Context) error {
	if _, err := c.run(ctx, "-F", "states"); err != nil {
		return fmt.Errorf("flush states: %w", err)
	}
	return nil
}

// KillStatesAll kills every state covering the whole address space
// (`pfctl -k 0.0.0.0/0 -k 0.0.0.0/0`), a finer-grained alternative to
// FlushStates that spares non-addressable state.
// Mutating: the caller (reconciler) is responsible for serialisation.
func (c *Client) KillStatesAll(ctx context.Context) error {
	if _, err := c.run(ctx, "-k", "0.0.0.0/0", "-k", "0.0.0.0/0"); err != nil {
		return fmt.Errorf("kill states: %w", err)
	}
	return nil
}

// FlushSources flushes source tracking entries (`pfctl -F Sources`).
// Mutating: the caller (reconciler) is responsible for serialisation.
func (c *Client) FlushSources(ctx context.Context) error {
	if _, err := c.run(ctx, "-F", "Sources"); err != nil {
		return fmt.Errorf("flush sources: %w", err)
	}
	return nil
}

// splitLines trims and drops empty lines from pfctl text output.
func splitLines(out string) []string {
	lines := strings.Split(out, "\n")
	res := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}
