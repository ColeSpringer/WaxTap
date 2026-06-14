package main

import (
	"time"

	"github.com/spf13/cobra"
)

// rootFlags holds the persistent (global) flag values. It is package-level so the
// top-level error renderer in main can read the --json setting after Execute
// returns, before any appEnv exists.
type rootFlags struct {
	json             bool
	quiet            bool
	verbose          bool
	config           string
	cacheDir         string
	noCache          bool
	tempDir          string
	proxy            string
	insecure         bool
	qps              float64
	cooldown         time.Duration
	hl               string
	gl               string
	sponsorblockURL  string
	profileOverride  string
	chromeMajor      int
	potokenURL       string
	playerContextURL string
	client           string
	sessionURL       string
	visitorData      string
	cookies          string
	apiKey           string
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

	pf := root.PersistentFlags()
	pf.BoolVar(&rootFlagsValue.json, "json", false, "emit machine-readable JSON instead of human output")
	pf.BoolVarP(&rootFlagsValue.quiet, "quiet", "q", false, "suppress progress and informational output")
	pf.BoolVarP(&rootFlagsValue.verbose, "verbose", "v", false, "enable verbose (debug) logging on stderr")
	pf.StringVar(&rootFlagsValue.config, "config", "", "path to a JSON config file (default: search the user config dir)")
	pf.StringVar(&rootFlagsValue.cacheDir, "cache-dir", "", "on-disk player cache directory (default: user cache dir)")
	pf.BoolVar(&rootFlagsValue.noCache, "no-cache", false, "disable the on-disk player cache")
	pf.StringVar(&rootFlagsValue.tempDir, "temp-dir", "", "directory for intermediate/staging files (default: OS temp)")
	pf.StringVar(&rootFlagsValue.proxy, "proxy", "", "proxy URL for YouTube and SponsorBlock requests; sidecars bypass it")
	pf.BoolVar(&rootFlagsValue.insecure, "insecure", false, "skip TLS verification for YouTube and SponsorBlock requests (diagnostics only)")
	pf.Float64Var(&rootFlagsValue.qps, "qps", 0, "per-host requests/sec cap (0 = unlimited)")
	pf.DurationVar(&rootFlagsValue.cooldown, "cooldown", 0, "base host cooldown after a rate-limit response (0 = none)")
	pf.StringVar(&rootFlagsValue.hl, "hl", "", "InnerTube host language, e.g. en, de, ja (default: en)")
	pf.StringVar(&rootFlagsValue.gl, "gl", "", "content region hint, e.g. US, DE, JP (default: US)")
	pf.StringVar(&rootFlagsValue.sponsorblockURL, "sponsorblock-url", "", "override the SponsorBlock API base URL (default: public server)")
	pf.StringVar(&rootFlagsValue.profileOverride, "profile-override", "", "path to a JSON client-profile override file (refresh client versions without a rebuild)")
	pf.IntVar(&rootFlagsValue.chromeMajor, "chrome-major", 0, "Chrome major for built-in WEB clients (0 = built-in default; conflicts with --profile-override)")
	pf.StringVar(&rootFlagsValue.potokenURL, "potoken-url", "", "base or full URL of a bgutil PO-token endpoint (enables WEB/GVS tokens; bypasses --proxy)")
	pf.StringVar(&rootFlagsValue.playerContextURL, "player-context-url", "", "base or full URL of an attested WEB player-context endpoint (requires --potoken-url on the same host; bypasses --proxy)")
	pf.StringVar(&rootFlagsValue.client, "client", "", "force one built-in client: web|ios|android_vr|web_embedded (conflicts with --profile-override; --player-context-url is tried first; ios byte delivery is best-effort)")
	pf.StringVar(&rootFlagsValue.sessionURL, "session-url", "", "base or full URL of a session endpoint returning {visitor_data, cookies} (requires --client; bypasses --proxy)")
	pf.StringVar(&rootFlagsValue.visitorData, "visitor-data", "", "adopt this exact X-Goog-Visitor-Id literal and skip WaxTap's bootstrap (needs a uniform --client)")
	pf.StringVar(&rootFlagsValue.cookies, "cookies", "", "Netscape cookie file to adopt alongside --visitor-data")
	pf.StringVar(&rootFlagsValue.apiKey, "api-key", "", "API key sent as X-API-Key to configured PO-token, player-context, and session sidecars (use HTTPS for remote sidecars)")

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
