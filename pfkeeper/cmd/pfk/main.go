// pfk - command line client for the pfkeeper control-plane gRPC API.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
)

// set via -ldflags at build time
var (
	version = "dev"
	commit  = "unknown"
)

type cli struct {
	tables  pfkeeperv1.TableServiceClient
	rules   pfkeeperv1.RuleServiceClient
	health  pfkeeperv1.HealthServiceClient
	status  pfkeeperv1.StatusServiceClient
	admin   pfkeeperv1.AdminServiceClient
	peering pfkeeperv1.PeeringServiceClient
	ctx     context.Context
}

func main() {
	fs := flag.NewFlagSet("pfk", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8443", "gRPC endpoint (host:port)")
	certFile := fs.String("cert", "", "client certificate PEM (mTLS)")
	keyFile := fs.String("key", "", "client key PEM")
	caFile := fs.String("ca", "", "CA certificate PEM")
	rpcTimeout := fs.Duration("timeout", 5*time.Second, "per-RPC timeout")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *rpcTimeout)
	defer cancel()

	conn, err := dial(ctx, *addr, *certFile, *keyFile, *caFile)
	if err != nil {
		fatal("dial %s: %v", *addr, err)
	}
	defer func() { _ = conn.Close() }()

	c := &cli{
		tables:  pfkeeperv1.NewTableServiceClient(conn),
		rules:   pfkeeperv1.NewRuleServiceClient(conn),
		health:  pfkeeperv1.NewHealthServiceClient(conn),
		status:  pfkeeperv1.NewStatusServiceClient(conn),
		admin:   pfkeeperv1.NewAdminServiceClient(conn),
		peering: pfkeeperv1.NewPeeringServiceClient(conn),
		ctx:     ctx,
	}

	if err := dispatch(c, args); err != nil {
		fmt.Fprintf(os.Stderr, "pfk: %v\n", err)
		os.Exit(1)
	}
}

func dial(_ context.Context, addr, certFile, keyFile, caFile string) (*grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if certFile != "" || caFile != "" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13}
		if certFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("load client cert: %w", err)
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		if caFile != "" {
			caPEM, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("read ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return nil, fmt.Errorf("no certificates in %s", caFile)
			}
			cfg.RootCAs = pool
		}
		creds = credentials.NewTLS(cfg)
	} else {
		creds = insecure.NewCredentials()
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

func dispatch(c *cli, args []string) error {
	switch cmd, rest := args[0], args[1:]; cmd {
	case "tables":
		return tablesCmd(c, rest)
	case "health":
		return healthCmd(c, rest)
	case "rules":
		return rulesCmd(c, rest)
	case "status":
		return statusCmd(c, rest)
	case "peer":
		return peerCmd(c, rest)
	case "admin":
		return adminCmd(c, rest)
	case "version":
		fmt.Printf("pfk %s (%s)\n", version, commit)
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func tablesCmd(c *cli, args []string) error {
	sub, rest := args[0], args[1:]
	reason := extractReason(&rest)
	switch sub {
	case "ls":
		resp, err := c.tables.ListTables(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		for _, t := range resp.GetTables() {
			health := "-"
			if h := t.GetHealth(); h != nil {
				health = fmt.Sprintf("%s:%d", h.GetType(), h.GetPort())
			}
			fmt.Printf("%-16s %-10s members: %s\n", t.GetName(), health, strings.Join(t.GetMembers(), ","))
		}
	case "get":
		if len(rest) != 1 {
			return fmt.Errorf("usage: pfk tables get <name>")
		}
		resp, err := c.tables.GetTable(c.ctx, &pfkeeperv1.GetTableRequest{Name: rest[0]})
		if err != nil {
			return err
		}
		fmt.Printf("table:     %s (persist=%v)\n", resp.GetTable().GetName(), resp.GetTable().GetPersist())
		fmt.Printf("desired:   %s\n", strings.Join(resp.GetTable().GetMembers(), ", "))
		fmt.Printf("effective: %s\n", strings.Join(resp.GetEffective(), ", "))
		fmt.Printf("revision:  %s\n", resp.GetRevision())
	case "set", "add", "delete":
		if len(rest) < 2 {
			return fmt.Errorf("usage: pfk tables %s <name> <member...> [-r reason]", sub)
		}
		name, members := rest[0], rest[1:]
		switch sub {
		case "set":
			resp, err := c.tables.SetTable(c.ctx, &pfkeeperv1.SetTableRequest{Name: name, Members: members, Reason: reason})
			if err != nil {
				return err
			}
			fmt.Println(resp.GetCommit())
		case "add":
			resp, err := c.tables.AddMembers(c.ctx, &pfkeeperv1.AddMembersRequest{Table: name, Members: members, Reason: reason})
			if err != nil {
				return err
			}
			fmt.Println(resp.GetCommit())
		case "delete":
			resp, err := c.tables.DeleteMembers(c.ctx, &pfkeeperv1.DeleteMembersRequest{Table: name, Members: members, Reason: reason})
			if err != nil {
				return err
			}
			fmt.Println(resp.GetCommit())
		}
	case "test":
		if len(rest) != 2 {
			return fmt.Errorf("usage: pfk tables test <name> <address>")
		}
		resp, err := c.tables.TestAddress(c.ctx, &pfkeeperv1.TestAddressRequest{Table: rest[0], Address: rest[1]})
		if err != nil {
			return err
		}
		fmt.Printf("%v\n", resp.GetFound())
	default:
		return fmt.Errorf("unknown tables subcommand %q", sub)
	}
	return nil
}

func healthCmd(c *cli, args []string) error {
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		var table string
		if len(rest) > 0 {
			table = rest[0]
		}
		resp, err := c.health.ListHealth(c.ctx, &pfkeeperv1.ListHealthRequest{Table: table})
		if err != nil {
			return err
		}
		for _, m := range resp.GetMembers() {
			fmt.Printf("%-16s %-16s %-8s code=%d reason=%s\n",
				m.GetTable(), m.GetAddress(), m.GetState(), m.GetStatusCode(), m.GetReason())
		}
	case "inspect", "probe":
		if len(rest) != 2 {
			return fmt.Errorf("usage: pfk health %s <table> <address>", sub)
		}
		table, addr := rest[0], rest[1]
		if sub == "inspect" {
			resp, err := c.health.Inspect(c.ctx, &pfkeeperv1.InspectRequest{Table: table, Address: addr})
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(resp.GetMember(), "", "  ")
			fmt.Println(string(out))
			return nil
		}
		resp, err := c.health.ForceCheck(c.ctx, &pfkeeperv1.ForceCheckRequest{Table: table, Address: addr})
		if err != nil {
			return err
		}
		out, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(out))
	default:
		return fmt.Errorf("unknown health subcommand %q", sub)
	}
	return nil
}

func rulesCmd(c *cli, args []string) error {
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		resp, err := c.rules.GetAnchors(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		for name, text := range resp.GetAnchors() {
			fmt.Printf("=== %s ===\n%s", name, text)
		}
	case "validate":
		if len(rest) != 1 {
			return fmt.Errorf("usage: pfk rules validate <file|->")
		}
		rules, err := readInput(rest[0])
		if err != nil {
			return err
		}
		resp, err := c.rules.Validate(c.ctx, &pfkeeperv1.ValidateRequest{Rules: rules})
		if err != nil {
			return err
		}
		if resp.GetOk() {
			fmt.Println("ok")
			return nil
		}
		return fmt.Errorf("invalid: %s", resp.GetError())
	case "apply-main":
		dry := len(rest) > 0 && rest[0] == "-dry-run"
		resp, err := c.rules.ApplyMainConf(c.ctx, &pfkeeperv1.ApplyMainConfRequest{DryRun: dry})
		if err != nil {
			return err
		}
		if resp.GetValidationError() != "" {
			return fmt.Errorf("validation: %s", resp.GetValidationError())
		}
		fmt.Printf("applied=%v\n", resp.GetApplied())
	default:
		return fmt.Errorf("unknown rules subcommand %q", sub)
	}
	return nil
}

func statusCmd(c *cli, args []string) error {
	sub := args[0]
	switch sub {
	case "pf":
		resp, err := c.status.GetPfStats(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		fmt.Printf("states entries=%d searches=%d inserts=%d removals=%d\n",
			resp.GetStatesEntries(), resp.GetStatesSearches(), resp.GetStatesInserts(), resp.GetStatesRemovals())
		fmt.Printf("counters match=%d state-mismatch=%d\n", resp.GetCounterMatch(), resp.GetCounterStateMismatch())
	case "carp":
		resp, err := c.status.CarpState(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		fmt.Printf("role=%s vhid=%s interface=%s\n", resp.GetRole(), resp.GetVhid(), resp.GetInterface())
	case "peer":
		resp, err := c.status.PeerState(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		fmt.Printf("reachable=%v peer_role=%s peer_revision=%s lag=%ds diverged=%v\n",
			resp.GetReachable(), resp.GetPeerRole(), resp.GetPeerRevision(), resp.GetLagSeconds(), resp.GetDiverged())
	case "version":
		resp, err := c.status.Version(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		fmt.Printf("pfkeeper %s (%s) node=%s\n", resp.GetVersion(), resp.GetCommit(), resp.GetNode())
	default:
		return fmt.Errorf("unknown status subcommand %q", sub)
	}
	return nil
}

func peerCmd(c *cli, args []string) error {
	sub, rest := args[0], args[1:]
	if sub != "sync-check" {
		return fmt.Errorf("unknown peer subcommand %q", sub)
	}
	if len(rest) != 2 {
		return fmt.Errorf("usage: pfk peer sync-check <config-hash> <revision>")
	}
	resp, err := c.peering.SyncCheck(c.ctx, &pfkeeperv1.SyncCheckRequest{ConfigHash: rest[0], Revision: rest[1]})
	if err != nil {
		return err
	}
	fmt.Printf("diverged=%v peer_revision=%s peer_hash=%s\n",
		resp.GetDiverged(), resp.GetPeerRevision(), resp.GetPeerConfigHash())
	return nil
}

func adminCmd(c *cli, args []string) error {
	sub, rest := args[0], args[1:]
	switch sub {
	case "sync":
		resp, err := c.admin.Sync(c.ctx, &emptypb.Empty{})
		if err != nil {
			return err
		}
		fmt.Printf("revision=%s applied_tables=%d\n", resp.GetRevision(), resp.GetAppliedTables())
	case "rollback":
		if len(rest) != 1 {
			return fmt.Errorf("usage: pfk admin rollback <commit>")
		}
		resp, err := c.admin.Rollback(c.ctx, &pfkeeperv1.RollbackRequest{Commit: rest[0]})
		if err != nil {
			return err
		}
		fmt.Printf("revision=%s\n", resp.GetRevision())
	default:
		return fmt.Errorf("unknown admin subcommand %q", sub)
	}
	return nil
}

// extractReason removes "-r <reason>" from args and returns the reason.
func extractReason(args *[]string) string {
	out := make([]string, 0, len(*args))
	reason := ""
	for i := 0; i < len(*args); i++ {
		if (*args)[i] == "-r" || (*args)[i] == "-reason" {
			if i+1 < len(*args) {
				reason = (*args)[i+1]
				i++
			}
			continue
		}
		out = append(out, (*args)[i])
	}
	*args = out
	return reason
}

func readInput(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pfk: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: pfk [flags] <command> [args]

global flags:
  -addr host:port   gRPC endpoint (default 127.0.0.1:8443; use :8444 with "peer")
  -cert/-key/-ca    mTLS client certificate, key and CA PEM
  -timeout 5s       per-RPC timeout

commands:
  tables ls | get <n> | set <n> <members...> | add <n> <members...> | delete <n> <members...> [-r reason] | test <n> <addr>
  health ls [table] | inspect <table> <addr> | probe <table> <addr>
  rules ls | validate <file|-> | apply-main [-dry-run]
  status pf | carp | peer | version
  peer sync-check <config-hash> <revision>
  admin sync | rollback <commit>
`)
}
