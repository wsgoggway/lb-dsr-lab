// Package pfctl wraps the FreeBSD pfctl(8) binary: strict-argv execution and
// text output parsers for the info (-si), interface (-sI -v) and table (-sT)
// subcommands. The exporter never touches /dev/pf ioctls directly - every
// value comes from parsed pfctl output.
package pfctl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a command with a strict argv (no shell) and returns its
// stdout. Stderr is never mixed into the output; on failure it is attached to
// the returned error instead.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs os/exec. The argv is fixed and passed element-wise to the
// kernel: there is no shell interpolation ever.
type ExecRunner struct{}

// Run executes name with args and returns stdout.
//
// A non-zero exit is reported as an error wrapping *exec.ExitError (via %w),
// so callers can inspect the exit code with errors.As. Stderr (trimmed) is
// appended to the error message for diagnostics.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
