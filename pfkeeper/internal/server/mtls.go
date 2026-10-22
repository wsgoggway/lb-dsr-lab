// Package server implements the pfkeeper gRPC API surface: mTLS listeners,
// role-based authorization, audit logging and the six services of
// pfkeeperv1 on top of the pfctl/state/reconcile/peer/health packages.
package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CertReloader serves a *tls.Certificate from disk and reloads it when the
// files change (fsnotify with a periodic fallback). The certificate pointer
// swap is atomic, so in-flight handshakes never see a torn state.
type CertReloader struct {
	certFile string
	keyFile  string

	mu   sync.RWMutex
	cert *tls.Certificate
}

// LoadTLSCert reads certFile/keyFile and returns a reloading certificate
// source. GetCertificate/GetClientCertificate hand out the current pair.
func LoadTLSCert(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	go r.watch()
	return r, nil
}

// Reload re-reads the certificate/key pair.
func (r *CertReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load tls cert %s/%s: %w", r.certFile, r.keyFile, err)
	}
	r.mu.Lock()
	r.cert = &cert
	r.mu.Unlock()
	return nil
}

// GetCertificate satisfies tls.Config for server-side handshakes.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// GetClientCertificate satisfies tls.Config for client-side handshakes.
func (r *CertReloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// debouncer coalesces rapid-fire fsnotify events into one reload.
type debouncer struct {
	mu    sync.Mutex
	timer *time.Timer
	after time.Duration
	fn    func()
}

func (d *debouncer) trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Reset(d.after)
		return
	}
	d.timer = time.AfterFunc(d.after, func() {
		d.mu.Lock()
		d.timer = nil
		d.mu.Unlock()
		d.fn()
	})
}

// watch reloads on fsnotify events and every 5 minutes as a fallback (in
// case the watcher misses atomic renames done by some deploy tooling).
func (r *CertReloader) watch() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	var d = &debouncer{after: 500 * time.Millisecond, fn: func() {
		if err := r.Reload(); err != nil {
			slog.Warn("tls cert reload failed", "error", err)
		} else {
			slog.Info("tls cert reloaded", "cert", r.certFile)
		}
	}}

	watcher, err := fsnotify.NewWatcher()
	var events chan fsnotify.Event
	if err == nil {
		defer watcher.Close() //nolint:errcheck // best-effort
		// Watch the directories: editors and cert tools replace files
		// via rename, which drops per-file watches.
		events = watcher.Events
		for _, f := range []string{r.certFile, r.keyFile} {
			_ = watcher.Add(filepath.Dir(f))
		}
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil // watcher gone; periodic fallback keeps running
				continue
			}
			if (ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create)) &&
				(ev.Name == r.certFile || ev.Name == r.keyFile) {
				d.trigger()
			}
		case <-ticker.C:
			if err := r.Reload(); err != nil {
				slog.Warn("tls cert periodic reload failed", "error", err)
			}
		}
	}
}

// ServerTLSConfig builds the mTLS server config: TLS 1.3 only, client
// certificates required and verified against caFile, own certificate
// hot-reloaded.
func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	reloader, err := LoadTLSCert(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		GetCertificate: reloader.GetCertificate,
	}, nil
}

// ClientTLSConfig builds the mTLS client config used by the daemon's own
// peer connection and by pfk: node/client certificate + CA pool.
func ClientTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	reloader, err := LoadTLSCert(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		RootCAs:              pool,
		GetClientCertificate: reloader.GetClientCertificate,
	}, nil
}
