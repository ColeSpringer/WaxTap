package media

import (
	"context"
	"io"
	"os"

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
// composition is measured as it will be encoded. channels folds the measurement
// to a downmix target (0 keeps the source layout). The caller owns med.
func (r *Runner) AnalyzeMedia(ctx context.Context, med format.Media, channels int) (*waxflow.AnalyzeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()
	res, err := r.engine.AnalyzeMedia(ctx, med, waxflow.AnalyzeOptions{Channels: channels})
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Analysis only ever fails on the media it is reading, so it takes the
	// input-side classification; the Media is already open, so there is no path to
	// name an I/O failure with.
	return res, classifyInputError(err, "")
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
// input, has this function decode and count the member itself. The timeline
// holds every member to its declared length (it counts what the member
// delivers and refuses a mismatch at the seam), so it refuses at plan time a
// member whose headers state no length (raw ADTS) or only an advisory one: a
// duration rounded to a time unit (ASF, a Matroska falling back to its Info
// Duration) or a count exact about the wrong thing (a WAV carrying MP3
// frames). A decode read to its end is the measurement the engine asks for,
// and the per-track analysis that precedes the group measurement is exactly
// that, so its count stands in for what such a member declared; a member
// whose own length is countable keeps it. The seam check then holds the run
// to the measurement rather than to the header, which is the engine's
// design (see waxflow.ConcatSource.Track): the header was never a number
// the decode could be held to.
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
				if n, err = r.countFrames(ctx, path, hint); err != nil {
					return nil, nil, err
				}
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

// countFrames reads path to its end and counts the frames the decode
// delivers, the measurement a timeline asks for of a member whose headers
// only estimate its length. It is a full decode, so it takes a concurrency
// slot like every other one here and stops at a cancellation; a caller that
// has already made the decode hands its count to OpenAlbumConcat instead.
func (r *Runner) countFrames(ctx context.Context, path, hint string) (int64, error) {
	if err := r.acquire(ctx); err != nil {
		return 0, err
	}
	defer r.release()
	src, closeSrc, err := openSource(path)
	if err != nil {
		return 0, err
	}
	defer closeSrc()
	m, err := format.Open(src, hint, nil)
	if err != nil {
		return 0, classifyInputError(err, path)
	}
	defer m.Close()
	buf := audio.Get(m.Info().Default().Fmt, 4096)
	defer audio.Put(buf)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		err := m.ReadChunk(buf)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, classifyInputError(err, path)
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
