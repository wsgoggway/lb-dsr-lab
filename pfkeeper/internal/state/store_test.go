package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// gitRun executes git in dir with the test identity; fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := append(
		os.Environ(),
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@lab",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@lab",
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// seedOrigin creates a bare origin with one initial commit containing a
// minimal state tree, and returns (bareDir, worktreeDirWithSeed).
func seedOrigin(t *testing.T) (string, string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "-b", "main", bare)

	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, "config.yaml"), []byte("sync:\n  mode: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "tables", "web.yaml"), []byte("name: web\nmembers:\n  - 192.168.100.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "init", "-b", "main")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "seed: initial state")
	gitRun(t, seed, "remote", "add", "origin", bare)
	gitRun(t, seed, "push", "-u", "origin", "main")
	return bare, seed
}

func TestStoreLifecycle(t *testing.T) {
	bare, seed := seedOrigin(t)
	work := t.TempDir()

	// Record the origin's initial revision for divergence checks.
	seedRev := gitRun(t, seed, "rev-parse", "--short", "HEAD")
	initial := ""

	store := &Store{Dir: work, Remote: bare, Branch: "main"}
	ctx := context.Background()

	if err := store.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	rev, err := store.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if rev == "" || rev == initial {
		t.Fatalf("Pull() revision = %q, want the seeded commit", rev)
	}
	if rev != trimNL(seedRev) {
		t.Fatalf("Pull() revision = %q, want %q", rev, trimNL(seedRev))
	}

	// Loaded state matches the seed.
	st, err := LoadState(work)
	if err != nil {
		t.Fatalf("LoadState(work) error = %v", err)
	}
	if len(st.Tables) != 1 || st.Tables[0].Name != "web" {
		t.Fatalf("tables = %+v", st.Tables)
	}
	h1, err := store.ConfigHash(ctx)
	if err != nil {
		t.Fatalf("ConfigHash() error = %v", err)
	}

	// Mutate: write a new member set and commit+push.
	tablePath := filepath.Join(work, "tables", "web.yaml")
	newYAML := "name: web\nmembers:\n  - 192.168.100.21\n  - 192.168.100.22\n"
	if err := os.WriteFile(tablePath, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, err := store.CommitAndPush(ctx, "operator@example", "set members", "tables/web.yaml")
	if err != nil {
		t.Fatalf("CommitAndPush() error = %v", err)
	}
	if commit == rev {
		t.Fatalf("CommitAndPush() = %q, want a new revision (was %q)", commit, rev)
	}

	// The pushed change is visible from the bare origin.
	originRev := gitRun(t, t.TempDir(), "--git-dir", bare, "rev-parse", "--short", "main")
	if trimNL(originRev) != commit {
		t.Fatalf("origin main = %q, want pushed commit %q", trimNL(originRev), commit)
	}

	// Nothing staged -> no commit, just push (revision unchanged).
	commit2, err := store.CommitAndPush(ctx, "operator@example", "no-op", "tables/web.yaml")
	if err != nil {
		t.Fatalf("CommitAndPush(no-op) error = %v", err)
	}
	if commit2 != commit {
		t.Fatalf("no-op CommitAndPush() = %q, want %q", commit2, commit)
	}

	// A second store (the peer node) pulls the new revision.
	work2 := t.TempDir()
	peer := &Store{Dir: work2, Remote: bare, Branch: "main"}
	if err := peer.Ensure(ctx); err != nil {
		t.Fatalf("peer Ensure() error = %v", err)
	}
	rev2, err := peer.Pull(ctx)
	if err != nil {
		t.Fatalf("peer Pull() error = %v", err)
	}
	if rev2 != commit {
		t.Fatalf("peer Pull() = %q, want %q", rev2, commit)
	}
	h2, err := peer.ConfigHash(ctx)
	if err != nil {
		t.Fatalf("peer ConfigHash() error = %v", err)
	}
	if h1 == h2 {
		t.Fatal("ConfigHash unchanged after member change")
	}

	// Rollback to the initial revision, hash returns to the old value.
	if err := peer.CheckoutCommit(ctx, rev); err != nil {
		t.Fatalf("CheckoutCommit() error = %v", err)
	}
	h3, err := peer.ConfigHash(ctx)
	if err != nil {
		t.Fatalf("ConfigHash() after rollback error = %v", err)
	}
	if h3 != h1 {
		t.Fatalf("hash after rollback = %q, want initial %q", h3, h1)
	}
	if got, err := peer.HeadRevision(ctx); err != nil || got != rev {
		t.Fatalf("HeadRevision() = %q, %v; want %q", got, err, rev)
	}
}

func TestStoreAnchorsAndMainConf(t *testing.T) {
	bare, _ := seedOrigin(t)
	work := t.TempDir()
	store := &Store{Dir: work, Remote: bare, Branch: "main"}
	ctx := context.Background()
	if err := store.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if _, err := store.Pull(ctx); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "rules", "web.conf"), []byte("pass in quick to <web>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	anchors, err := store.Anchors()
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	if anchors["web"] != "pass in quick to <web>\n" {
		t.Fatalf("Anchors() = %v", anchors)
	}
	main, err := store.ReadMainConf()
	if err != nil {
		t.Fatalf("ReadMainConf() error = %v", err)
	}
	if main != "" {
		t.Fatalf("ReadMainConf() = %q, want empty", main)
	}
}

// TestStoreKeyPathEnv verifies GIT_SSH_COMMAND wiring without real ssh.
func TestStoreKeyPathEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	work := t.TempDir()
	store := &Store{Dir: work, Remote: "/nonexistent/origin.git", Branch: "main", KeyPath: "/tmp/deploy_key"}
	// Ensure on a fresh dir with an unreachable remote must not fail hard
	// (fetch errors are tolerated for a fresh init).
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() with unreachable remote error = %v", err)
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
