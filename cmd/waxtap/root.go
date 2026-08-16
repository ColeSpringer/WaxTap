package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rootFlags holds values for the persistent output flags. The package-level
// value also lets main read --json after Execute returns, before an appEnv exists.
type rootFlags struct {
	json    bool
	quiet   bool
	verbose bool
}

var rootFlagsValue rootFlags

// jsonRequested reports whether --json survives a parse of args. Flag parsing
// aborts at the first bad flag, so a --json after one never reaches
// rootFlagsValue, and the error still has to honor the contract. An allowlisted
// parse gives real parser semantics: -- terminates, --json=false wins, and an
// unknown flag's value is stripped without swallowing a following --json.
//
// A flag value that is literally "--json" is a benign false positive: a JSON
// error document instead of a human one, on a path that is already failing.
func jsonRequested(args []string) bool {
	fs := pflag.NewFlagSet("probe", pflag.ContinueOnError)
	fs.ParseErrorsAllowlist = pflag.ParseErrorsAllowlist{UnknownFlags: true}
	fs.SetOutput(io.Discard)
	v := fs.Bool("json", false, "")
	_ = fs.Parse(args)
	return *v
}

// newRootCmd builds the root command, its persistent flags, and the full
// subcommand set. cobra adds the `help` and `completion` commands on its own.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "waxtap",
		Short: "Audio-focused YouTube downloader and local-audio processor",
		Long: "WaxTap downloads the best available audio from YouTube (or processes a\n" +
			"local file) and can transcode, cut time ranges, remove SponsorBlock\n" +
			"segments, and measure or normalize loudness. It is a single static binary\n" +
			"with no external runtime dependency.\n\n" +
			"Every command supports --json for a stable, scriptable output contract.",
		// Keep --version in sync with the version subcommand, including
		// go-install builds that rely on module build info.
		Version: resolveVersion(),
		// Errors and usage are rendered once, centrally, in main; silence cobra's
		// own printing so failures are not reported twice.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	// cobra's built-in template prints "waxtap version <v>"; match newVersionCmd
	// so the flag and the subcommand agree, Go version included.
	root.SetVersionTemplate(fmt.Sprintf("waxtap {{.Version}} (%s)\n", runtime.Version()))

	// Keep only output flags persistent. Other flags belong to the commands that
	// use them, so extraction flags follow the subcommand.
	pf := root.PersistentFlags()
	pf.BoolVar(&rootFlagsValue.json, "json", false, "emit machine-readable JSON instead of human output")
	pf.BoolVarP(&rootFlagsValue.quiet, "quiet", "q", false, "suppress progress and informational output (on success, print only the output path)")
	pf.BoolVarP(&rootFlagsValue.verbose, "verbose", "v", false, "enable verbose (debug) logging on stderr")

	root.AddCommand(
		newInfoCmd(),
		newFormatsCmd(),
		newDownloadCmd(),
		newCutCmd(),
		newTranscodeCmd(),
		newNormalizeCmd(),
		newSponsorBlockCmd(),
		newCacheCmd(),
		newDoctorCmd(),
		newVersionCmd(),
		newExitCodesCmd(),
	)
	wrapUsageErrors(root)
	return root
}

// wrapUsageErrors makes every command's argument- and flag-parsing failures map
// to a usageError (exit code 2), and silences cobra's own error/usage printing so
// the central renderer in main reports each failure exactly once.
func wrapUsageErrors(cmd *cobra.Command) {
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		// Parsing stopped at the offending token, so anything after it (--json
		// included) never landed in rootFlagsValue.
		return unparsedFlagsError(err.Error())
	})
	if inner := cmd.Args; inner != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := inner(c, args); err != nil {
				return &usageError{msg: err.Error()}
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		wrapUsageErrors(sub)
	}
}
