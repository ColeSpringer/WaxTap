package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

func newNormalizeCmd() *cobra.Command {
	var (
		measure      bool
		target       float64
		peakMode     string
		format       string
		bitrate      int
		bitDepth     int
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
		Short: "Normalize or measure loudness (EBU R128)",
		Long: "Normalize loudness to --loudness-target and write re-encoded audio by\n" +
			"default. Use --measure-loudness to report integrated loudness without\n" +
			"writing output. With --album, normalization applies a shared gain to\n" +
			"every track while preserving track-to-track differences. Use --album\n" +
			"--measure-loudness to analyze the files as one set.\n\n" +
			"Normalization measures EBU R128 loudness and applies a single scalar gain.\n" +
			"--peak-mode cap (the default) caps that gain so the true peak stays under\n" +
			"-1.0 dBTP, which is transparent but means a loud source can land well below\n" +
			"the target: a source already peaking at 0 dBTP takes at most -1.0 dB of gain\n" +
			"whatever the target, so the miss can be 10 LU or more. --peak-mode limit\n" +
			"applies the full gain and lets the true-peak limiter catch the overshoot,\n" +
			"at the cost of transparency. Because the limiter gives back part of the\n" +
			"gain it is handed, limit measures its own output and corrects, re-encoding\n" +
			"up to 4 times to land within 0.3 LU of the target; it reports\n" +
			"loudness-target-missed for any miss past that 0.3 LU, since a search that\n" +
			"stopped outside its own tolerance is one that gave up.\n\n" +
			"--album applies one uniform gain in a single pass and defaults to limit,\n" +
			"whichever way --peak-mode is set for single files. Under limit the gain\n" +
			"aims at the target and the per-track limiter gives part of it back on the\n" +
			"loudest tracks, which compresses the track-to-track spacing; --peak-mode\n" +
			"cap instead clamps the one gain by the album's least true-peak headroom,\n" +
			"which leaves the limiter idle and reproduces that spacing exactly, at the\n" +
			"cost of landing short of the target. Every track is measured at the width\n" +
			"its own encode delivers, so a lossy encoder's fold of a surround master is\n" +
			"in the peaks the clamp holds. One hot master sets the headroom for every\n" +
			"track, so the miss can be large.\n" +
			"On an Opus source that stays Opus, cap writes the gain into the Opus header\n" +
			"(the OpusHead output gain, which every compliant player applies) and copies\n" +
			"the packets untouched, in whichever container the output names, so the run\n" +
			"costs no generation of loss; --json reports loudness.headerGain. limit, an\n" +
			"explicit --bitrate, a cut, or a downmix re-encode as before, and --album\n" +
			"takes the same path under cap when every member is Opus.\n\n" +
			"Neither mode has anything to give back when the gain attenuates, so a loud\n" +
			"album lands on target either way. cap reports loudness-target-missed when\n" +
			"the clamp holds it more than 1 LU short; limit reports it when the measured\n" +
			"album misses by more than 1 LU, which only a boosting gain can do.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			if err := validateNormalizeModeFlags(cmd, measure); err != nil {
				return err
			}

			pm, err := parsePeakMode(peakMode)
			if err != nil {
				return err
			}

			if album {
				return runAlbum(cmd, env, args, albumParams{
					measure: measure, target: target, peakMode: pm, format: format,
					bitrate: bitrate, bitDepth: bitDepth, dir: dir, collisionStr: collisionStr,
				})
			}
			// Reject an explicitly empty --out/--dir (usually an unset $VAR) before the
			// single-file or directory path applies its default. It runs after the
			// measure-mode check (which owns these flags when measuring) and after the
			// --album branch (which requires --dir and rejects --out, so album keeps
			// its own specific errors instead of the generic "omit it" hint).
			if err := rejectEmptyFlags(cmd, "out", "dir"); err != nil {
				return err
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
					measure: measure, target: target, peakMode: pm, format: format,
					bitrate: bitrate, bitDepth: bitDepth, channels: channels, downmix: downmix,
					collisionStr: collisionStr, concurrency: concurrency,
				})
			}

			if err := validateLocalSourceFlags(cmd, env.cfg, isLocalFile(source), downmix); err != nil {
				return err
			}
			if err := validateNormalizeInputFlags(cmd, measure, false, false); err != nil {
				return err
			}
			if err := validateItag(cmd, itag); err != nil {
				return err
			}
			layout, doDownmix, err := resolveChannels(cmd, env.cfg, channels, downmix)
			if err != nil {
				return err
			}
			if measure {
				if explicit != "" {
					return usagef("--measure-loudness does not write output; remove the output path")
				}
				return runMeasure(cmd, env, source, itag, codec, sourcePolicy, noFallback, layout, target)
			}

			// The write path validates stdout output and the source before format
			// inference. The measure path validates URL sources through dispatchProcess.
			if err := preflightProcessOutput(source, explicit); err != nil {
				return err
			}
			// Check the output path before format inference so a directory gets a
			// useful error instead of a missing file extension error.
			if err := rejectDirOutput(explicit); err != nil {
				return err
			}
			// When --format is omitted, infer it from the output extension.
			if format == "" && filepath.Ext(explicit) == "" {
				return usagef("normalizing a file requires an output path or --format (e.g. flac); use --measure-loudness to analyze without writing output")
			}
			var tf waxtap.TranscodeFormat
			inferred, kept := false, false
			if format != "" {
				tf, err = parseTranscodeFormat(format)
			} else {
				// No --format: the output extension names a container, which
				// keeps the source codec when it can hold it and otherwise
				// runs its own usual encoder. A kept Opus source takes the
				// header-gain path, the one normalize that is not a re-encode;
				// every other kept codec is re-encoded, which is what normalize
				// means.
				tf, kept, inferred, err = inferOutputFormat(cmd.Context(), env, source, filepath.Ext(explicit))
			}
			if err != nil {
				return err
			}
			if tf == waxtap.FormatCopy {
				return usagef("normalization re-encodes; copy is not a valid output format")
			}
			specLayout, specDownmix := downmixFields(layout, doDownmix)
			spec := waxtap.ProcessSpec{
				// FromContainer travels here as it does on transcode: the
				// user ran normalize, which names an encode, but not which
				// codec. A container that could not hold the source picked
				// that, and a lossy answer it never asked for is reported.
				// For a URL, whose codec no probe here can see, the pipeline
				// settles it against the staged file, and a format-named
				// extension the same way: a target that carries the source
				// keeps it, and the gain rides in an Opus head rather than
				// buying a generation.
				Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate, BitDepth: bitDepth, FromContainer: inferred && !kept},
				Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: target, PeakMode: pm},
				Channels:  specLayout,
				Downmix:   specDownmix,
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
				return emitSkip(env, "exists", outPath)
			}
			if err := noteKnobFlags(env, tf, bitrate, bitDepth); err != nil {
				return err
			}
			spec.Output = outputFor(outPath, mc)
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
	f.BoolVar(&measure, "measure-loudness", false, "measure loudness without writing output")
	f.Float64Var(&target, "loudness-target", -14, "target integrated loudness (LUFS)")
	bindPeakModeFlag(f, &peakMode)
	f.StringVarP(&format, "format", "f", "", "output format: "+formatChoices(false)+formatSpellingNote)
	bindBitrateFlag(f, &bitrate)
	bindBitDepthFlag(f, &bitDepth)
	f.StringVarP(&out, "out", "o", "", "output file path for one input")
	f.BoolVar(&album, "album", false, "treat all inputs as one album (group loudness)")
	f.StringVarP(&dir, "dir", "d", "", "output directory for a directory input, or for the files --album writes")
	f.IntVar(&itag, "itag", 0, "select an exact itag (URL input)")
	f.StringVar(&codec, "codec", "", "select the best source matching a codec (hard filter, URL input)")
	bindSourceSelectionFlags(f, &channels, &downmix, &noFallback)
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source policy for a URL input: minimize-loss|best-native|prefer:<codec> (prefer:<codec> is a preference, not a filter)")
	bindCollisionFlag(f, &collisionStr)
	f.BoolVarP(&recursive, "recursive", "r", false, "recurse into subdirectories for a directory input")
	f.IntVar(&concurrency, "concurrency", 0, "parallel jobs (0 runs serially)")
	bindConfigFlags(f)
	bindNetworkFlags(f)
	bindPlayerExtractionFlags(f)
	return cmd
}

// validateNormalizeModeFlags rejects flags that affect normalized output when
// --measure-loudness is set.
func validateNormalizeModeFlags(cmd *cobra.Command, measure bool) error {
	if !measure {
		return nil
	}
	return rejectChangedFlags(cmd, "cannot be combined with --measure-loudness", "loudness-target", "peak-mode", "format", "bitrate", "bit-depth", "out", "dir", "collision", "channels", "downmix")
}

// validateNormalizeInputFlags rejects flags that do not apply to the selected
// input shape.
func validateNormalizeInputFlags(cmd *cobra.Command, measure, directory, album bool) error {
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
	case measure:
		// The write/normalization flags are already rejected by
		// validateNormalizeModeFlags; only the directory-batch flags remain.
		return rejectChangedFlags(cmd, "is only used with a directory input", "recursive", "concurrency")
	default: // single-file write
		return rejectChangedFlags(cmd, "is only used with a directory input", "recursive", "concurrency", "dir")
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
		noteForcedIOSIncomplete(env, err)
		return err
	}
	return emitResult(env, res)
}

type albumParams struct {
	measure      bool
	target       float64
	peakMode     waxtap.PeakMode
	format       string
	bitrate      int
	bitDepth     int
	dir          string
	collisionStr string
}

func runAlbum(cmd *cobra.Command, env *appEnv, inputs []string, p albumParams) error {
	if err := validateNormalizeInputFlags(cmd, p.measure, false, true); err != nil {
		return err
	}
	// The album default stays limit whether or not --peak-mode was given: cap is
	// the default for a single file, but for an album it is the mode that gives up
	// hitting the target, so it is opted into rather than inherited.
	peakMode := waxtap.PeakLimit
	if cmd.Flags().Changed("peak-mode") {
		peakMode = p.peakMode
	}
	for _, in := range inputs {
		if !isLocalFile(in) {
			return usagef("--album works on local files only (%q is not a file)", in)
		}
	}
	if p.measure {
		res, err := env.client.MeasureAlbum(cmd.Context(), inputs)
		if err != nil {
			return err
		}
		return emitAlbumMeasure(env, inputs, res)
	}

	if p.format == "" {
		return usagef("normalizing an album requires --format (e.g. flac); use --measure-loudness to analyze without writing files")
	}
	if p.dir == "" {
		return usagef("--album writes one file per track; pass --dir")
	}
	if err := rejectDirIsFile(p.dir); err != nil {
		return err
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
	// Album outputs are named after their inputs' stems, so two tracks from
	// different folders can land on one path. The library refuses that by track
	// number; naming the inputs is what tells the user which two, before
	// anything is written.
	seen := map[string]int{}
	for i, in := range inputs {
		stem := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
		outPath, _, err := resolveCollision(filepath.Join(p.dir, stem+"."+transcodeExt(tf)), mc)
		if err != nil {
			return err
		}
		if prev, dup := seen[outPath]; dup {
			return usagef("inputs %q and %q both map to output %q; rename one or choose a different --dir",
				displayPath(inputs[prev]), displayPath(in), displayPath(outPath))
		}
		seen[outPath] = i
		tracks[i] = waxtap.AlbumTrack{Input: in, Output: outPath}
	}
	if err := noteKnobFlags(env, tf, p.bitrate, p.bitDepth); err != nil {
		return err
	}
	res, err := env.client.ProcessAlbum(cmd.Context(), tracks, p.target,
		waxtap.TranscodeSpec{Format: tf, Bitrate: p.bitrate, BitDepth: p.bitDepth},
		waxtap.WithAlbumPeakMode(peakMode))
	if err != nil {
		return err
	}
	return emitAlbumProcess(env, inputs, res)
}

func emitAlbumMeasure(env *appEnv, inputs []string, res *waxtap.AlbumLoudnessResult) error {
	if env.jsonMode() {
		warns := make([]warningJSON, len(res.Warnings))
		for i, w := range res.Warnings {
			warns[i] = warningJSON{Code: w.Code.String(), Detail: w.Detail}
		}
		return env.emitJSON(struct {
			SchemaVersion int              `json:"schemaVersion"`
			Album         loudnessInfoJSON `json:"album"`
			Tracks        []albumTrackJSON `json:"tracks"`
			Warnings      []warningJSON    `json:"warnings,omitempty"`
			Notes         []noteJSON       `json:"notes,omitempty"`
		}{schemaVersion, albumInfoJSON(res.Album), albumTracksJSON(inputs, res.PerTrack, nil, nil), warns, env.notesJSON()})
	}
	// A measurement runs without an event stream, so its warnings surface here
	// rather than through the progress renderer, as the processing path's do.
	for _, w := range res.Warnings {
		env.info("warning: [%s] %s\n", w.Code, w.Detail)
	}
	env.printf("Album:  %s LUFS, LRA %s\n\n", humanLUFS(res.Album.IntegratedLUFS), humanLUFS(res.Album.LRA))
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tLUFS\tTRACK")
	for i, l := range res.PerTrack {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", i+1, humanLUFS(l.IntegratedLUFS), filepath.Base(inputs[i]))
	}
	return tw.Flush()
}

func emitAlbumProcess(env *appEnv, inputs []string, res *waxtap.AlbumProcessResult) error {
	if env.jsonMode() {
		warns := make([]warningJSON, len(res.Warnings))
		for i, w := range res.Warnings {
			warns[i] = warningJSON{Code: w.Code.String(), Detail: w.Detail}
		}
		var delivered *loudnessInfoJSON
		if res.Delivered != nil {
			d := albumInfoJSON(*res.Delivered)
			delivered = &d
		}
		return env.emitJSON(struct {
			SchemaVersion   int               `json:"schemaVersion"`
			Album           loudnessInfoJSON  `json:"album"`
			GainDB          jsonFloat         `json:"gainDb"`
			LoudnessApplied bool              `json:"loudnessApplied"`
			HeaderGain      bool              `json:"headerGain,omitempty"`
			Delivered       *loudnessInfoJSON `json:"delivered,omitempty"`
			Tracks          []albumTrackJSON  `json:"tracks"`
			Warnings        []warningJSON     `json:"warnings,omitempty"`
			Notes           []noteJSON        `json:"notes,omitempty"`
		}{schemaVersion, albumInfoJSON(res.Album), jsonFloat(res.GainDB), res.LoudnessApplied, res.HeaderGain, delivered, albumTracksJSON(inputs, res.PerTrack, res.Outputs, res.TagCarry), warns, env.notesJSON()})
	}
	// Album processing runs without an event stream, so carry warnings surface
	// here rather than through the progress renderer.
	for _, w := range res.Warnings {
		env.info("warning: [%s] %s\n", w.Code, w.Detail)
	}
	if res.LoudnessApplied {
		env.printf("Album:  %s LUFS; applied %+.1f dB to each track", humanLUFS(res.Album.IntegratedLUFS), res.GainDB)
		if res.Delivered != nil {
			env.printf("; delivered %s LUFS", humanLUFS(res.Delivered.IntegratedLUFS))
		}
		if res.HeaderGain {
			env.printf("; gain written to the Opus headers, packets copied untouched")
		}
	} else {
		// No album loudness to derive a gain from, so the tracks were re-encoded
		// untouched. The unmeasurable warning above carries the cause.
		env.printf("Album:  %s LUFS; no gain applied, tracks re-encoded unchanged", humanLUFS(res.Album.IntegratedLUFS))
	}
	env.printf("\n\n")
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	// Per-track values are input measurements; processed output is not measured
	// here. METADATA is the track's carry receipt, the single-file Metadata: line.
	fmt.Fprintln(tw, "#\tIN-LUFS\tMETADATA\tOUTPUT")
	for i := range res.Outputs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", i+1, humanLUFS(res.PerTrack[i].IntegratedLUFS), albumCarryCell(res.TagCarry, i), displayPath(res.Outputs[i]))
	}
	return tw.Flush()
}

// albumCarryCell is a track's metadata receipt for the album table, "-" for a
// track that had nothing to carry.
func albumCarryCell(carries []*waxtap.TagCarry, i int) string {
	if i >= len(carries) || carries[i] == nil {
		return "-"
	}
	return carrySummary(carries[i])
}

type albumTrackJSON struct {
	Input          string        `json:"input"`
	Output         string        `json:"output,omitempty"`
	IntegratedLUFS jsonFloat     `json:"integratedLufs"`
	TagCarry       *tagCarryJSON `json:"tagCarry,omitempty"`
}

// albumTracksJSON renders the per-track rows. outputs and carries are nil for
// a measurement, which writes nothing and carries nothing.
func albumTracksJSON(inputs []string, perTrack []waxtap.LoudnessInfo, outputs []string, carries []*waxtap.TagCarry) []albumTrackJSON {
	out := make([]albumTrackJSON, len(perTrack))
	for i, l := range perTrack {
		out[i] = albumTrackJSON{Input: displayPath(inputs[i]), IntegratedLUFS: jsonFloat(l.IntegratedLUFS)}
		if outputs != nil {
			out[i].Output = displayPath(outputs[i])
		}
		if carries != nil {
			out[i].TagCarry = tagCarryToJSON(carries[i])
		}
	}
	return out
}

func albumInfoJSON(l waxtap.LoudnessInfo) loudnessInfoJSON {
	return loudnessInfoJSON{
		IntegratedLUFS: jsonFloat(l.IntegratedLUFS),
		TruePeakDBTP:   jsonFloat(l.TruePeakDBTP),
		LRA:            jsonFloat(l.LRA),
		SamplePeakDB:   jsonFloat(l.SamplePeakDB),
	}
}
