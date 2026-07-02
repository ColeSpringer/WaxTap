package main

import (
	"strings"
	"time"

	"github.com/colespringer/waxtap/v2/sponsorblock"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Shared flag binders keep help text and behavior consistent across commands.
// Configuration, network, and player flags use FlagSet-owned storage because
// loadConfig reads them by name.

// bindConfigFlags registers flags used to locate configuration and the player
// cache. The cache command registers them as persistent flags so its subcommands
// inherit them.
func bindConfigFlags(f *pflag.FlagSet) {
	f.String("config", "", "path to a JSON config file (default: search the user config dir)")
	f.String("cache-dir", "", "on-disk player cache directory (default: user cache dir)")
	f.Bool("no-cache", false, "disable the on-disk player cache")
}

// bindNetworkFlags registers flags shared by commands that make YouTube or
// SponsorBlock requests.
func bindNetworkFlags(f *pflag.FlagSet) {
	f.String("proxy", "", "proxy URL for YouTube and SponsorBlock requests; sidecars bypass it")
	f.Bool("insecure", false, "skip TLS verification for YouTube and SponsorBlock requests (diagnostics only)")
	f.Float64("qps", 0, "per-host requests/sec cap (0 = unlimited)")
	f.Duration("cooldown", 0, "base host cooldown after a rate-limit response (0 = none)")
	f.String("hl", "", "InnerTube host language, e.g. en, de, ja (default: en)")
	f.String("gl", "", "content region hint, e.g. US, DE, JP (default: US)")
	f.String("sponsorblock-url", "", "override the SponsorBlock API base URL (default: public server)")
}

// bindPlayerExtractionFlags registers flags used to resolve and stage streams.
// SponsorBlock does not use them because it fetches segment metadata by video ID
// without resolving a player.
func bindPlayerExtractionFlags(f *pflag.FlagSet) {
	f.String("temp-dir", "", "directory for intermediate/staging files (default: OS temp)")
	f.String("profile-override", "", "path to a JSON client-profile override file (refresh client versions without a rebuild)")
	f.Int("chrome-major", 0, "Chrome major for built-in WEB clients (0 = built-in default; conflicts with --profile-override)")
	f.String("potoken-url", "", "base or full URL of a bgutil PO-token endpoint (enables WEB/GVS tokens; bypasses --proxy)")
	f.String("player-context-url", "", "base or full URL of an attested WEB player-context endpoint (requires --potoken-url on the same host; bypasses --proxy)")
	f.String("client", "", "force one built-in client: web|ios|android_vr|web_embedded (conflicts with --profile-override; --player-context-url is tried first; avoid ios for audio downloads)")
	f.String("session-url", "", "base or full URL of a session endpoint returning {visitor_data, cookies} (requires --client; bypasses --proxy)")
	f.String("visitor-data", "", "adopt this exact X-Goog-Visitor-Id literal and skip WaxTap's bootstrap (needs a uniform --client)")
	f.String("cookies", "", "Netscape cookie file to adopt alongside --visitor-data")
	f.String("api-key", "", "API key sent as X-API-Key to configured PO-token, player-context, and session sidecars (use HTTPS for remote sidecars)")
}

// bindSponsorBlockFlag registers --sponsorblock. When passed without a value, it
// selects the default music_offtopic category. The usage argument describes any
// command-specific behavior.
func bindSponsorBlockFlag(f *pflag.FlagSet, cats *string, usage string) {
	f.StringVar(cats, "sponsorblock", "", usage)
	if fl := f.Lookup("sponsorblock"); fl != nil {
		fl.NoOptDefVal = string(sponsorblock.CategoryMusicOffTopic)
	}
}

// sponsorblockArgs wraps base with a clearer error for a common pflag ambiguity.
// Since --sponsorblock accepts a bare form, a space-separated value is parsed as
// a positional argument. Specific categories must use the "=" form.
//
// hasOutputPositional is true for commands with an optional trailing output slot
// such as cut. Setting --out removes that slot.
func sponsorblockArgs(base cobra.PositionalArgs, hasOutputPositional bool) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		// After parsing, pflag cannot tell a bare flag from an explicit value equal
		// to NoOptDefVal. In either case, only impossible positional shapes trigger
		// the hint.
		if fl := cmd.Flags().Lookup("sponsorblock"); fl != nil &&
			cmd.Flags().Changed("sponsorblock") && fl.Value.String() == fl.NoOptDefVal {
			if arg, ok := sponsorblockMisparse(cmd, args, hasOutputPositional); ok {
				return usagef("--sponsorblock needs its value attached with '=' (e.g. --sponsorblock=%s); a space starts a new argument", arg)
			}
		}
		return base(cmd, args)
	}
}

// sponsorblockMisparse finds a positional argument that likely belongs to
// --sponsorblock. The flag accepts an optional value, so
// `--sponsorblock <cats> <url>` parses as a bare flag plus positionals. A
// comma-separated category list is treated as the missing value in any slot. A
// single category word is only reported when it is surplus and not in the
// target/input slot, because names like "sponsor" can also be real files.
func sponsorblockMisparse(cmd *cobra.Command, args []string, hasOutputPositional bool) (string, bool) {
	// Budget is the number of legitimate positionals: one target, plus an optional
	// output slot for cut unless --out supplies it.
	budget := 1
	if hasOutputPositional && !cmd.Flags().Changed("out") {
		budget = 2
	}
	surplus := len(args) > budget
	for i, a := range args {
		if !isCategoryList(a) {
			continue
		}
		switch {
		case surplus && (strings.Contains(a, ",") || i > 0):
			// Beyond the budget: a comma list, or a category word past the first
			// positional, is a misplaced value. A lone category word at args[0] may be
			// a category-named target/input, so leave it for arity.
			return a, true
		case !surplus && strings.Contains(a, ","):
			// In-budget comma lists get the same treatment; it is more helpful to
			// explain the flag form than to accept `sponsor,intro` as an output name.
			return a, true
		}
	}
	// Widened pass: a misspelled category (not a valid category word, so the loop
	// above skipped it) also strands the real target. Only for commands whose sole
	// positional is a video target; the category loop continues past every
	// non-category arg, so this must be a separate pass. Return the first surplus
	// arg that is not a YouTube target, leaving a genuine `<id> <id>` to arity.
	if surplus && !hasOutputPositional {
		for _, a := range args {
			if !looksLikeYouTubeTarget(a) {
				return a, true
			}
		}
	}
	return "", false
}

// isCategoryList reports whether s contains at least one valid SponsorBlock
// category and no invalid categories. Empty comma-separated parts are ignored to
// match parseCategories.
func isCategoryList(s string) bool {
	n := 0
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue // parseCategories skips empty parts; mirror it
		}
		if !sponsorblock.Category(p).Valid() {
			return false
		}
		n++
	}
	return n > 0
}

// bindCutFlags registers the time-range cut flags shared by download and cut.
func bindCutFlags(f *pflag.FlagSet, ranges *[]string, cutMode *string, crossfade *time.Duration, sbOnError *string) {
	f.StringArrayVar(ranges, "cut-range", nil, "remove a time range start-end (repeatable)")
	f.StringVar(cutMode, "cut-mode", "smart", "cut rendering: smart|copy|accurate")
	f.DurationVar(crossfade, "crossfade", 0, "crossfade duration at splice points (default off)")
	f.StringVar(sbOnError, "sponsorblock-on-error", "proceed", "on SponsorBlock fetch failure: proceed|fail")
}

// bindCollisionFlag registers --collision with the shared help text.
func bindCollisionFlag(f *pflag.FlagSet, collisionStr *string) {
	f.StringVar(collisionStr, "collision", "", "existing file behavior: fail|overwrite|auto-number|skip (default: fail)")
}

// bindBitrateFlag registers --bitrate with the shared help text. Copying and
// remuxing are not transcodes, so the text refers to lossy formats.
func bindBitrateFlag(f *pflag.FlagSet, bitrate *int) {
	f.IntVar(bitrate, "bitrate", 0, "target bitrate in bits per second for lossy formats (0 uses the preset default)")
}

// rejectEmptyFlags errors when any named string flag is explicitly set to an
// empty or whitespace-only value (usually an unset shell/env $VAR); omitting the
// flag uses the default. Unknown names are skipped, so it is safe to pass a flag
// a command lacks.
func rejectEmptyFlags(cmd *cobra.Command, names ...string) error {
	for _, name := range names {
		f := cmd.Flags().Lookup(name)
		if f != nil && f.Changed && strings.TrimSpace(f.Value.String()) == "" {
			return usagef("empty --%s path; omit it to use the default location", name)
		}
	}
	return nil
}
