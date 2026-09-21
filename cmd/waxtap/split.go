package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

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
			"Track rather than being folded into track 1 or dropped. A sheet indexing\n" +
			"several files is refused: its tracks are already separate.\n\n" +
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
			// The sheet may name another file and still describe this rip (one
			// written beside a .wav, used on a .flac of it), so the mismatch is
			// reported rather than refused.
			if base := sheetFileBase(plan.SheetFile); base != "" && base != filepath.Base(rip) {
				env.note(noteCueFileMismatch, "the sheet names %q; splitting %q by it", base, filepath.Base(rip))
			}

			outs := make([]string, len(plan.Pieces))
			seen := map[string]int{}
			for i, p := range plan.Pieces {
				out, _, cerr := resolveCollision(filepath.Join(outDir, pieceName(p)+"."+transcodeExt(tf)), mc)
				if cerr != nil {
					return cerr
				}
				if prev, dup := seen[out]; dup {
					return usagef("tracks %d and %d both map to %q; their titles differ only in characters a filename cannot hold", plan.Pieces[prev].Track, p.Track, displayPath(out))
				}
				seen[out] = i
				outs[i] = out
			}
			if err := noteKnobFlags(env, tf, bitrate, bitDepth); err != nil {
				return err
			}

			res, err := env.client.Split(cmd.Context(), plan, outs, waxtap.TranscodeSpec{Format: tf, Bitrate: bitrate, BitDepth: bitDepth})
			if err != nil {
				return err
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
// lead-in piece has no track and no title.
func pieceName(p waxtap.SplitPiece) string {
	title := p.Title
	switch {
	case p.Track == 0:
		title = "Hidden Track"
	case title == "":
		title = fmt.Sprintf("Track %02d", p.Track)
	}
	return truncateBytes(fmt.Sprintf("%02d - %s", p.Track, sanitizeStem(title)), maxStemBytes)
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
		return env.emitJSON(struct {
			SchemaVersion int              `json:"schemaVersion"`
			Input         string           `json:"input"`
			Cue           string           `json:"cue"`
			Album         splitAlbumJSON   `json:"album"`
			Pieces        []splitPieceJSON `json:"pieces"`
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
			pieces, warns, env.notesJSON(),
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
	env.printf("Split:    %s by %s (%s)\n\n", displayPath(plan.Input), displayPath(sheetAt), countOf(plan.TrackCount(), "track"))
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
