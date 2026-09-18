package waxtap

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
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
// starts and ends in samples, and the metadata the sheet carries for it. It is
// read-only arithmetic, so a caller can show it (or name the outputs from it)
// before any audio is written.
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
	Pieces   []SplitPiece // in file order
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
	// on some discs. It is kept as a piece rather than folded into track 1.
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
	// EndSample is media.ToEnd on the last piece, which runs to whatever the
	// file holds.
	StartSample, EndSample int64
}

// TrackCount is the number of pieces that are tracks: the lead-in, when
// present, is not one, so it is what TRACKTOTAL states and what a caller
// counting the record's tracks wants.
func (p *SplitPlan) TrackCount() int {
	n := 0
	for _, piece := range p.Pieces {
		if piece.Track > 0 {
			n++
		}
	}
	return n
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
// fall. It writes nothing. Every refusal is about the pair: a sheet that
// indexes several files describes a rip whose tracks are already separate, a
// one-track sheet divides nothing, and a start past the file's length says the
// sheet does not describe this rip.
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
	sh, err := cue.Parse(sheet)
	if err != nil {
		return nil, fmt.Errorf("%w: cue sheet: %v", waxerr.ErrUnsupportedInput, err)
	}
	f, err := sh.SingleFile()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrIncompatibleSpec, err)
	}
	for _, t := range f.Tracks {
		if t.Type != "AUDIO" {
			return nil, fmt.Errorf("%w: track %d is %s, a data track; the sheet describes a mixed-mode disc", waxerr.ErrIncompatibleSpec, t.Number, t.Type)
		}
	}
	cuts, err := f.Cuts(audio.SampleRate)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrIncompatibleSpec, err)
	}
	// The length the cuts are checked against is what a read delivers: a Xing
	// MP3 rip declares its full count however short the file is, and a split
	// that trusted it would fail on a later piece with the earlier ones already
	// written.
	samples := audio.Samples
	total := probe.Format.Duration
	if probe.LengthClaimed {
		length, lerr := runner.MeasureLength(ctx, input)
		if lerr != nil {
			return nil, lerr
		}
		samples, total = length.Samples, length.Duration
	}
	if samples > 0 && cuts[len(cuts)-1] >= samples {
		return nil, fmt.Errorf("%w: the sheet's last track starts at sample %d, past the file's %d; the sheet does not describe this rip", waxerr.ErrIncompatibleSpec, cuts[len(cuts)-1], samples)
	}

	plan := &SplitPlan{Input: input, SheetFile: f.Name, Rate: audio.SampleRate, Duration: total}
	plan.Album = SplitAlbum{Title: sh.Title, Performer: sh.Performer, Catalog: sh.Catalog}
	plan.Album.Date, _ = sh.Rem("DATE")
	plan.Album.Genre, _ = sh.Rem("GENRE")
	plan.Album.Comment, _ = sh.Rem("COMMENT")
	plan.Album.DiscNumber, _ = sh.Rem("DISCNUMBER")
	plan.Album.DiscTotal, _ = sh.Rem("TOTALDISCS")
	// The pieces are [0,c0), [c0,c1), ..., [cn, end). cue.Cuts drops a first
	// start of 0 and keeps one past it, so the piece count is len(cuts)+1 and
	// the tracks pair by position, offset by one when the audio before track 1
	// is a piece of its own.
	starts := append([]int64{0}, cuts...)
	lead := len(starts) > len(f.Tracks)
	for i, start := range starts {
		p := SplitPiece{StartSample: start, EndSample: media.ToEnd, Start: media.SampleTime(start, audio.SampleRate)}
		if i+1 < len(starts) {
			p.EndSample = starts[i+1]
		}
		ti := i
		if lead {
			ti = i - 1
		}
		if ti >= 0 {
			t := f.Tracks[ti]
			p.Track, p.Title, p.ISRC = t.Number, t.Title, t.ISRC
			p.Performer = cmp.Or(t.Performer, sh.Performer)
		}
		plan.Pieces = append(plan.Pieces, p)
	}
	return plan, nil
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
			if sameFile(out, outputs[j]) {
				return nil, fmt.Errorf("%w: pieces %d and %d share output %q", waxerr.ErrIncompatibleSpec, i+1, j+1, out)
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
	if p.Track == 0 {
		// The lead-in is no track: it has no title, no number, and no ISRC.
		ed.Clear(tag.Title).Clear(tag.TrackNumber).Clear(tag.TrackTotal).Clear(tag.ISRC)
	} else {
		take(tag.Title, p.Title)
		take(tag.ISRC, p.ISRC)
		set(tag.TrackNumber, strconv.Itoa(p.Track))
		set(tag.TrackTotal, strconv.Itoa(plan.TrackCount()))
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
