package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

func newNormalizeCmd() *cobra.Command {
	var (
		apply          bool
		target         float64
		loudnessTarget float64
		transcode      string
		bitrate        int
		out            string
		album          bool
		dir            string
		itag           int
		codec          string
		noFallback     bool
		sourcePolicy   string
		collisionStr   string
	)
	cmd := &cobra.Command{
		Use:   "normalize <input> [output]",
		Short: "Measure or normalize loudness (EBU R128)",
		Long: "Measure integrated loudness (default) or normalize to a target LUFS with\n" +
			"--apply (or by setting --target), fused into a transcode. With --album,\n" +
			"measure a set of files as one album, or with --apply bake the shared album\n" +
			"gain into each track (preserving track-to-track differences).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			target, err = resolveLoudnessTarget(cmd, target, loudnessTarget)
			if err != nil {
				return err
			}
			doApply := apply || cmd.Flags().Changed("target") || cmd.Flags().Changed("loudness-target")

			if album {
				return runAlbum(cmd, env, args, albumParams{
					apply: doApply, target: target, transcode: transcode, bitrate: bitrate, dir: dir, collisionStr: collisionStr,
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

			if !doApply {
				return runMeasure(cmd, env, source, target, itag, codec, sourcePolicy, noFallback)
			}

			if transcode == "" {
				return usagef("normalize --apply re-encodes; pass --transcode (e.g. flac)")
			}
			tf, err := parseTranscodeFormat(transcode)
			if err != nil {
				return err
			}
			spec := waxtap.ProcessSpec{
				Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate},
				Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: target},
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
			// Normalize has no channel-layout preference.
			sel, policy, err := urlSelection(itag, codec, sourcePolicy, waxtap.LayoutAny)
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
	f.BoolVar(&apply, "apply", false, "normalize (re-encode) instead of only measuring")
	f.Float64Var(&target, "target", -14, "target integrated loudness (LUFS) for --apply (alias: --loudness-target)")
	f.Float64Var(&loudnessTarget, "loudness-target", -14, "alias of --target (consistent with download --loudness-target)")
	f.StringVar(&transcode, "transcode", "", "output format for --apply: flac|alac|wav|mp3|aac|opus|vorbis")
	f.IntVar(&bitrate, "bitrate", 0, "target bitrate for lossy formats")
	f.StringVarP(&out, "out", "o", "", "output file path (single --apply)")
	f.BoolVar(&album, "album", false, "treat all inputs as one album (group loudness)")
	f.StringVarP(&dir, "dir", "d", "", "output directory for --album --apply")
	f.IntVar(&itag, "itag", 0, "itag to download (URL source)")
	f.StringVar(&codec, "codec", "", "codec to download (URL source)")
	f.BoolVar(&noFallback, "no-fallback", false, "disable WEB-context, watch-page, and incomplete-download fallbacks")
	f.StringVar(&sourcePolicy, "source-policy", "minimize-loss", "source tradeoff for a URL source")
	f.StringVar(&collisionStr, "collision", "", "on existing file: fail|overwrite|auto-number|skip")
	return cmd
}

// resolveLoudnessTarget returns the value supplied through either target flag.
// Conflicting values are rejected.
func resolveLoudnessTarget(cmd *cobra.Command, target, alias float64) (float64, error) {
	tset := cmd.Flags().Changed("target")
	aset := cmd.Flags().Changed("loudness-target")
	switch {
	case tset && aset && target != alias:
		return 0, usagef("--target and --loudness-target are aliases; set only one (got %g and %g)", target, alias)
	case aset:
		return alias, nil
	default:
		return target, nil
	}
}

// runMeasure measures a single source and prints its loudness without writing a
// re-encoded file (the unchanged audio is discarded).
func runMeasure(cmd *cobra.Command, env *appEnv, source string, target float64, itag int, codec, sourcePolicy string, noFallback bool) error {
	spec := waxtap.ProcessSpec{
		Loudness: &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly, Target: target},
		Output:   waxtap.ToWriter(io.Discard),
	}
	sel, policy, err := urlSelection(itag, codec, sourcePolicy, waxtap.LayoutAny)
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
	transcode    string
	bitrate      int
	dir          string
	collisionStr string
}

func runAlbum(cmd *cobra.Command, env *appEnv, inputs []string, p albumParams) error {
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

	if p.transcode == "" {
		return usagef("--album --apply re-encodes; pass --transcode (e.g. flac)")
	}
	if p.dir == "" {
		return usagef("--album --apply writes one file per track; pass --dir")
	}
	tf, err := parseTranscodeFormat(p.transcode)
	if err != nil {
		return err
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
