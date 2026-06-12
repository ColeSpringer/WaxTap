package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/youtube"
	"github.com/spf13/cobra"
)

// isLocalFile reports whether arg names an existing regular file (so a process
// command treats it as a local input rather than a URL).
func isLocalFile(arg string) bool {
	fi, err := os.Stat(arg)
	return err == nil && !fi.IsDir()
}

// dispatchProcess runs a ProcessSpec against a local file or a YouTube URL and
// returns the Result. A live progress reporter is attached for the duration.
// noFallback applies only to URL sources.
func dispatchProcess(ctx context.Context, env *appEnv, source string, sel waxtap.AudioSelector, policy waxtap.SourcePolicy, spec waxtap.ProcessSpec, noFallback bool) (*waxtap.Result, error) {
	prog := env.newProgress()
	spec.Events = prog.handle
	defer prog.finish()

	if isLocalFile(source) {
		return env.client.Process(ctx, waxtap.ProcessRequest{Input: source, ProcessSpec: spec})
	}
	if _, err := youtube.ExtractVideoID(source); err != nil {
		// Report both accepted input forms because this is usually a mistyped
		// local path, not an intended video ID.
		if errors.Is(err, waxtap.ErrInvalidVideoID) || errors.Is(err, waxtap.ErrVideoIDTooShort) {
			return nil, usagef("no such file and not a valid YouTube URL or ID: %s", source)
		}
		return nil, err
	}
	return env.client.Download(ctx, waxtap.Request{URL: source, Audio: sel, SourcePolicy: policy, NoFallback: noFallback, ProcessSpec: spec})
}

// resolveProcessOutput resolves the output path for a single-file process command
// and applies the collision mode. explicit is the positional/--out path (may be
// empty). When empty and the source is local, the name is derived from the input;
// a URL source requires an explicit output.
func resolveProcessOutput(source, explicit, newExt, tag string, mode collisionMode) (path string, skip bool, err error) {
	if explicit == "" {
		if !isLocalFile(source) {
			return "", false, usagef("provide an output path (positional or --out) for a URL source")
		}
		explicit = deriveLocalOutput(source, newExt, tag)
	}
	return resolveCollision(explicit, mode)
}

// deriveLocalOutput builds an output path beside a local input: the input stem
// with a new extension (when newExt is set) and a tag suffix when the result
// would otherwise collide with the input.
func deriveLocalOutput(input, newExt, tag string) string {
	dir := filepath.Dir(input)
	ext := filepath.Ext(input)
	stem := strings.TrimSuffix(filepath.Base(input), ext)
	if newExt != "" {
		ext = "." + newExt
	}
	out := filepath.Join(dir, stem+ext)
	if sameLocalPath(out, input) {
		out = filepath.Join(dir, stem+" ("+tag+")"+ext)
	}
	return out
}

func sameLocalPath(a, b string) bool {
	pa, e1 := filepath.Abs(a)
	pb, e2 := filepath.Abs(b)
	return e1 == nil && e2 == nil && pa == pb
}

// warnALACToAlacExt warns when an ALAC stream will use an MP4 container despite
// the output path ending in ".alac".
func warnALACToAlacExt(env *appEnv, outPath string, tf waxtap.TranscodeFormat) {
	if tf == waxtap.FormatALAC && strings.EqualFold(filepath.Ext(outPath), ".alac") {
		env.info("note: .alac output uses an MP4 container; use .m4a for the conventional filename\n")
	}
}

func newCutCmd() *cobra.Command {
	var (
		out          string
		ranges       []string
		sbCats       string
		cutMode      string
		crossfade    time.Duration
		sbOnError    string
		transcode    string
		bitrate      int
		itag         int
		codec        string
		channels     string
		downmix      bool
		noFallback   bool
		sourcePolicy string
		collisionStr string
	)
	cmd := &cobra.Command{
		Use:   "cut <input> [output]",
		Short: "Remove time ranges and/or SponsorBlock segments",
		Long: "Cut time ranges out of a local audio file or a YouTube video. Provide one\n" +
			"or more --cut-range, and/or --cut-sponsorblock (YouTube only). Smart mode\n" +
			"stream-copies when cutting alone and fuses the cut into a transcode when\n" +
			"one is requested.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			source := args[0]
			explicit := out
			if len(args) == 2 {
				if out != "" {
					return usagef("give the output once (positional or --out, not both)")
				}
				explicit = args[1]
			}

			rangeList, err := parseRanges(ranges)
			if err != nil {
				return err
			}
			sbSet := sbCats != ""
			if len(rangeList) == 0 && !sbSet {
				return usagef("nothing to cut: pass --cut-range and/or --cut-sponsorblock")
			}
			if sbSet && isLocalFile(source) {
				return usagef("--cut-sponsorblock needs a YouTube source (no video ID for a local file)")
			}

			mode, err := parseCutMode(cutMode)
			if err != nil {
				return err
			}
			layout, doDownmix, err := resolveChannels(cmd, env.cfg, channels, downmix)
			if err != nil {
				return err
			}
			cs := &waxtap.CutSpec{Ranges: rangeList, Mode: mode, Crossfade: crossfade}
			if sbSet {
				cats, err := parseCategories(sbCats)
				if err != nil {
					return err
				}
				pol, err := parseSponsorErrorPolicy(sbOnError)
				if err != nil {
					return err
				}
				cs.SponsorBlock, cs.OnError = cats, pol
			}

			spec := waxtap.ProcessSpec{Cut: cs, Channels: layout, Downmix: doDownmix}
			newExt := ""
			var tf waxtap.TranscodeFormat // FormatCopy when no transcode is requested
			if transcode != "" {
				var terr error
				if tf, terr = parseTranscodeFormat(transcode); terr != nil {
					return terr
				}
				spec.Transcode = &waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate}
				newExt = transcodeExt(tf)
			}

			mc, err := collisionFor(cmd, collisionStr)
			if err != nil {
				return err
			}
			outPath, skip, err := resolveProcessOutput(source, explicit, newExt, "cut", mc)
			if err != nil {
				return err
			}
			if skip {
				env.info("skipped (exists): %s\n", outPath)
				return nil
			}
			warnALACToAlacExt(env, outPath, tf)
			spec.Output = waxtap.ToFile(outPath)

			sel, policy, err := urlSelection(itag, codec, sourcePolicy, layout)
			if err != nil {
				return err
			}
			res, err := dispatchProcess(cmd.Context(), env, source, sel, policy, spec, noFallback)
			if err != nil {
				return err
			}
			return emitResult(env, res)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "output file path")
	f.StringArrayVar(&ranges, "cut-range", nil, "remove a time range start-end (repeatable)")
	f.StringVar(&sbCats, "cut-sponsorblock", "", "remove SponsorBlock categories (YouTube only; comma-separated after =, for example --cut-sponsorblock=intro,outro; bare flag = music_offtopic)")
	f.StringVar(&cutMode, "cut-mode", "smart", "cut rendering: smart|copy|accurate")
	f.DurationVar(&crossfade, "crossfade", 0, "crossfade duration at splice points (default off)")
	f.StringVar(&sbOnError, "sponsorblock-onerror", "proceed", "on SponsorBlock fetch failure: proceed|fail")
	f.StringVar(&transcode, "transcode", "", "also transcode: flac|alac|wav|mp3|aac|opus|vorbis")
	f.IntVar(&bitrate, "bitrate", 0, "target bitrate for lossy transcodes")
	f.IntVar(&itag, "itag", 0, "itag to download (URL source)")
	f.StringVar(&codec, "codec", "", "codec to download (URL source)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source tradeoff for a URL source")
	f.StringVar(&collisionStr, "collision", "", "on existing file: fail|overwrite|auto-number|skip")
	if fl := f.Lookup("cut-sponsorblock"); fl != nil {
		fl.NoOptDefVal = "music_offtopic"
	}
	return cmd
}

func newTranscodeCmd() *cobra.Command {
	var (
		out          string
		format       string
		bitrate      int
		itag         int
		codec        string
		channels     string
		downmix      bool
		noFallback   bool
		sourcePolicy string
		collisionStr string
	)
	cmd := &cobra.Command{
		Use:   "transcode <input> [output]",
		Short: "Transcode a local file or YouTube audio to another format",
		Long: "Re-encode audio to a target format. The format comes from --format or is\n" +
			"inferred from the output file extension. FLAC/ALAC/WAV are lossless\n" +
			"re-encodes (no further loss); copy/remux is the only no-re-encode path.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			source := args[0]
			explicit := out
			if len(args) == 2 {
				if out != "" {
					return usagef("give the output once (positional or --out, not both)")
				}
				explicit = args[1]
			}

			tf, err := transcodeFormatFor(format, explicit)
			if err != nil {
				return err
			}
			layout, doDownmix, err := resolveChannels(cmd, env.cfg, channels, downmix)
			if err != nil {
				return err
			}
			spec := waxtap.ProcessSpec{Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate}, Channels: layout, Downmix: doDownmix}

			mc, err := collisionFor(cmd, collisionStr)
			if err != nil {
				return err
			}
			outPath, skip, err := resolveProcessOutput(source, explicit, transcodeExt(tf), "transcoded", mc)
			if err != nil {
				return err
			}
			if skip {
				env.info("skipped (exists): %s\n", outPath)
				return nil
			}
			warnALACToAlacExt(env, outPath, tf)
			spec.Output = waxtap.ToFile(outPath)

			sel, policy, err := urlSelection(itag, codec, sourcePolicy, layout)
			if err != nil {
				return err
			}
			res, err := dispatchProcess(cmd.Context(), env, source, sel, policy, spec, noFallback)
			if err != nil {
				return err
			}
			return emitResult(env, res)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "output file path")
	f.StringVarP(&format, "format", "f", "", "output format: copy|flac|alac|wav|mp3|aac|opus|vorbis")
	f.IntVar(&bitrate, "bitrate", 0, "target bitrate (bits/sec) for lossy formats")
	f.IntVar(&itag, "itag", 0, "itag to download (URL source)")
	f.StringVar(&codec, "codec", "", "codec to download (URL source)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source tradeoff for a URL source")
	f.StringVar(&collisionStr, "collision", "", "on existing file: fail|overwrite|auto-number|skip")
	return cmd
}

// transcodeFormatFor resolves the transcode format from --format, falling back to
// the output file's extension.
func transcodeFormatFor(format, output string) (waxtap.TranscodeFormat, error) {
	if format != "" {
		return parseTranscodeFormat(format)
	}
	if ext := strings.TrimPrefix(filepath.Ext(output), "."); ext != "" {
		return parseTranscodeFormat(ext)
	}
	return 0, usagef("specify --format or an output file with an extension")
}

// urlSelection builds the audio selector and source policy used when a process
// command is given a URL source. layout sets its preferred native channel
// layout.
func urlSelection(itag int, codec, sourcePolicy string, layout waxtap.ChannelLayout) (waxtap.AudioSelector, waxtap.SourcePolicy, error) {
	sel, err := audioSelector(itag, codec, layout)
	if err != nil {
		return sel, waxtap.SourcePolicy{}, err
	}
	policy, err := parseSourcePolicy(sourcePolicy)
	return sel, policy, err
}

// collisionFor resolves the collision mode from a --collision flag, defaulting to
// fail (these commands name an explicit output, so silent renaming is surprising).
func collisionFor(cmd *cobra.Command, value string) (collisionMode, error) {
	if !cmd.Flags().Changed("collision") {
		return collisionFail, nil
	}
	return parseCollisionMode(value)
}
