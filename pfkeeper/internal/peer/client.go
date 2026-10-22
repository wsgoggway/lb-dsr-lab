package peer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pfkeeperv1 "github.com/wsgoggway/lb-dsr-lab/pfkeeper/api/pfkeeperv1"
)

// DefaultCallTimeout bounds every individual RPC issued by PeerClient.
const DefaultCallTimeout = 3 * time.Second

// PeerClient is the gRPC client towards the peer node (PeeringService on
// port 8444). One client per daemon; it dials lazily and is safe for
// concurrent use.
type PeerClient struct {
	// Endpoint is the gRPC target, e.g. "lb2:8444" or
	// "passthrough:///bufnet" for in-memory tests.
	Endpoint string
	// TLS, when set, is used as transport credentials; otherwise insecure
	// (the lab pairs protect transport with node certificates in M6).
	TLS *tls.Config
	// NodeID/PeerID identify the endpoints in diagnostics.
	NodeID string
	PeerID string
	// CallTimeout bounds each RPC (DefaultCallTimeout when zero).
	CallTimeout time.Duration
	// Dialer overrides transport dialing; tests inject a bufconn dialer.
	Dialer func(ctx context.Context, addr string) (net.Conn, error)

	mu   sync.Mutex
	conn *grpc.ClientConn
}

// callCtx derives a per-call context with the client timeout unless the
// caller already set a deadline.
func (c *PeerClient) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := c.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// grpcClient lazily connects and returns the generated service client.
func (c *PeerClient) grpcClient() (pfkeeperv1.PeeringServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return pfkeeperv1.NewPeeringServiceClient(c.conn), nil
	}
	var creds credentials.TransportCredentials
	if c.TLS != nil {
		creds = credentials.NewTLS(c.TLS)
	} else {
		creds = insecure.NewCredentials()
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if c.Dialer != nil {
		opts = append(opts, grpc.WithContextDialer(c.Dialer))
	}
	conn, err := grpc.NewClient(c.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("peer dial %s: %w", c.Endpoint, err)
	}
	c.conn = conn
	return pfkeeperv1.NewPeeringServiceClient(conn), nil
}

// SyncCheck compares our config hash/revision with the peer's.
func (c *PeerClient) SyncCheck(ctx context.Context, configHash, revision string) (*pfkeeperv1.SyncCheckResponse, error) {
	cli, err := c.grpcClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := cli.SyncCheck(ctx, &pfkeeperv1.SyncCheckRequest{ConfigHash: configHash, Revision: revision})
	if err != nil {
		return nil, fmt.Errorf("peer %s: sync check: %w", c.PeerID, err)
	}
	return resp, nil
}

// PushConfig sends an effective-config snapshot to the peer (MASTER -> BACKUP).
func (c *PeerClient) PushConfig(ctx context.Context, snap *pfkeeperv1.EffectiveConfigSnapshot) error {
	cli, err := c.grpcClient()
	if err != nil {
		return err
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	if _, err := cli.PushConfig(ctx, snap); err != nil {
		return fmt.Errorf("peer %s: push config: %w", c.PeerID, err)
	}
	return nil
}

// PublishHealth sends our per-member health view to the peer (both roles).
func (c *PeerClient) PublishHealth(ctx context.Context, view *pfkeeperv1.HealthView) error {
	cli, err := c.grpcClient()
	if err != nil {
		return err
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	if _, err := cli.PublishHealth(ctx, view); err != nil {
		return fmt.Errorf("peer %s: publish health: %w", c.PeerID, err)
	}
	return nil
}

// ProxyToMaster forwards a client mutation to the MASTER node (used by the
// BACKUP's gRPC server in M6 to redirect writes).
func (c *PeerClient) ProxyToMaster(ctx context.Context, method string, payload []byte, actorCN string) ([]byte, error) {
	cli, err := c.grpcClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := cli.RedirectWrite(ctx, &pfkeeperv1.RedirectWriteRequest{Method: method, Payload: payload, ActorCn: actorCN})
	if err != nil {
		return nil, fmt.Errorf("peer %s: redirect write %s: %w", c.PeerID, method, err)
	}
	return resp.GetPayload(), nil
}

// Close tears down the underlying connection.
func (c *PeerClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}
