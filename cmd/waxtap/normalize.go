package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

func newNormalizeCmd() *cobra.Command {
	var (
		apply        bool
		target       float64
		format       string
		bitrate      int
		out          string
		album        bool
		dir          string
		itag         int
		codec        string
		channels     string
		downmix      bool
		noFallback   bool
		sourcePolicy string
		collisionStr string
		recursive    bool
		concurrency  int
	)
	cmd := &cobra.Command{
		Use:   "normalize <input> [output]",
		Short: "Measure or normalize loudness (EBU R128)",
		Long: "Measure integrated loudness without writing output (default), or use\n" +
			"--apply to normalize to --loudness-target and write re-encoded audio.\n" +
			"With --album, measure a set of files as one album, or with --apply use\n" +
			"the shared album gain for each track while preserving track-to-track\n" +
			"differences.\n\n" +
			"Applying normalization uses one ffmpeg loudnorm pass with true-peak\n" +
			"limiting. A loud source may therefore land slightly below the target\n" +
			"(for example, -14.9 for -14).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			if err := validateNormalizeModeFlags(cmd, apply); err != nil {
				return err
			}

			if album {
				return runAlbum(cmd, env, args, albumParams{
					apply: apply, target: target, format: format, bitrate: bitrate, dir: dir, collisionStr: collisionStr,
				})
			}

			source := args[0]
			explicit := out
			if len(args) >= 2 {
				if out != "" {
					return usagef("give the output once (positional or --out, not both)")
				}
				explicit = args[1]
			}
			if len(args) > 2 {
				return usagef("multiple inputs require --album")
			}

			if fi, serr := os.Stat(source); serr == nil && fi.IsDir() {
				return runDirectoryNormalize(cmd, env, directoryNormalizeParams{
					root: source, explicit: explicit, dir: dir, recursive: recursive,
					apply: apply, target: target, format: format, bitrate: bitrate,
					channels: channels, downmix: downmix,
					collisionStr: collisionStr, concurrency: concurrency,
				})
			}

			if err := validateLocalSourceFlags(cmd, source); err != nil {
				return err
			}
			if err := validateNormalizeInputFlags(cmd, apply, false, false); err != nil {
				return err
			}
			layout, doDownmix, err := resolveChannels(cmd, env.cfg, channels, downmix)
			if err != nil {
				return err
			}
			if !apply {
				if explicit != "" {
					return usagef("measurement does not write output; remove the output path or use --apply")
				}
				return runMeasure(cmd, env, source, itag, codec, sourcePolicy, noFallback, layout, target)
			}

			// When --format is omitted, infer it from the output extension.
			if format == "" && filepath.Ext(explicit) == "" {
				return usagef("normalize --apply re-encodes; pass --format (e.g. flac) or an output file with an extension")
			}
			tf, err := transcodeFormatFor(format, explicit)
			if err != nil {
				return err
			}
			if tf == waxtap.FormatCopy {
				return usagef("normalization re-encodes; copy is not a valid output format")
			}
			spec := waxtap.ProcessSpec{
				Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate},
				Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: target},
				Channels:  layout,
				Downmix:   doDownmix,
			}
			mc, err := collisionFor(cmd, collisionStr)
			if err != nil {
				return err
			}
			outPath, skip, err := resolveProcessOutput(source, explicit, transcodeExt(tf), "normalized", mc)
			if err != nil {
				return err
			}
			if skip {
				env.info("skipped (exists): %s\n", outPath)
				return nil
			}
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
	f.BoolVar(&apply, "apply", false, "write normalized audio instead of measuring only")
	f.Float64Var(&target, "loudness-target", -14, "target integrated loudness (LUFS) for --apply")
	f.StringVarP(&format, "format", "f", "", "output format for --apply: flac|alac|wav|mp3|aac|opus|vorbis")
	f.IntVar(&bitrate, "bitrate", 0, "target bitrate in bits/sec for lossy formats (0 = preset default)")
	f.StringVarP(&out, "out", "o", "", "output file path for one input")
	f.BoolVar(&album, "album", false, "treat all inputs as one album (group loudness)")
	f.StringVarP(&dir, "dir", "d", "", "output directory for a directory input or --album --apply")
	f.IntVar(&itag, "itag", 0, "select an exact itag (URL input)")
	f.StringVar(&codec, "codec", "", "select the best source matching a codec (hard filter, URL input)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source policy for a URL input: minimize-loss|best-native|prefer:<codec> (prefer:<codec> is a preference, not a filter)")
	f.StringVar(&collisionStr, "collision", "", "on existing file: fail|overwrite|auto-number|skip (default: fail)")
	f.BoolVarP(&recursive, "recursive", "r", false, "recurse into subdirectories for a directory input")
	f.IntVar(&concurrency, "concurrency", 0, "number of parallel ffmpeg jobs (0 = serial)")
	return cmd
}

// validateNormalizeModeFlags rejects write-only flags in the default measure
// mode. This avoids accepting a command that appears to write output but does not.
func validateNormalizeModeFlags(cmd *cobra.Command, apply bool) error {
	if apply {
		return nil
	}
	// Channel selection and downmixing affect encoded output, which measure mode
	// does not produce.
	return rejectChangedFlags(cmd, "requires --apply", "loudness-target", "format", "bitrate", "out", "dir", "collision", "channels", "downmix")
}

// validateNormalizeInputFlags rejects flags that do not apply to the selected
// input shape.
func validateNormalizeInputFlags(cmd *cobra.Command, apply, directory, album bool) error {
	if directory || album {
		if err := rejectChangedFlags(cmd, "is only used with a URL input", "itag", "codec", "source-policy", "no-fallback"); err != nil {
			return err
		}
	}
	switch {
	case album:
		// Album processing does not support channel selection or downmixing.
		return rejectChangedFlags(cmd, "is not used with --album", "recursive", "concurrency", "out", "channels", "downmix")
	case directory:
		return nil
	case apply:
		return rejectChangedFlags(cmd, "is only used with a directory input", "recursive", "concurrency", "dir")
	default:
		return rejectChangedFlags(cmd, "is only used with a directory input", "recursive", "concurrency")
	}
}

// runMeasure measures a single source and prints its loudness without writing a
// re-encoded file (the unchanged audio is discarded).
func runMeasure(cmd *cobra.Command, env *appEnv, source string, itag int, codec, sourcePolicy string, noFallback bool, layout waxtap.ChannelLayout, target float64) error {
	spec := waxtap.ProcessSpec{
		// Carry the target so a measure-only JSON result does not report zero.
		Loudness: &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly, Target: target},
		Output:   waxtap.ToWriter(io.Discard),
	}
	sel, policy, err := urlSelection(itag, codec, sourcePolicy, layout)
	if err != nil {
		return err
	}
	res, err := dispatchProcess(cmd.Context(), env, source, sel, policy, spec, noFallback)
	if err != nil {
		return err
	}
	return emitResult(env, res)
}

type albumParams struct {
	apply        bool
	target       float64
	format       string
	bitrate      int
	dir          string
	collisionStr string
}

func runAlbum(cmd *cobra.Command, env *appEnv, inputs []string, p albumParams) error {
	if err := validateNormalizeInputFlags(cmd, p.apply, false, true); err != nil {
		return err
	}
	for _, in := range inputs {
		if !isLocalFile(in) {
			return usagef("--album works on local files only (%q is not a file)", in)
		}
	}
	if !p.apply {
		res, err := env.client.MeasureAlbum(cmd.Context(), inputs)
		if err != nil {
			return err
		}
		return emitAlbumMeasure(env, inputs, res)
	}

	if p.format == "" {
		return usagef("--album --apply re-encodes; pass --format (e.g. flac)")
	}
	if p.dir == "" {
		return usagef("--album --apply writes one file per track; pass --dir")
	}
	tf, err := parseTranscodeFormat(p.format)
	if err != nil {
		return err
	}
	if tf == waxtap.FormatCopy {
		return usagef("album normalization re-encodes; --format copy is not supported")
	}
	mc, err := collisionFor(cmd, p.collisionStr)
	if err != nil {
		return err
	}
	if mc == collisionSkip {
		return usagef("--collision skip is not supported with --album")
	}

	tracks := make([]waxtap.AlbumTrack, len(inputs))
	for i, in := range inputs {
		stem := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
		outPath, _, err := resolveCollision(filepath.Join(p.dir, stem+"."+transcodeExt(tf)), mc)
		if err != nil {
			return err
		}
		tracks[i] = waxtap.AlbumTrack{Input: in, Output: outPath}
	}
	res, err := env.client.ProcessAlbum(cmd.Context(), tracks, p.target, waxtap.TranscodeSpec{Format: tf, Bitrate: p.bitrate})
	if err != nil {
		return err
	}
	return emitAlbumProcess(env, inputs, res)
}

func emitAlbumMeasure(env *appEnv, inputs []string, res *waxtap.AlbumLoudnessResult) error {
	if env.jsonMode() {
		return env.emitJSON(struct {
			SchemaVersion int              `json:"schemaVersion"`
			Album         loudnessInfoJSON `json:"album"`
			Tracks        []albumTrackJSON `json:"tracks"`
		}{schemaVersion, albumInfoJSON(res.Album), albumTracksJSON(inputs, res.PerTrack, nil)})
	}
	env.printf("Album:  %s LUFS, LRA %s\n\n", humanLUFS(res.Album.IntegratedLUFS), humanLUFS(res.Album.LRA))
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tLUFS\tTRACK")
	for i, l := range res.PerTrack {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", i+1, humanLUFS(l.IntegratedLUFS), filepath.Base(inputs[i]))
	}
	tw.Flush()
	return nil
}

func emitAlbumProcess(env *appEnv, inputs []string, res *waxtap.AlbumProcessResult) error {
	if env.jsonMode() {
		return env.emitJSON(struct {
			SchemaVersion int              `json:"schemaVersion"`
			Album         loudnessInfoJSON `json:"album"`
			GainDB        jsonFloat        `json:"gainDb"`
			Tracks        []albumTrackJSON `json:"tracks"`
		}{schemaVersion, albumInfoJSON(res.Album), jsonFloat(res.GainDB), albumTracksJSON(inputs, res.PerTrack, res.Outputs)})
	}
	env.printf("Album:  %s LUFS; applied %+.1f dB to each track\n\n", humanLUFS(res.Album.IntegratedLUFS), res.GainDB)
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tLUFS\tOUTPUT")
	for i := range res.Outputs {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", i+1, humanLUFS(res.PerTrack[i].IntegratedLUFS), res.Outputs[i])
	}
	tw.Flush()
	return nil
}

type albumTrackJSON struct {
	Input          string    `json:"input"`
	Output         string    `json:"output,omitempty"`
	IntegratedLUFS jsonFloat `json:"integratedLufs"`
}

func albumTracksJSON(inputs []string, perTrack []waxtap.LoudnessInfo, outputs []string) []albumTrackJSON {
	out := make([]albumTrackJSON, len(perTrack))
	for i, l := range perTrack {
		out[i] = albumTrackJSON{Input: inputs[i], IntegratedLUFS: jsonFloat(l.IntegratedLUFS)}
		if outputs != nil {
			out[i].Output = outputs[i]
		}
	}
	return out
}

func albumInfoJSON(l waxtap.LoudnessInfo) loudnessInfoJSON {
	return loudnessInfoJSON{
		IntegratedLUFS: jsonFloat(l.IntegratedLUFS),
		TruePeakDBTP:   jsonFloat(l.TruePeakDBTP),
		LRA:            jsonFloat(l.LRA),
		Threshold:      jsonFloat(l.Threshold),
	}
}
