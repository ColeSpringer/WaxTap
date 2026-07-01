package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/youtube"
	"github.com/spf13/cobra"
)

// isLocalFile reports whether arg names an existing regular file (so a process
// command treats it as a local input rather than a URL).
func isLocalFile(arg string) bool {
	fi, err := os.Stat(arg)
	return err == nil && !fi.IsDir()
}

// validateLocalSourceFlags rejects URL-selection flags for a local input.
func validateLocalSourceFlags(cmd *cobra.Command, source string) error {
	if !isLocalFile(source) {
		return nil
	}
	return rejectChangedFlags(cmd, "is only used with a URL input", "itag", "codec", "source-policy", "no-fallback")
}

// validateProcessSource reports whether source is usable by a process command:
// an existing local file, or a clean YouTube URL/ID. A missing path whose shape
// is not a valid video ID is reported as a missing file, since that is the more
// likely intent than a mistyped ID. Commands call it before collision handling so
// an existing output cannot hide a missing input.
func validateProcessSource(source string) error {
	if isLocalFile(source) {
		return nil
	}
	// Process commands take a file path or a clean ID/URL. Strict extraction keeps
	// an ID-shaped filename such as "aqz-KE-bpKQ.opus" from being treated as a
	// video download when the intended local file is missing.
	if _, err := youtube.ExtractVideoIDStrict(source); err != nil {
		// Report both accepted input forms because this is usually a mistyped
		// local path, not an intended video ID.
		if errors.Is(err, waxtap.ErrInvalidVideoID) ||
			errors.Is(err, waxtap.ErrVideoIDTooShort) ||
			errors.Is(err, waxtap.ErrVideoIDTooLong) {
			return usagef("no such file and not a valid YouTube URL or ID: %s", source)
		}
		return err
	}
	return nil
}

// rejectStdoutOutput rejects `-o -` on a process command. Only download streams
// raw media to stdout; transcode, normalize, and cut write files. explicit is the
// merged positional/--out value.
func rejectStdoutOutput(explicit string) error {
	if explicit == "-" {
		return usagef("stdout streaming (-o -) is only supported by download; give a file path")
	}
	return nil
}

// preflightProcessOutput keeps early process-command validation in one order:
// reject stdout output first, then validate the source. It must run before format
// inference so input mistakes are not hidden by format errors.
func preflightProcessOutput(source, explicit string) error {
	if err := rejectStdoutOutput(explicit); err != nil {
		return err
	}
	return validateProcessSource(source)
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
	// runMeasure does not call resolveProcessOutput, so URL sources are validated
	// here as well.
	if err := validateProcessSource(source); err != nil {
		return nil, err
	}
	// A watch?v=X&list=Y URL processes only the video. The note makes that explicit
	// for process commands, matching info, formats, and download.
	noteDroppedPlaylist(env, source, "processing only this video; the playlist is ignored")
	return env.client.Download(ctx, waxtap.Request{URL: source, Audio: sel, SourcePolicy: policy, NoFallback: noFallback, ProcessSpec: spec})
}

// resolveProcessOutput validates the source, then resolves the output path for a
// single-file process command and applies the collision mode. explicit is the
// positional/--out path (may be empty). When empty and the source is local, the
// name is derived from the input; a URL source requires an explicit output.
//
// Validating first keeps an existing output from hiding a missing input.
func resolveProcessOutput(source, explicit, newExt, tag string, mode collisionMode) (path string, skip bool, err error) {
	if err := validateProcessSource(source); err != nil {
		return "", false, err
	}
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

// isLosslessFormat reports whether a transcode preset ignores --bitrate. The CLI
// keeps this small mirror because the TranscodeFormat-to-codec mapping is
// unexported. FormatCopy is a remux, not an encoder, but it ignores --bitrate the
// same way.
func isLosslessFormat(tf waxtap.TranscodeFormat) bool {
	return tf == waxtap.FormatCopy || tf == waxtap.FormatFLAC ||
		tf == waxtap.FormatALAC || tf == waxtap.FormatWAV
}

// warnBitrateIgnoredIfLossless notes that --bitrate has no effect on a lossless or
// copy target, so a user who set it deliberately gets a signal instead of silence.
// Call it once per invocation with the parsed format, not per batch item.
func warnBitrateIgnoredIfLossless(env *appEnv, tf waxtap.TranscodeFormat, bitrate int) {
	if bitrate > 0 && isLosslessFormat(tf) {
		env.info("note: --bitrate is ignored for lossless and copy targets\n")
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
		Use:   "cut <input> [output]",
		Short: "Remove time ranges and/or SponsorBlock segments",
		Long: "Cut time ranges out of a local audio file or a YouTube video. Provide one\n" +
			"or more --cut-range, and/or --sponsorblock (YouTube only). Smart mode\n" +
			"stream-copies when cutting alone and fuses the cut into a transcode when\n" +
			"one is requested.",
		Args: sponsorblockArgs(cobra.RangeArgs(1, 2), true),
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

			// Check directory input before generic source validation so the user sees
			// the specific directory message instead of a missing-file fallback.
			if fi, serr := os.Stat(source); serr == nil && fi.IsDir() {
				return usagef("cut does not support a directory input; pass a single file or a YouTube URL")
			}
			if err := validateLocalSourceFlags(cmd, source); err != nil {
				return err
			}
			if err := validateItag(cmd, itag); err != nil {
				return err
			}
			// Keep empty SponsorBlock validation before source validation so this flag
			// error is reported consistently across cut, download, and preview.
			if err := rejectEmptySponsorBlock(cmd, sbCats); err != nil {
				return err
			}
			// Reject stdout output, invalid sources, and directory outputs before
			// parsing cut format details. These direct input errors should not be
			// masked by bitrate or format inference.
			if err := preflightProcessOutput(source, explicit); err != nil {
				return err
			}
			if err := rejectDirOutput(explicit); err != nil {
				return err
			}

			// Parse every shared cut flag before the early returns so cut and
			// download apply the same validation.
			rangeList, mode, pol, err := parseCutInputs(ranges, cutMode, sbOnError, crossfade)
			if err != nil {
				return err
			}
			sbSet := sbCats != ""
			if len(rangeList) == 0 && !sbSet {
				return usagef("nothing to cut: pass --cut-range and/or --sponsorblock")
			}
			if sbSet && isLocalFile(source) {
				return usagef("--sponsorblock needs a YouTube source (no video ID for a local file)")
			}
			if cmd.Flags().Changed("sponsorblock-on-error") && !sbSet {
				return usagef("--sponsorblock-on-error requires --sponsorblock")
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
				cs.SponsorBlock, cs.OnError = cats, pol
			}

			spec := waxtap.ProcessSpec{Cut: cs, Channels: layout, Downmix: doDownmix}
			newExt := ""
			var tf waxtap.TranscodeFormat // FormatCopy when no transcode is requested
			// Choose the transcode format once. An explicit --format wins. Re-encoding
			// cuts can also use a recognized output extension, which lets
			// `cut in.flac --crossfade 500ms -o out.mp3` work without a separate
			// --format. Plain copy cuts treat the extension only as a container hint,
			// and copy/remux pseudo-formats are not valid output extensions.
			haveFormat := false
			if format != "" {
				if tf, err = parseTranscodeFormat(format); err != nil {
					return err
				}
				haveFormat = true
			} else if mode == waxtap.CutAccurate || crossfade > 0 {
				if ext := strings.TrimPrefix(filepath.Ext(explicit), "."); ext != "" {
					if f, perr := parseTranscodeFormat(ext); perr == nil && f != waxtap.FormatCopy {
						tf, haveFormat = f, true
					}
				}
			}
			// Checked after inference so --bitrate pairs with an inferred lossy format.
			if cmd.Flags().Changed("bitrate") && !haveFormat {
				return usagef("--bitrate requires --format")
			}
			if haveFormat {
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
			warnBitrateIgnoredIfLossless(env, tf, bitrate)
			spec.Output = waxtap.ToFile(outPath)

			sel, policy, err := urlSelection(itag, codec, sourcePolicy, layout)
			if err != nil {
				return err
			}
			res, err := dispatchProcess(cmd.Context(), env, source, sel, policy, spec, noFallback)
			if err != nil {
				noteForcedIOSIncomplete(env, err)
				return err
			}
			return emitResult(env, res)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "output file path")
	bindCutFlags(f, &ranges, &cutMode, &crossfade, &sbOnError)
	bindSponsorBlockFlag(f, &sbCats, "remove SponsorBlock categories (YouTube only; comma-separated; bare flag selects music_offtopic; use sponsorblock to preview)")
	f.StringVarP(&format, "format", "f", "", "also re-encode to: flac|alac|wav|mp3|aac|opus|vorbis")
	bindBitrateFlag(f, &bitrate)
	f.IntVar(&itag, "itag", 0, "select an exact itag (URL input)")
	f.StringVar(&codec, "codec", "", "select the best source matching a codec (hard filter, URL input)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source policy for a URL input: minimize-loss|best-native|prefer:<codec> (prefer:<codec> is a preference, not a filter)")
	bindCollisionFlag(f, &collisionStr)
	bindConfigFlags(f)
	bindNetworkFlags(f)
	bindPlayerExtractionFlags(f)
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
		dir          string
		recursive    bool
		force        bool
		concurrency  int
	)
	cmd := &cobra.Command{
		Use:   "transcode <input> [output]",
		Short: "Transcode a local file or YouTube audio to another format",
		Long: "Re-encode audio to a target format. The format comes from --format or is\n" +
			"inferred from the output file extension. FLAC/ALAC/WAV are lossless\n" +
			"re-encodes (no further loss); copy/remux is the only no-re-encode path.\n" +
			"When both --format and an output extension are given, the extension must be\n" +
			"a container that can hold the format (for example, mp3 uses .mp3 or .mka,\n" +
			"not .flac).",
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

			if fi, serr := os.Stat(source); serr == nil && fi.IsDir() {
				return runDirectoryTranscode(cmd, env, directoryTranscodeParams{
					root: source, explicit: explicit, dir: dir, recursive: recursive,
					format: format, bitrate: bitrate, channels: channels, downmix: downmix,
					collisionStr: collisionStr, force: force, concurrency: concurrency,
				})
			}
			if err := rejectChangedFlags(cmd, "is only used with a directory input", "dir", "recursive", "concurrency"); err != nil {
				return err
			}
			if err := validateLocalSourceFlags(cmd, source); err != nil {
				return err
			}
			if err := validateItag(cmd, itag); err != nil {
				return err
			}

			// Validate stdout output and the source before format inference so direct
			// input errors are not hidden by "specify --format".
			if err := preflightProcessOutput(source, explicit); err != nil {
				return err
			}
			// Check the output path before format inference so a directory gets a
			// useful error instead of a missing file extension error.
			if err := rejectDirOutput(explicit); err != nil {
				return err
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
			warnBitrateIgnoredIfLossless(env, tf, bitrate)
			spec.Output = waxtap.ToFile(outPath)

			// If a local file already uses the requested codec and no other transform
			// is pending, stream-copy it instead of encoding it again. This avoids
			// unnecessary work and an extra lossy pass for MP3, AAC, Opus, and Vorbis.
			//
			// The shortcut only works when ffmpeg can infer the output container from
			// the extension. Codec-name paths such as .alac rely on the encode preset
			// to provide a muxer, and stream copy has no preset.
			remuxNoop := false
			if !force && spec.Transcode != nil && targetCodecFamily(tf) != "" && !batchTransforms(spec) &&
				isLocalFile(source) && transcode.CanInferContainer(outPath) {
				// Check the requested format before rewriting to copy, so an
				// incompatible extension such as opus into .flac is still rejected.
				if err := waxtap.ValidateProcessSpec(spec); err != nil {
					return err
				}
				// If probing fails or reports a different codec family, run the normal
				// encode path.
				if probed, perr := env.client.ProbeCodec(cmd.Context(), source); perr == nil && matchesTargetFamily(probed, tf) {
					spec.Transcode.Format = waxtap.FormatCopy
					remuxNoop = true
				}
			}

			sel, policy, err := urlSelection(itag, codec, sourcePolicy, layout)
			if err != nil {
				return err
			}
			res, err := dispatchProcess(cmd.Context(), env, source, sel, policy, spec, noFallback)
			if err != nil {
				noteForcedIOSIncomplete(env, err)
				return err
			}
			if remuxNoop {
				env.info("note: %s is already %s; copied without re-encoding (use --force to re-encode)\n", source, targetCodecFamily(tf))
			}
			return emitResult(env, res)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "output file path (single file)")
	f.StringVarP(&format, "format", "f", "", "output format: copy|flac|alac|wav|mp3|aac|opus|vorbis")
	bindBitrateFlag(f, &bitrate)
	f.IntVar(&itag, "itag", 0, "select an exact itag (URL input)")
	f.StringVar(&codec, "codec", "", "select the best source matching a codec (hard filter, URL input)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source policy for a URL input: minimize-loss|best-native|prefer:<codec> (prefer:<codec> is a preference, not a filter)")
	bindCollisionFlag(f, &collisionStr)
	f.StringVarP(&dir, "dir", "d", "", "output directory for a directory input (default: beside each input)")
	f.BoolVarP(&recursive, "recursive", "r", false, "recurse into subdirectories for a directory input")
	f.BoolVar(&force, "force", false, "re-encode even when the source already matches the target format")
	f.IntVar(&concurrency, "concurrency", 0, "parallel ffmpeg jobs (0 runs serially)")
	bindConfigFlags(f)
	bindNetworkFlags(f)
	bindPlayerExtractionFlags(f)
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
