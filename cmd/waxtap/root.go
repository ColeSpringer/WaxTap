package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rootFlags holds the values of the persistent output flags, read back from the
// command that ran.
type rootFlags struct {
	json    bool
	quiet   bool
	verbose bool
}

// outputFlags reads the persistent output flags off cmd's root. pflag shares one
// *Flag between the root's persistent set and a subcommand's merged set, so the
// parsed value is there whichever command ran, and main can read it after
// Execute returns, before an appEnv exists. A command built without a root, which
// is how tests reach one subcommand, has no such flags and gets the defaults:
// its output mode is its own, not whatever the last root run asked for.
func outputFlags(cmd *cobra.Command) rootFlags {
	pf := cmd.Root().PersistentFlags()
	set := func(name string) bool {
		v, err := pf.GetBool(name)
		return err == nil && v
	}
	return rootFlags{json: set("json"), quiet: set("quiet"), verbose: set("verbose")}
}

// runNotes holds the current run's note collector so report can attach notes to
// an error envelope, which is rendered from main after the appEnv is gone. It is
// process state rather than per-run state: newRootCmd resets it and every appEnv
// replaces it, with a mutex because playlist items collect concurrently. Two runs
// in one process share it, which only tests do.
var (
	runNotesMu sync.Mutex
	runNotes   *noteCollector
)

func setRunNotes(c *noteCollector) {
	runNotesMu.Lock()
	defer runNotesMu.Unlock()
	runNotes = c
}

// runSignal is the signal context main installs, so any renderer can ask
// whether a signal fired. A per-item record classifies its own error with no
// access to main's context, and a cancellation is the one class whose meaning
// depends on that: one a signal produced is an interrupt, one WaxTap made
// itself is a defect.
//
// Process state for the same reason runNotes is, and read through a function
// so the nil case (a test driving a command directly) answers "no signal".
var (
	runSignalMu sync.Mutex
	runSignal   context.Context
)

func setRunSignal(ctx context.Context) {
	runSignalMu.Lock()
	defer runSignalMu.Unlock()
	runSignal = ctx
}

// signalFired reports that the run's signal context is done, which only a
// SIGINT or SIGTERM can do.
func signalFired() bool {
	runSignalMu.Lock()
	ctx := runSignal
	runSignalMu.Unlock()
	return ctx != nil && ctx.Err() != nil
}

// currentRunNotes returns the notes collected by this run, or nil.
func currentRunNotes() []noteJSON {
	runNotesMu.Lock()
	c := runNotes
	runNotesMu.Unlock()
	if c == nil {
		return nil
	}
	return c.snapshot()
}

// jsonRequested reports whether --json survives a parse of args. Flag parsing
// aborts at the first bad flag, so a --json after one never reaches the flag set,
// and the error still has to honor the contract. An allowlisted parse gives real
// parser semantics: -- terminates, --json=false wins, and an unknown flag's value
// is stripped without swallowing a following --json.
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
	// A fresh command is a fresh run: drop any collector left by an earlier one so
	// its notes cannot end up on this run's error envelope. Tests build root
	// commands in the same process, which is where that would show.
	setRunNotes(nil)
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
	// The parsed values are read back through outputFlags, so these need no
	// binding of their own; a bound variable would be state outliving the command.
	pf := root.PersistentFlags()
	pf.Bool("json", false, "emit machine-readable JSON instead of human output")
	pf.BoolP("quiet", "q", false, "suppress progress and informational output (on success, print only the output path)")
	pf.BoolP("verbose", "v", false, "enable verbose (debug) logging on stderr")

	root.AddCommand(
		newInfoCmd(),
		newFormatsCmd(),
		newDownloadCmd(),
		newCutCmd(),
		newTranscodeCmd(),
		newNormalizeCmd(),
		newSplitCmd(),
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
		// included) never landed in the flag set.
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
