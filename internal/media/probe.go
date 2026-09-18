package media

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// ProbeResult is a probe of a local audio file: container metadata plus the
// audio track's codec details.
type ProbeResult struct {
	Format  ProbeFormat   // container metadata
	Streams []ProbeStream // audio tracks
	// Tags are the source's embedded tags under canonical uppercase keys
	// (TITLE, ARTIST, ...), as far as the demuxer parses them (WavPack and APE
	// APEv2 text items, ASF metadata, MP4 ilst). Nil when the container carries
	// none or the demuxer does not read them; WaxLabel remains the authority
	// for the formats it can parse.
	Tags map[string][]string
	// Warnings are WaxFlow's notes on input damage its tolerant parser worked
	// around: a declared length the frames do not reach, unparsable bytes
	// skipped, a chunk size clamped to the file. Nil for an undamaged input.
	//
	// They matter because the parser's tolerance is otherwise invisible: the
	// probe reports the length it could actually read, so a truncated file and
	// a short recording look alike without them. Damage the parser does not
	// notice leaves this empty (a rewritten frame inside a file of the right
	// length reads as clean), so an empty list is not a certificate of health.
	//
	// Damage only, and only what reading the headers finds. A demuxer that
	// walks its payload lazily (MP3, bare or inside a WAV or AIFF-C; ADTS; a
	// Matroska with only an advisory length) reports damage past the head
	// from the read that reaches it, which Result.InputWarnings carries.
	Warnings []string
	// Notes are what WaxFlow did with an input that is not damaged: a stream
	// it ignored, a chapter list it capped at its own limit, a timeline it
	// rescaled, a band its decoder does not synthesize, a delay field it
	// declined to apply. The engine once folded them in with Warnings and now
	// keeps them apart, so a caller reporting damage reports Warnings alone.
	// Nil when the engine had nothing to say.
	Notes []string
	// LengthClaimed says the default track's length is a claim the headers
	// make, not a count a read confirmed: the demuxer walks its payload
	// lazily and has not reached the end (container.Walker: MP3, bare or in
	// a WAV or AIFF-C; ADTS; any Matroska whose open did not walk it, which
	// is every non-Opus track), or it states an advisory total (ASF), or
	// none at all. A cut resolved against such a length can declare a span
	// the file does not hold, so the pipeline measures the file first
	// (Runner.MeasureLength), at the cost of a decode on those cut runs when
	// the walk cannot settle the count itself.
	LengthClaimed bool
}

// ProbeFormat describes the container.
type ProbeFormat struct {
	Container string        // identified container name ("webm", "mka", "flac", ...)
	Duration  time.Duration // container duration, or 0 when unknown
	Size      int64         // bytes, or 0 when unknown
}

// ProbeStream describes one audio track.
type ProbeStream struct {
	CodecType  string        // always "audio"
	CodecName  string        // codecName-mapped: "opus", "aac", "flac", "pcm", ...
	SampleRate int           // Hz
	Channels   int           // channel count
	Duration   time.Duration // track duration, or 0 when unknown
	// Samples is WaxFlow's raw frame count after gapless trimming (its
	// Track.Samples): -1 when the container does not state one (raw ADTS), 0
	// for a track that decodes to no audio at all, and positive otherwise. The
	// trimming matters for the zero case's precise meaning: a file whose every
	// stored sample is declared encoder delay or padding also reports 0, which
	// is still the honest answer, since decoding it delivers nothing.
	//
	// It exists because Duration collapses -1 and 0 into 0 and a caller then
	// cannot tell "unknown length" from "no frames at all" - which are opposite
	// facts, and the difference between guessing at a cut and knowing there is
	// nothing to cut.
	Samples int64
	// SamplesExact and SamplesAdvisory are WaxFlow's own qualifiers on
	// Samples: exact marks a hard length the decoder is trimmed to (Ogg
	// Opus and Vorbis); advisory marks a total a decode is not expected to
	// match (ASF, a Matroska on its Info Duration, a WAV carrying MP3
	// frames). Both false is a counted total whose mismatch would be damage.
	SamplesExact    bool
	SamplesAdvisory bool
}

// AudioStream returns the first audio track and true, or a zero stream and false
// when the result carries none. Probe rejects no-audio inputs, so a result
// obtained from Probe always has one.
func (p ProbeResult) AudioStream() (ProbeStream, bool) {
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			return s, true
		}
	}
	return ProbeStream{}, false
}

// hintFor returns WaxFlow's container hint for a path: its extension without the
// leading dot, or "" to let WaxFlow sniff the container.
func hintFor(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// Probe inspects a local input and returns the parsed result. Undecodable media
// and media with no audio track are reported as waxerr.ErrUnsupportedInput.
func (r *Runner) Probe(ctx context.Context, input string) (ProbeResult, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return ProbeResult{}, err
	}
	defer closeSrc()
	return r.probeSource(ctx, src, input, hintFor(input))
}

func (r *Runner) probeSource(ctx context.Context, src container.Source, input, hint string) (ProbeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return ProbeResult{}, err
	}
	defer r.release()

	// OpenDemuxer runs the same resolve, open, and trackless check a non-strict
	// Probe does (format/format.go:200-217); it also hands back the demuxer, so
	// its Walker state can inform LengthClaimed below.
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ProbeResult{}, ctxErr
		}
		return ProbeResult{}, classifyInputError(err, input)
	}
	pr := mapProbe(info, src.Size())
	def := info.Default()
	if w, ok := demux.(container.Walker); (ok && !w.Walked()) || def.SamplesAdvisory || def.Samples < 0 {
		pr.LengthClaimed = true
	}
	return pr, nil
}

// mapProbe converts a WaxFlow probe into a ProbeResult. It never errors; the
// caller (probeSource) has already ensured the input decoded.
//
// The default track is reported first (so AudioStream describes it) and its
// duration is Format.Duration: cuts and measurements run against the default
// track, so a file whose default audio is not its longest track must not report
// a longer duration than the track a cut will actually address.
func mapProbe(info *format.Info, size int64) ProbeResult {
	pr := ProbeResult{Format: ProbeFormat{Container: info.Container, Size: size}, Tags: info.Tags, Warnings: info.Warnings, Notes: info.Notes}
	stream := func(t container.Track) ProbeStream {
		return ProbeStream{
			CodecType:       "audio",
			CodecName:       codecName(t.Codec),
			SampleRate:      t.Fmt.Rate,
			Channels:        t.Fmt.Channels,
			Duration:        trackDuration(t.Samples, t.Fmt.Rate),
			Samples:         t.Samples,
			SamplesExact:    t.SamplesExact,
			SamplesAdvisory: t.SamplesAdvisory,
		}
	}
	if len(info.Tracks) == 0 {
		return pr
	}
	def := info.Default()
	pr.Streams = append(pr.Streams, stream(def))
	pr.Format.Duration = trackDuration(def.Samples, def.Fmt.Rate)
	for _, t := range info.Tracks {
		if t.ID == def.ID {
			continue
		}
		pr.Streams = append(pr.Streams, stream(t))
	}
	return pr
}

// trackDuration converts a sample count and rate to a duration. Samples is -1
// when unknown (raw ADTS), yielding 0.
func trackDuration(samples int64, rate int) time.Duration {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(float64(samples) / float64(rate) * float64(time.Second))
}
