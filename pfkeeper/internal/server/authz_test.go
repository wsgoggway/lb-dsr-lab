package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
)

// TestAuthorizedMatrix covers the role/method matrix of the interceptor's
// pure decision function.
func TestAuthorizedMatrix(t *testing.T) {
	tests := []struct {
		role   string
		method string
		wantOK bool
	}{
		{RoleAdmin, "/pfkeeper.v1.TableService/SetTable", true},
		{RoleAdmin, "/pfkeeper.v1.AdminService/Rollback", true},
		{RoleAdmin, "/pfkeeper.v1.StatusService/Version", true},

		{RoleOperator, "/pfkeeper.v1.TableService/SetTable", true},
		{RoleOperator, "/pfkeeper.v1.TableService/ListTables", true},
		{RoleOperator, "/pfkeeper.v1.HealthService/ForceCheck", true},
		{RoleOperator, "/pfkeeper.v1.AdminService/Sync", true},
		{RoleOperator, "/pfkeeper.v1.AdminService/Rollback", false},
		{RoleOperator, "/pfkeeper.v1.RuleService/ApplyMainConf", false},
		{RoleOperator, "/pfkeeper.v1.PeeringService/SyncCheck", false},

		{RoleViewer, "/pfkeeper.v1.TableService/ListTables", true},
		{RoleViewer, "/pfkeeper.v1.StatusService/GetPfStats", true},
		{RoleViewer, "/pfkeeper.v1.RuleService/Diff", true},
		{RoleViewer, "/pfkeeper.v1.TableService/SetTable", false},
		{RoleViewer, "/pfkeeper.v1.HealthService/ForceCheck", false},
		{RoleViewer, "/pfkeeper.v1.PeeringService/SyncCheck", false},

		{RoleNode, "/pfkeeper.v1.PeeringService/PushConfig", true},
		{RoleNode, "/pfkeeper.v1.PeeringService/SyncCheck", true},
		{RoleNode, "/pfkeeper.v1.TableService/SetTable", false},
		{RoleNode, "/pfkeeper.v1.StatusService/Version", false},

		{"unknown", "/pfkeeper.v1.StatusService/Version", false},
	}
	for _, tt := range tests {
		t.Run(tt.role+" "+tt.method, func(t *testing.T) {
			err := authorized(Identity{CN: "x", Role: tt.role}, tt.method)
			if got := err == nil; got != tt.wantOK {
				t.Fatalf("authorized(%s, %s): err=%v, want ok=%v", tt.role, tt.method, err, tt.wantOK)
			}
		})
	}
}

// grpcPeerContext attaches a synthetic TLS peer info carrying cert.
func grpcPeerContext(cert *x509.Certificate) context.Context {
	tlsInfo := credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}},
	}
	return grpcpeer.NewContext(context.Background(), &grpcpeer.Peer{AuthInfo: tlsInfo})
}

// TestIdentityFromPeerInfo checks identity extraction from a synthetic TLS
// peer info (no sockets involved).
func TestIdentityFromPeerInfo(t *testing.T) {
	ca := genCA(t)
	certPEM, _ := ca.genLeaf(t, "operator1", RoleOperator, false)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	id, err := IdentityFromPeer(grpcPeerContext(cert))
	if err != nil {
		t.Fatalf("IdentityFromPeer() error = %v", err)
	}
	if id.CN != "operator1" || id.Role != RoleOperator {
		t.Fatalf("identity = %+v, want operator1/operator", id)
	}

	// A certificate without the role URI SAN is unauthenticated.
	noRolePEM, _ := ca.genLeaf(t, "no-role", "", false)
	block2, _ := pem.Decode(noRolePEM)
	cert2, err := x509.ParseCertificate(block2.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := IdentityFromPeer(grpcPeerContext(cert2)); err == nil {
		t.Fatal("IdentityFromPeer() without role SAN: error = nil, want error")
	}

	// Plaintext context -> unauthenticated.
	if _, err := IdentityFromPeer(context.Background()); err == nil {
		t.Fatal("IdentityFromPeer(plaintext ctx): error = nil, want error")
	}
}

func TestMTLSHandshakeAndAuth(t *testing.T) {
	ca := genCA(t)
	serverCertPEM, serverKeyPEM := ca.genLeaf(t, "lb1", RoleNode, true)
	viewerCertPEM, viewerKeyPEM := ca.genLeaf(t, "viewer1", RoleViewer, false)
	nodeCertPEM, nodeKeyPEM := ca.genLeaf(t, "lb2", RoleNode, false)

	certF, keyF, caF := writeCerts(t, serverCertPEM, serverKeyPEM, ca.certPEM)
	tlsCfg, err := ServerTLSConfig(certF, keyF, caF)
	if err != nil {
		t.Fatalf("ServerTLSConfig() error = %v", err)
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(AuthUnaryInterceptor(nil)))
	s := New(Config{Version: "test", Commit: "abc", Node: "lb1"})
	pfkeeperv1.RegisterStatusServiceServer(srv, s)
	pfkeeperv1.RegisterTableServiceServer(srv, s)
	pfkeeperv1.RegisterPeeringServiceServer(srv, s)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	t.Run("viewer reads Version", func(t *testing.T) {
		conn := dialTest(t, lis.Addr().String(), ca.certPEM, viewerCertPEM, viewerKeyPEM)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pfkeeperv1.NewStatusServiceClient(conn).Version(ctx, &emptypb.Empty{}); err != nil {
			t.Fatalf("Version with viewer cert: %v", err)
		}
	})

	t.Run("client without certificate rejected by mTLS", func(t *testing.T) {
		conn := dialTest(t, lis.Addr().String(), ca.certPEM, nil, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := pfkeeperv1.NewStatusServiceClient(conn).Version(ctx, &emptypb.Empty{})
		if err == nil {
			t.Fatal("Version without client cert: error = nil, want handshake failure")
		}
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %s (%v), want Unavailable (TLS handshake failure)", status.Code(err), err)
		}
	})

	t.Run("node cert denied on client mutation", func(t *testing.T) {
		conn := dialTest(t, lis.Addr().String(), ca.certPEM, nodeCertPEM, nodeKeyPEM)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := pfkeeperv1.NewTableServiceClient(conn).SetTable(ctx,
			&pfkeeperv1.SetTableRequest{Name: "web", Members: []string{"10.0.0.1"}})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("SetTable with node cert: %v, want PermissionDenied", err)
		}
	})

	t.Run("viewer cert denied on PeeringService", func(t *testing.T) {
		conn := dialTest(t, lis.Addr().String(), ca.certPEM, viewerCertPEM, viewerKeyPEM)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := pfkeeperv1.NewPeeringServiceClient(conn).SyncCheck(ctx, &pfkeeperv1.SyncCheckRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("SyncCheck with viewer cert: %v, want PermissionDenied", err)
		}
	})

	t.Run("node cert allowed on PeeringService", func(t *testing.T) {
		conn := dialTest(t, lis.Addr().String(), ca.certPEM, nodeCertPEM, nodeKeyPEM)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pfkeeperv1.NewPeeringServiceClient(conn).SyncCheck(ctx, &pfkeeperv1.SyncCheckRequest{}); err != nil {
			t.Fatalf("SyncCheck with node cert: %v", err)
		}
	})
}

// dialTest connects with mTLS client credentials from PEM bytes (cert may be
// empty for the no-certificate case).
func dialTest(t *testing.T, addr string, caPEM, certPEM, keyPEM []byte) *grpc.ClientConn {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("bad CA PEM")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	if certPEM != nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
