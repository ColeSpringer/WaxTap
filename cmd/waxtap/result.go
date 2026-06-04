package main

import (
	"strconv"
	"strings"

	"github.com/colespringer/waxtap"
)

// emitResult prints a single Result as human text or JSON.
func emitResult(env *appEnv, res *waxtap.Result) error {
	if env.jsonMode() {
		return env.emitJSON(resultToJSON(res))
	}
	renderResultHuman(env, res)
	return nil
}

func renderResultHuman(env *appEnv, res *waxtap.Result) {
	switch res.SourceKind {
	case waxtap.SourceLocalFile:
		env.printf("Input:    %s\n", res.InputPath)
	default:
		if res.Title != "" {
			env.printf("Title:    %s\n", res.Title)
		}
		env.printf("Video ID: %s\n", res.VideoID)
	}
	if res.OutputPath != "" {
		env.printf("Output:   %s\n", res.OutputPath)
	} else {
		env.printf("Output:   (streamed)\n")
	}

	env.printf("Source:   %s\n", formatLabel(res.SourceFormat))
	if res.Transcoded {
		env.printf("Encoded:  %s\n", formatLabel(res.OutputFormat))
	}
	if res.SourceBytes > 0 || res.OutputBytes > 0 {
		env.printf("Size:     %s in → %s out\n", humanBytes(res.SourceBytes), humanBytes(res.OutputBytes))
	}

	if effects := effectSummary(res); effects != "" {
		env.printf("Applied:  %s\n", effects)
	}
	if res.Loudness != nil {
		renderLoudness(env, res.Loudness)
	}
	for _, w := range res.Warnings {
		env.printf("warning:  [%s] %s\n", w.Code, w.Detail)
	}
}

func renderLoudness(env *appEnv, l *waxtap.LoudnessResult) {
	if l.Input != nil {
		env.printf("Loudness: input %s LUFS, true-peak %s dBTP, LRA %s\n",
			humanLUFS(l.Input.IntegratedLUFS), humanLUFS(l.Input.TruePeakDBTP), humanLUFS(l.Input.LRA))
	}
	if l.Output != nil {
		env.printf("          output %s LUFS (target %s)\n", humanLUFS(l.Output.IntegratedLUFS), humanLUFS(l.Target))
	}
}

// effectSummary joins the applied effects into a short comma-separated list.
func effectSummary(res *waxtap.Result) string {
	var parts []string
	if res.Transcoded {
		parts = append(parts, "transcode")
	}
	if res.CutApplied {
		parts = append(parts, "cut")
	}
	if res.SponsorBlockApplied {
		parts = append(parts, "sponsorblock")
	}
	if res.LoudnessApplied {
		parts = append(parts, "normalize")
	} else if res.LoudnessMeasured {
		parts = append(parts, "loudness-measure")
	}
	return strings.Join(parts, ", ")
}

func formatLabel(f waxtap.Format) string {
	codec := dash(f.Codec)
	if f.Extension != "" {
		codec += " (" + f.Extension + ")"
	}
	if kbps := f.EffectiveBitrate() / 1000; kbps > 0 {
		codec += " " + strconv.Itoa(kbps) + " kbps"
	}
	return codec
}

// ---------------------------------------------------------------------------
// JSON view
// ---------------------------------------------------------------------------

type loudnessInfoJSON struct {
	IntegratedLUFS jsonFloat `json:"integratedLufs"`
	TruePeakDBTP   jsonFloat `json:"truePeakDbtp"`
	LRA            jsonFloat `json:"lra"`
	Threshold      jsonFloat `json:"threshold"`
}

type loudnessJSON struct {
	Input  *loudnessInfoJSON `json:"input,omitempty"`
	Output *loudnessInfoJSON `json:"output,omitempty"`
	Target jsonFloat         `json:"target"`
}

type warningJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type resultJSON struct {
	SchemaVersion int    `json:"schemaVersion"`
	SourceKind    string `json:"sourceKind"`
	VideoID       string `json:"videoId,omitempty"`
	Title         string `json:"title,omitempty"`
	InputPath     string `json:"inputPath,omitempty"`
	OutputPath    string `json:"outputPath,omitempty"`

	SourceFormat formatJSON `json:"sourceFormat"`
	OutputFormat formatJSON `json:"outputFormat"`

	SourceBytes int64 `json:"sourceBytes"`
	OutputBytes int64 `json:"outputBytes"`

	Transcoded          bool `json:"transcoded"`
	CutApplied          bool `json:"cutApplied"`
	SponsorBlockApplied bool `json:"sponsorBlockApplied"`
	LoudnessMeasured    bool `json:"loudnessMeasured"`
	LoudnessApplied     bool `json:"loudnessApplied"`

	Loudness *loudnessJSON `json:"loudness,omitempty"`
	Warnings []warningJSON `json:"warnings,omitempty"`
}

func resultToJSON(res *waxtap.Result) resultJSON {
	out := resultJSON{
		SchemaVersion:       schemaVersion,
		SourceKind:          res.SourceKind.String(),
		VideoID:             res.VideoID,
		Title:               res.Title,
		InputPath:           res.InputPath,
		OutputPath:          res.OutputPath,
		SourceFormat:        formatToJSON(res.SourceFormat),
		OutputFormat:        formatToJSON(res.OutputFormat),
		SourceBytes:         res.SourceBytes,
		OutputBytes:         res.OutputBytes,
		Transcoded:          res.Transcoded,
		CutApplied:          res.CutApplied,
		SponsorBlockApplied: res.SponsorBlockApplied,
		LoudnessMeasured:    res.LoudnessMeasured,
		LoudnessApplied:     res.LoudnessApplied,
	}
	if res.Loudness != nil {
		lj := &loudnessJSON{Target: jsonFloat(res.Loudness.Target)}
		lj.Input = loudnessInfoToJSON(res.Loudness.Input)
		lj.Output = loudnessInfoToJSON(res.Loudness.Output)
		out.Loudness = lj
	}
	for _, w := range res.Warnings {
		out.Warnings = append(out.Warnings, warningJSON{Code: w.Code.String(), Detail: w.Detail})
	}
	return out
}

func loudnessInfoToJSON(l *waxtap.LoudnessInfo) *loudnessInfoJSON {
	if l == nil {
		return nil
	}
	return &loudnessInfoJSON{
		IntegratedLUFS: jsonFloat(l.IntegratedLUFS),
		TruePeakDBTP:   jsonFloat(l.TruePeakDBTP),
		LRA:            jsonFloat(l.LRA),
		Threshold:      jsonFloat(l.Threshold),
	}
}
