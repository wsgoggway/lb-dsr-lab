// pfk - command line client for the pfkeeper control-plane gRPC API.
package main

import (
	"flag"
	"fmt"
	"os"
)

// set via -ldflags at build time
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	_ = flag.CommandLine
	fmt.Fprintf(os.Stderr, "pfk %s (%s): command %q is not implemented yet (M6)\n", version, commit, os.Args[1])
	os.Exit(2)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: pfk <command> [args]

commands (implemented in M6):
  tables ls|get|set|add|delete|test
  health ls|inspect|probe
  rules ls|validate|apply-main
  status pf|carp|peer|version
  peer sync-check
  admin sync|rollback
`)
}
