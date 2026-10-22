// pfkeeper daemon: manages local pf tables/rules from the git desired state.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// set via -ldflags at build time
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	config := flag.String("config", "/usr/local/etc/pfkeeper/pfkeeper.yaml", "daemon config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("pfkeeper %s (%s)\n", version, commit)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("pfkeeper starting", "version", version, "commit", commit, "config", *config)

	// TODO(M6): wire config load, pfctl runner, state store, reconciler,
	// health probers, watchers, metrics and the gRPC servers (client 8443,
	// peer 8444). M0 stub exits cleanly so the skeleton is deployable.
	slog.Info("nothing to do yet: implementation pending (M1-M7)")
}
