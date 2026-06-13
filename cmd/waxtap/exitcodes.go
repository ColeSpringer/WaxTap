package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// exitCodeEntry documents one process exit code for the `exit-codes` topic.
type exitCodeEntry struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// exitCodeTable documents the process exit codes. Keep it in sync with the
// classifier in output.go and the table in README.md.
var exitCodeTable = []exitCodeEntry{
	{0, "success"},
	{1, "unclassified error"},
	{2, "invalid request (usage, bad ID, playlist URL to a video command, incompatible spec, unsupported input, requested format unavailable, unknown --client, invalid config)"},
	{3, "video unavailable, restricted, login required, live, or no audio formats"},
	{4, "extraction, cipher, or playlist parsing failure (WaxTap may need an update)"},
	{5, "rate limited"},
	{6, "ffmpeg/ffprobe not found"},
	{7, "incomplete stream or expired stream URL (another client may work)"},
	{8, "PO token required (none configured, mint failed, or YouTube rejected it)"},
	{9, "network failure (dead --proxy, unreachable sidecar, or connection error)"},
	{10, "local I/O failure (e.g. an unwritable output directory)"},
	{130, "canceled (SIGINT)"},
}

// newExitCodesCmd prints the process exit codes.
func newExitCodesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exit-codes",
		Short: "Print the process exit codes used by every command",
		Long: "Print the stable process exit codes WaxTap returns for each failure\n" +
			"class. The same class is carried in the --json error envelope's\n" +
			"error.code field. Scripts may rely on these codes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if rootFlagsValue.json {
				return writeJSON(out, struct {
					SchemaVersion int             `json:"schemaVersion"`
					ExitCodes     []exitCodeEntry `json:"exitCodes"`
				}{schemaVersion, exitCodeTable})
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CODE\tMEANING")
			for _, e := range exitCodeTable {
				fmt.Fprintf(tw, "%d\t%s\n", e.Code, e.Meaning)
			}
			return tw.Flush()
		},
	}
}
