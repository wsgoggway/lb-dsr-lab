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
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/render"
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
	// Services are the rendered service plans of this pass (the service
	// anchors are merged into st.Anchors and applied by the regular anchor
	// loop); callers publish them as the service_probability gauge.
	Services []render.Plan
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

// HealthyView is the health layer's opinion handed to the config layer:
// table -> set of addresses that are UP. A table absent from the map (or a
// nil map entirely) means "no health opinion yet — treat everyone as
// healthy" (config mode); an address absent from a present table's set is
// NOT healthy and must never be re-added by the config layer.
type HealthyView map[string]map[string]bool

// allowedMembers filters the desired members down to the ones the config
// layer may apply: desired ∩ healthy. nil healthy for the table = all.
func allowedMembers(desired []string, healthy map[string]bool) []string {
	if healthy == nil {
		return desired
	}
	out := make([]string, 0, len(desired))
	for _, m := range desired {
		if healthy[m] {
			out = append(out, m)
		}
	}
	return out
}

// Reconcile detects drift and applies the desired tables and anchors.
//
// Semantics (v1):
//   - each desired table is compared to the live members against ALLOWED =
//     desired ∩ healthy; TableReplace is issued ONLY on diff (or when the
//     table is missing and allowed is non-empty) and carries the allowed set;
//   - a member excluded by health (in desired, not in healthy) is NOT
//     missing: the health layer owns add/remove of unhealthy members, so the
//     config layer never fights it (t08 layer-conflict fix);
//   - a desired table whose allowed set is empty is not created (empty
//     replace is allowed only for a table that already exists);
//   - tables present in pf but absent from the desired state are untouched;
//   - each desired anchor (rules/*.conf) is loaded into pfkeeper/<name> when
//     its live rules differ; the anchors of the desired services
//     (state.ServiceDef) are rendered first (render.All, health-filtered and
//     therefore re-derived on every pass) and merged into st.Anchors under
//     the anchor name = service name, so they flow through the very same
//     drift-compare + LoadAnchor mechanism.
//
// healthy == nil means config mode: every desired member is allowed. The
// rendered plans are reported in the result for metrics/observability.
func (r *ConfigReconciler) Reconcile(ctx context.Context, st *state.State, revision string, healthy HealthyView) (ReconcileResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	res := ReconcileResult{Revision: revision}

	plans, err := render.All(st, healthy)
	if err != nil {
		return res, fmt.Errorf("reconcile services: %w", err)
	}
	res.Services = plans

	diffs, anchorDrift, err := r.detectLocked(ctx, st, healthy)
	if err != nil {
		return res, err
	}
	res.Diffs = diffs
	res.Drift = len(diffs) > 0 || anchorDrift

	// Tables.
	for _, td := range st.Tables {
		allowed := allowedMembers(state.Addrs(td.Members), healthy[td.Name])
		current, exists, err := r.currentTable(ctx, td.Name)
		if err != nil {
			return res, fmt.Errorf("reconcile table %q: %w", td.Name, err)
		}
		if !exists && len(allowed) == 0 {
			continue // never create an empty table
		}
		if exists && sameSet(current, allowed) {
			continue // no diff -> no replace
		}
		if err := r.pf.TableReplace(ctx, td.Name, allowed); err != nil {
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
// mutating anything (used for the drift metric and SyncCheck). Drift is
// computed against ALLOWED = desired ∩ healthy (see Reconcile); healthy ==
// nil means config mode. Service anchors are rendered into st.Anchors first,
// so their health-dependent drift is part of the answer.
func (r *ConfigReconciler) DetectDrift(ctx context.Context, st *state.State, healthy HealthyView) ([]pfctl.TableDiff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := render.All(st, healthy); err != nil {
		return nil, fmt.Errorf("drift services: %w", err)
	}
	diffs, _, err := r.detectLocked(ctx, st, healthy)
	return diffs, err
}

// ApplyServices re-renders the service anchors for st (the health-transition
// path: weighted chains reference member addresses inline, so a member flip
// must regenerate the chain, not only the table membership) and re-applies
// exactly those anchors that drifted from live pf — same drift-compare and
// LoadAnchor path as Reconcile. It takes the shared pfctl mutation mutex.
// Returns the rendered plans (for the service_probability gauge) and the
// number of anchors actually applied.
func (r *ConfigReconciler) ApplyServices(ctx context.Context, st *state.State, healthy HealthyView) ([]render.Plan, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	plans, err := render.All(st, healthy)
	if err != nil {
		return nil, 0, fmt.Errorf("apply services: %w", err)
	}
	applied := 0
	for _, plan := range plans {
		current, err := r.pf.AnchorsShow(ctx, AnchorName(plan.Anchor))
		if err != nil {
			// An anchor unknown to pf shows as empty/exit-1; load it fresh.
			current = nil
		}
		if sameText(current, plan.Rules) {
			continue
		}
		if err := r.loadAnchor(ctx, plan.Anchor, plan.Rules); err != nil {
			return plans, applied, fmt.Errorf("apply service anchor %q: %w", plan.Anchor, err)
		}
		applied++
	}
	return plans, applied, nil
}

// detectLocked computes table diffs and anchor drift; caller holds r.mu.
func (r *ConfigReconciler) detectLocked(ctx context.Context, st *state.State, healthy HealthyView) ([]pfctl.TableDiff, bool, error) {
	var diffs []pfctl.TableDiff
	for _, td := range st.Tables {
		allowed := allowedMembers(state.Addrs(td.Members), healthy[td.Name])
		current, exists, err := r.currentTable(ctx, td.Name)
		if err != nil {
			return nil, false, fmt.Errorf("drift table %q: %w", td.Name, err)
		}
		var d pfctl.TableDiff
		if !exists {
			d.Missing = allowed
		} else {
			want := map[string]bool{}
			for _, m := range allowed {
				want[m] = true
			}
			live := map[string]bool{}
			for _, m := range current {
				live[m] = true
				if !want[m] {
					d.Extra = append(d.Extra, m)
				}
			}
			for _, m := range allowed {
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
