package media

import (
	"context"
	"fmt"
	"os"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// AnalyzeFile measures the loudness of a whole local file. channels, when 1 or 2,
// measures after folding to that channel count so a two-pass gain matches a later
// downmixing encode; 0 keeps the source layout. The loudness package maps the
// result to its own type; this keeps the WaxFlow engine and the concurrency bound
// in one place.
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
		return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	return res, nil
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
	return res, err
}

// OpenAlbumConcat opens the gapless concatenation of inputs as one Media, for a
// group loudness measurement. WaxFlow conforms members whose rate differs from
// the envelope's, and opens each member lazily: only the track headers are read
// up front (one descriptor at a time), and Concat opens and closes each member's
// file as the timeline reaches it, so a large album holds one descriptor open
// rather than one per track. The caller closes the returned Media via closer.
func (r *Runner) OpenAlbumConcat(inputs []string) (format.Media, func() error, error) {
	members := make([]waxflow.ConcatSource, len(inputs))
	for i, in := range inputs {
		track, err := r.albumTrack(in)
		if err != nil {
			return nil, nil, err
		}
		path, hint := in, hintFor(in)
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
		return container.Track{}, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	return info.Default(), nil
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
