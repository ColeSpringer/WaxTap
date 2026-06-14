package main

import (
	"github.com/spf13/cobra"
)

// rootFlags holds values for the persistent output flags. The package-level
// value also lets main read --json after Execute returns, before an appEnv exists.
type rootFlags struct {
	json    bool
	quiet   bool
	verbose bool
}

var rootFlagsValue rootFlags

// newRootCmd builds the root command, its persistent flags, and the full
// subcommand set. cobra adds the `help` and `completion` commands on its own.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "waxtap",
		Short: "Audio-focused YouTube downloader and local-audio processor",
		Long: "WaxTap downloads the best available audio from YouTube (or processes a\n" +
			"local file) and can transcode, cut time ranges, remove SponsorBlock\n" +
			"segments, and measure or normalize loudness. Processing commands require\n" +
			"ffmpeg and ffprobe on PATH.\n\n" +
			"Every command supports --json for a stable, scriptable output contract.",
		// Keep --version in sync with the version subcommand, including
		// go-install builds that rely on module build info.
		Version: resolveVersion(),
		// Errors and usage are rendered once, centrally, in main; silence cobra's
		// own printing so failures are not reported twice.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	// Keep only output flags persistent. Other flags belong to the commands that
	// use them, so extraction flags follow the subcommand.
	pf := root.PersistentFlags()
	pf.BoolVar(&rootFlagsValue.json, "json", false, "emit machine-readable JSON instead of human output")
	pf.BoolVarP(&rootFlagsValue.quiet, "quiet", "q", false, "suppress progress and informational output")
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
		return &usageError{msg: err.Error()}
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
