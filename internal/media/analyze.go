package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// AnalyzeFile measures the loudness of a whole local file. channels, when 1 or 2,
// measures after folding to that channel count so a two-pass gain matches a later
// downmixing encode; 0 keeps the source layout. The loudness package maps the
// result to its own type; this keeps the WaxFlow engine and the concurrency bound
// in one place. It also returns the damage the read found, complete as of the
// end of the file; see Result.InputWarnings.
func (r *Runner) AnalyzeFile(ctx context.Context, input string, channels int) (*waxflow.AnalyzeResult, []string, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return nil, nil, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return nil, nil, err
	}
	defer r.release()
	// Opened here rather than through the engine's Analyze, which opens and
	// closes the media itself: the damage list is live on the media and
	// complete only once the read has reached the end, so it is read off the
	// media after the analysis and before the close. The engine's own
	// AnalyzeMedia runs inside the slot this function holds.
	med, err := r.engine.OpenStream(src, hintFor(input))
	if err != nil {
		return nil, nil, classifyInputError(err, input)
	}
	defer med.Close()
	res, err := r.engine.AnalyzeMedia(ctx, med, waxflow.AnalyzeOptions{Channels: channels})
	if err != nil {
		// A cancellation is not bad input: preserve ctx.Err() so callers classify it
		// as canceled (exit 130), not unsupported-input (exit 2).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, classifyInputError(err, input)
	}
	return res, InputWarnings(med), nil
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
func (r *Runner) AnalyzeMedia(ctx context.Context, med format.Media, input string, channels int) (*waxflow.AnalyzeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()
	res, err := r.engine.AnalyzeMedia(ctx, med, waxflow.AnalyzeOptions{Channels: channels})
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Analysis only ever fails on the media it is reading, so it takes the
	// input-side classification.
	return res, classifyInputError(err, input)
}

// AnalyzeGroup measures inputs as one programme: each member decoded once at
// channels[i] (its delivered width; 0 keeps its own) and the group's gates
// run over every member's blocks at once (waxflow.Engine.AnalyzeGroup). It
// returns the group figure, each member's own, and the damage each member's
// read found. The members are decoded one after another inside the engine,
// so the call takes one concurrency slot for its whole length; an album of
// N tracks is N decodes, where the Concat pass this replaces was N decodes
// on top of a per-track pass.
//
// No seam and no envelope: a member whose headers only estimate its length
// (a WMA, a Matroska on its Info Duration) is measured to its end without a
// declaration to be held to, and a mono member is measured as mono rather
// than duplicated across a stereo pair. Errors name the member index, which
// the album caller turns into a track name.
//
// It holds one descriptor per member for the whole measurement, where the
// Concat pass it replaces held one at a time: waxflow.GroupMember takes an
// already-open Media the caller owns, with no lazy open to defer it, so an
// album of N tracks is N open files. An ask for one is in
// docs/upstream-requests.md.
func (r *Runner) AnalyzeGroup(ctx context.Context, inputs []string, channels []int) (*waxflow.AnalyzeResult, []waxflow.AnalyzeResult, [][]string, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, nil, nil, err
	}
	defer r.release()
	members := make([]waxflow.GroupMember, 0, len(inputs))
	medias := make([]format.Media, 0, len(inputs))
	defer func() {
		for _, m := range medias {
			_ = m.Close()
		}
	}()
	for i, in := range inputs {
		m, err := openFileMedia(in, hintFor(in))
		if err != nil {
			// The member index, in the wording the engine uses for a member
			// it could not read, so the album caller names the track from one
			// pattern rather than two. The cause travels as it is: a file the
			// filesystem would not open is an I/O failure (exit 10), not bad
			// input, and openFileMedia has already classified the refusals
			// that are.
			return nil, nil, nil, fmt.Errorf("group member %d: %w", i, err)
		}
		medias = append(medias, m)
		ch := 0
		if i < len(channels) {
			ch = channels[i]
		}
		members = append(members, waxflow.GroupMember{Media: m, Channels: ch})
	}
	res, err := r.engine.AnalyzeGroup(ctx, members, waxflow.AnalyzeOptions{})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, nil, ctxErr
		}
		// No file is named here: the member index is in the text, and the
		// album caller turns that into a track name.
		return nil, nil, nil, classifyInputError(err, "")
	}
	warnings := make([][]string, len(medias))
	for i, m := range medias {
		warnings[i] = InputWarnings(m)
	}
	return &res.Group, res.Members, warnings, nil
}

// Length is what a measurement of a file delivers: the frame count, its
// duration at the source rate, the damage the read found on the way, and the
// read's remarks on a file that is not damaged.
type Length struct {
	Samples  int64
	Duration time.Duration
	Warnings []string
	Notes    []string
}

// MeasureLength is the length input really has: the measurement a timeline
// asks for of a member whose headers only estimate its length, and the one
// a cut needs of a payload the demuxer walks lazily. The walk is the cheap
// measurement, frame headers only, and it now settles the count in both
// directions for every container that has one, confirming or replacing what
// the headers declared (ADTS, MP3 bare or in a WAV or AIFF-C, Matroska, a
// fragmented MP4). Only ASF has no walk, so that alone decodes to EOF and
// counts what the decode delivers. A fallback to the decode path opens and
// demuxes the file a second time: countFrames needs its own format.Open, not
// the demuxer walkLength already held. Either way the answer is what a read of
// the file yields, with the damage the read found. A decode takes a
// concurrency slot like every other one here and stops at a cancellation.
func (r *Runner) MeasureLength(ctx context.Context, input string) (Length, error) {
	walked, ok, err := r.walkLength(ctx, input)
	if err != nil || ok {
		return walked, err
	}
	hint := hintFor(input)
	n, rate, warnings, err := r.countFrames(ctx, input, hint)
	if err != nil {
		return Length{}, err
	}
	return Length{Samples: n, Duration: trackDuration(n, rate), Warnings: warnings}, nil
}

// walkLength measures input by walking it, reporting ok=false when the
// container has no walk to settle the count with and the caller has to decode.
// It is MeasureLength's first half, split out so the walk can be tested apart
// from the decode fallback.
//
// A non-Walker settles on its own declaration when that declaration was itself
// a measurement at open (a FLAC verified against its tail, an Ogg granule), and
// otherwise gives up: an ASF states a duration nothing checked. A Walker always
// walks, even one already declaring an exact count, because the walk is what
// confirms or shrinks that count now and is cheaper than the decode it saves.
func (r *Runner) walkLength(ctx context.Context, input string) (Length, bool, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return Length{}, false, err
	}
	defer closeSrc()
	// A walk is a full header scan, real work like any decode, so it takes
	// a concurrency slot too. The slot is released with this call, before
	// the caller falls through to countFrames, which acquires its own: held
	// past that point, a concurrency-1 Runner would deadlock against itself.
	if err := r.acquire(ctx); err != nil {
		return Length{}, false, err
	}
	defer r.release()
	hint := hintFor(input)
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return Length{}, false, classifyInputError(err, input)
	}
	before := info.Default()
	if _, isWalker := demux.(container.Walker); !isWalker {
		if !before.SamplesExact {
			return Length{}, false, nil
		}
		return lengthOf(before, info), true, nil
	}
	settled, walkErr, ctxErr := walkDefault(ctx, demux, info, before)
	switch {
	case ctxErr != nil:
		return Length{}, false, ctxErr
	case walkErr != nil:
		return Length{}, false, classifyInputError(walkErr, input)
	case settled.SamplesExact:
		return lengthOf(settled, info), true, nil
	}
	return Length{}, false, nil
}

// walkDefault walks a lazily walked demuxer and returns want as the walk
// settled it, with info's warnings and notes refolded. A demuxer already walked
// is left alone and its current track returned.
//
// The three returns are separate because the two callers classify them
// differently: a measurement reports a failed walk as bad input, while a copy
// cut declines into its re-encode fallback. A cancellation is neither, and it
// is checked on both sides of the walk because container.Walker.Walk takes no
// ctx and cannot be interrupted mid-scan; that guard is the only thing bounding
// it, which is why it lives here rather than at each call site.
func walkDefault(ctx context.Context, demux container.Demuxer, info *format.Info, want container.Track) (settled container.Track, walkErr, ctxErr error) {
	if w, ok := demux.(container.Walker); ok && !w.Walked() {
		if err := ctx.Err(); err != nil {
			return want, nil, err
		}
		if err := w.Walk(); err != nil {
			return want, err, nil
		}
		if err := ctx.Err(); err != nil {
			return want, nil, err
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

// lengthOf reports a settled track as a Length, carrying the read's own
// warnings and notes: the walk's damage ("the metadata frame declares N frames
// but the run holds M", a dropped truncated frame) and its remarks on a file
// that is merely imprecise (a one-frame clean shortfall, an overrun).
func lengthOf(t container.Track, info *format.Info) Length {
	return Length{
		Samples:  t.Samples,
		Duration: trackDuration(t.Samples, t.Fmt.Rate),
		Warnings: sourceWarnings(info.Warnings),
		Notes:    sourceWarnings(info.Notes),
	}
}

// countFrames reads path to its end and counts the frames the decode
// delivers, the measurement a timeline asks for of a member whose headers
// only estimate its length; it is MeasureLength's fallback for a payload no
// walk can settle (a Xing MP3, an ASF). It is a full decode, so it takes a
// concurrency slot like every other one here and stops at a cancellation; a
// caller that has already made the decode hands its count to OpenAlbumConcat
// instead. Besides the count it returns the rate the file was read at and the
// damage the read found (InputWarnings), which MeasureLength reports as its
// own.
func (r *Runner) countFrames(ctx context.Context, path, hint string) (int64, int, []string, error) {
	if err := r.acquire(ctx); err != nil {
		return 0, 0, nil, err
	}
	defer r.release()
	src, closeSrc, err := openSource(path)
	if err != nil {
		return 0, 0, nil, err
	}
	defer closeSrc()
	m, err := format.Open(src, hint, nil)
	if err != nil {
		return 0, 0, nil, classifyInputError(err, path)
	}
	defer m.Close()
	rate := m.Info().Default().Fmt.Rate
	buf := audio.Get(m.Info().Default().Fmt, 4096)
	defer audio.Put(buf)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, nil, err
		}
		err := m.ReadChunk(buf)
		if err == io.EOF {
			return n, rate, InputWarnings(m), nil
		}
		if err != nil {
			return 0, 0, nil, classifyInputError(err, path)
		}
		n += int64(buf.N)
	}
}

// openFileMedia opens path as a Media whose Close also closes the underlying
// file, so a lazily-opened Concat member releases its descriptor on advance.
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
