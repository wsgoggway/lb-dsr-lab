package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"sync"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/server"
)

// Flags (parsed in main).
var (
	flagConfig    = flag.String("config", "/usr/local/etc/pfkeeper/pfkeeper.yaml", "daemon config file")
	flagLog       = flag.String("log", "", "additional log file (JSON lines)")
	flagVersionOn = flag.Bool("version", false, "print version and exit")
)

// syncMutex returns the process-wide pfctl mutation mutex shared by the
// config reconciler and the health controller.
func syncMutex() *sync.Mutex { return &sync.Mutex{} }

// clientTLS wraps server.ClientTLSConfig for the peer connection.
func clientTLS(cert, key, ca string) (*tls.Config, error) {
	return server.ClientTLSConfig(cert, key, ca)
}

// serverTLSConfig wraps server.ServerTLSConfig for the listeners.
func serverTLSConfig(cert, key, ca string) (*tls.Config, error) {
	return server.ServerTLSConfig(cert, key, ca)
}

// auditInterceptor adapts the audit logger for denied calls.
func auditInterceptor(logger *slog.Logger) func(context.Context, server.Identity, string, error) {
	return func(_ context.Context, id server.Identity, method string, err error) {
		server.AuditErr(logger, id.String(), method, nil, "denied", err)
	}
}
