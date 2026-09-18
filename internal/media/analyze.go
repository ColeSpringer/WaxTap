package media

import (
	"context"
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
// classification, the way AnalyzeFile's does: a mid-read failure (a bounded span
// that asks for more than a truncated file holds) then reports as *fs.PathError
// instead of bad input. Pass "" when med has no single file to name, a
// concatenated timeline with several members (an album's group pass). channels
// folds the measurement to a downmix target (0 keeps the source layout). The
// caller owns med.
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

// OpenAlbumConcat opens the gapless concatenation of inputs as one Media, for a
// group loudness measurement. WaxFlow conforms members whose rate differs from
// the envelope's, and opens each member lazily: only the track headers are read
// up front (one descriptor at a time), and Concat opens and closes each member's
// file as the timeline reaches it, so a large album holds one descriptor open
// rather than one per track. The caller closes the returned Media via closer.
//
// measured is what a full decode of each input delivered, one frame count per
// input in order; an entry below zero, or a slice too short to reach the
// input, has this function measure the member itself (MeasureLength: a walk
// when the container allows it, a decode otherwise). The timeline
// holds every member to its declared length (it counts what the member
// delivers and refuses a mismatch at the seam), so it refuses at plan time a
// member whose headers state no length (raw ADTS) or only an advisory one: a
// duration rounded to a time unit (ASF, a Matroska falling back to its Info
// Duration) or a count exact about the wrong thing (a WAV carrying MP3
// frames). A walk or a decode read to its end is the measurement the engine
// asks for, and the per-track analysis that precedes the group measurement
// is exactly that, so its count stands in for what such a member declared;
// a member whose own length is countable keeps it. The seam check then
// holds the run to the measurement rather than to the header, which is the
// engine's design (see waxflow.ConcatSource.Track): the header was never a
// number the decode could be held to.
func (r *Runner) OpenAlbumConcat(ctx context.Context, inputs []string, measured []int64) (format.Media, func() error, error) {
	members := make([]waxflow.ConcatSource, len(inputs))
	for i, in := range inputs {
		track, err := r.albumTrack(in)
		if err != nil {
			return nil, nil, err
		}
		path, hint := in, hintFor(in)
		if track.Samples < 0 || track.SamplesAdvisory {
			n := int64(-1)
			if i < len(measured) {
				n = measured[i]
			}
			if n < 0 {
				// Hazard: a truncated ADTS can walk one frame long, and Concat
				// enforces that count at the seam (ADTS-walker entry,
				// docs/upstream-requests.md). Every caller here is spared by
				// pre-filling measured or naming a freshly written output; an
				// unmeasured, possibly-truncated member would hit it.
				length, err := r.MeasureLength(ctx, path)
				if err != nil {
					return nil, nil, err
				}
				n = length.Samples
			}
			track.Samples, track.SamplesAdvisory = n, false
		}
		members[i] = waxflow.ConcatSource{Track: track, Open: func() (format.Media, error) {
			return openFileMedia(path, hint)
		}}
	}
	med, err := waxflow.Concat(members, waxflow.ConcatOptions{})
	if err != nil {
		return nil, nil, err
	}
	return med, med.Close, nil
}

// albumTrack reads one input's default track headers, holding its descriptor only
// for the read.
func (r *Runner) albumTrack(input string) (container.Track, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return container.Track{}, err
	}
	defer closeSrc()
	_, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return container.Track{}, classifyInputError(err, input)
	}
	return info.Default(), nil
}

// Length is what a decode of a file delivers: the frame count, its duration
// at the source rate, and the damage the read found on the way.
type Length struct {
	Samples  int64
	Duration time.Duration
	Warnings []string
}

// MeasureLength is the length input really has: the measurement a timeline
// asks for of a member whose headers only estimate its length, and the one
// a cut needs of a payload the demuxer walks lazily. The walk is the cheap
// measurement, frame headers only, and it settles a count the container
// stated as unknown or advisory (ADTS, MP3 in a WAV or AIFF-C, a Matroska
// on its Info Duration). It leaves a Xing count alone even when it found
// fewer frames (WaxFlow: "a count a metadata frame stated stands"), and
// an ASF has no walk at all, so those decode to EOF and count what the
// decode delivers. A fallback to the decode path opens and demuxes the
// file a second time: countFrames needs its own format.Open, not the
// demuxer this function already held for the walk attempt. Either way
// the answer is what a read of the file yields, with the damage the read
// found. A decode takes a concurrency slot like every other one here and
// stops at a cancellation.
func (r *Runner) MeasureLength(ctx context.Context, input string) (Length, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return Length{}, err
	}
	hint := hintFor(input)
	walked, ok, err := func() (Length, bool, error) {
		defer closeSrc()
		// A walk is a full header scan, real work like any decode, so it takes
		// a concurrency slot too. The slot is scoped to this closure and
		// released before the caller falls through to countFrames, which
		// acquires its own: held past that point, a concurrency-1 Runner
		// would deadlock against itself.
		if err := r.acquire(ctx); err != nil {
			return Length{}, false, err
		}
		defer r.release()
		demux, info, err := format.OpenDemuxer(src, hint, nil)
		if err != nil {
			return Length{}, false, classifyInputError(err, input)
		}
		before := info.Default()
		w, isWalker := demux.(container.Walker)
		if !isWalker || w.Walked() || !(before.Samples < 0 || before.SamplesAdvisory) {
			return Length{}, false, nil
		}
		// container.Walker.Walk takes no ctx and cannot be interrupted mid-scan;
		// checking on either side is all that bounds it.
		if err := ctx.Err(); err != nil {
			return Length{}, false, err
		}
		if err := w.Walk(); err != nil {
			return Length{}, false, classifyInputError(err, input)
		}
		if err := ctx.Err(); err != nil {
			return Length{}, false, err
		}
		format.RefreshWarnings(info, demux)
		for _, t := range demux.Tracks() {
			if t.ID == before.ID && t.Samples >= 0 && !t.SamplesAdvisory {
				return Length{Samples: t.Samples, Duration: trackDuration(t.Samples, t.Fmt.Rate), Warnings: sourceWarnings(info.Warnings)}, true, nil
			}
		}
		return Length{}, false, nil
	}()
	if err != nil || ok {
		return walked, err
	}
	n, rate, warnings, err := r.countFrames(ctx, input, hint)
	if err != nil {
		return Length{}, err
	}
	return Length{Samples: n, Duration: trackDuration(n, rate), Warnings: warnings}, nil
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
func openFileMedia(path, hint string) (format.Media, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	src, err := container.FileSource(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	m, err := format.Open(src, hint, nil)
	if err != nil {
		_ = f.Close()
		return nil, err
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
