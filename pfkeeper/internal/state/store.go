package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Store is a service-owned git working tree holding the desired state.
// All git access goes through go-git (the FreeBSD lab guests have no git
// binary); ctx parameters are kept for interface stability — go-git v5 does
// not take contexts, so long network ops are bounded by transport timeouts
// instead.
//
// Pull is a deterministic fetch+hard-reset against the remote branch: the
// working tree belongs to pfkeeper and is never dirty on purpose.
type Store struct {
	Dir    string // working tree (created by Ensure when missing)
	Remote string // origin URL (file path or ssh://)
	Branch string // branch to track and push (e.g. "main")
	// KeyPath, when set and the remote is ssh://, is the deploy key used for
	// authentication (ed25519 and friends, parsed by x/crypto/ssh).
	KeyPath string
}

// shortLen matches `git rev-parse --short` for revision strings.
const shortLen = 7

// open opens the working tree repository.
func (s *Store) open() (*git.Repository, error) {
	r, err := git.PlainOpen(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", s.Dir, err)
	}
	return r, nil
}

// auth builds the transport credentials for the remote: an SSH public-key
// auth from KeyPath for ssh:// remotes (user from the URL, "git" fallback),
// none for local paths and plain http(s) (lab).
func (s *Store) auth() (transport.AuthMethod, error) {
	if s.KeyPath == "" || !strings.HasPrefix(s.Remote, "ssh://") {
		return nil, nil
	}
	user := "git"
	if u, err := url.Parse(s.Remote); err == nil && u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	auth, err := gitssh.NewPublicKeysFromFile(user, s.KeyPath, "")
	if err != nil {
		return nil, fmt.Errorf("store: ssh auth from %s: %w", s.KeyPath, err)
	}
	return auth, nil
}

// fetchRefSpec is the standard mirror of remote heads into remote-tracking
// refs.
const fetchRefSpec = "+refs/heads/*:refs/remotes/origin/*"

// fetch fetches origin; empty/unchanged remotes are not errors. Tolerant
// governs whether an unreachable/empty remote is swallowed (fresh init) or
// reported (steady-state sync).
func (s *Store) fetch(r *git.Repository, tolerant bool) error {
	if s.Remote == "" {
		return nil
	}
	auth, err := s.auth()
	if err != nil {
		if tolerant {
			return nil
		}
		return err
	}
	err = r.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		Force:      true,
		RefSpecs:   []config.RefSpec{config.RefSpec(fetchRefSpec)},
	})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository), isNoMatchingRefSpec(err):
		// Remote exists but carries no refs at all.
		return nil
	case tolerant:
		// Fresh init against an empty/unreachable remote: start empty.
		return nil
	default:
		return fmt.Errorf("store: fetch origin: %w", err)
	}
}

// isNoMatchingRefSpec reports go-git's "remote has no matching refs" fetch
// result (empty remote).
func isNoMatchingRefSpec(err error) bool {
	var noMatch *git.NoMatchingRefSpecError
	return errors.As(err, &noMatch)
}

// remoteBranchRef resolves origin/<branch>; ok=false when the remote branch
// does not exist (fresh/empty remote).
func (s *Store) remoteBranchRef(r *git.Repository) (plumbing.Hash, bool, error) {
	ref, err := r.ResolveRevision(plumbing.Revision("refs/remotes/origin/" + s.Branch))
	if err != nil || ref.IsZero() {
		return plumbing.ZeroHash, false, nil
	}
	return *ref, true, nil
}

// resetHard hard-resets the worktree (and the current branch pointer) to hash.
// On an unborn HEAD (fresh init) it creates the branch at the hash instead —
// go-git's Reset cannot move a reference that does not exist yet.
func (s *Store) resetHard(r *git.Repository, hash plumbing.Hash) error {
	wt, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("store: worktree: %w", err)
	}
	if _, err := r.Head(); err != nil {
		// Unborn HEAD: point the tracked branch at the fetched commit and
		// check it out (this go-git forbids Branch+Hash in one Checkout).
		branch := plumbing.NewBranchReferenceName(s.Branch)
		if err := r.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
			return fmt.Errorf("store: branch %s: %w", s.Branch, err)
		}
		if err := wt.Checkout(&git.CheckoutOptions{Branch: branch, Force: true}); err != nil {
			return fmt.Errorf("store: checkout %s: %w", hash.String()[:shortLen], err)
		}
		return nil
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: hash}); err != nil {
		return fmt.Errorf("store: reset --hard %s: %w", hash.String()[:shortLen], err)
	}
	return nil
}

// Ensure makes Dir a ready-to-use checkout of Remote:
//   - Dir/.git missing: PlainInit on Branch, create origin, fetch and
//     hard-reset to origin/<branch> when the branch exists; an empty or
//     unreachable remote leaves an empty (allowed) initial state;
//   - otherwise: fetch to refresh remote-tracking refs.
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
		r, err := git.PlainInitWithOptions(s.Dir, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(s.Branch)},
		})
		if err != nil {
			return fmt.Errorf("store: init: %w", err)
		}
		if s.Remote != "" {
			if _, err := r.CreateRemote(&config.RemoteConfig{
				Name:  "origin",
				URLs:  []string{s.Remote},
				Fetch: []config.RefSpec{config.RefSpec(fetchRefSpec)},
			}); err != nil {
				return fmt.Errorf("store: remote origin: %w", err)
			}
			if err := s.fetch(r, true); err != nil {
				return err
			}
			if ref, ok, _ := s.remoteBranchRef(r); ok {
				if err := s.resetHard(r, ref); err != nil {
					return err
				}
			}
		}
		return nil
	}
	r, err := s.open()
	if err != nil {
		return err
	}
	return s.fetch(r, false)
}

// Pull fast-forwards the working tree to origin/<branch> via fetch + hard
// reset and returns the short revision now checked out. A missing remote
// branch keeps the local state ("", nil on a fresh empty tree).
func (s *Store) Pull(ctx context.Context) (string, error) {
	r, err := s.open()
	if err != nil {
		return "", err
	}
	if err := s.fetch(r, false); err != nil {
		return "", fmt.Errorf("store pull: %w", err)
	}
	ref, ok, err := s.remoteBranchRef(r)
	if err != nil {
		return "", fmt.Errorf("store pull: %w", err)
	}
	if !ok {
		return s.HeadRevision(ctx)
	}
	if err := s.resetHard(r, ref); err != nil {
		return "", fmt.Errorf("store pull: %w", err)
	}
	return s.HeadRevision(ctx)
}

// CommitAndPush stages paths, commits them with the actor as author
// (name=actor, email=actor@pfkeeper; skipped when nothing is staged) and
// pushes the branch (fast-forward only). A push rejection rebases our work
// onto origin via one Pull and retries exactly once; a second failure is
// returned. Returns the short commit now at HEAD.
func (s *Store) CommitAndPush(ctx context.Context, actor, message string, paths ...string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("store commit: no paths given")
	}
	if actor == "" {
		actor = "unknown"
	}
	r, err := s.open()
	if err != nil {
		return "", err
	}
	wt, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("store commit: %w", err)
	}
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(s.Dir, p)); err == nil {
			if _, err := wt.Add(p); err != nil {
				return "", fmt.Errorf("store commit: add %s: %w", p, err)
			}
			continue
		}
		// Path gone from the worktree: drop it from the index when tracked.
		if _, err := wt.Remove(p); err != nil {
			continue // was never tracked; nothing to do
		}
	}

	status, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("store commit: status: %w", err)
	}
	if !status.IsClean() {
		if _, err := wt.Commit(message, &git.CommitOptions{
			Author: &object.Signature{
				Name:  actor,
				Email: actor + "@pfkeeper",
				When:  time.Now(),
			},
		}); err != nil {
			return "", fmt.Errorf("store commit: %w", err)
		}
	}
	if err := s.pushWithRetry(ctx, r); err != nil {
		return "", err
	}
	return s.HeadRevision(ctx)
}

// pushWithRetry pushes the branch; on a non-fast-forward rejection it pulls
// (hard reset re-applies our committed work onto origin) and retries once.
func (s *Store) pushWithRetry(ctx context.Context, r *git.Repository) error {
	auth, _ := s.auth()
	spec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", s.Branch, s.Branch))
	err := r.Push(&git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []config.RefSpec{spec},
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if !isNonFastForward(err) {
		return fmt.Errorf("store push: %w", err)
	}
	// Rebase onto what the remote has: our commit is already in the object
	// store and re-applied by the hard reset, then we retry exactly once.
	if _, perr := s.Pull(ctx); perr != nil {
		return fmt.Errorf("store push rejected and pull failed: %w (push error: %v)", perr, err)
	}
	if err2 := r.Push(&git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []config.RefSpec{spec},
	}); err2 != nil && !errors.Is(err2, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("store push after pull: %w", err2)
	}
	return nil
}

// isNonFastForward matches go-git's push rejection (typed error or message).
func isNonFastForward(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "non-fast-forward")
}

// CheckoutCommit moves the branch to commit (rollback: hard reset + branch
// pointer). Nothing is pushed here; the next mutation pushes the rewound
// branch.
func (s *Store) CheckoutCommit(ctx context.Context, commit string) error {
	r, err := s.open()
	if err != nil {
		return err
	}
	rev, err := r.ResolveRevision(plumbing.Revision(commit))
	if err != nil {
		return fmt.Errorf("store checkout %q: %w", commit, err)
	}
	if err := s.resetHard(r, *rev); err != nil {
		return fmt.Errorf("store checkout %q: %w", commit, err)
	}
	return nil
}

// HeadRevision returns the short hash of the current HEAD commit, "" (nil)
// when HEAD is unborn (fresh empty state).
func (s *Store) HeadRevision(ctx context.Context) (string, error) {
	_ = ctx
	r, err := s.open()
	if err != nil {
		return "", fmt.Errorf("store head: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) || strings.Contains(err.Error(), "reference not found") {
			return "", nil
		}
		return "", fmt.Errorf("store head: %w", err)
	}
	return head.Hash().String()[:shortLen], nil
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
