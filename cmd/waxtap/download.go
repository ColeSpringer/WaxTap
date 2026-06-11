package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/youtube"
	"github.com/spf13/cobra"
)

// downloadFlags holds every download/process option. The cut/transcode/normalize
// commands reuse the spec-building helpers but expose narrower flag sets.
type downloadFlags struct {
	itag      int
	codec     string
	bestAudio bool

	out          string
	dir          string
	template     string
	collisionStr string
	skipExisting bool

	transcode string
	bitrate   int

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

	collision collisionMode // resolved in RunE
	archive   *downloadArchive
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
	f.BoolVar(&df.bestAudio, "best-audio", true, "select the best audio stream (default)")
	f.IntVar(&df.itag, "itag", 0, "select an exact itag instead of best audio")
	f.StringVar(&df.codec, "codec", "", "select the best stream matching a codec (e.g. opus, aac)")
	f.StringVarP(&df.out, "out", "o", "", "exact output file path (single video only)")
	f.StringVarP(&df.dir, "dir", "d", "", "output directory for templated filenames (default: .)")
	f.StringVar(&df.template, "output-template", defaultTemplate, "filename template ({title} {id} {author} {itag} {ext} {index})")
	f.StringVar(&df.collisionStr, "collision", "", "on existing file: fail|overwrite|auto-number|skip")
	f.BoolVar(&df.skipExisting, "skip-existing", false, "skip when the target file already exists")
	f.StringVar(&df.transcode, "transcode", "", "transcode to: copy|flac|alac|wav|mp3|aac|opus|vorbis")
	f.IntVar(&df.bitrate, "bitrate", 0, "target bitrate (bits/sec) for lossy transcodes (0 = preset default)")
	f.StringVar(&df.sbCats, "cut-sponsorblock", "", "remove SponsorBlock categories (comma-separated after =, for example --cut-sponsorblock=intro,outro; bare flag = music_offtopic)")
	f.StringArrayVar(&df.ranges, "cut-range", nil, "remove a time range start-end (repeatable)")
	f.StringVar(&df.cutMode, "cut-mode", "smart", "cut rendering: smart|copy|accurate")
	f.DurationVar(&df.crossfade, "crossfade", 0, "crossfade duration at splice points (default off)")
	f.StringVar(&df.sbOnError, "sponsorblock-onerror", "proceed", "on SponsorBlock fetch failure: proceed|fail")
	f.BoolVar(&df.normalize, "normalize", false, "normalize loudness to --loudness-target (fused into the encode)")
	f.BoolVar(&df.measure, "measure", false, "measure loudness without altering audio")
	f.Float64Var(&df.loudTarget, "loudness-target", -14, "target integrated loudness (LUFS) for --normalize")
	f.StringVar(&df.sourcePolicy, "source-policy", "minimize-loss", "source tradeoff: minimize-loss|best-native|prefer:<codec>")
	f.StringVar(&df.archivePath, "download-archive", "", "record fetched IDs to this file and skip them on future runs")
	f.BoolVar(&df.writeInfoJSON, "write-info-json", false, "write a <output>.info.json sidecar")
	f.IntVar(&df.maxItems, "max-items", 0, "cap playlist items (0 = all)")
	f.IntVar(&df.concurrency, "concurrency", 0, "parallel downloads for playlists (0 = config/default)")
	f.IntVar(&df.maxDownloads, "max-downloads", 0, "maximum download attempts per playlist run (0 = unlimited; skips do not count)")
	f.DurationVar(&df.sleepInterval, "sleep-interval", 0, "minimum delay before each playlist download after the first (e.g. 5s)")
	f.DurationVar(&df.maxSleepInterval, "max-sleep-interval", 0, "maximum randomized delay between playlist downloads (requires --sleep-interval)")
	f.BoolVar(&df.listOnly, "list", false, "list playlist entries without downloading")

	// Allow `--cut-sponsorblock` with no value to mean the default category.
	if fl := f.Lookup("cut-sponsorblock"); fl != nil {
		fl.NoOptDefVal = string(sponsorblock.CategoryMusicOffTopic)
	}
}

func runDownload(cmd *cobra.Command, df *downloadFlags, arg string) error {
	env, err := setup(cmd)
	if err != nil {
		return err
	}
	if err := df.resolve(cmd); err != nil {
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
		return runSingleDownload(cmd.Context(), env, df, arg)
	case hasPlaylist || errors.Is(idErr, waxtap.ErrIsPlaylist):
		return runPlaylistDownload(cmd.Context(), env, df, arg)
	default:
		return idErr
	}
}

// resolve validates flag combinations and computes the effective collision mode.
// It runs before any network work, so spec errors (bad ranges, an incompatible
// --normalize, etc.) fail fast instead of after a download.
func (df *downloadFlags) resolve(cmd *cobra.Command) error {
	if df.out != "" && df.dir != "" {
		return usagef("--out and --dir are mutually exclusive")
	}
	if df.out == "" && df.dir == "" {
		df.dir = "."
	}
	mode := collisionAutoNumber
	switch {
	case cmd.Flags().Changed("collision"):
		m, err := parseCollisionMode(df.collisionStr)
		if err != nil {
			return err
		}
		mode = m
	case df.skipExisting:
		mode = collisionSkip
	case df.out != "":
		mode = collisionFail // explicit single-file output is not auto-renamed
	}
	df.collision = mode

	// Validate playlist pacing before starting network work.
	if df.sleepInterval < 0 || df.maxSleepInterval < 0 {
		return usagef("--sleep-interval and --max-sleep-interval must be non-negative")
	}
	if df.maxDownloads < 0 {
		return usagef("--max-downloads must be non-negative")
	}
	if df.maxSleepInterval > 0 && df.sleepInterval == 0 {
		return usagef("--max-sleep-interval requires --sleep-interval")
	}
	if df.maxSleepInterval > 0 && df.maxSleepInterval < df.sleepInterval {
		return usagef("--max-sleep-interval must be >= --sleep-interval")
	}

	// Validate the processing spec up front (it is pure); buildRequest rebuilds it
	// per item later. This surfaces incompatible flag combinations before any
	// extraction or download.
	if _, err := df.buildProcessSpec(); err != nil {
		return err
	}
	return nil
}

// runPlaylistDownload lists or downloads a playlist.
func runPlaylistDownload(ctx context.Context, env *appEnv, df *downloadFlags, url string) error {
	if df.out != "" {
		return usagef("--out cannot name a whole playlist; use --dir")
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
	if runErr != nil {
		return runErr // the partial-run summary has already been written
	}
	return sumErr
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
	return emitResult(env, res)
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

// buildRequest assembles a Download request for url delivering to outPath.
func (df *downloadFlags) buildRequest(url, outPath string) (waxtap.Request, error) {
	sel, err := audioSelector(df.itag, df.codec)
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
	return waxtap.Request{URL: url, Audio: sel, SourcePolicy: policy, ProcessSpec: spec}, nil
}

// buildProcessSpec builds the shared ProcessSpec (transcode/cut/loudness) from
// the flags, without an Output.
func (df *downloadFlags) buildProcessSpec() (waxtap.ProcessSpec, error) {
	spec := waxtap.ProcessSpec{SkipIfExists: df.skipExisting}
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
		return spec, usagef("--normalize re-encodes; add --transcode <format> (e.g. flac), not copy")
	}
	spec.Loudness = loud
	return spec, nil
}

// transcodeFormat returns the requested format and whether one was set.
func (df *downloadFlags) transcodeFormat() (waxtap.TranscodeFormat, bool, error) {
	if df.transcode == "" {
		return 0, false, nil
	}
	f, err := parseTranscodeFormat(df.transcode)
	return f, err == nil, err
}

// buildCutSpec builds a CutSpec from explicit ranges and/or --cut-sponsorblock.
// sbSet reports whether the SponsorBlock flag was present (so the bare form, which
// yields the default category, still enables SponsorBlock).
func (df *downloadFlags) buildCutSpec() (*waxtap.CutSpec, error) {
	ranges, err := parseRanges(df.ranges)
	if err != nil {
		return nil, err
	}
	sbSet := df.sbCats != ""
	if len(ranges) == 0 && !sbSet {
		return nil, nil
	}
	mode, err := parseCutMode(df.cutMode)
	if err != nil {
		return nil, err
	}
	cs := &waxtap.CutSpec{Ranges: ranges, Mode: mode, Crossfade: df.crossfade}
	if sbSet {
		cats, err := parseCategories(df.sbCats)
		if err != nil {
			return nil, err
		}
		pol, err := parseSponsorErrorPolicy(df.sbOnError)
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
		return nil, usagef("--normalize and --measure are mutually exclusive")
	case df.normalize:
		return &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: df.loudTarget}, nil
	case df.measure:
		return &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly, Target: df.loudTarget}, nil
	default:
		return nil, nil
	}
}

// pathReserver hands out output paths for concurrent playlist downloads. It
// tracks paths already chosen in this run, not only paths already present on disk.
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
		return "", false, usagef("output file already exists: %s (use --collision to change behavior)", path)
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

	sel, err := audioSelector(df.itag, df.codec)
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
