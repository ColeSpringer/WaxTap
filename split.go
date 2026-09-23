package waxtap

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// The sheet parser only. It is a standalone package with no engine type in
	// its API; everything that touches the engine goes through internal/media,
	// as the rest of this package does.
	"github.com/colespringer/waxflow/cue"
	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// SplitPlan is what a CUE sheet says about dividing one rip: where each piece
// starts and ends in samples, the metadata the sheet carries for it, and what
// the sheet lists that no piece is written for. It is read-only arithmetic, so
// a caller can show it (or name the outputs from it) before any audio is
// written.
type SplitPlan struct {
	Input string
	// SheetFile is the FILE the sheet names, as spelled. A sheet that names
	// another file may still describe this rip (a sheet written beside a .wav
	// and used on a .flac of it), so it is reported rather than enforced.
	SheetFile string
	// Rate is the source's sample rate, which the sheet's CD frames were
	// converted at.
	Rate int
	// Duration is the rip's length as this plan measured it, which is what a
	// piece reaching the end runs to and what a chapter remap resolves
	// against. It is the read length, not the header's claim, for a source
	// whose headers only claim one.
	Duration time.Duration
	Album    SplitAlbum
	// Pieces are what Split writes, in file order.
	Pieces []SplitPiece
	// Skipped is what the sheet lists that no piece is written for, in the
	// sheet's order: its data tracks, and the pregap after one kept in a FILE
	// of its own. Empty for a sheet of audio tracks alone.
	Skipped []SplitSkip
	// TrackTotal is what TRACKTOTAL states: the disc's own count, every
	// numbered TRACK the sheet lists in any of its files up to and including
	// the last audio one. A mixed-mode disc's audio is so numbered 2 and up of
	// a total that counts its data track, and an Enhanced CD's data track,
	// in a session no player counts, is left out. Audio ahead of track 1, the
	// lead-in or a TRACK 00, is not a track of the disc's.
	TrackTotal int
}

// SplitAlbum is the disc-level metadata a sheet carries.
type SplitAlbum struct {
	Title, Performer, Catalog string
	Date, Genre, Comment      string // REM DATE, REM GENRE, REM COMMENT
	DiscNumber, DiscTotal     string // REM DISCNUMBER, REM TOTALDISCS
}

// SplitPiece is one span of the rip and the metadata for it.
type SplitPiece struct {
	// Track is the sheet's TRACK number, 0 for audio before the first track's
	// INDEX 01: a pregap, or hidden track one audio, which is a song of its own
	// on some discs. It is kept as a piece rather than folded into track 1. A
	// sheet that writes that audio as TRACK 00 gets the same 0, with the title
	// it gave.
	Track int
	// Title is the track's TITLE, "" for the lead-in.
	Title string
	// Performer is the track's PERFORMER, else the disc's.
	Performer string
	ISRC      string
	// Start is the piece's position for display; the cut is made at
	// StartSample.
	Start time.Duration
	// StartSample and EndSample bound the piece, [StartSample, EndSample).
	// EndSample is ToEnd on a piece that runs to whatever the file holds,
	// which the last piece does unless a data track the rip holds follows it.
	StartSample, EndSample int64
}

// ToEnd is the EndSample of a piece, or of a skip, that runs to the rip's end.
const ToEnd = media.ToEnd

// TrackCount is the number of pieces that are tracks: the lead-in is not one,
// and no piece is written for a data track. It is how many of the disc's
// TrackTotal tracks the split writes.
func (p *SplitPlan) TrackCount() int {
	n := 0
	for _, piece := range p.Pieces {
		if piece.Track > 0 {
			n++
		}
	}
	return n
}

// SplitSkip is one thing a sheet lists that the split writes no piece for: a
// data track, which a mixed-mode disc carries ahead of its audio (TRACK 01
// MODE1/2352) and an Enhanced CD after it, or the pregap after a data track
// kept in a FILE of its own, which is in the data track's mode. Cut and written
// they would be noise named after a song; the audio keeps the disc's own
// numbering around them.
type SplitSkip struct {
	// Track is the sheet's TRACK number, 0 for the pregap after a data track
	// in another FILE, which no track names.
	Track int
	// Type is the track's datatype, uppercased (MODE1/2352), "" for that
	// pregap.
	Type string
	// Start, StartSample and EndSample bound the span the rip holds, as a
	// piece's do, and End is where it ends: the rip's own length for a span
	// that runs to the end. StartSample == EndSample is a track the rip holds
	// none of: one listed at frame 0 beside the audio, given a FILE of its
	// own, or lying past the rip's end.
	Start, End             time.Duration
	StartSample, EndSample int64
}

// pieceCut describes piece i as the one span of the rip it keeps, so a carry
// remaps the rip's chapters and synced lyrics onto the piece's own timeline.
func (p *SplitPlan) pieceCut(i int) *appliedCut {
	piece := p.Pieces[i]
	end := p.Duration
	if piece.EndSample != media.ToEnd {
		end = media.SampleTime(piece.EndSample, p.Rate)
	}
	if end <= piece.Start {
		return nil
	}
	return &appliedCut{keeps: []cutrange.Range{{Start: piece.Start, End: end}}, total: p.Duration}
}

// SplitResult reports a completed split.
type SplitResult struct {
	Outputs []string // in piece order
	// TagCarry itemizes each piece's metadata carry, in piece order; a nil
	// entry is a piece whose carry did not run (the rip had nothing to carry).
	TagCarry []*TagCarry
	Warnings []Warning
}

// PlanSplit reads a CUE sheet against a local rip and returns where the pieces
// fall. It writes nothing. Every refusal is about the pair: a sheet whose
// audio is indexed against several files describes a rip whose tracks are
// already separate, a one-track sheet divides nothing, a rip that holds no
// audio has nothing to divide, and a start past the file's length says the
// sheet does not describe this rip. A data track is not a refusal, whether the
// sheet lists it beside the audio or in a FILE of its own: it is a piece
// nothing writes, listed in Skipped, and the audio keeps the disc's numbering
// around it.
func (c *Client) PlanSplit(ctx context.Context, input string, sheet []byte) (*SplitPlan, error) {
	runner := c.engine()
	probe, err := runner.Probe(ctx, input)
	if err != nil {
		return nil, err
	}
	audio, ok := probe.AudioStream()
	if !ok {
		return nil, fmt.Errorf("%w: no audio stream in %s", waxerr.ErrUnsupportedInput, input)
	}
	if audio.SampleRate%cue.FramesPerSecond != 0 {
		// A sheet addresses CD frames, 1/75 s; a rate 75 does not divide cannot
		// place them on a sample, and a CUE sheet describes a CD rip.
		return nil, fmt.Errorf("%w: %d Hz is not a CD-family rate, so the sheet's frames do not land on samples", waxerr.ErrUnsupportedInput, audio.SampleRate)
	}
	// Read strictly: a line the parser cannot read is a refusal, since a line
	// skipped instead would move a cut without a word.
	sh, err := cue.Parse(sheet)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	f, err := sh.SingleFile()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrIncompatibleSpec, err)
	}
	// The length the pieces are checked against is what a read delivers: a
	// Xing MP3 rip declares its full count however short the file is, and a
	// split that trusted it would fail on a later piece with the earlier ones
	// already written. It is also what places a data track after the audio:
	// no demuxer delivers audio past the count it declares, so a data track
	// addressed past it is past the audio whatever the file holds behind it.
	samples := audio.Samples
	total := probe.Format.Duration
	if probe.LengthClaimed {
		length, lerr := runner.MeasureLength(ctx, input)
		if lerr != nil {
			return nil, lerr
		}
		samples, total = length.Samples, length.Duration
	}
	if samples == 0 {
		// Known, not unknown: the probe says -1 for a count it lacks, and a
		// lacking one was measured above.
		return nil, fmt.Errorf("%w: %s holds no audio frames, so there is nothing to split", waxerr.ErrUnsupportedInput, input)
	}
	// The one funnel every splitter divides a file by, so a sheet handed to
	// WaxFlow's own cuts at the same samples: the lead-in before track 1 is a
	// piece of its own, and a data track is a piece nothing writes.
	pieces, err := f.Pieces(audio.SampleRate, samples)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrIncompatibleSpec, err)
	}

	plan := &SplitPlan{Input: input, SheetFile: f.Name, Rate: audio.SampleRate, Duration: total, TrackTotal: trackTotal(sh)}
	plan.Album = SplitAlbum{Title: sh.Title, Performer: sh.Performer, Catalog: sh.Catalog}
	plan.Album.Date, _ = sh.Rem("DATE")
	plan.Album.Genre, _ = sh.Rem("GENRE")
	plan.Album.Comment, _ = sh.Rem("COMMENT")
	plan.Album.DiscNumber, _ = sh.Rem("DISCNUMBER")
	plan.Album.DiscTotal, _ = sh.Rem("TOTALDISCS")
	plan.Pieces, plan.Skipped = splitPieces(sh, f, pieces, audio.SampleRate, total)
	return plan, nil
}

// splitPieces pairs the file's pieces with the sheet's tracks: the pieces the
// split writes, in order, and everything the sheet lists that it does not, in
// the sheet's order, with the span the rip holds of each. total is the rip's
// length, where a span that runs to the end ends.
func splitPieces(sh *cue.Sheet, f *cue.File, pieces []cue.Piece, rate int, total time.Duration) (out []SplitPiece, skipped []SplitSkip) {
	// endOf is a piece's end as the plan states it: any negative To is the
	// file's end.
	endOf := func(c cue.Piece) int64 {
		if c.To < 0 {
			return ToEnd
		}
		return c.To
	}
	skipOf := func(c cue.Piece) SplitSkip {
		s := SplitSkip{Start: media.SampleTime(c.From, rate), StartSample: c.From, EndSample: endOf(c), End: total}
		if s.EndSample != ToEnd {
			s.End = media.SampleTime(s.EndSample, rate)
		}
		return s
	}
	// The data tracks the rip holds part of, by index in f, and the pregap
	// after a data track in an earlier FILE, which no track names.
	held := map[int]cue.Piece{}
	var pregap *cue.Piece
	// A lead-in ahead of a TRACK 00's INDEX 01 is that track's own pregap,
	// and there is no track before it for the audio to belong to, so it is
	// folded into track 0 rather than kept as a second numberless piece
	// that would take the same name.
	fold := false
	for i, c := range pieces {
		switch {
		case c.Audio && c.Track < 0 && i+1 < len(pieces) && pieces[i+1].Audio && pieces[i+1].Track == 0 && f.Tracks[0].Number == 0:
			fold = true
		case c.Audio:
			p := SplitPiece{Start: media.SampleTime(c.From, rate), StartSample: c.From, EndSample: endOf(c)}
			if c.Track >= 0 {
				t := f.Tracks[c.Track]
				p.Track, p.Title, p.ISRC = t.Number, t.Title, t.ISRC
				p.Performer = cmp.Or(t.Performer, sh.Performer)
			}
			if fold && c.Track == 0 {
				p.Start, p.StartSample = 0, 0
			}
			out = append(out, p)
		case c.Track >= 0:
			held[c.Track] = c
		default:
			pregap = &c
		}
	}
	for i := range sh.Files {
		file := &sh.Files[i]
		if file == f && pregap != nil {
			skipped = append(skipped, skipOf(*pregap))
		}
		for ti := range file.Tracks {
			t := &file.Tracks[ti]
			if t.IsAudio() {
				continue
			}
			s := SplitSkip{Track: t.Number, Type: t.Type}
			if c, ok := held[ti]; ok && file == f {
				s = skipOf(c)
				s.Track, s.Type = t.Number, t.Type
			}
			skipped = append(skipped, s)
		}
	}
	return out, skipped
}

// trackTotal is the disc's track count for SplitPlan.TrackTotal: the highest
// number among the sheet's tracks, in any FILE, up to and including the last
// audio one. The highest rather than a count, since a sheet may leave the
// data track out and number its audio from 02, or skip a number, and the disc
// has that many tracks either way.
func trackTotal(sh *cue.Sheet) int {
	high, total := 0, 0
	for _, f := range sh.Files {
		for _, t := range f.Tracks {
			high = max(high, t.Number)
			if t.IsAudio() {
				total = high
			}
		}
	}
	return total
}

// Split writes plan's pieces to outputs, one file per piece in plan order,
// re-encoding each under spec. Every piece decodes only its own span, so the
// whole split costs about one decode of the rip. The sheet's metadata is
// written onto each piece over the rip's own tags and cover art; a metadata
// failure warns and leaves the audio, the way a single file's carry does.
func (c *Client) Split(ctx context.Context, plan *SplitPlan, outputs []string, spec TranscodeSpec) (*SplitResult, error) {
	if plan == nil || len(plan.Pieces) == 0 {
		return nil, fmt.Errorf("%w: no split plan", waxerr.ErrIncompatibleSpec)
	}
	if len(outputs) != len(plan.Pieces) {
		return nil, fmt.Errorf("%w: %d outputs for %d pieces", waxerr.ErrIncompatibleSpec, len(outputs), len(plan.Pieces))
	}
	if spec.Format == FormatCopy {
		return nil, fmt.Errorf("%w: a split decodes the rip, so it needs a real format, not copy", waxerr.ErrIncompatibleSpec)
	}
	if err := validateBitrate(&spec); err != nil {
		return nil, err
	}
	if err := validateBitDepth(&spec); err != nil {
		return nil, err
	}
	codec := transcodeCodec(spec.Format)
	for i, out := range outputs {
		if out == "" {
			return nil, fmt.Errorf("%w: piece %d has no output path", waxerr.ErrIncompatibleSpec, i+1)
		}
		if sameFile(out, plan.Input) {
			return nil, fmt.Errorf("%w: piece %d would overwrite the rip at %q", waxerr.ErrIncompatibleSpec, i+1, out)
		}
		for j := i + 1; j < len(outputs); j++ {
			if sameFile(out, outputs[j]) || foldsTogether(out, outputs[j]) {
				return nil, fmt.Errorf("%w: pieces %d and %d share output %q, or differ in it only by case", waxerr.ErrIncompatibleSpec, i+1, j+1, out)
			}
		}
		if err := media.CheckOutputContainer(codec, out); err != nil {
			return nil, err
		}
	}

	runner := c.engine()
	res := &SplitResult{Outputs: make([]string, len(outputs)), TagCarry: make([]*TagCarry, len(outputs))}
	em := newEmitter(nil, "")
	var carry albumCarryWarns
	var levels albumLevels
	damaged := albumInputRemarks{code: WarnInputDamage}
	noted := albumInputRemarks{code: WarnInputNote}
	// One probe of the rip, for the facts every piece shares: the source codec
	// (a lossy rip's decode can overshoot, which the clipping policy suppresses)
	// and the engine's remarks on the file.
	in := probeAudio(ctx, runner, plan.Input)
	noted.observe(plan.Input, inputNote(in.notes))
	enc := media.Spec{Codec: codec, Bitrate: spec.Bitrate, BitDepth: spec.BitDepth}
	for i, out := range outputs {
		p := plan.Pieces[i]
		if err := ensureParentDir(out); err != nil {
			return nil, fmt.Errorf("waxtap.Split: piece %d (%s): %w", i+1, out, err)
		}
		sres, err := runner.RenderSpan(ctx, plan.Input, out, p.StartSample, p.EndSample, enc)
		if err != nil {
			return nil, fmt.Errorf("waxtap.Split: piece %d (%s): %w", i+1, out, err)
		}
		// The damage is the rip's, found again by each piece's read; naming the
		// piece is what tells the listener which span the read gave up in.
		damaged.observe(out, inputDamageNote(sres.InputWarnings))
		levels.observe(out, in.codec, sres.Levels)
		tem := newEmitter(nil, "")
		tc, terr := c.splitTags(ctx, plan, i, out, tem)
		if terr != nil {
			tem.warn(WarnTagCarry, fmt.Sprintf("could not write the sheet's metadata onto %s: %v", filepath.Base(out), terr))
		}
		res.TagCarry[i] = tc
		carry.observe(em, tem.collected())
		res.Outputs[i] = out
	}
	carry.warn(em)
	damaged.warn(em)
	noted.warn(em)
	levels.warn(em, PeakCap)
	res.Warnings = em.collected()
	return res, nil
}

// splitTags writes a piece's metadata: the rip's own tags and pictures carried
// first (the transfer, own-audio values dropped since the audio was
// re-encoded), then the sheet's fields on top. Per-track fields (title, ISRC,
// the numbering) are the sheet's whether or not it filled them in, since the
// rip's own describe the whole disc; the artist falls back to the disc's
// performer, which is the track's on an ordinary rip. Disc-level fields fill
// only where the rip carried none, since a tagged rip usually knows its album
// better than a REM line does.
func (c *Client) splitTags(ctx context.Context, plan *SplitPlan, i int, out string, em *emitter) (*TagCarry, error) {
	p := plan.Pieces[i]
	// The piece is one kept span of the rip, so the rip's chapters and synced
	// lyrics are remapped onto it the way a cut's are: shifted to the piece's
	// own timeline, dropped where the piece does not hold them. Carried as
	// they stand they would point at disc positions the piece never had.
	tc := c.carryTags(ctx, plan.Input, out, out, plan.pieceCut(i), ownAudioDrop, em)
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		return tc, err
	}
	ed := doc.Edit()
	set := func(k tag.Key, v string) {
		if v != "" {
			ed.Set(k, v)
		}
	}
	fill := func(k tag.Key, v string) {
		if _, ok := doc.Get(k); !ok {
			set(k, v)
		}
	}
	// Per-track fields are the sheet's whether or not it filled them in: the
	// rip's own TITLE and ISRC describe the whole disc, so keeping them on a
	// piece would label every track with the album's name.
	take := func(k tag.Key, v string) {
		if v == "" {
			ed.Clear(k)
			return
		}
		ed.Set(k, v)
	}
	take(tag.Title, p.Title)
	take(tag.ISRC, p.ISRC)
	if p.Track == 0 {
		// Audio ahead of track 1 is no track of the disc's, so it has no
		// number, whether the sheet left it unnamed (the lead-in) or wrote it
		// as TRACK 00; a title it gave stays, as any track's does.
		ed.Clear(tag.TrackNumber).Clear(tag.TrackTotal)
	} else {
		set(tag.TrackNumber, strconv.Itoa(p.Track))
		// A plan that states no total clears the rip's own, which described a
		// single file and under the sheet's numbers would read "2 of 1".
		total := ""
		if plan.TrackTotal > 0 {
			total = strconv.Itoa(plan.TrackTotal)
		}
		take(tag.TrackTotal, total)
	}
	set(tag.Artist, p.Performer)
	fill(tag.Album, plan.Album.Title)
	fill(tag.AlbumArtist, plan.Album.Performer)
	fill(tag.CatalogNumber, plan.Album.Catalog)
	fill(tag.RecordingDate, plan.Album.Date)
	fill(tag.Genre, plan.Album.Genre)
	fill(tag.Comment, plan.Album.Comment)
	fill(tag.DiscNumber, plan.Album.DiscNumber)
	fill(tag.DiscTotal, plan.Album.DiscTotal)
	planned, err := ed.Prepare()
	if err != nil {
		return tc, err
	}
	_, _, err = executeSaveBack(ctx, planned)
	return tc, err
}

// foldsTogether reports whether two output paths would be one file on a file
// system that folds case, as NTFS and APFS do. A split refuses the pair on
// every system, so a set of pieces is the same set everywhere, and the later
// piece cannot replace the earlier where the fold applies. Normalization is
// not folded: two titles apart only in a composed accent are not a case a
// sheet produces.
func foldsTogether(a, b string) bool {
	pa, e1 := filepath.Abs(a)
	pb, e2 := filepath.Abs(b)
	return e1 == nil && e2 == nil && strings.EqualFold(pa, pb)
}
