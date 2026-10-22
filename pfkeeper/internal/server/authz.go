package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Role names carried in the client certificate URI SAN
// "pfkeeper://role/<role>".
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
	RoleNode     = "node"
)

// Identity is the authenticated caller extracted from the peer certificate.
type Identity struct {
	CN   string
	Role string
}

// String renders the identity for logs and error messages.
func (i Identity) String() string { return fmt.Sprintf("%s(%s)", i.CN, i.Role) }

// roleURIPrefix is the URI SAN prefix carrying the role.
const roleURIPrefix = "pfkeeper://role/"

// IdentityFromPeer extracts the caller identity from the gRPC TLS peer info.
func IdentityFromPeer(ctx context.Context) (Identity, error) {
	peerInfo, ok := grpcpeer.FromContext(ctx)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "no peer info (plaintext connection?)")
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "peer did not authenticate via TLS")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return Identity{}, status.Error(codes.Unauthenticated, "no verified client certificate")
	}
	cert := tlsInfo.State.VerifiedChains[0][0]
	id := Identity{CN: cert.Subject.CommonName}
	for _, uri := range cert.URIs {
		if after, found := strings.CutPrefix(uri.String(), roleURIPrefix); found {
			id.Role = after
			break
		}
	}
	if id.Role == "" {
		return id, status.Errorf(codes.Unauthenticated,
			"certificate %q carries no %s<role> URI SAN", id.CN, roleURIPrefix)
	}
	return id, nil
}

// identityFromCert is the pure form used by tests: CN + URI SANs of a parsed
// certificate.
func identityFromCert(cert *x509.Certificate) Identity {
	id := Identity{CN: cert.Subject.CommonName}
	for _, uri := range cert.URIs {
		if after, found := strings.CutPrefix(uri.String(), roleURIPrefix); found {
			id.Role = after
			break
		}
	}
	return id
}

// Method classes. PeeringService lives on the peer listener (8444) and is
// node-only; everything else is a client-facing service on 8443.
var (
	peeringMethods = map[string]bool{
		"/pfkeeper.v1.PeeringService/PushConfig":    true,
		"/pfkeeper.v1.PeeringService/PublishHealth": true,
		"/pfkeeper.v1.PeeringService/SyncCheck":     true,
		"/pfkeeper.v1.PeeringService/RedirectWrite": true,
	}
	readMethods = map[string]bool{
		"/pfkeeper.v1.TableService/ListTables":  true,
		"/pfkeeper.v1.TableService/GetTable":    true,
		"/pfkeeper.v1.TableService/TestAddress": true,
		"/pfkeeper.v1.RuleService/GetAnchors":   true,
		"/pfkeeper.v1.RuleService/Validate":     true,
		"/pfkeeper.v1.RuleService/Diff":         true,
		"/pfkeeper.v1.HealthService/ListHealth": true,
		"/pfkeeper.v1.HealthService/Inspect":    true,
		"/pfkeeper.v1.StatusService/GetPfStats": true,
		"/pfkeeper.v1.StatusService/CarpState":  true,
		"/pfkeeper.v1.StatusService/PeerState":  true,
		"/pfkeeper.v1.StatusService/Version":    true,
	}
	// operatorMutations are the mutation methods an operator may call:
	// Table mutations, HealthService/ForceCheck and AdminService/Sync.
	operatorMutations = map[string]bool{
		"/pfkeeper.v1.TableService/SetTable":      true,
		"/pfkeeper.v1.TableService/AddMembers":    true,
		"/pfkeeper.v1.TableService/DeleteMembers": true,
		"/pfkeeper.v1.HealthService/ForceCheck":   true,
		"/pfkeeper.v1.AdminService/Sync":          true,
	}
	// adminOnlyMutations require the admin role.
	adminOnlyMutations = map[string]bool{
		"/pfkeeper.v1.RuleService/ApplyMainConf": true,
		"/pfkeeper.v1.AdminService/Rollback":     true,
	}
)

// authorized reports whether identity may call the full gRPC method.
// Matrix (design M6):
//
//	node     — PeeringService only (peer listener);
//	admin    — everything except PeeringService;
//	operator — reads + operator-level mutations;
//	viewer   — reads only.
func authorized(id Identity, fullMethod string) error {
	if peeringMethods[fullMethod] {
		if id.Role == RoleNode {
			return nil
		}
		return fmt.Errorf("peering methods require role %q, caller is %q", RoleNode, id.Role)
	}
	switch id.Role {
	case RoleAdmin:
		return nil
	case RoleOperator:
		if readMethods[fullMethod] || operatorMutations[fullMethod] {
			return nil
		}
		if adminOnlyMutations[fullMethod] {
			return fmt.Errorf("%s requires the %q role", fullMethod, RoleAdmin)
		}
		return fmt.Errorf("operator is not allowed to call %s", fullMethod)
	case RoleViewer:
		if readMethods[fullMethod] {
			return nil
		}
		return fmt.Errorf("viewer may only call read methods, not %s", fullMethod)
	case RoleNode:
		return fmt.Errorf("node certificates may only call PeeringService, not %s", fullMethod)
	default:
		return fmt.Errorf("unknown role %q", id.Role)
	}
}

// AuthUnaryInterceptor resolves the caller identity and enforces the
// role/method matrix; failures carry the identity for the audit trail.
func AuthUnaryInterceptor(audit func(ctx context.Context, id Identity, method string, err error)) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id, err := IdentityFromPeer(ctx)
		if err != nil {
			return nil, err
		}
		if err := authorized(id, info.FullMethod); err != nil {
			err = status.Errorf(codes.PermissionDenied, "identity %s: %v", id, err)
			if audit != nil {
				audit(ctx, id, info.FullMethod, err)
			}
			return nil, err
		}
		return handler(identityToContext(ctx, id), req)
	}
}

// identityContextKey carries the Identity to service handlers.
type identityContextKey struct{}

// identityToContext attaches the verified identity to the handler context.
func identityToContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFromContext returns the identity attached by the auth interceptor.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}
