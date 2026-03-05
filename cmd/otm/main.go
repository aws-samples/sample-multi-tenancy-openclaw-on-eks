package main

import (
	"fmt"
	"os"

	"github.com/shawn/openclaw-tenancy/cmd/otm/cmd"
)

var (
	version   = "v0.1.0"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	cmd.SetVersion(version, commit, buildDate)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
