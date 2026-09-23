package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxtap/v3"
)

func newSplitCmd() *cobra.Command {
	var (
		sheetPath    string
		format       string
		bitrate      int
		bitDepth     int
		dir          string
		collisionStr string
	)
	cmd := &cobra.Command{
		Use:   "split <rip>",
		Short: "Split a single-file rip by its CUE sheet",
		Long: "Cut one rip into a file per track at the sheet's INDEX 01 positions, exact\n" +
			"to the CD frame (1/75 s). Pieces are named NN - Title.ext and tagged from\n" +
			"the sheet (title, performer, track numbers, disc title as album, REM\n" +
			"DATE/GENRE, CATALOG, ISRC), with the rip's own tags and cover art carried\n" +
			"underneath. Audio before the first track's INDEX 01 becomes 00 - Hidden\n" +
			"Track rather than being folded into track 1 or dropped, unless it follows a\n" +
			"data track in a FILE of its own, when it is that track's pregap and is\n" +
			"skipped. A data track (TRACK 01 MODE1/2352 on a mixed-mode disc) is never\n" +
			"written as audio: it is skipped with a note and the audio keeps the disc's\n" +
			"own numbering. A sheet indexing its audio against several files is refused,\n" +
			"since its tracks are already separate; a FILE holding no audio, as EAC and\n" +
			"XLD write the data track, is not one of them.\n\n" +
			"--cue names the sheet, defaulting to one beside the rip with the rip's own\n" +
			"stem. A split always decodes, so --format names the encoder; it is\n" +
			"inferred from the rip's extension only when that is a lossless one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			rip := args[0]
			if !isLocalFile(rip) {
				return usagef("split works on local files; %q is not one", rip)
			}
			if fi, serr := os.Stat(rip); serr == nil && fi.IsDir() {
				return usagef("split takes one rip, not a directory")
			}
			if err := rejectEmptyFlags(cmd, "cue", "dir", "format"); err != nil {
				return err
			}

			sheetAt := sheetPath
			if sheetAt == "" {
				beside := strings.TrimSuffix(rip, filepath.Ext(rip)) + ".cue"
				if fi, serr := os.Stat(beside); serr != nil || fi.IsDir() {
					return usagef("pass --cue; no sheet named %s sits beside the rip", filepath.Base(beside))
				}
				sheetAt = beside
			}
			sheet, err := os.ReadFile(sheetAt)
			if errors.Is(err, fs.ErrNotExist) {
				// A mistyped --cue is a request problem, not a failing disk.
				return usagef("no cue sheet at %s", displayPath(sheetAt))
			}
			if err != nil {
				return err
			}

			tf, err := splitFormat(format, rip)
			if err != nil {
				return err
			}
			mc, err := collisionFor(cmd, collisionStr)
			if err != nil {
				return err
			}
			if mc == collisionSkip {
				return usagef("--collision skip is not supported with split; a partly written set is worse than a refusal")
			}
			outDir := dir
			if outDir == "" {
				outDir = filepath.Dir(rip)
			}
			if err := rejectDirIsFile(outDir); err != nil {
				return err
			}

			plan, err := env.client.PlanSplit(cmd.Context(), rip, sheet)
			if err != nil {
				return err
			}
			outs := make([]string, len(plan.Pieces))
			for i, p := range plan.Pieces {
				out, _, cerr := resolveCollision(filepath.Join(outDir, pieceName(p)+"."+transcodeExt(tf)), mc)
				if cerr != nil {
					return cerr
				}
				// Two names apart only in case are one file on NTFS and APFS,
				// where the later piece would replace the earlier: refused on
				// every system, so a set is the same set everywhere.
				for j, prev := range outs[:i] {
					if strings.EqualFold(prev, out) {
						return usagef("tracks %d and %d both map to %q; their names differ only in case or in characters a filename cannot hold", plan.Pieces[j].Track, p.Track, displayPath(out))
					}
				}
				outs[i] = out
			}
			if err := noteKnobFlags(env, tf, bitrate, bitDepth); err != nil {
				return err
			}

			res, err := env.client.Split(cmd.Context(), plan, outs, waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate, BitDepth: bitDepth})
			if err != nil {
				return err
			}
			// Said once the set exists: a refused split skipped nothing. The
			// sheet may name another file and still describe this rip (one
			// written beside a .wav, used on a .flac of it), so the mismatch
			// is a remark, not a refusal.
			if base := sheetFileBase(plan.SheetFile); base != "" && base != filepath.Base(rip) {
				env.note(noteCueFileMismatch, "the sheet names %q; splitting %q by it", base, filepath.Base(rip))
			}
			for _, s := range plan.Skipped {
				env.note(noteCueDataTrack, "%s", skipNote(s))
			}
			return emitSplit(env, plan, res, sheetAt)
		},
	}
	f := cmd.Flags()
	f.StringVar(&sheetPath, "cue", "", "CUE sheet to split by (default: the sheet beside the rip)")
	f.StringVarP(&format, "format", "f", "", "output format: "+formatChoices(false)+formatSpellingNote)
	bindBitrateFlag(f, &bitrate)
	bindBitDepthFlag(f, &bitDepth)
	f.StringVarP(&dir, "dir", "d", "", "output directory (default: the rip's own)")
	bindCollisionFlag(f, &collisionStr)
	return cmd
}

// splitFormat resolves the piece format: --format, else the rip's own extension
// when that names a lossless encoder. A split always decodes the rip, so a
// lossy rip has to say which generation it accepts rather than silently taking
// another one.
func splitFormat(format, rip string) (waxtap.TranscodeFormat, error) {
	if format != "" {
		tf, err := parseTranscodeFormat(format)
		if err != nil {
			return 0, err
		}
		if tf == waxtap.FormatCopy {
			return 0, usagef("a split decodes the rip; pass a real format such as flac")
		}
		return tf, nil
	}
	ext := strings.TrimPrefix(filepath.Ext(rip), ".")
	tf, err := parseTranscodeFormat(ext)
	if err != nil || tf == waxtap.FormatCopy || !isLosslessFormat(tf) {
		return 0, usagef("a split re-encodes the rip; pass --format (flac keeps a lossless rip lossless)")
	}
	return tf, nil
}

// sheetFileBase is the file name a sheet's FILE line points at. The sheet is a
// document, not this platform's file system: one written on Windows spells its
// path with backslashes, which filepath.Base does not split on Unix. An empty
// or separator-only name yields "".
func sheetFileBase(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// pieceName is the output stem of a piece: the track number, zero-padded to two
// digits, and the sheet's title, sanitized the way every other name is. A
// lead-in piece has no track and no title, unless the sheet wrote it as TRACK
// 00 and titled it.
func pieceName(p waxtap.SplitPiece) string {
	title := p.Title
	switch {
	case title != "":
	case p.Track == 0:
		title = "Hidden Track"
	default:
		title = fmt.Sprintf("Track %02d", p.Track)
	}
	return truncateBytes(fmt.Sprintf("%02d - %s", p.Track, sanitizeStem(title)), maxStemBytes)
}

// skipNote says what a split left unwritten and why. The datatype is a token
// off the sheet, so it is quoted and bounded: the line reaches a terminal.
func skipNote(s waxtap.SplitSkip) string {
	span := humanDuration(s.Start) + " to " + humanDuration(s.End) + " of the rip"
	switch {
	case s.Type == "":
		return "skipped the pregap after the disc's data track, " + span + ", which is in the data track's mode, not audio"
	case s.StartSample == s.EndSample:
		return fmt.Sprintf("skipped TRACK %02d %q, a data track the rip holds none of; the audio keeps the disc's numbering", s.Track, clipToken(s.Type, 32))
	}
	return fmt.Sprintf("skipped TRACK %02d %q, a data track, not audio: %s", s.Track, clipToken(s.Type, 32), span)
}

// clipToken bounds a token from the sheet for a message: cut to n bytes on a
// rune boundary, with an ellipsis for what was cut.
func clipToken(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

type splitAlbumJSON struct {
	Title      string `json:"title,omitempty"`
	Performer  string `json:"performer,omitempty"`
	Catalog    string `json:"catalog,omitempty"`
	Date       string `json:"date,omitempty"`
	Genre      string `json:"genre,omitempty"`
	Comment    string `json:"comment,omitempty"`
	DiscNumber string `json:"discNumber,omitempty"`
	DiscTotal  string `json:"discTotal,omitempty"`
}

type splitPieceJSON struct {
	Track     int           `json:"track"`
	Title     string        `json:"title,omitempty"`
	Performer string        `json:"performer,omitempty"`
	ISRC      string        `json:"isrc,omitempty"`
	Start     float64       `json:"start"` // seconds into the rip
	Output    string        `json:"output"`
	TagCarry  *tagCarryJSON `json:"tagCarry,omitempty"`
}

// splitSkipJSON is one entry of a split document's skipped list: a track the
// sheet lists that no piece was written for, with its span of the rip in
// seconds when the rip holds any of it.
type splitSkipJSON struct {
	Track int      `json:"track"`
	Type  string   `json:"type,omitempty"`
	Start *float64 `json:"start,omitempty"`
	End   *float64 `json:"end,omitempty"`
}

// emitSplit renders the one document a split produces.
func emitSplit(env *appEnv, plan *waxtap.SplitPlan, res *waxtap.SplitResult, sheetAt string) error {
	pieces := make([]splitPieceJSON, len(plan.Pieces))
	for i, p := range plan.Pieces {
		pieces[i] = splitPieceJSON{
			Track:     p.Track,
			Title:     p.Title,
			Performer: p.Performer,
			ISRC:      p.ISRC,
			Start:     p.Start.Seconds(),
			Output:    displayPath(res.Outputs[i]),
		}
		if i < len(res.TagCarry) {
			pieces[i].TagCarry = tagCarryToJSON(res.TagCarry[i])
		}
	}
	if env.jsonMode() {
		warns := make([]warningJSON, len(res.Warnings))
		for i, w := range res.Warnings {
			warns[i] = warningJSON{Code: w.Code.String(), Detail: w.Detail}
		}
		skips := make([]splitSkipJSON, len(plan.Skipped))
		for i, s := range plan.Skipped {
			skips[i] = splitSkipJSON{Track: s.Track, Type: s.Type}
			if s.StartSample != s.EndSample {
				start, end := s.Start.Seconds(), s.End.Seconds()
				skips[i].Start, skips[i].End = &start, &end
			}
		}
		return env.emitJSON(struct {
			SchemaVersion int              `json:"schemaVersion"`
			Input         string           `json:"input"`
			Cue           string           `json:"cue"`
			Album         splitAlbumJSON   `json:"album"`
			TrackTotal    int              `json:"trackTotal"`
			Pieces        []splitPieceJSON `json:"pieces"`
			Skipped       []splitSkipJSON  `json:"skipped,omitempty"`
			Warnings      []warningJSON    `json:"warnings,omitempty"`
			Notes         []noteJSON       `json:"notes,omitempty"`
		}{
			schemaVersion, displayPath(plan.Input), displayPath(sheetAt),
			splitAlbumJSON{
				Title:      plan.Album.Title,
				Performer:  plan.Album.Performer,
				Catalog:    plan.Album.Catalog,
				Date:       plan.Album.Date,
				Genre:      plan.Album.Genre,
				Comment:    plan.Album.Comment,
				DiscNumber: plan.Album.DiscNumber,
				DiscTotal:  plan.Album.DiscTotal,
			},
			plan.TrackTotal, pieces, skips, warns, env.notesJSON(),
		})
	}
	for _, w := range res.Warnings {
		env.info("warning: [%s] %s\n", w.Code, w.Detail)
	}
	if env.quiet() {
		// --quiet is the path-only contract every other command keeps: one
		// output path per line and nothing else on stdout, so a script can
		// read the set a split wrote.
		for _, p := range res.Outputs {
			env.printf("%s\n", displayPath(p))
		}
		return nil
	}
	// The disc's count, and how many of its tracks were written when that is
	// fewer: a data track skipped, or a sheet that numbers past what it lists.
	tracks := countOf(plan.TrackTotal, "track")
	if written := plan.TrackCount(); written < plan.TrackTotal {
		tracks = fmt.Sprintf("%d of %s", written, tracks)
	}
	env.printf("Split:    %s by %s (%s)\n\n", displayPath(plan.Input), displayPath(sheetAt), tracks)
	tw := tabwriter.NewWriter(env.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSTART\tTITLE\tMETADATA\tOUTPUT")
	for i, p := range plan.Pieces {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", p.Track, humanDuration(p.Start), pieceTitle(p), albumCarryCell(res.TagCarry, i), displayPath(res.Outputs[i]))
	}
	return tw.Flush()
}

// pieceTitle is the TITLE cell: the sheet's title, or what the piece is when it
// has none.
func pieceTitle(p waxtap.SplitPiece) string {
	switch {
	case p.Title != "":
		return p.Title
	case p.Track == 0:
		return "(hidden track)"
	}
	return "-"
}
