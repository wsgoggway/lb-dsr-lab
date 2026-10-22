package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun executes git in dir with the test identity. ONLY the test fixtures
// (the bare origin and its seed commit) use exec git — the client Store
// under test goes through go-git exclusively.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@lab",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@lab",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// seedOrigin creates a bare origin with one initial commit containing a
// minimal state tree (exec git, test fixture only) and returns
// (bareDir, seedWorktreeDir, shortRevOfSeed).
func seedOrigin(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitRun(t, root, "init", "--bare", "-b", "main", bare)

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
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

	seedRev := strings.TrimSpace(gitRun(t, seed, "rev-parse", "--short=7", "HEAD"))
	return bare, seed, seedRev
}

func TestStoreLifecycleGoGit(t *testing.T) {
	ctx := context.Background()
	bare, _, seedRev := seedOrigin(t)
	work := t.TempDir()

	store := &Store{Dir: work, Remote: bare, Branch: "main"}
	if err := store.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	// Pull brings the seeded state; revision matches origin.
	rev, err := store.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if rev != seedRev {
		t.Fatalf("Pull() revision = %q, want seed %q", rev, seedRev)
	}
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

	// Mutate and commit as the actor.
	tablePath := filepath.Join(work, "tables", "web.yaml")
	newYAML := "name: web\nmembers:\n  - 192.168.100.21\n  - 192.168.100.22\n"
	if err := os.WriteFile(tablePath, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	actor := "operator@example"
	commit, err := store.CommitAndPush(ctx, actor, "set members", "tables/web.yaml")
	if err != nil {
		t.Fatalf("CommitAndPush() error = %v", err)
	}
	if commit == rev || len(commit) != 7 {
		t.Fatalf("CommitAndPush() = %q, want a new 7-char revision (was %q)", commit, rev)
	}

	// The commit reached the bare origin with the right author.
	originRev := strings.TrimSpace(gitRun(t, "", "--git-dir", bare, "rev-parse", "--short=7", "main"))
	if originRev != commit {
		t.Fatalf("origin main = %q, want pushed commit %q", originRev, commit)
	}
	author := strings.TrimSpace(gitRun(t, "", "--git-dir", bare, "log", "-1", "--format=%an", "main"))
	if author != actor {
		t.Fatalf("commit author = %q, want %q", author, actor)
	}

	// Nothing staged -> no commit, just push (revision unchanged).
	commit2, err := store.CommitAndPush(ctx, actor, "no-op", "tables/web.yaml")
	if err != nil {
		t.Fatalf("CommitAndPush(no-op) error = %v", err)
	}
	if commit2 != commit {
		t.Fatalf("no-op CommitAndPush() = %q, want %q", commit2, commit)
	}

	// A second store (the peer node) pulls the new revision.
	peerStore := &Store{Dir: t.TempDir(), Remote: bare, Branch: "main"}
	if err := peerStore.Ensure(ctx); err != nil {
		t.Fatalf("peer Ensure() error = %v", err)
	}
	rev2, err := peerStore.Pull(ctx)
	if err != nil {
		t.Fatalf("peer Pull() error = %v", err)
	}
	if rev2 != commit {
		t.Fatalf("peer Pull() = %q, want %q", rev2, commit)
	}
	h2, err := peerStore.ConfigHash(ctx)
	if err != nil {
		t.Fatalf("peer ConfigHash() error = %v", err)
	}
	if h1 == h2 {
		t.Fatal("ConfigHash unchanged after member change")
	}

	// Rollback to the initial revision; hash returns to the old value.
	if err := peerStore.CheckoutCommit(ctx, rev); err != nil {
		t.Fatalf("CheckoutCommit() error = %v", err)
	}
	if got, err := peerStore.HeadRevision(ctx); err != nil || got != rev {
		t.Fatalf("HeadRevision() = %q, %v; want %q", got, err, rev)
	}
	h3, err := peerStore.ConfigHash(ctx)
	if err != nil {
		t.Fatalf("ConfigHash() after rollback error = %v", err)
	}
	if h3 != h1 {
		t.Fatalf("hash after rollback = %q, want initial %q", h3, h1)
	}
}

// TestStoreFreshInitEmptyRemote: a fresh init against an empty bare remote
// leaves an allowed empty initial state; the first commit pushes the branch.
func TestStoreFreshInitEmptyRemote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitRun(t, root, "init", "--bare", "-b", "main", bare)

	store := &Store{Dir: filepath.Join(root, "work"), Remote: bare, Branch: "main"}
	if err := store.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() on empty remote error = %v", err)
	}
	rev, err := store.Pull(ctx)
	if err != nil || rev != "" {
		t.Fatalf("Pull() on empty state = (%q, %v), want (\"\", nil)", rev, err)
	}
	if got, err := store.HeadRevision(ctx); err != nil || got != "" {
		t.Fatalf("HeadRevision() on empty state = (%q, %v), want (\"\", nil)", got, err)
	}

	// First commit from the empty state lands on origin main.
	if err := os.WriteFile(filepath.Join(store.Dir, "config.yaml"), []byte("sync:\n  mode: and\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, err := store.CommitAndPush(ctx, "admin", "bootstrap", "config.yaml")
	if err != nil {
		t.Fatalf("CommitAndPush() from empty state error = %v", err)
	}
	originRev := strings.TrimSpace(gitRun(t, "", "--git-dir", bare, "rev-parse", "--short=7", "main"))
	if originRev != commit {
		t.Fatalf("origin main = %q, want %q", originRev, commit)
	}
}

// TestStoreSSHAuthParsing verifies the deploy-key parsing path (ed25519)
// without touching the network: Ensure against an unreachable ssh remote
// must not fail (fresh init tolerates), and auth() must build.
func TestStoreSSHAuthParsing(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available")
	}
	keyPath := filepath.Join(t.TempDir(), "deploy_ed25519")
	out, err := exec.Command(keygen, "-t", "ed25519", "-N", "", "-f", keyPath, "-C", "pfkeeper-test").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}

	store := &Store{
		Dir:     t.TempDir(),
		Remote:  "ssh://lab@127.0.0.1/nonexistent/repo.git",
		Branch:  "main",
		KeyPath: keyPath,
	}
	auth, err := store.auth()
	if err != nil {
		t.Fatalf("auth() error = %v (ed25519 key must parse)", err)
	}
	if auth == nil {
		t.Fatal("auth() = nil with ssh remote and key path")
	}
	// Unreachable ssh remote on a fresh dir must not fail Ensure.
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() with unreachable ssh remote error = %v", err)
	}
}

// TestStoreAnchorsAndMainConf covers the pure-file helpers (no git involved).
func TestStoreAnchorsAndMainConf(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir, Branch: "main"}
	if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules", "web.conf"), []byte("pass in quick to <web>\n"), 0o644); err != nil {
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
