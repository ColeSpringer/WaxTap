package media

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// AnalyzeFile measures the loudness of a whole local file. channels, when 1 or 2,
// measures after folding to that channel count so a two-pass gain matches a later
// downmixing encode; 0 keeps the source layout. The loudness package maps the
// result to its own type; this keeps the WaxFlow engine and the concurrency bound
// in one place. The damage the read found is on the result's InputWarnings,
// complete as of the end of the file and in the source's own terms; see
// Result.InputWarnings.
func (r *Runner) AnalyzeFile(ctx context.Context, input string, channels int) (*waxflow.AnalyzeResult, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return nil, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()
	res, err := r.engine.Analyze(ctx, src, hintFor(input), waxflow.AnalyzeOptions{Channels: channels})
	if err != nil {
		// A cancellation is not bad input: preserve ctx.Err() so callers classify it
		// as canceled (exit 130), not unsupported-input (exit 2).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, classifyInputError(err, input)
	}
	res.InputWarnings = sourceWarnings(res.InputWarnings)
	return res, nil
}

// AnalyzeMedia measures the loudness of an already-open Media, so a cut/downmix
// composition is measured as it will be encoded. input names the file for error
// classification, the way AnalyzeFile's does: a genuine read failure then
// reports as *fs.PathError instead of bad input. A span that outruns a truncated
// file is not one of those: it is the file deviating from its headers, and it
// reports as unsupported input with no path (see classifyEngineError). Pass ""
// when med has no single file to name, a concatenated timeline with several
// members (an album's group pass). channels folds the measurement to a downmix
// target (0 keeps the source layout). The caller owns med.
//
// The damage the read found comes back on the result in the source's own
// terms; med is left open, the caller's to close.
func (r *Runner) AnalyzeMedia(ctx context.Context, med format.Media, input string, channels int) (*waxflow.AnalyzeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()
	res, err := r.engine.AnalyzeMedia(ctx, med, waxflow.AnalyzeOptions{Channels: channels})
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil {
		res.InputWarnings = sourceWarnings(res.InputWarnings)
	}
	// Analysis only ever fails on the media it is reading, so it takes the
	// input-side classification.
	return res, classifyInputError(err, input)
}

// AnalyzeGroup measures inputs as one programme: each member decoded once at
// channels[i] (its delivered width; 0 keeps its own) and the group's gates
// run over every member's blocks at once (waxflow.Engine.AnalyzeGroup). It
// returns the group figure and each member's own, with the damage each
// member's read found on that member's InputWarnings, in the source's own
// terms (see Result.InputWarnings). The members are decoded one after
// another inside the engine, so the call takes one concurrency slot for its
// whole length; an album of N tracks is N decodes.
//
// No seam and no envelope: a member whose headers only estimate its length
// (a WMA, a Matroska on its Info Duration) is measured to its end without a
// declaration to be held to, and a mono member is measured as mono rather
// than duplicated across a stereo pair.
//
// Each member is opened when the engine reaches it and closed before the
// next opens (waxflow.GroupMember.Open), so an album holds one descriptor
// at a time however long it is. Every input is opened and closed once
// before the first decode, so a member the filesystem refuses is reported
// without decoding the album ahead of it. An open that fails stops the run
// there. Its
// error names the member index in the wording the engine uses, so the album
// caller names the track from one pattern rather than two, and the cause
// travels as it is: a file the filesystem would not open is an I/O failure
// (exit 10), not bad input, and openFileMedia has already classified the
// refusals that are. The engine hands the same failure back annotated with
// its own code, under which classifyInputError would report a missing file
// as bad input, which is why the closure keeps the error itself.
func (r *Runner) AnalyzeGroup(ctx context.Context, inputs []string, channels []int) (*waxflow.AnalyzeResult, []waxflow.AnalyzeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, nil, err
	}
	defer r.release()
	// A member the filesystem will not hand over is worth finding before any
	// decoding starts: the engine reaches member N only after decoding the
	// N-1 before it, so a missing or unreadable last track would otherwise
	// cost the whole album to report. One open and close each, no headers
	// read, and the wording the failures below use. A file that disappears
	// after this still fails there, which is what that path is for.
	for i, in := range inputs {
		fi, err := os.Stat(in)
		if err != nil {
			return nil, nil, fmt.Errorf("group member %d: %w", i, err)
		}
		if !fi.Mode().IsRegular() {
			// A directory, a FIFO, a device: the engine's refusal to make,
			// as bad input rather than an I/O failure, and openFileMedia
			// below makes it by name. Not opened here, since Windows
			// refuses a directory open and unix does not.
			continue
		}
		f, err := os.OpenFile(in, os.O_RDONLY|container.OpenNonblock, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("group member %d: %w", i, err)
		}
		_ = f.Close()
	}
	open := r.openMember
	if open == nil {
		open = openFileMedia
	}
	var openErr error
	openFailed := -1
	members := make([]waxflow.GroupMember, len(inputs))
	for i, in := range inputs {
		ch := 0
		if i < len(channels) {
			ch = channels[i]
		}
		members[i] = waxflow.GroupMember{Channels: ch, Open: func() (format.Media, error) {
			m, err := open(in, hintFor(in))
			if err != nil {
				openErr, openFailed = err, i
			}
			return m, err
		}}
	}
	res, err := r.engine.AnalyzeGroup(ctx, members, waxflow.AnalyzeOptions{})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		if openErr != nil {
			return nil, nil, fmt.Errorf("group member %d: %w", openFailed, openErr)
		}
		// No file is named here: the member index is in the text, and the
		// album caller turns that into a track name.
		return nil, nil, classifyInputError(err, "")
	}
	for i := range res.Members {
		res.Members[i].InputWarnings = sourceWarnings(res.Members[i].InputWarnings)
	}
	return &res.Group, res.Members, nil
}

// Length is what a measurement of a file delivers: the frame count, its
// duration at the source rate, the damage the read found on the way, and the
// read's remarks on a file that is not damaged.
type Length struct {
	Samples  int64
	Duration time.Duration
	Warnings []string
	Notes    []string
	// Counted says the count came from decoding the whole file, the one route
	// left for a source whose headers and positions only state times (WMA
	// Pro, Voice and Lossless in ASF). A later decode of the same file that
	// delivers less is then the engine disagreeing with itself, not the file
	// holding less than it said; see Measured.
	Counted bool
}

// Measured is a source's length as a measurement found it, in the form a cut
// or a span is held to it: the count, and whether it came from decoding the
// file (Length.Counted). The zero value means nobody measured. A measurement
// of no samples is never carried in one: every caller refuses a file that
// holds no audio before it has a span to hold to it.
type Measured struct {
	Samples int64
	Counted bool
}

// Measured is the length as a cut carries it.
func (l Length) Measured() Measured { return Measured{Samples: l.Samples, Counted: l.Counted} }

// track is the measurement as WaxFlow takes it (waxflow.SliceMeasured): the
// count and how it was taken, or a track with no length for the zero value,
// which leaves the source's own declaration alone.
func (m Measured) track() container.Track {
	if m.Samples <= 0 {
		return container.Track{Samples: -1}
	}
	return container.Track{Samples: m.Samples, SamplesCounted: m.Counted}
}

// MeasureLength is the length input really has: the measurement a timeline
// asks for of a member whose headers only estimate its length, and the one a
// cut or a split needs of a payload the demuxer walks lazily. It is WaxFlow's
// own measure (waxflow.MeasureLength), which takes the cheapest route the file
// offers: a length the open already verified costs nothing, a deferred walk
// reads frame headers (ADTS, MP3 bare or in a WAV or AIFF-C, Matroska, a
// fragmented MP4, the frame counts of a WMA v1/v2 track), a source that seeks
// exactly is seeked past its end, and only one whose positions are stated
// times (WMA Pro, Voice and Lossless) is decoded to its end and counted, which
// Counted marks. Either way the answer is what a read of the file yields, with
// the damage the read found. It takes a concurrency slot like every other read
// here, and every route stops at a cancellation.
func (r *Runner) MeasureLength(ctx context.Context, input string) (Length, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return Length{}, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return Length{}, err
	}
	defer r.release()
	med, err := format.Open(src, hintFor(input), nil)
	if err != nil {
		return Length{}, classifyInputError(err, input)
	}
	defer med.Close()
	t, err := waxflow.MeasureLength(ctx, med)
	if err != nil {
		// A cancellation is not bad input: the context's own error keeps it
		// classified as canceled (exit 130).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Length{}, ctxErr
		}
		return Length{}, classifyInputError(err, input)
	}
	// The read's own findings: the walk's damage ("the metadata frame declares
	// N frames but the run holds M", a dropped truncated frame) and its remarks
	// on a file that is merely imprecise (a one-frame clean shortfall, an
	// overrun).
	info := med.Info()
	return Length{
		Samples:  t.Samples,
		Duration: trackDuration(t.Samples, t.Fmt.Rate),
		Warnings: sourceWarnings(info.Warnings),
		Notes:    sourceWarnings(info.Notes),
		Counted:  t.SamplesCounted,
	}, nil
}

// walkDefault finishes a demuxer's deferred walk under ctx and returns want as
// the walk settled it, with info's warnings and notes refolded. A demuxer that
// defers none is left alone and its current track returned. Walk is called
// whether or not Walked already reads true: a walk a read or a seek finished
// has measured the payload, and only Walk, free by then, puts that on the
// track.
//
// The three returns are separate because a copy cut declines a failed walk
// into its re-encode fallback, and a cancellation is not a failed walk: the
// walk reads ctx before each step and stops with it, and that comes back as
// ctxErr, the context's own error.
func walkDefault(ctx context.Context, demux container.Demuxer, info *format.Info, want container.Track) (settled container.Track, walkErr, ctxErr error) {
	if w, ok := demux.(container.Walker); ok {
		if err := w.Walk(ctx); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return want, nil, cerr
			}
			return want, err, nil
		}
	}
	format.RefreshWarnings(info, demux)
	for _, t := range demux.Tracks() {
		if t.ID == want.ID {
			return t, nil, nil
		}
	}
	return want, nil, nil
}

// openFileMedia opens path as a Media whose Close also closes the underlying
// file, so an album member the group pass opens on demand releases its
// descriptor when the engine moves on.
// It classifies what it opens the way openSource does: the open's own failure
// travels untouched, so a permission or missing-file error stays an I/O
// failure with its path, while a source the engine refuses to read (a
// directory, FIFO, device, or socket) and a container it cannot parse come
// back as bad input.
func openFileMedia(path, hint string) (format.Media, error) {
	// O_NONBLOCK keeps a FIFO with no writer from blocking the open; the
	// engine's regular-file check then refuses it by name. Harmless on a
	// regular file, and FileSource reads through the *os.File either way.
	f, err := os.OpenFile(path, os.O_RDONLY|container.OpenNonblock, 0)
	if err != nil {
		return nil, err
	}
	src, err := container.FileSource(f)
	if err != nil {
		_ = f.Close()
		return nil, classifyInputError(err, path)
	}
	m, err := format.Open(src, hint, nil)
	if err != nil {
		_ = f.Close()
		return nil, classifyInputError(err, path)
	}
	return &closingMedia{Media: m, closeFile: f.Close}, nil
}

// closingMedia closes the underlying file when the Media is closed.
type closingMedia struct {
	format.Media
	closeFile func() error
}

func (m *closingMedia) Close() error {
	err := m.Media.Close()
	if cerr := m.closeFile(); err == nil {
		err = cerr
	}
	return err
}
