// pf_exporter - Prometheus exporter for FreeBSD pf, served on /metrics.
//
// Every value is produced by exec'ing /sbin/pfctl (no /dev/pf ioctls):
//
//	pfctl -si      state table size + monotonic counters
//	pfctl -sI -v   interface listing (verbose counter blocks where present)
//	pfctl -sT      table names -> per-table member counts via -T show
//
// Conventional metrics port 9100 is taken by pfkeeper on the lab balancers,
// so the default listener is 0.0.0.0:9102.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/wsgoggway/lb-dsr-lab/exporters/pf-exporter/internal/collector"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("web.listen-address", "0.0.0.0:9102",
		"address:port to serve /metrics on")
	pfctlPath := flag.String("pfctl.path", "/sbin/pfctl", "path to the pfctl binary")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	slog.Info("pf_exporter starting", "version", version, "listen", *listen, "pfctl", *pfctlPath)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collector.New(*pfctlPath, nil))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><head><title>pf_exporter</title></head>"+
			"<body><h1>pf_exporter %s</h1><p><a href=\"/metrics\">metrics</a></p></body></html>", version)
	})

	if err := http.ListenAndServe(*listen, mux); err != nil {
		slog.Error("pf_exporter: http server failed", "err", err)
		os.Exit(1)
	}
}
