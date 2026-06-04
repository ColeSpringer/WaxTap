package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the CLI version. It is overridable at build time with
// -ldflags "-X main.version=v1.2.3"; otherwise it falls back to the module's
// build info (set for `go install module@version`).
var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the WaxTap version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := resolveVersion()
			if rootFlagsValue.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int    `json:"schemaVersion"`
					Version       string `json:"version"`
					GoVersion     string `json:"goVersion"`
				}{schemaVersion, v, runtime.Version()})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "waxtap %s (%s)\n", v, runtime.Version())
			return nil
		},
	}
}
