package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
}

func newDownloadCmd() *cobra.Command {
	df := &downloadFlags{}
	cmd := &cobra.Command{
		Use:   "download <url>",
		Short: "Download audio from a video or playlist",
		Long: "Download the selected audio stream from a YouTube video, optionally\n" +
			"transcoding, cutting, removing SponsorBlock segments, and normalizing\n" +
			"loudness. A playlist URL is expanded and its entries are downloaded with\n" +
			"bounded parallelism.\n\n" +
			"Use --out for a single exact file, or --dir with --output-template to name\n" +
			"files automatically (the default).",
		Args: cobra.ExactArgs(1),
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
	f.StringVarP(&df.out, "out", "o", "", "output file path (single video only)")
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
	f.BoolVar(&df.listOnly, "list", false, "list playlist entries without downloading")

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
	// parameter follows the video path unless --list is set; playlist-only inputs
	// expand.
	_, idErr := youtube.ExtractVideoID(arg)
	_, plErr := youtube.ExtractPlaylistID(arg)
	isVideo := idErr == nil
	hasPlaylist := plErr == nil

	switch {
	case df.listOnly:
		if !hasPlaylist {
			return usagef("--list needs a playlist URL or ID")
		}
		return runPlaylistDownload(cmd.Context(), env, df, arg)
	case isVideo:
		if err := rejectChangedFlags(cmd, "is only used with a playlist input",
			"max-items", "concurrency", "max-downloads", "sleep-interval", "max-sleep-interval"); err != nil {
			return err
		}
		return runSingleDownload(cmd.Context(), env, df, arg)
	case hasPlaylist || errors.Is(idErr, waxtap.ErrIsPlaylist):
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
	if df.out != "" && df.dir != "" {
		return usagef("--out and --dir are mutually exclusive")
	}
	if df.out != "" && cmd.Flags().Changed("output-template") {
		return usagef("--output-template cannot be used with --out")
	}
	if cmd.Flags().Changed("bitrate") && df.format == "" {
		return usagef("--bitrate requires --format")
	}
	if cmd.Flags().Changed("loudness-target") && !df.normalize {
		return usagef("--loudness-target requires --normalize")
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
		pl, err := env.client.Enumerate(ctx, url, waxtap.EnumerateOptions{MaxItems: df.maxItems, Enrich: true})
		if err != nil {
			return err
		}
		return emitPlaylistList(env, pl)
	}

	out := &syncWriter{env: env}
	reserver := newPathReserver()
	res, runErr := env.client.DownloadPlaylist(ctx, url, waxtap.PlaylistDownloadOptions{
		MaxItems:         df.maxItems,
		Concurrency:      df.concurrency, // 0 => library uses config/default
		MaxDownloads:     df.maxDownloads,
		SleepInterval:    df.sleepInterval,
		MaxSleepInterval: df.maxSleepInterval,
		Resolve: func(rctx context.Context, e waxtap.PlaylistEntry) (waxtap.Request, string, error) {
			return resolveItem(rctx, env, df, reserver, e.VideoID, e.Title, e.Author, e.Index+1)
		},
		OnItem: func(o waxtap.PlaylistItemOutcome) {
			// Write sidecars and the archive before the result line.
			if o.Attempted && o.Err == nil && o.Result != nil {
				finishItem(env, df, o.Entry.VideoID, o.Result)
				warnChannelLayout(env, df, o.Result)
				measureNote(env, o.Result)
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
		total:          res.Enumerated,
		ok:             res.Downloaded,
		skipped:        res.Skipped,
		resolveFailed:  res.ResolveFailed,
		downloadFailed: res.DownloadFailed,
		remaining:      res.Remaining,
		enumErrors:     len(res.EnumErrors),
		capReached:     res.CapReached,
	})
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
func runSingleDownload(ctx context.Context, env *appEnv, df *downloadFlags, arg string) error {
	req, skipped, err := resolveItem(ctx, env, df, nil, arg, "", "", 0)
	if err != nil {
		return err
	}
	if skipped != "" {
		env.info("skipped (%s)\n", skipped)
		if env.jsonMode() {
			return env.emitJSON(struct {
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
		return err
	}

	id, _ := youtube.ExtractVideoID(arg) // already validated by resolveItem
	finishItem(env, df, id, res)
	if err := emitResult(env, res); err != nil {
		return err
	}
	warnChannelLayout(env, df, res)
	measureNote(env, res)
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

	target := df.out
	if target == "" {
		td, nerr := env.namingData(ctx, idOrURL, df, fallbackTitle, fallbackAuthor, index)
		if nerr != nil {
			return waxtap.Request{}, "", nerr
		}
		target = filepath.Join(df.dir, resolveOutputName(df.template, td))
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
	spec.Output = waxtap.ToFile(outPath)
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
	ranges, mode, pol, err := parseCutInputs(df.ranges, df.cutMode, df.sbOnError)
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

	taken := func(p string) bool { return r.claimed[p] || pathExists(p) }
	if !taken(path) {
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
