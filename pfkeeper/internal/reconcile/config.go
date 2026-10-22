// Package reconcile applies the git desired state to the local pf ruleset.
//
// Two layers share one pfctl mutation mutex:
//
//   - ConfigReconciler: desired-vs-current diff and atomic replace of the
//     tables/anchors declared in the state (config layer);
//   - HealthController: point add/delete of members filtered by health
//     consensus (health layer).
//
// Neither layer logs — the daemon calls them and logs the returned results.
package reconcile

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// AnchorPrefix namespaces all pfkeeper anchors inside the main ruleset.
const AnchorPrefix = "pfkeeper"

// AnchorName returns the fully qualified pf anchor name for an anchor
// declared in the state (rules/<name>.conf -> pfkeeper/<name>).
func AnchorName(name string) string { return AnchorPrefix + "/" + name }

// ReconcileResult summarises one Reconcile pass.
type ReconcileResult struct {
	Revision       string
	TablesApplied  int // TableReplace calls actually issued
	AnchorsApplied int // LoadAnchor calls actually issued
	Drift          bool
	Diffs          []pfctl.TableDiff
}

// ConfigReconciler diffs the desired state against live pf and applies it.
//
// Concurrency: mu is the process-wide pfctl mutation mutex, shared with
// HealthController; every Reconcile holds it for the whole detect+apply
// section so reads and writes stay atomic.
type ConfigReconciler struct {
	pf       *pfctl.Client
	mu       *sync.Mutex
	store    *state.Store
	autoHeal bool
}

// NewConfigReconciler wires the reconciler. mu may be nil (a private mutex is
// created), but the daemon must share one mutex across both layers.
func NewConfigReconciler(pf *pfctl.Client, mu *sync.Mutex, store *state.Store, autoHeal bool) *ConfigReconciler {
	if mu == nil {
		mu = &sync.Mutex{}
	}
	return &ConfigReconciler{pf: pf, mu: mu, store: store, autoHeal: autoHeal}
}

// AutoHeal reports the configured auto-heal flag (divergence heal on MASTER).
func (r *ConfigReconciler) AutoHeal() bool { return r.autoHeal }

// Reconcile detects drift and applies the desired tables and anchors.
//
// Semantics (v1):
//   - each desired table is compared to the live members; TableReplace is
//     issued ONLY on diff (or when the table is missing and desired non-empty);
//   - a desired table with no members is not created (empty replace is
//     allowed only for a table that already exists);
//   - tables present in pf but absent from the desired state are untouched;
//   - each desired anchor (rules/*.conf) is loaded into pfkeeper/<name> when
//     its live rules differ.
func (r *ConfigReconciler) Reconcile(ctx context.Context, st *state.State, revision string) (ReconcileResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	res := ReconcileResult{Revision: revision}

	diffs, anchorDrift, err := r.detectLocked(ctx, st)
	if err != nil {
		return res, err
	}
	res.Diffs = diffs
	res.Drift = len(diffs) > 0 || anchorDrift

	// Tables.
	for _, td := range st.Tables {
		current, exists, err := r.currentTable(ctx, td.Name)
		if err != nil {
			return res, fmt.Errorf("reconcile table %q: %w", td.Name, err)
		}
		if !exists && len(td.Members) == 0 {
			continue // never create an empty table
		}
		if exists && sameSet(current, td.Members) {
			continue // no diff -> no replace
		}
		if err := r.pf.TableReplace(ctx, td.Name, td.Members); err != nil {
			return res, fmt.Errorf("reconcile table %q: %w", td.Name, err)
		}
		res.TablesApplied++
	}

	// Anchors.
	for name, rules := range st.Anchors {
		current, err := r.pf.AnchorsShow(ctx, AnchorName(name))
		if err != nil {
			// An anchor unknown to pf shows as empty/exit-1; load it fresh.
			current = nil
		}
		if sameText(current, rules) {
			continue
		}
		if err := r.loadAnchor(ctx, name, rules); err != nil {
			return res, fmt.Errorf("reconcile anchor %q: %w", name, err)
		}
		res.AnchorsApplied++
	}
	return res, nil
}

// DetectDrift reports desired-vs-live differences per desired table without
// mutating anything (used for the drift metric and SyncCheck).
func (r *ConfigReconciler) DetectDrift(ctx context.Context, st *state.State) ([]pfctl.TableDiff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	diffs, _, err := r.detectLocked(ctx, st)
	return diffs, err
}

// detectLocked computes table diffs and anchor drift; caller holds r.mu.
func (r *ConfigReconciler) detectLocked(ctx context.Context, st *state.State) ([]pfctl.TableDiff, bool, error) {
	var diffs []pfctl.TableDiff
	for _, td := range st.Tables {
		current, exists, err := r.currentTable(ctx, td.Name)
		if err != nil {
			return nil, false, fmt.Errorf("drift table %q: %w", td.Name, err)
		}
		var d pfctl.TableDiff
		if !exists {
			d.Missing = td.Members
		} else {
			want := map[string]bool{}
			for _, m := range td.Members {
				want[m] = true
			}
			live := map[string]bool{}
			for _, m := range current {
				live[m] = true
				if !want[m] {
					d.Extra = append(d.Extra, m)
				}
			}
			for _, m := range td.Members {
				if !live[m] {
					d.Missing = append(d.Missing, m)
				}
			}
		}
		if len(d.Missing) > 0 || len(d.Extra) > 0 {
			d.Table = td.Name
			diffs = append(diffs, d)
		}
	}

	anchorDrift := false
	for name, rules := range st.Anchors {
		current, err := r.pf.AnchorsShow(ctx, AnchorName(name))
		if err != nil || !sameText(current, rules) {
			anchorDrift = true
		}
	}
	return diffs, anchorDrift, nil
}

// currentTable returns live members of a table; exists=false when pf reports
// the table missing (exit 1 with "no table ..." on stderr).
func (r *ConfigReconciler) currentTable(ctx context.Context, name string) ([]string, bool, error) {
	members, err := r.pf.TableShow(ctx, name)
	if err == nil {
		return members, true, nil
	}
	if code, ok := pfctl.ExitCode(err); ok && code == 1 && strings.Contains(strings.ToLower(err.Error()), "no table") {
		return nil, false, nil
	}
	return nil, false, err
}

// loadAnchor writes rules to a temp file and loads it into pfkeeper/<name>.
func (r *ConfigReconciler) loadAnchor(ctx context.Context, name, rules string) error {
	tmp, err := os.CreateTemp("", "pfkeeper-anchor-*.conf")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
	if _, err := tmp.WriteString(rules); err != nil {
		tmp.Close() //nolint:errcheck // best-effort
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	return r.pf.LoadAnchor(ctx, AnchorName(name), tmpName)
}

// ApplyMainConf validates conf/pf.conf from the state and, unless dryRun,
// installs it as the main ruleset. Explicit-only operation: regular
// reconcile never touches the main conf.
func (r *ConfigReconciler) ApplyMainConf(ctx context.Context, dryRun bool) error {
	main, err := r.store.ReadMainConf()
	if err != nil {
		return fmt.Errorf("apply main conf: %w", err)
	}
	if strings.TrimSpace(main) == "" {
		return fmt.Errorf("apply main conf: conf/pf.conf is empty in the state")
	}
	tmp, err := os.CreateTemp("", "pfkeeper-main-*.conf")
	if err != nil {
		return fmt.Errorf("apply main conf: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
	if _, err := tmp.WriteString(main); err != nil {
		tmp.Close() //nolint:errcheck // best-effort
		return fmt.Errorf("apply main conf: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("apply main conf: close %s: %w", tmpName, err)
	}
	if err := r.pf.ValidateMain(ctx, tmpName); err != nil {
		return fmt.Errorf("apply main conf: %w", err)
	}
	if dryRun {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.pf.LoadMainConf(ctx, tmpName); err != nil {
		return fmt.Errorf("apply main conf: %w", err)
	}
	return nil
}

// sameSet compares member sets order-insensitively.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// sameText compares live anchor lines with the desired rules text
// (whitespace-normalised: trimmed lines, trailing empties dropped).
func sameText(liveLines []string, want string) bool {
	var live []string
	for _, l := range liveLines {
		if l = strings.TrimSpace(l); l != "" {
			live = append(live, l)
		}
	}
	var wantLines []string
	for _, l := range strings.Split(want, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			wantLines = append(wantLines, l)
		}
	}
	return strings.Join(live, "\n") == strings.Join(wantLines, "\n")
}
