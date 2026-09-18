package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxtap/v3"
)

// emitResult prints a single Result as human text or JSON.
func emitResult(env *appEnv, res *waxtap.Result) error {
	if env.jsonMode() {
		doc := resultToJSON(res)
		if wroteNothing(env, res) {
			// The measurement sank to io.Discard, so the engine's byte count and output
			// format describe the input, not a file. The human renderer says "none
			// (measurement only)"; dropping both is how JSON says it. Local sources
			// already omitted the format, since formatDTOs gates it on Transcoded, so
			// this is also what makes the two source kinds agree.
			doc.OutputBytes, doc.OutputFormat = 0, nil
		}
		doc.Notes = env.notesJSON()
		return env.emitJSON(doc)
	}
	renderResultHuman(env, res)
	return nil
}

// skipJSON is the --json document for a run that wrote nothing because the output
// already existed. It is a terminal outcome like any other, so it owes stdout a
// document; download has emitted this shape since before the other commands did.
type skipJSON struct {
	SchemaVersion int        `json:"schemaVersion"`
	Skipped       string     `json:"skipped"`
	OutputPath    string     `json:"outputPath,omitempty"`
	Notes         []noteJSON `json:"notes,omitempty"`
}

// emitSkip reports a run that wrote nothing because the output already existed:
// the --json document, or under --quiet the existing path on stdout so
// `out=$(waxtap ... --quiet)` yields a path for a skip as it does for a write, or
// the human line on stderr. An empty path takes the pathless forms, which is
// download's archive skip. JSON wins over quiet, matching emitResult.
func emitSkip(env *appEnv, reason, path string) error {
	if env.jsonMode() {
		return env.emitJSON(skipJSON{schemaVersion, reason, displayPath(path), env.notesJSON()})
	}
	if path == "" {
		env.info("skipped (%s)\n", reason)
		return nil
	}
	if env.quiet() {
		env.printf("%s\n", displayPath(path))
		return nil
	}
	env.info("skipped (%s): %s\n", reason, displayPath(path))
	return nil
}

// wroteNothing reports whether res is a measurement that produced no file. A
// measure-only run streaming to stdout (download -o -) also has an empty
// OutputPath but did deliver the audio, so audioStream excludes it.
func wroteNothing(env *appEnv, res *waxtap.Result) bool {
	return measureOnly(res) && res.OutputPath == "" && !env.audioStream
}

// displayPath normalizes a local path for reporting, so one document does not
// mix separators: paths WaxTap builds use the OS separator while paths echoed
// from user input keep whatever was typed. Apply it where a path is reported,
// never where one is parsed, so error messages still quote what the user gave.
//
// It converts separators and nothing else. filepath.Clean would also resolve
// ".." lexically, which is wrong for a path a script is meant to open: through
// a symlinked directory "links/../real/out.mp3" and "real/out.mp3" name
// different files, and --quiet stdout and --json outputPath are exactly that
// kind of path.
//
// It guards rather than assumes. "" and "-" (the stdout sink) pass through, and
// so does anything that parses as an absolute URL, since FromSlash would rewrite
// "https://x/y" to "https:\\x\y" on Windows. Result.InputPath is local-only
// today, but this must not depend on that staying true.
func displayPath(p string) string {
	if p == "" || p == "-" {
		return p
	}
	// A single-letter scheme is a Windows drive letter ("C:/out/x.flac"), not a URL.
	if u, err := url.Parse(p); err == nil && u.IsAbs() && len(u.Scheme) > 1 {
		return p
	}
	return filepath.FromSlash(p)
}

// measureOnly reports whether res is a pure loudness measurement: loudness was
// measured and no transcode, cut, or normalization altered the media. Callers add
// their own OutputPath check, since a measure-only run may write an unaltered copy
// (download --measure-loudness) or nothing at all (normalize --measure-loudness).
func measureOnly(res *waxtap.Result) bool {
	return res.LoudnessMeasured && !res.Transcoded && !res.CutApplied && !res.LoudnessApplied
}

func renderResultHuman(env *appEnv, res *waxtap.Result) {
	// Quiet mode prints only the output path to stdout and routes warnings to
	// stderr, so callers can capture the path directly. A measure-only run has an
	// empty OutputPath and prints nothing.
	if env.quiet() {
		if res.OutputPath != "" {
			env.printf("%s\n", displayPath(res.OutputPath))
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(env.errOut, "warning: [%s] %s\n", w.Code, w.Detail)
		}
		return
	}
	switch res.SourceKind {
	case waxtap.SourceLocalFile:
		env.printf("Input:    %s\n", displayPath(res.InputPath))
	default:
		if res.Title != "" {
			env.printf("Title:    %s\n", res.Title)
		}
		env.printf("Video ID: %s\n", res.VideoID)
	}
	// A measure-only run that sank to io.Discard (normalize --measure-loudness) has
	// no output path and meaningless OutputBytes, so name the intent instead of a
	// phantom write.
	measured := wroteNothing(env, res)
	switch {
	case measured:
		env.printf("Output:   none (measurement only)\n")
	case res.OutputPath != "":
		env.printf("Output:   %s\n", displayPath(res.OutputPath))
	default:
		env.printf("Output:   (streamed)\n")
	}

	env.printf("Source:   %s\n", formatLabel(res.SourceFormat))
	if res.Client != "" {
		env.printf("Client:   %s%s\n", res.Client, watchPageSuffix(res.ViaWatchPage))
	}
	if res.Transcoded {
		env.printf("Encoded:  %s\n", formatLabel(res.OutputFormat))
	}
	switch {
	case measured:
		env.printf("Size:     %s analyzed\n", humanBytes(res.SourceBytes))
	case res.SourceBytes > 0 || res.OutputBytes > 0:
		env.printf("Size:     %s in, %s out\n", humanBytes(res.SourceBytes), humanBytes(res.OutputBytes))
	}

	if effects := effectSummary(res); effects != "" {
		env.printf("Applied:  %s\n", effects)
	}
	if res.TagCarry != nil {
		env.printf("Metadata: %s\n", carrySummary(res.TagCarry))
	}
	if res.Loudness != nil {
		renderLoudness(env, res)
	}
	// Warnings were already printed live on stderr during the non-quiet run.
}

// renderLoudness prints the measurement block. A non-finite figure renders as
// "n/a", which says nothing on its own, so each one is followed by the reason
// the library recorded for it.
func renderLoudness(env *appEnv, res *waxtap.Result) {
	l := res.Loudness
	if l.Input != nil {
		env.printf("Loudness: input %s LUFS, true-peak %s dBTP, LRA %s\n",
			humanLUFS(l.Input.IntegratedLUFS), humanLUFS(l.Input.TruePeakDBTP), humanLUFS(l.Input.LRA))
		if nonFinite(l.Input.IntegratedLUFS) {
			env.printf("          (%s)\n", unmeasurableNote(res, "input"))
		}
	}
	if l.Output != nil {
		if res.LoudnessApplied {
			env.printf("          output %s LUFS (target %s, gain %+.1f dB)\n", humanLUFS(l.Output.IntegratedLUFS), humanLUFS(l.Target), l.GainDB)
		} else {
			env.printf("          output %s LUFS (target %s)\n", humanLUFS(l.Output.IntegratedLUFS), humanLUFS(l.Target))
		}
		if l.HeaderGain {
			env.printf("          gain written to the Opus header; packets copied untouched\n")
		}
		if nonFinite(l.Output.IntegratedLUFS) {
			env.printf("          (%s)\n", unmeasurableNote(res, "output"))
		}
	}
}

// unmeasurableNote returns the library's explanation for side's unusable
// figure. The warning already reads as a sentence naming its side, so it is
// echoed whole rather than rebuilt here, which keeps one wording for the two
// surfaces to drift apart in.
//
// The fallback covers a result carrying a non-finite figure with no warning to
// go with it: older library versions, and a caller that filtered the warnings.
// It says less, and it never guesses a cause.
func unmeasurableNote(res *waxtap.Result, side string) string {
	for _, w := range res.Warnings {
		if w.Code == waxtap.WarnLoudnessUnmeasurable && strings.HasPrefix(w.Detail, side+" ") {
			return w.Detail
		}
	}
	return side + " integrated loudness could not be measured"
}

// carrySummary words a metadata carry as one line, the receipt beside the
// warning: what landed, counted by kind, then what was dropped or left off,
// and what a cut removed. The warning already explains a loss; this names it.
func carrySummary(tc *waxtap.TagCarry) string {
	if tc.Error != "" {
		return "none carried (see warning)"
	}
	var tags, tagsDropped, tagsLeftOff int
	landedSets := map[waxtap.CarryKind]int{}
	droppedSets := map[waxtap.CarryKind]bool{}
	var removed []string
	for _, it := range tc.Items {
		switch {
		case it.Kind == waxtap.CarryField:
			switch it.Disposition {
			case waxtap.DispositionCarried, waxtap.DispositionDowngraded:
				tags++
			case waxtap.DispositionDropped:
				tagsDropped++
			case waxtap.DispositionExcluded:
				tagsLeftOff++
			}
		case it.Disposition == waxtap.DispositionDropped:
			droppedSets[it.Kind] = true
		case it.Disposition == waxtap.DispositionRemoved:
			// Nothing landed; the removal is counted below.
		default:
			landedSets[it.Kind] += it.Count
		}
		if it.Removed > 0 {
			removed = append(removed, carryCount(it.Removed, removedNoun(it.Kind)))
		}
	}
	var landed, lost []string
	if tags > 0 {
		landed = append(landed, carryCount(tags, "tag"))
	}
	if n := landedSets[waxtap.CarryPictures]; n > 0 {
		landed = append(landed, carryCount(n, "picture"))
	}
	if n := landedSets[waxtap.CarryChapters]; n > 0 {
		landed = append(landed, carryCount(n, "chapter"))
	}
	if landedSets[waxtap.CarrySyncedLyrics] > 0 {
		landed = append(landed, "synced lyrics")
	}
	if tagsDropped > 0 {
		lost = append(lost, carryCount(tagsDropped, "tag"))
	}
	for _, k := range []waxtap.CarryKind{waxtap.CarryPictures, waxtap.CarryChapters, waxtap.CarrySyncedLyrics} {
		if droppedSets[k] {
			lost = append(lost, setNoun(k))
		}
	}
	line := "none carried"
	if len(landed) > 0 {
		line = strings.Join(landed, ", ") + " carried"
	}
	if len(lost) > 0 {
		line += "; " + strings.Join(lost, ", ") + " dropped"
	}
	if tagsLeftOff > 0 {
		line += "; " + carryCount(tagsLeftOff, "tag") + " left off"
	}
	if len(removed) > 0 {
		line += " (" + strings.Join(removed, ", ") + " removed by the cut)"
	}
	return line
}

// carryCount is "1 tag" / "2 tags", for the nouns carrySummary counts.
func carryCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// setNoun names a metadata set the way the warning does.
func setNoun(k waxtap.CarryKind) string {
	switch k {
	case waxtap.CarryPictures:
		return "pictures"
	case waxtap.CarryChapters:
		return "chapters"
	}
	return "synced lyrics"
}

// removedNoun names what a cut removes from a set: whole chapters, or the
// lines of a synced-lyrics set.
func removedNoun(k waxtap.CarryKind) string {
	if k == waxtap.CarryChapters {
		return "chapter"
	}
	return "lyric line"
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

type loudnessInfoJSON struct {
	IntegratedLUFS jsonFloat `json:"integratedLufs"`
	TruePeakDBTP   jsonFloat `json:"truePeakDbtp"`
	LRA            jsonFloat `json:"lra"`
	SamplePeakDB   jsonFloat `json:"samplePeakDb"`
}

type loudnessJSON struct {
	Input  *loudnessInfoJSON `json:"input,omitempty"`
	Output *loudnessInfoJSON `json:"output,omitempty"`
	Target jsonFloat         `json:"target"`
	// GainDB is the gain the delivered file carries, present when the run
	// applied one. HeaderGain says it rode in the Opus header with the
	// packets copied untouched.
	GainDB     *jsonFloat `json:"gainDb,omitempty"`
	HeaderGain bool       `json:"headerGain,omitempty"`
}

type warningJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// tagCarryJSON is the --json view of a local process's metadata carry: one
// item per tag field and per set, in the library's stable spellings. The key
// is absent when no carry ran; error stands in for the items when the carry
// itself failed.
type tagCarryJSON struct {
	Items []carryItemJSON `json:"items,omitempty"`
	Error string          `json:"error,omitempty"`
}

type carryItemJSON struct {
	Kind        string `json:"kind"`
	Key         string `json:"key,omitempty"`
	Count       int    `json:"count"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	Removed     int    `json:"removed,omitempty"`
}

func tagCarryToJSON(tc *waxtap.TagCarry) *tagCarryJSON {
	if tc == nil {
		return nil
	}
	out := &tagCarryJSON{Error: tc.Error}
	for _, it := range tc.Items {
		out.Items = append(out.Items, carryItemJSON{
			Kind:        it.Kind.String(),
			Key:         it.Key,
			Count:       it.Count,
			Disposition: it.Disposition.String(),
			Reason:      it.Reason,
			Removed:     it.Removed,
		})
	}
	return out
}

type resultJSON struct {
	SchemaVersion int    `json:"schemaVersion"`
	SourceKind    string `json:"sourceKind"`
	VideoID       string `json:"videoId,omitempty"`
	Title         string `json:"title,omitempty"`
	InputPath     string `json:"inputPath,omitempty"`
	OutputPath    string `json:"outputPath,omitempty"`
	Client        string `json:"client,omitempty"`
	ViaWatchPage  bool   `json:"viaWatchPage,omitempty"`

	// SourceFormat is always present. OutputFormat is omitted for unchanged local
	// sources, matching the human summary's "Encoded:" line. Interfaces allow
	// YouTube sources to emit formatJSON and local sources to emit localFormatJSON.
	SourceFormat any `json:"sourceFormat,omitempty"`
	OutputFormat any `json:"outputFormat,omitempty"`

	SourceBytes int64 `json:"sourceBytes"`
	OutputBytes int64 `json:"outputBytes"`

	Transcoded          bool `json:"transcoded"`
	CutApplied          bool `json:"cutApplied"`
	SponsorBlockApplied bool `json:"sponsorBlockApplied"`
	LoudnessMeasured    bool `json:"loudnessMeasured"`
	LoudnessApplied     bool `json:"loudnessApplied"`

	Loudness *loudnessJSON `json:"loudness,omitempty"`
	// TagCarry itemizes a local process's metadata carry; see tagCarryJSON.
	TagCarry *tagCarryJSON `json:"tagCarry,omitempty"`
	Warnings []warningJSON `json:"warnings,omitempty"`
	// Notes are the run's note: diagnostics, in the same {code, detail} shape as
	// Warnings. They were stderr-only and --quiet-gated, so --json --quiet, the
	// combination a script is most likely to use, saw none of them.
	Notes []noteJSON `json:"notes,omitempty"`
}

func resultToJSON(res *waxtap.Result) resultJSON {
	out := resultJSON{
		SchemaVersion:       schemaVersion,
		SourceKind:          res.SourceKind.String(),
		VideoID:             res.VideoID,
		Title:               res.Title,
		InputPath:           displayPath(res.InputPath),
		OutputPath:          displayPath(res.OutputPath),
		Client:              res.Client,
		ViaWatchPage:        res.ViaWatchPage,
		SourceBytes:         res.SourceBytes,
		OutputBytes:         res.OutputBytes,
		Transcoded:          res.Transcoded,
		CutApplied:          res.CutApplied,
		SponsorBlockApplied: res.SponsorBlockApplied,
		LoudnessMeasured:    res.LoudnessMeasured,
		LoudnessApplied:     res.LoudnessApplied,
	}
	out.SourceFormat, out.OutputFormat = formatDTOs(res)
	out.TagCarry = tagCarryToJSON(res.TagCarry)
	if res.Loudness != nil {
		lj := &loudnessJSON{Target: jsonFloat(res.Loudness.Target)}
		lj.Input = loudnessInfoToJSON(res.Loudness.Input)
		lj.Output = loudnessInfoToJSON(res.Loudness.Output)
		if res.LoudnessApplied {
			g := jsonFloat(res.Loudness.GainDB)
			lj.GainDB, lj.HeaderGain = &g, res.Loudness.HeaderGain
		}
		out.Loudness = lj
	}
	for _, w := range res.Warnings {
		out.Warnings = append(out.Warnings, warningJSON{Code: w.Code.String(), Detail: w.Detail})
	}
	return out
}

// localFormatJSON is the --json view of a local-file format. Local probes record
// only codec and extension, so network-only formatJSON fields are omitted.
type localFormatJSON struct {
	Codec     string `json:"codec"`
	Extension string `json:"extension,omitempty"`
}

func localFormatToJSON(f waxtap.Format) localFormatJSON {
	return localFormatJSON{Codec: f.Codec, Extension: f.Extension}
}

// formatDTOs chooses the JSON shape for sourceFormat and outputFormat. It returns
// nil for omitted outputFormat so omitempty removes the field instead of encoding
// null.
func formatDTOs(res *waxtap.Result) (src, out any) {
	if res.SourceKind == waxtap.SourceLocalFile {
		src = localFormatToJSON(res.SourceFormat)
		if res.Transcoded {
			out = localFormatToJSON(res.OutputFormat)
		}
		return src, out
	}
	src = formatToJSON(res.SourceFormat)
	out = formatToJSON(res.OutputFormat)
	return src, out
}

func loudnessInfoToJSON(l *waxtap.LoudnessInfo) *loudnessInfoJSON {
	if l == nil {
		return nil
	}
	return &loudnessInfoJSON{
		IntegratedLUFS: jsonFloat(l.IntegratedLUFS),
		TruePeakDBTP:   jsonFloat(l.TruePeakDBTP),
		LRA:            jsonFloat(l.LRA),
		SamplePeakDB:   jsonFloat(l.SamplePeakDB),
	}
}
