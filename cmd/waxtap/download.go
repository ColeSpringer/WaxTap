package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/youtube"
	"github.com/spf13/cobra"
)

// downloadFlags holds every download/process option. The cut/transcode/normalize
// commands reuse the spec-building helpers but expose narrower flag sets.
type downloadFlags struct {
	itag  int
	codec string

	out          string
	dir          string
	template     string
	collisionStr string

	format  string
	bitrate int

	channels   string
	downmix    bool
	noFallback bool

	sbCats    string
	ranges    []string
	cutMode   string
	crossfade time.Duration
	sbOnError string

	normalize  bool
	measure    bool
	loudTarget float64

	sourcePolicy  string
	archivePath   string
	writeInfoJSON bool

	maxItems         int
	concurrency      int
	maxDownloads     int
	sleepInterval    time.Duration
	maxSleepInterval time.Duration
	listOnly         bool

	collision        collisionMode        // resolved in RunE
	layout           waxtap.ChannelLayout // resolved in RunE from --channels
	channelsExplicit bool                 // true when a flag or config value requested a layout
	archive          *downloadArchive
	streamW          io.Writer // stdout sink when --out is "-"; nil for a file sink
}

func newDownloadCmd() *cobra.Command {
	df := &downloadFlags{}
	cmd := &cobra.Command{
		Use:   "download <url>",
		Short: "Download audio from a video, playlist, or channel",
		Long: "Download the selected audio stream from a YouTube video, optionally\n" +
			"transcoding, cutting, removing SponsorBlock segments, and normalizing\n" +
			"loudness. A playlist URL, or a channel URL (which resolves to the\n" +
			"channel's uploads feed), is expanded and its entries are downloaded with\n" +
			"bounded parallelism.\n\n" +
			"Use --out for a single exact file, or --dir with --output-template to name\n" +
			"files automatically (the default).",
		Args: sponsorblockArgs(cobra.ExactArgs(1), false),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownload(cmd, df, args[0])
		},
	}
	bindDownloadFlags(cmd, df)
	return cmd
}

func bindDownloadFlags(cmd *cobra.Command, df *downloadFlags) {
	f := cmd.Flags()
	f.IntVar(&df.itag, "itag", 0, "select an exact itag")
	f.StringVar(&df.codec, "codec", "", "select the best source matching a codec (hard filter)")
	f.StringVarP(&df.out, "out", "o", "", "output file path, or - for stdout (single video; - is not atomic: a mid-stream failure can leave a truncated stream, and the error/exit code go to stderr)")
	f.StringVarP(&df.dir, "dir", "d", "", "output directory for templated filenames (default: .)")
	f.StringVar(&df.template, "output-template", defaultTemplate, "filename template ({title} {id} {author} {itag} {ext} {index})")
	bindCollisionFlag(f, &df.collisionStr)
	f.StringVarP(&df.format, "format", "f", "", "output format: copy|flac|alac|wav|mp3|aac|opus|vorbis")
	bindBitrateFlag(f, &df.bitrate)
	bindSourceSelectionFlags(f, &df.channels, &df.downmix, &df.noFallback)
	bindSponsorBlockFlag(f, &df.sbCats, "remove SponsorBlock categories (comma-separated; bare flag selects music_offtopic; use sponsorblock to preview)")
	bindCutFlags(f, &df.ranges, &df.cutMode, &df.crossfade, &df.sbOnError)
	f.BoolVar(&df.normalize, "normalize", false, "normalize loudness to --loudness-target (fused into the encode)")
	f.BoolVar(&df.measure, "measure-loudness", false, "measure loudness without altering the audio (still writes the downloaded file)")
	f.Float64Var(&df.loudTarget, "loudness-target", -14, "target integrated loudness (LUFS) for --normalize")
	f.StringVar(&df.sourcePolicy, "source-policy", "minimize-loss", "source policy: minimize-loss|best-native|prefer:<codec> (prefer:<codec> is a preference, not a filter)")
	f.StringVar(&df.archivePath, "download-archive", "", "record fetched IDs to this file and skip them on future runs")
	f.BoolVar(&df.writeInfoJSON, "write-info-json", false, "write a <output>.info.json sidecar")
	f.IntVar(&df.maxItems, "max-items", 0, "cap playlist items (0 = all)")
	f.IntVar(&df.concurrency, "concurrency", 0, "parallel playlist downloads (0 uses the default of 2)")
	f.IntVar(&df.maxDownloads, "max-downloads", 0, "maximum download attempts per playlist run (0 = unlimited; skips do not count)")
	f.DurationVar(&df.sleepInterval, "sleep-interval", 0, "minimum delay before each playlist download after the first (e.g. 5s)")
	f.DurationVar(&df.maxSleepInterval, "max-sleep-interval", 0, "maximum randomized delay between playlist downloads (requires --sleep-interval)")
	f.BoolVar(&df.listOnly, "list", false, "list playlist or channel entries without downloading")

	bindConfigFlags(f)
	bindNetworkFlags(f)
	bindPlayerExtractionFlags(f)
}

func runDownload(cmd *cobra.Command, df *downloadFlags, arg string) error {
	env, err := setup(cmd)
	if err != nil {
		return err
	}
	if err := df.resolve(cmd, env); err != nil {
		return err
	}
	if df.archivePath != "" {
		df.archive, err = openArchive(df.archivePath)
		if err != nil {
			return err
		}
	}

	// Classify the argument before doing network work. A watch URL with a list
	// parameter follows the video path unless --list is set; playlist-only and
	// channel inputs expand (a channel resolves to its uploads feed).
	_, idErr := youtube.ExtractVideoID(arg)
	_, plErr := youtube.ExtractPlaylistID(arg)
	_, chErr := youtube.ExtractChannelRef(arg)
	isVideo := idErr == nil
	hasPlaylist := plErr == nil
	isChannel := chErr == nil

	switch {
	case df.listOnly:
		if !hasPlaylist && !isChannel {
			return usagef("--list needs a playlist or channel URL or ID")
		}
		return runPlaylistDownload(cmd.Context(), env, df, arg)
	case isVideo:
		if err := rejectChangedFlags(cmd, "is only used with a playlist input",
			"max-items", "concurrency", "max-downloads", "sleep-interval", "max-sleep-interval"); err != nil {
			return err
		}
		// A watch?v=X&list=Y URL downloads only the video; note the dropped playlist.
		noteDroppedPlaylist(env, arg, "pass the playlist URL to download every item, or add --list to enumerate")
		return runSingleDownload(cmd.Context(), env, df, arg)
	case hasPlaylist || isChannel || errors.Is(idErr, waxtap.ErrIsPlaylist):
		return runPlaylistDownload(cmd.Context(), env, df, arg)
	default:
		return idErr
	}
}

// maxConcurrency bounds the CLI playlist worker pool. DownloadPlaylist applies
// the same limit for library callers.
const maxConcurrency = 64

// clampConcurrency validates the requested concurrency. It preserves zero so
// each caller can choose its own default. Values above maxConcurrency are
// clamped after a note is printed.
func clampConcurrency(env *appEnv, n int) (int, error) {
	if n < 0 {
		return 0, usagef("--concurrency must be non-negative")
	}
	if n > maxConcurrency {
		env.info("note: --concurrency %d exceeds the maximum of %d; clamping to %d\n", n, maxConcurrency, maxConcurrency)
		return maxConcurrency, nil
	}
	return n, nil
}

// resolve validates download flags and computes values used by every item. It
// runs before network work so invalid requests fail before a playlist starts.
func (df *downloadFlags) resolve(cmd *cobra.Command, env *appEnv) error {
	cfg := env.cfg
	if df.listOnly {
		if err := rejectChangedFlags(cmd, "cannot be used with --list",
			"itag", "codec", "out", "dir", "output-template", "collision",
			"format", "bitrate", "channels", "downmix", "cut-range", "sponsorblock",
			"cut-mode", "crossfade", "sponsorblock-on-error", "normalize", "measure-loudness",
			"loudness-target", "source-policy", "no-fallback", "download-archive",
			"write-info-json", "concurrency", "max-downloads", "sleep-interval", "max-sleep-interval"); err != nil {
			return err
		}
	}
	// An explicitly empty path flag (usually an unset shell/env $VAR) would
	// silently fall back to the default location; reject it before that default
	// is applied. --output-template has a non-empty default and validates an
	// empty string, so Changed distinguishes it too.
	if err := rejectEmptyFlags(cmd, "out", "dir", "output-template"); err != nil {
		return err
	}
	if err := validateItag(cmd, df.itag); err != nil {
		return err
	}
	if df.out != "" && df.dir != "" {
		return usagef("--out and --dir are mutually exclusive")
	}
	if df.out != "" && cmd.Flags().Changed("output-template") {
		return usagef("--output-template cannot be used with --out")
	}
	// Map -o - to the stdout writer sink. Reset first so a reused command struct
	// never carries a stale writer. A writer sink has no file path, so reject the
	// flags that need one.
	df.streamW = nil
	if df.out == "-" {
		if df.writeInfoJSON {
			return usagef("--write-info-json cannot be used with -o - (a writer sink has no file path to attach a sidecar to)")
		}
		if cmd.Flags().Changed("collision") {
			return usagef("--collision cannot be used with -o - (stdout is not a file path)")
		}
		// Refuse to flood a terminal with binary audio when the user forgot to
		// redirect; piping or a file redirect is required.
		if isTerminal(env.out) {
			return usagef("refusing to write audio to the terminal; redirect to a file (e.g. > track.opus) or pipe to another command (e.g. | ffprobe -)")
		}
		df.streamW = env.out
	}
	// Validate every template before network work; the default template passes here.
	if err := validateOutputTemplate(df.template); err != nil {
		return err
	}
	if cmd.Flags().Changed("bitrate") && df.format == "" {
		return usagef("--bitrate requires --format")
	}
	// Report this once per run, not once per playlist item.
	if tf, has, terr := df.transcodeFormat(); terr == nil && has {
		warnBitrateIgnoredIfLossless(env, tf, df.bitrate)
	}
	if cmd.Flags().Changed("loudness-target") && !df.normalize {
		return usagef("--loudness-target requires --normalize")
	}
	if err := rejectEmptySponsorBlock(cmd, df.sbCats); err != nil {
		return err
	}
	if cmd.Flags().Changed("sponsorblock-on-error") && df.sbCats == "" {
		return usagef("--sponsorblock-on-error requires --sponsorblock")
	}
	if (cmd.Flags().Changed("cut-mode") || cmd.Flags().Changed("crossfade")) && len(df.ranges) == 0 && df.sbCats == "" {
		return usagef("--cut-mode and --crossfade require --cut-range or --sponsorblock")
	}
	if df.out == "" && df.dir == "" {
		df.dir = "."
	}
	if err := rejectDirIsFile(df.dir); err != nil {
		return err
	}
	// Default to fail so an existing file is never silently overwritten or
	// renamed; an explicit --collision opts into the other behaviors.
	mode := collisionFail
	if cmd.Flags().Changed("collision") {
		m, err := parseCollisionMode(df.collisionStr)
		if err != nil {
			return err
		}
		mode = m
	}
	df.collision = mode

	layout, downmix, err := resolveChannels(cmd, cfg, df.channels, df.downmix)
	if err != nil {
		return err
	}
	df.layout, df.downmix = layout, downmix
	// Track whether the user requested a layout so the CLI can report a mismatch
	// after delivery.
	df.channelsExplicit = cmd.Flags().Changed("channels") || cfg.channels != ""

	// Validate playlist pacing before starting network work.
	if df.sleepInterval < 0 || df.maxSleepInterval < 0 {
		return usagef("--sleep-interval and --max-sleep-interval must be non-negative")
	}
	if df.maxItems < 0 {
		return usagef("--max-items must be non-negative")
	}
	if df.maxDownloads < 0 {
		return usagef("--max-downloads must be non-negative")
	}
	// Preserve zero so the library applies the playlist default.
	n, err := clampConcurrency(env, df.concurrency)
	if err != nil {
		return err
	}
	df.concurrency = n
	if df.maxSleepInterval > 0 && df.sleepInterval == 0 {
		return usagef("--max-sleep-interval requires --sleep-interval")
	}
	if df.maxSleepInterval > 0 && df.maxSleepInterval < df.sleepInterval {
		return usagef("--max-sleep-interval must be >= --sleep-interval")
	}

	// Validate processing options before extraction or playlist enumeration.
	// buildProcessSpec handles CLI constraints; ValidateProcessSpec enforces the
	// library-level invariants.
	spec, err := df.buildProcessSpec()
	if err != nil {
		return err
	}
	if err := waxtap.ValidateProcessSpec(spec); err != nil {
		return err
	}
	return nil
}

// runPlaylistDownload lists or downloads a playlist.
func runPlaylistDownload(ctx context.Context, env *appEnv, df *downloadFlags, url string) error {
	if df.out != "" {
		return usagef("--out cannot be used with a playlist; use --dir")
	}
	if df.listOnly {
		opts := waxtap.EnumerateOptions{MaxItems: df.maxItems, Enrich: true}
		// Listing performs enumeration and enrichment before printing any rows. On
		// an interactive stderr, show a transient status line for those network
		// phases. JSON, quiet, piped stderr, and verbose mode stay untouched.
		var hb *listHeartbeat
		if isTerminal(env.errOut) && !env.jsonMode() && !env.quiet() && !env.cfg.verbose {
			hb = &listHeartbeat{w: env.errOut}
			opts.OnProgress = func(items int) {
				hb.write(fmt.Sprintf("enumerating... %d items", items))
			}
			opts.OnEnrichProgress = func(done, total int) {
				hb.write(fmt.Sprintf("enriching... %d/%d items", done, total))
			}
		}
		pl, err := env.client.Enumerate(ctx, url, opts)
		hb.finish() // safe when hb is nil; ends the transient line before output/error
		if err != nil {
			return err
		}
		return emitPlaylistList(env, pl)
	}

	out := &syncWriter{env: env}
	reserver := newPathReserver()
	// OnItem may run concurrently, so track the actionable-WEB signal atomically and
	// emit the nudge once after the run instead of per item.
	var actionableWeb atomic.Bool
	res, runErr := env.client.DownloadPlaylist(ctx, url, waxtap.PlaylistDownloadOptions{
		MaxItems:         df.maxItems,
		Concurrency:      df.concurrency, // 0 => library uses config/default
		MaxDownloads:     df.maxDownloads,
		SleepInterval:    df.sleepInterval,
		MaxSleepInterval: df.maxSleepInterval,
		BuildRequest: func(rctx context.Context, e waxtap.PlaylistEntry) (waxtap.Request, string, error) {
			return resolveItem(rctx, env, df, reserver, e.VideoID, e.Title, e.Author, e.Index+1)
		},
		OnItem: func(o waxtap.PlaylistItemOutcome) {
			// Write sidecars and the archive before the result line.
			if o.Attempted && o.Err == nil && o.Result != nil {
				finishItem(env, df, o.Entry.VideoID, o.Result)
				warnChannelLayout(env, df, o.Result)
				warnContainerExtMismatch(env, df, o.Result)
				measureNote(env, o.Result)
			}
			if webOutcomeActionable(o.Result, o.Err) {
				actionableWeb.Store(true)
			}
			out.emitItem(o.Entry, o.Result, o.SkipReason, o.Err)
		},
	})
	if res == nil {
		return runErr
	}

	// Enumeration may return a partial listing with item errors.
	for _, perr := range res.EnumErrors {
		env.info("warning: playlist enumeration: %v\n", perr)
	}
	sumErr := out.emitSummary(playlistSummary{
		total:              res.Enumerated,
		ok:                 res.Downloaded,
		skipped:            res.Skipped,
		buildRequestFailed: res.BuildRequestFailed,
		downloadFailed:     res.DownloadFailed,
		remaining:          res.Remaining,
		enumErrors:         len(res.EnumErrors),
		capReached:         res.CapReached,
	})
	// Emit the WEB-sources nudge once if any item capped, fell back, or failed on a
	// WEB path. It goes to stderr, so it is safe in JSON mode too.
	if actionableWeb.Load() {
		noteUseBothWebSources(env)
	}
	// JSON mode has already written the item records and summary. Preserve the
	// failure exit code without appending another JSON document.
	err := sumErr
	if runErr != nil {
		err = runErr // the partial-run summary has already been written
	}
	if env.jsonMode() {
		return alreadyRendered(err)
	}
	return err
}

// runSingleDownload downloads one video with a live progress bar.
func runSingleDownload(ctx context.Context, env *appEnv, df *downloadFlags, arg string) (err error) {
	// When streaming to stdout, every status line and JSON document must go to
	// stderr so stdout carries only audio bytes. rep is a copy with out redirected,
	// and one deferred seam keeps any returned error off stdout (the audio sink):
	// render it to stderr and tell main not to write an error document there. This
	// covers every return below, including pre-stream and emit failures.
	rep := env
	if df.streamW != nil {
		se := *env
		se.out = env.errOut
		rep = &se
		defer func() {
			if err == nil {
				return
			}
			if _, already := errors.AsType[*alreadyRenderedError](err); !already {
				renderError(env.errOut, env.jsonMode(), err)
			}
			err = alreadyRendered(err)
		}()
	}

	req, skipped, err := resolveItem(ctx, env, df, nil, arg, "", "", 0)
	if err != nil {
		return err
	}
	if skipped != "" {
		rep.info("skipped (%s)\n", skipped)
		if rep.jsonMode() {
			return rep.emitJSON(struct {
				SchemaVersion int    `json:"schemaVersion"`
				Skipped       string `json:"skipped"`
			}{schemaVersion, skipped})
		}
		return nil
	}

	prog := env.newProgress()
	req.Events = prog.handle
	res, err := env.client.Download(ctx, req)
	prog.finish()
	if err != nil {
		noteForcedIOSIncomplete(env, err)
		noteUseBothWebSourcesIfActionable(env, res, err) // res is nil here; err-gated
		return err
	}

	id, _ := youtube.ExtractVideoID(arg) // already validated by resolveItem
	finishItem(env, df, id, res)
	if err := emitResult(rep, res); err != nil {
		return err
	}
	warnChannelLayout(env, df, res)
	warnContainerExtMismatch(env, df, res)
	measureNote(env, res)
	noteUseBothWebSourcesIfActionable(env, res, nil)
	return nil
}

// resolveItem builds a request for one item, or returns a skip reason. Playlist
// callers pass a reserver and fallback metadata; single-video callers pass nil
// and empty fallbacks.
func resolveItem(ctx context.Context, env *appEnv, df *downloadFlags, reserve *pathReserver, idOrURL, fallbackTitle, fallbackAuthor string, index int) (waxtap.Request, string, error) {
	id, err := youtube.ExtractVideoID(idOrURL)
	if err != nil {
		return waxtap.Request{}, "", err
	}
	if df.archive != nil && df.archive.Has(id) {
		return waxtap.Request{}, "archive", nil
	}

	// Streaming to stdout has no file path: skip naming and collision handling and
	// pass "-" through (buildRequest routes to the writer sink).
	if df.streamW != nil {
		req, err := df.buildRequest(idOrURL, "-")
		if err != nil {
			return waxtap.Request{}, "", err
		}
		return req, "", nil
	}

	target := df.out
	if target == "" {
		td, nerr := env.namingData(ctx, idOrURL, df, fallbackTitle, fallbackAuthor, index)
		if nerr != nil {
			return waxtap.Request{}, "", nerr
		}
		target = filepath.Join(df.dir, resolveOutputName(df.template, td))
		if err := ensureUnderDir(df.dir, target); err != nil {
			return waxtap.Request{}, "", err
		}
	}
	resolved, skip, err := reserve.reserveOr(target, df.collision)
	if err != nil {
		return waxtap.Request{}, "", err
	}
	if skip {
		return waxtap.Request{}, "exists", nil
	}
	if tf, has, terr := df.transcodeFormat(); terr == nil && has {
		warnALACToAlacExt(env, resolved, tf)
	}

	req, err := df.buildRequest(idOrURL, resolved)
	if err != nil {
		return waxtap.Request{}, "", err
	}
	return req, "", nil
}

// finishItem writes the optional sidecar and archive entry after a successful
// download. Errors are reported as warnings.
func finishItem(env *appEnv, df *downloadFlags, id string, res *waxtap.Result) {
	if df.writeInfoJSON && res.OutputPath != "" {
		if werr := writeInfoSidecar(res.OutputPath, res); werr != nil {
			env.info("warning: could not write info sidecar: %v\n", werr)
		}
	}
	if df.archive != nil {
		if aerr := df.archive.Add(id); aerr != nil {
			env.info("warning: could not update download archive: %v\n", aerr)
		}
	}
}

// warnChannelLayout reports when the delivered audio does not satisfy an
// explicitly requested layout. Exact itag selections, LayoutAny, and unknown
// channel counts do not produce a warning.
func warnChannelLayout(env *appEnv, df *downloadFlags, res *waxtap.Result) {
	if !df.channelsExplicit || df.itag > 0 || df.layout == waxtap.LayoutAny {
		return
	}
	delivered := res.OutputFormat.Channels
	if delivered <= 0 {
		// Transcoded results do not record an output channel count. Use the source
		// count, adjusted for an applied downmix.
		delivered = res.SourceFormat.Channels
		if target := df.layout.ChannelCount(); df.downmix && target > 0 && delivered > target {
			delivered = target
		}
	}
	if delivered <= 0 || df.layout.Matches(delivered) {
		return
	}
	env.info("note: requested %s; delivered %s\n", df.layout, channelCountLabel(delivered))
}

// warnContainerExtMismatch reports when a keep-source download uses an output
// extension that names a different container. Operations that run ffmpeg remux
// the stream into the requested container and cannot produce this mismatch.
func warnContainerExtMismatch(env *appEnv, df *downloadFlags, res *waxtap.Result) {
	if _, has, _ := df.transcodeFormat(); has {
		return // a --format (copy/transcode) muxes to the named container
	}
	// Only keep-source delivery writes the stream bytes without remuxing.
	if res.Transcoded || res.CutApplied || res.SponsorBlockApplied || res.LoudnessApplied {
		return
	}
	if res.OutputPath == "" || res.SourceFormat.Extension == "" {
		return
	}
	outExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(res.OutputPath), "."))
	srcExt := strings.ToLower(res.SourceFormat.Extension)
	if outExt == "" || sameContainer(outExt, srcExt) {
		return
	}
	env.info("note: output path uses .%s, but the source container is .%s; bytes were not re-encoded (rename to .%s or pass --format to convert)\n", outExt, srcExt, srcExt)
}

// sameContainer reports whether two file extensions name the same media
// container. Both .m4a and .mp4 use the ISO base media file format.
func sameContainer(a, b string) bool {
	if a == b {
		return true
	}
	mp4 := func(e string) bool { return e == "m4a" || e == "mp4" }
	return mp4(a) && mp4(b)
}

// channelCountLabel names mono and stereo and renders other layouts as a channel
// count.
func channelCountLabel(ch int) string {
	switch ch {
	case 1:
		return "mono (1ch)"
	case 2:
		return "stereo (2ch)"
	default:
		return fmt.Sprintf("%dch", ch)
	}
}

// measureNote reports the output path for a measure-only run. Processing
// operations suppress the note because the output is no longer an unaltered copy.
func measureNote(env *appEnv, res *waxtap.Result) {
	if res.LoudnessMeasured && !res.Transcoded && !res.CutApplied && !res.LoudnessApplied && res.OutputPath != "" {
		env.info("note: wrote unaltered copy to %s\n", res.OutputPath)
	}
}

// noteUseBothWebSourcesIfActionable prints the "supply both WEB sources" note only
// when the run hit a WEB cap, fallback, or failure a second source could have
// helped. A clean WEB context delivery stays silent.
func noteUseBothWebSourcesIfActionable(env *appEnv, res *waxtap.Result, err error) {
	if msg, ok := webSourcesNote(env.cfg); ok && webOutcomeActionable(res, err) {
		env.info("%s\n", msg)
	}
}

// webOutcomeActionable reports whether a download outcome shows a WEB-specific
// cap, fallback, or failure a second source could address. A failed Download
// returns a nil Result, so the err checks carry that case; a clean success (nil
// err, no WEB warning) returns false.
//
// The fallback signal is WarnWebContextFallback, not a "client != WEB_CONTEXT"
// check: that warning fires only when a configured WEB context did not deliver, so
// the default chain settling on ANDROID_VR (the client that works out of the box)
// on a partial-WEB config no longer trips the note on every success. Plain
// IO/disk/network errors are not WEB-relevant.
func webOutcomeActionable(res *waxtap.Result, err error) bool {
	if res != nil {
		for _, w := range res.Warnings {
			if w.Code == waxtap.WarnWebContextRetry || w.Code == waxtap.WarnWebContextFallback {
				return true
			}
		}
	}
	return errors.Is(err, waxtap.ErrIncompleteStream) ||
		errors.Is(err, waxtap.ErrExtractionFailed) ||
		errors.Is(err, waxtap.ErrNeedsPOToken) ||
		isProviderError(err)
}

// buildRequest assembles a Download request for url delivering to outPath.
func (df *downloadFlags) buildRequest(url, outPath string) (waxtap.Request, error) {
	sel, err := audioSelector(df.itag, df.codec, df.layout)
	if err != nil {
		return waxtap.Request{}, err
	}
	policy, err := parseSourcePolicy(df.sourcePolicy)
	if err != nil {
		return waxtap.Request{}, err
	}
	spec, err := df.buildProcessSpec()
	if err != nil {
		return waxtap.Request{}, err
	}
	if df.streamW != nil {
		spec.Output = waxtap.ToWriter(df.streamW)
	} else {
		spec.Output = waxtap.ToFile(outPath)
	}
	return waxtap.Request{URL: url, Audio: sel, SourcePolicy: policy, NoFallback: df.noFallback, ProcessSpec: spec}, nil
}

// buildProcessSpec builds the shared ProcessSpec (transcode/cut/loudness) from
// the flags, without an Output.
func (df *downloadFlags) buildProcessSpec() (waxtap.ProcessSpec, error) {
	spec := waxtap.ProcessSpec{Channels: df.layout, Downmix: df.downmix, IncludeMetadata: df.writeInfoJSON}
	tf, hasTranscode, err := df.transcodeFormat()
	if err != nil {
		return spec, err
	}
	if hasTranscode {
		spec.Transcode = &waxtap.TranscodeSpec{Format: tf, Bitrate: df.bitrate}
	}
	cut, err := df.buildCutSpec()
	if err != nil {
		return spec, err
	}
	spec.Cut = cut
	loud, err := df.buildLoudnessSpec()
	if err != nil {
		return spec, err
	}
	// Loudness normalization is a filter, so it needs a real encode target.
	if loud != nil && loud.Mode == waxtap.LoudnessApply && (!hasTranscode || tf == waxtap.FormatCopy) {
		return spec, usagef("--normalize re-encodes; add --format <format> (e.g. flac), not copy")
	}
	spec.Loudness = loud
	return spec, nil
}

// transcodeFormat returns the requested format and whether one was set.
func (df *downloadFlags) transcodeFormat() (waxtap.TranscodeFormat, bool, error) {
	if df.format == "" {
		return 0, false, nil
	}
	f, err := parseTranscodeFormat(df.format)
	return f, err == nil, err
}

// buildCutSpec builds a CutSpec from explicit ranges and/or --sponsorblock.
// sbSet reports whether the SponsorBlock flag was present (so the bare form, which
// yields the default category, still enables SponsorBlock).
func (df *downloadFlags) buildCutSpec() (*waxtap.CutSpec, error) {
	// Parse every shared cut flag before extraction, even when there is nothing to
	// cut.
	ranges, mode, pol, err := parseCutInputs(df.ranges, df.cutMode, df.sbOnError, df.crossfade)
	if err != nil {
		return nil, err
	}
	sbSet := df.sbCats != ""
	if len(ranges) == 0 && !sbSet {
		return nil, nil
	}
	cs := &waxtap.CutSpec{Ranges: ranges, Mode: mode, Crossfade: df.crossfade}
	if sbSet {
		cats, err := parseCategories(df.sbCats)
		if err != nil {
			return nil, err
		}
		cs.SponsorBlock = cats
		cs.OnError = pol
	}
	return cs, nil
}

func (df *downloadFlags) buildLoudnessSpec() (*waxtap.LoudnessSpec, error) {
	switch {
	case df.normalize && df.measure:
		return nil, usagef("--normalize and --measure-loudness are mutually exclusive")
	case df.normalize:
		return &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: df.loudTarget}, nil
	case df.measure:
		// Carry the target so a measure-only JSON result does not report zero.
		return &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly, Target: df.loudTarget}, nil
	default:
		return nil, nil
	}
}

// pathReserver hands out output paths for concurrent playlist downloads. It
// tracks paths already chosen in this run as well as paths present on disk.
type pathReserver struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func newPathReserver() *pathReserver { return &pathReserver{claimed: map[string]bool{}} }

// reserveOr resolves a collision and claims the chosen path. A nil reserver (the
// single-video path, which has no concurrency) falls back to plain
// resolveCollision.
func (r *pathReserver) reserveOr(path string, mode collisionMode) (string, bool, error) {
	if r == nil {
		return resolveCollision(path, mode)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Use one stat for both the directory guard and the first collision check,
	// matching resolveCollision's handling of the candidate path.
	exists, isDir := statOutputPath(path)
	if isDir {
		return "", false, dirOutputError(path)
	}
	taken := func(p string) bool { return r.claimed[p] || pathExists(p) }
	if !r.claimed[path] && !exists {
		r.claimed[path] = true
		return path, false, nil
	}
	switch mode {
	case collisionOverwrite:
		r.claimed[path] = true
		return path, false, nil
	case collisionSkip:
		return path, true, nil
	case collisionAutoNumber:
		next := nextAvailableFunc(path, taken)
		r.claimed[next] = true
		return next, false, nil
	default: // collisionFail
		return "", false, usagef("output file already exists: %s (set --collision to auto-number, overwrite, or skip)", path)
	}
}

// namingData gathers the template fields for a download. It fetches InfoBasic only
// when needed: when the title is unknown (single video), the output extension
// depends on the source (a copy/keep download), or the template references a
// lookup-only field such as {itag}.
func (env *appEnv) namingData(ctx context.Context, url string, df *downloadFlags, title, author string, index int) (templateData, error) {
	id, err := youtube.ExtractVideoID(url)
	if err != nil {
		return templateData{}, err
	}
	td := templateData{ID: id, Title: title, Author: author, Index: index, Ext: "webm"}

	tf, hasTranscode, terr := df.transcodeFormat()
	if terr != nil {
		return td, terr
	}
	extKnown := hasTranscode && tf != waxtap.FormatCopy
	if extKnown {
		td.Ext = transcodeExt(tf)
	}

	// {itag} comes from the Info lookup below.
	needItag := strings.Contains(df.template, "{itag}")
	if td.Title != "" && extKnown && !needItag {
		return td, nil // all requested template fields are already known
	}

	sel, err := audioSelector(df.itag, df.codec, df.layout)
	if err != nil {
		return td, err
	}
	policy, err := parseSourcePolicy(df.sourcePolicy)
	if err != nil {
		return td, err
	}
	video, err := env.client.Info(ctx, url, waxtap.InfoBasic)
	if err != nil {
		return td, err
	}
	if td.Title == "" {
		td.Title = video.Title
	}
	if td.Author == "" {
		td.Author = video.Author
	}
	if idx, serr := sel.Select(video.Formats, policy, waxtap.Target{}); serr == nil {
		td.Itag = video.Formats[idx].Itag
		if !extKnown {
			if e := video.Formats[idx].Extension; e != "" {
				td.Ext = e
			}
		}
	}
	return td, nil
}
