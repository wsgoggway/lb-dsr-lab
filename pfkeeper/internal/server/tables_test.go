package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/peer"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/pfctl"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/reconcile"
	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// fakeMaster is the MASTER side of the peering listener for the redirect
// test: it answers SyncCheck and records RedirectWrite calls.
type fakeMaster struct {
	pfkeeperv1.UnimplementedPeeringServiceServer

	mu         sync.Mutex
	redirects  int
	lastMethod string
}

func (f *fakeMaster) SyncCheck(_ context.Context, _ *pfkeeperv1.SyncCheckRequest) (*pfkeeperv1.SyncCheckResponse, error) {
	return &pfkeeperv1.SyncCheckResponse{Diverged: false, PeerRevision: "masterrev"}, nil
}

func (f *fakeMaster) PublishHealth(_ context.Context, _ *pfkeeperv1.HealthView) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (f *fakeMaster) RedirectWrite(_ context.Context, req *pfkeeperv1.RedirectWriteRequest) (*pfkeeperv1.RedirectWriteResponse, error) {
	f.mu.Lock()
	f.redirects++
	f.lastMethod = req.GetMethod()
	f.mu.Unlock()
	// Decode-then-respond per method; only SetTable is exercised here.
	switch req.GetMethod() {
	case "/pfkeeper.v1.TableService/SetTable":
		setReq := &pfkeeperv1.SetTableRequest{}
		if err := proto.Unmarshal(req.GetPayload(), setReq); err != nil {
			return nil, err
		}
		if setReq.GetName() == "" {
			return nil, errors.New("empty table name")
		}
		payload, err := proto.Marshal(&pfkeeperv1.SetTableResponse{Commit: "masterrev1"})
		if err != nil {
			return nil, err
		}
		return &pfkeeperv1.RedirectWriteResponse{Payload: payload}, nil
	default:
		return nil, errors.New("unsupported method")
	}
}

// TestSetTableRedirectsToMaster: a BACKUP with a reachable peer proxies the
// TableService mutation via RedirectWrite and returns the master's commit.
func TestSetTableRedirectsToMaster(t *testing.T) {
	ctx := context.Background()
	master := &fakeMaster{}
	ln := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pfkeeperv1.RegisterPeeringServiceServer(srv, master)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	peerCli := &peer.PeerClient{
		Endpoint: "passthrough:///bufnet",
		NodeID:   "lb2",
		PeerID:   "lb1",
		Dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		},
	}
	defer func() { _ = peerCli.Close() }()

	store := &state.Store{Dir: t.TempDir(), Branch: "main"}
	pf := pfctl.NewClient("/sbin/pfctl", &fakePfRunner{failAll: true})
	rec := reconcile.NewConfigReconciler(pf, nil, store, true)
	ctl := peer.NewController(peer.NewStaticRole(string(peer.RoleBackup)), "lb2", "lb1:8444", peerCli, rec, nil, store)
	ctl.SyncOnce(ctx) // the peer is now considered reachable

	s := New(Config{
		Pf:      pf,
		Store:   store,
		Rec:     rec,
		Ctl:     ctl,
		PeerCli: peerCli,
		Role:    func() string { return string(peer.RoleBackup) },
		Audit:   discardLogger(),
	})

	resp, err := s.SetTable(ctx, &pfkeeperv1.SetTableRequest{
		Name:    "web",
		Members: []string{"10.0.0.1"},
		Reason:  "redirect test",
	})
	if err != nil {
		t.Fatalf("SetTable() error = %v", err)
	}
	if resp.GetCommit() != "masterrev1" {
		t.Fatalf("commit = %q, want the master's", resp.GetCommit())
	}
	master.mu.Lock()
	defer master.mu.Unlock()
	if master.redirects != 1 || !strings.HasSuffix(master.lastMethod, "SetTable") {
		t.Fatalf("redirects = %d method = %q, want 1 SetTable", master.redirects, master.lastMethod)
	}
}

// TestTableTestLocal verifies the read-only path against a stub pf runner.
func TestTableTestLocal(t *testing.T) {
	store := &state.Store{Dir: t.TempDir(), Branch: "main"}
	pf := pfctl.NewClient("/sbin/pfctl", &fakePfRunner{found: true})
	rec := reconcile.NewConfigReconciler(pf, nil, store, true)
	s := New(Config{Pf: pf, Store: store, Rec: rec, Audit: discardLogger()})

	resp, err := s.TestAddress(context.Background(), &pfkeeperv1.TestAddressRequest{Table: "web", Address: "10.0.0.1"})
	if err != nil {
		t.Fatalf("TestAddress() error = %v", err)
	}
	if !resp.GetFound() {
		t.Fatal("found = false, want true")
	}
}

// fakePfRunner answers pfctl calls in-process (no real binary).
type fakePfRunner struct {
	failAll bool
	found   bool
}

func (r *fakePfRunner) Run(_ context.Context, _ string, _ ...string) (string, error) {
	if r.failAll {
		return "", errors.New("pfctl: not available in this test")
	}
	if r.found {
		return "1/1 addresses match\n", nil
	}
	return "", nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gitRun is the exec-git fixture helper (mirrors state/store_test.go).
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@lab",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@lab",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// TestSetMemberWeightLocal: the weight mutation goes through the regular
// pipeline (validate -> write -> commit with actor), preserves the weights
// of the other members and rejects unknown members and out-of-range weights.
func TestSetMemberWeightLocal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitRun(t, root, "init", "--bare", "-b", "main", bare)

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "tables", "web.yaml"), []byte(
		"name: web\nmembers:\n  - addr: 192.168.100.21\n    weight: 5\n  - addr: 192.168.100.22\n    weight: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "init", "-b", "main")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "seed: weighted table")
	gitRun(t, seed, "remote", "add", "origin", bare)
	gitRun(t, seed, "push", "-u", "origin", "main")

	work := filepath.Join(root, "work")
	store := &state.Store{Dir: work, Remote: bare, Branch: "main"}
	if err := store.Ensure(ctx); err != nil {
		t.Fatalf("store ensure: %v", err)
	}
	pf := pfctl.NewClient("/sbin/pfctl", &fakePfRunner{})
	rec := reconcile.NewConfigReconciler(pf, nil, store, true)
	s := New(Config{Pf: pf, Store: store, Rec: rec, Audit: discardLogger()})

	resp, err := s.SetMemberWeight(ctx, &pfkeeperv1.SetMemberWeightRequest{
		Table: "web", Address: "192.168.100.21", Weight: 0, Reason: "drain rs1",
	})
	if err != nil {
		t.Fatalf("SetMemberWeight() error = %v", err)
	}
	if resp.GetCommit() == "" {
		t.Fatal("commit is empty, want the mutation commit")
	}

	st, err := state.LoadState(work)
	if err != nil {
		t.Fatal(err)
	}
	weights := map[string]int{}
	for _, m := range st.Tables[0].Members {
		weights[m.Addr] = m.Weight
	}
	if weights["192.168.100.21"] != 0 || weights["192.168.100.22"] != 3 {
		t.Fatalf("weights after mutation = %v, want {21:0 drain, 22:3 preserved}", weights)
	}

	// Unknown member -> InvalidArgument.
	if _, err := s.SetMemberWeight(ctx, &pfkeeperv1.SetMemberWeightRequest{
		Table: "web", Address: "10.9.9.9", Weight: 1,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown member error = %v, want InvalidArgument", err)
	}
	// Out-of-range weight -> InvalidArgument.
	if _, err := s.SetMemberWeight(ctx, &pfkeeperv1.SetMemberWeightRequest{
		Table: "web", Address: "192.168.100.21", Weight: 101,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("weight 101 error = %v, want InvalidArgument", err)
	}
	// Empty table -> InvalidArgument.
	if _, err := s.SetMemberWeight(ctx, &pfkeeperv1.SetMemberWeightRequest{
		Address: "192.168.100.21", Weight: 1,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty table error = %v, want InvalidArgument", err)
	}
}
