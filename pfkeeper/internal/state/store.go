package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Store is a service-owned git working tree holding the desired state.
// All git access goes through exec with `-C <Dir>` (no go-git): the working
// tree belongs to pfkeeper, so Pull is a deterministic fetch+reset --hard
// against the remote branch (chosen over `git pull --rebase` exactly because
// the tree is never locally dirty on purpose and reset is idempotent).
type Store struct {
	Dir    string // working tree (created by Ensure when missing)
	Remote string // origin URL (file path or ssh:)
	Branch string // branch to track and push (e.g. "main")
	// KeyPath, when set, becomes the deploy key of GIT_SSH_COMMAND:
	// "ssh -i <KeyPath> -o StrictHostKeyChecking=accept-new".
	KeyPath string
}

// gitIdentity is stamped into the repo at Ensure; the human actor goes into
// the commit message, not the committer fields.
const (
	identityName  = "pfkeeper"
	identityEmail = "pfkeeper@localhost"
)

// exitCode reports the process exit code if err wraps *exec.ExitError; ok is
// false for any other error kind.
func exitCode(err error) (code int, ok bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// runGit executes git -C <Dir> <args...> and returns stdout.
func (s *Store) runGit(ctx context.Context, args ...string) (string, error) {
	return s.runGitEnv(ctx, nil, args...)
}

// runGitEnv is runGit with extra environment variables.
func (s *Store) runGitEnv(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	full := append([]string{"-C", s.Dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	env := append(os.Environ(), extraEnv...)
	if s.KeyPath != "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+s.KeyPath+" -o StrictHostKeyChecking=accept-new")
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Ensure makes Dir a ready-to-use clone of Remote:
//   - Dir/.git missing: git init -b <branch>, add origin, fetch, checkout -B
//     <branch> origin/<branch> (tolerating a missing remote branch);
//   - otherwise: plain fetch to refresh remote-tracking refs.
//
// In both cases the local git identity is stamped for later commits.
func (s *Store) Ensure(ctx context.Context) error {
	if s.Dir == "" {
		return fmt.Errorf("store: Dir is empty")
	}
	if s.Branch == "" {
		s.Branch = "main"
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return fmt.Errorf("store: mkdir %s: %w", s.Dir, err)
		}
		if _, err := s.runGit(ctx, "init", "-b", s.Branch); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if s.Remote != "" {
			if _, err := s.runGit(ctx, "remote", "add", "origin", s.Remote); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			// A fresh remote may have no branch yet; tolerate fetch failure.
			_, _ = s.runGit(ctx, "fetch", "origin")
			_, _ = s.runGit(ctx, "checkout", "-B", s.Branch, "origin/"+s.Branch)
		}
	} else {
		if _, err := s.runGit(ctx, "fetch", "origin"); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	for _, cfg := range [][2]string{{"user.name", identityName}, {"user.email", identityEmail}} {
		if _, err := s.runGit(ctx, "config", cfg[0], cfg[1]); err != nil {
			return fmt.Errorf("store: set %s: %w", cfg[0], err)
		}
	}
	return nil
}

// Pull fast-forwards the working tree to origin/<branch> via fetch + reset
// --hard (deterministic; the tree is service-owned) and returns the short
// revision now checked out. A missing origin/<branch> keeps the current HEAD.
func (s *Store) Pull(ctx context.Context) (string, error) {
	if _, err := s.runGit(ctx, "fetch", "origin"); err != nil {
		return "", fmt.Errorf("store pull: %w", err)
	}
	if _, err := s.runGit(ctx, "rev-parse", "--verify", "-q", "origin/"+s.Branch); err != nil {
		// Remote branch not there (yet): keep local state untouched.
		return s.HeadRevision(ctx)
	}
	if _, err := s.runGit(ctx, "reset", "--hard", "origin/"+s.Branch); err != nil {
		return "", fmt.Errorf("store pull: %w", err)
	}
	return s.HeadRevision(ctx)
}

// CommitAndPush stages paths, commits "actor: message" (skipping the commit
// when nothing is staged) and pushes with --force-with-lease. On a push
// rejection it rebases on origin once and retries the push a single time;
// a second failure is returned. Returns the short commit pushed (or HEAD
// revision when nothing new was committed).
func (s *Store) CommitAndPush(ctx context.Context, actor, message string, paths ...string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("store commit: no paths given")
	}
	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := s.runGit(ctx, addArgs...); err != nil {
		return "", fmt.Errorf("store commit: %w", err)
	}
	if _, err := s.runGit(ctx, "diff", "--cached", "--quiet"); err != nil {
		// exit 1 = there are staged changes; anything else is a real error.
		if code, ok := exitCode(err); !ok || code != 1 {
			return "", fmt.Errorf("store commit: %w", err)
		}
		commitMsg := fmt.Sprintf("%s: %s", actor, message)
		if _, err := s.runGit(ctx, "commit", "-m", commitMsg); err != nil {
			return "", fmt.Errorf("store commit: %w", err)
		}
	}
	rev, err := s.HeadRevision(ctx)
	if err != nil {
		return "", err
	}
	if err := s.pushWithRetry(ctx); err != nil {
		return "", err
	}
	return rev, nil
}

// pushWithRetry pushes with lease; on rejection rebases once and retries.
func (s *Store) pushWithRetry(ctx context.Context) error {
	_, err := s.runGit(ctx, "push", "--force-with-lease", "origin", s.Branch)
	if err == nil {
		return nil
	}
	// Rebase onto what the remote has, then retry exactly once.
	if _, rerr := s.runGit(ctx, "pull", "--rebase", "origin", s.Branch); rerr != nil {
		return fmt.Errorf("store push rejected and rebase failed: %w (push error: %v)", rerr, err)
	}
	if _, err2 := s.runGit(ctx, "push", "--force-with-lease", "origin", s.Branch); err2 != nil {
		return fmt.Errorf("store push after rebase: %w", err2)
	}
	return nil
}

// CheckoutCommit moves the branch to commit (`git checkout -B <branch>
// <commit>`), i.e. a rollback. Nothing is pushed here; the next mutation
// force-pushes the rewound branch.
func (s *Store) CheckoutCommit(ctx context.Context, commit string) error {
	if _, err := s.runGit(ctx, "checkout", "-B", s.Branch, commit); err != nil {
		return fmt.Errorf("store checkout %q: %w", commit, err)
	}
	return nil
}

// HeadRevision returns the short hash of the current HEAD commit.
func (s *Store) HeadRevision(ctx context.Context) (string, error) {
	out, err := s.runGit(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("store head: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ConfigHash returns sha256 over the sorted (path, content) stream of
// config.yaml, tables/*.yaml, rules/*.conf and conf/pf.conf. Absent files are
// skipped; a path and a NUL byte precede each content block so that renames
// and content shifts change the hash.
func (s *Store) ConfigHash(ctx context.Context) (string, error) {
	_ = ctx // hash is content-only; ctx kept for signature symmetry
	relPaths := []string{"config.yaml", "conf/pf.conf"}
	for _, pattern := range []string{"tables/*.yaml", "rules/*.conf"} {
		matches, err := filepath.Glob(filepath.Join(s.Dir, pattern))
		if err != nil {
			return "", fmt.Errorf("store hash: %w", err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(s.Dir, m)
			if err != nil {
				return "", fmt.Errorf("store hash: %w", err)
			}
			relPaths = append(relPaths, rel)
		}
	}
	sort.Strings(relPaths)

	h := sha256.New()
	for _, rel := range relPaths {
		b, err := os.ReadFile(filepath.Join(s.Dir, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("store hash: read %s: %w", rel, err)
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadMainConf returns conf/pf.conf content, "" when the file is absent.
func (s *Store) ReadMainConf() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "conf", "pf.conf"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read conf/pf.conf: %w", err)
	}
	return string(b), nil
}

// Anchors returns rules/*.conf keyed by file base name.
func (s *Store) Anchors() (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "rules", "*.conf"))
	if err != nil {
		return nil, fmt.Errorf("store anchors: %w", err)
	}
	res := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("store anchors: read %s: %w", f, err)
		}
		res[strings.TrimSuffix(filepath.Base(f), ".conf")] = string(b)
	}
	return res, nil
}
