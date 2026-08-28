package media

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Spec describes a transcode. Codec selects the output codec; Bitrate overrides
// lossy preset defaults in bits per second; BitDepth forces integer output for
// the codecs that hold integer PCM; Channels downmixes (1 or 2) when positive;
// GainDB applies a normalization gain (0 is a no-op). The zero value is a
// container remux (CodecCopy) with no processing.
type Spec struct {
	Codec    Codec
	Bitrate  int
	BitDepth int     // forced output depth (16 or 24); 0 follows the decoded stream
	Channels int     // output channel count (downmix); 0 keeps the source layout
	GainDB   float64 // scalar normalization gain in dB; 0 is a no-op
	// Tags are embedded by the muxer at encode time, for the outputs whose only
	// tag form is written with the audio (WavPack and APE take their APEv2 block
	// there; WaxLabel cannot identify either format, so the usual post-pass fails
	// rather than adding anything). Callers leave it nil for every other codec,
	// whose finished files the WaxLabel post-pass tags. A remux ignores it: the
	// remux paths carry the source's own demuxer-read tags instead.
	Tags []Tag
}

// Tag is one canonical metadata field for mux-time embedding, re-exported so
// callers outside this package can build tag lists without importing WaxFlow.
type Tag = container.Tag

// muxEmbedsFormat is the one statement of which WaxFlow output formats take
// their tags at mux time in WaxTap's flows: the two whose finished files
// WaxLabel cannot read back. It narrows WaxFlow's OutputEmbedsTags; the MP4
// and Ogg families also accept mux-time tags upstream, but their finished
// files post-pass fine, which WaxTap prefers since that path also carries
// pictures and chapters. MuxEmbedsTags, MuxEmbedsExt, and remuxTags all
// resolve through here so the three vocabularies (codec enum, format string,
// output extension) cannot drift.
func muxEmbedsFormat(format string) bool {
	return format == "wavpack" || format == "ape"
}

// MuxEmbedsTags reports whether c's muxer writes its tags with the audio, so
// metadata must be supplied via Spec.Tags rather than a WaxLabel post-pass on
// the finished file.
func (c Codec) MuxEmbedsTags() bool {
	f, ok := codecFormat(c)
	return ok && muxEmbedsFormat(f)
}

// MuxEmbedsExt reports whether the output extension (lowercased, no dot) names
// a container whose format embeds tags at mux time, for callers that must
// predict a mux-tagged target before the pipeline resolves one from the
// extension.
func MuxEmbedsExt(ext string) bool {
	return muxEmbedsFormat(waxflow.OutputFormatForExt(ext))
}

// TagsFromMap flattens probe-shaped tags (canonical uppercase keys, values in
// order) into a deterministic Tag list: keys sorted, each key's values kept in
// stored order. Muxers write tags in list order, so a map iteration here would
// shuffle the output's tag block between runs.
func TagsFromMap(m map[string][]string) []Tag {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var tags []Tag
	for _, k := range keys {
		for _, v := range m[k] {
			tags = append(tags, Tag{Key: k, Value: v})
		}
	}
	return tags
}

// Result reports a completed transcode.
type Result struct {
	Output string // final output path
	Size   int64  // output size in bytes (0 if it could not be stat'd)
	Codec  Codec  // codec the output was encoded with
	// Levels is WaxFlow's level measurement of the encode. It is zero for a
	// container copy, which never re-derives samples.
	Levels Levels
}

// Levels carries WaxFlow's level measurement of one encode as numbers, so the
// caller sets the warning policy (thresholds, per-cause remedies, aggregation)
// instead of receiving a pre-rendered sentence. See waxflow.TranscodeResult for
// the field semantics. The zero value reports nothing.
type Levels struct {
	ClippedSamples int64   // channel samples the integer output clamped
	Samples        int64   // output frames (per channel)
	Channels       int     // output channel count
	TruePeak       float64 // output true peak, linear, 1.0 = full scale
	Quantized      bool    // a quantizer ran (float cut to integer)
}

// Note renders WaxFlow's one-line level warning for these levels, or "" when
// they warrant none: the clipped-sample count, else a true peak past full
// scale on a quantized output. The wording is WaxFlow's own, reconstructed
// through its LevelNote so the two never drift; callers may append remedies.
func (l Levels) Note() string {
	r := waxflow.TranscodeResult{
		Samples:        l.Samples,
		Format:         audio.Format{Channels: l.Channels},
		ClippedSamples: l.ClippedSamples,
		TruePeak:       l.TruePeak,
		Quantized:      l.Quantized,
	}
	return r.LevelNote()
}

// levelsOf extracts the level fields from an engine transcode result.
func levelsOf(r *waxflow.TranscodeResult) Levels {
	return Levels{
		ClippedSamples: r.ClippedSamples,
		Samples:        r.Samples,
		Channels:       r.Format.Channels,
		TruePeak:       r.TruePeak,
		Quantized:      r.Quantized,
	}
}

// Transcode reads input, applies spec, and writes the result to output. The
// output is staged in a temp file in output's directory and atomically renamed
// into place on success; on failure or cancellation the temp is removed and any
// existing file at output is left untouched.
//
// CodecCopy is a whole-file container remux (no re-encode); it rejects a channel
// or gain change, which require decoding.
func (r *Runner) Transcode(ctx context.Context, input, output string, spec Spec) (Result, error) {
	if spec.Codec == CodecCopy && (spec.Channels > 0 || spec.GainDB != 0) {
		return Result{}, fmt.Errorf("%w: a container copy cannot change channels or loudness", waxerr.ErrIncompatibleSpec)
	}

	src, closeSrc, err := openSource(input)
	if err != nil {
		return Result{}, err
	}
	defer closeSrc()

	staged, err := tempfile.New(output)
	if err != nil {
		return Result{}, err
	}
	defer staged.Discard() // no-op after Commit; cleans up on every error path

	if err := r.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer r.release()

	var levels Levels
	if spec.Codec == CodecCopy {
		// remux classifies its own failures; classifying again here would wrap an
		// already-mapped error a second time.
		if err := r.remux(ctx, src, input, output, staged); err != nil {
			return Result{}, err
		}
	} else {
		opts := encodeOptions(spec)
		format, _ := codecFormat(spec.Codec)
		opts.Container = containerFor(format, hintFor(output))
		tres, err := r.engine.Transcode(ctx, src, hintFor(input), staged, opts)
		if err != nil {
			return Result{}, classifyEngineError(err, input, output)
		}
		levels = levelsOf(tres)
	}

	if err := staged.Commit(); err != nil {
		return Result{}, err
	}
	res := Result{Output: output, Codec: spec.Codec, Levels: levels}
	if fi, serr := os.Stat(output); serr == nil {
		res.Size = fi.Size()
	}
	return res, nil
}

// RemuxContainer remuxes input to output using an explicit WaxFlow container
// override, e.g. "progressive" to flatten a fragmented MP4 into a tag-writable
// progressive one. It is a packet copy (no re-encode). input and output may be
// the same path; the source is closed before the atomic rename.
func (r *Runner) RemuxContainer(ctx context.Context, input, output, container string) error {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = closeSrc()
		}
	}()

	if err := r.acquire(ctx); err != nil {
		return err
	}
	defer r.release()

	staged, err := tempfile.New(output)
	if err != nil {
		return err
	}
	defer staged.Discard()

	demux, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return classifyInputError(err, input)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return remuxDeclined(track.Codec)
	}
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: container, Tags: remuxTags(outFormat, info)}
	if _, err := r.engine.RemuxDemuxer(ctx, demux, track, staged, opts); err != nil {
		return classifyEngineError(err, input, output)
	}
	// Close the source before the rename: on Windows a rename over an open file
	// fails, and input may equal output.
	closed = true
	if err := closeSrc(); err != nil {
		return err
	}
	return staged.Commit()
}

// remux rewrites the source packets into the container the output extension
// names, choosing WaxFlow's output format from the source codec (the codec must
// survive the trip) so no re-encode happens. It takes the paths rather than their
// container hints so its failures can name the file they are about.
func (r *Runner) remux(ctx context.Context, src container.Source, input, output string, dst *tempfile.File) error {
	demux, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return classifyInputError(err, input)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return remuxDeclined(track.Codec)
	}
	opts := waxflow.TranscodeOptions{
		Format:    outFormat,
		Container: containerFor(outFormat, hintFor(output)),
		Tags:      remuxTags(outFormat, info),
	}
	if _, err := r.engine.RemuxDemuxer(ctx, demux, track, dst, opts); err != nil {
		return classifyEngineError(err, input, output)
	}
	return nil
}

// remuxTags returns the mux-time tags a remux to outFormat must carry: the
// source's own demuxer-read tags, and only for the formats whose muxer is the
// sole tag path (WavPack, APE). A WaxFlow remux never forwards tags on its own,
// so without this a .wv-to-.wv copy would silently strip the APEv2 block; with
// any other output the post-pass restores metadata from the source file and
// mux-time tags would be written twice. A remux copies the audio bytes
// unchanged, so tags describing the source's own audio (ReplayGain and kin)
// still hold and are carried too.
func remuxTags(outFormat string, info *format.Info) []Tag {
	if !muxEmbedsFormat(outFormat) {
		return nil
	}
	return TagsFromMap(info.Tags)
}

// DropOwnAudioTags returns tags without the keys that describe the audio they
// were read from (ReplayGain, encoder stamps, an AcoustID fingerprint), which a
// re-encode, cut, or gain change invalidates. The predicate is WaxLabel's own,
// so this cannot drift from what its transfer excludes. A whole-file remux
// keeps them instead: the audio bytes are unchanged, so the values still hold.
func DropOwnAudioTags(tags []Tag) []Tag {
	kept := tags[:0:0]
	for _, t := range tags {
		if tag.Key(t.Key).DescribesOwnAudio() {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// remuxDeclined reports that a codec cannot be packet-copied. PCM gets its own
// wording because the obvious advice, re-encode instead, sounds lossy and is not.
func remuxDeclined(id codec.ID) error {
	if id == codec.PCM {
		return fmt.Errorf("%w: cannot container-copy PCM audio: its sample layout belongs to the container (RIFF is little-endian, AIFF big-endian), so the packets cannot move unchanged; drop --format copy to re-encode, which is bit-exact for PCM",
			waxerr.ErrIncompatibleSpec)
	}
	return fmt.Errorf("%w: cannot remux %s audio", waxerr.ErrIncompatibleSpec, codecName(id))
}

// containerFor returns the WaxFlow Container override for delivering format into
// the container named by ext, or "" for the format's own default container.
//
// It is format-aware because the right override depends on both: AAC/ALAC need
// "progressive" (else WaxFlow ships fragmented CMAF, which Apple players and tag
// editors reject), and FLAC into .ogg needs "ogg" (else WaxFlow writes a bare
// FLAC stream in a .ogg file). Opus/Vorbis are Ogg natively, so "" suffices.
//
// Callers must pair format with an ext that can hold it; this does not
// re-validate, and WaxFlow errors on an override its row rejects rather than
// falling back. The facade validates through CheckOutputContainer, the pipeline
// through containerAccepts and containerCodec. AIFF accepts no override and falls
// through to "".
func containerFor(format, ext string) string {
	switch ext {
	case "mka", "mkv":
		return "mka"
	case "webm":
		return "webm"
	case "aac":
		return "adts"
	case "ogg", "oga":
		if format == "flac" {
			return "ogg"
		}
	}
	// The AAC family and ALAC mux into MP4 whatever the path is named, so the
	// override is keyed on the format rather than an .m4a extension: -o out.alac
	// and an extensionless -o out need it as much as -o out.m4a does.
	if format == "aac" || format == "he-aac" || format == "alac" {
		return ContainerProgressive
	}
	return ""
}

// ContainerProgressive is WaxFlow's flat (moov+mdat) MP4 container override,
// re-exported for callers that remux through RemuxContainer.
const ContainerProgressive = waxflow.ContainerProgressive

// codecToFormat maps a source codec ID to the WaxFlow output format that carries
// it unchanged, for a remux. It reports false for a codec WaxTap cannot remux.
//
// PCM is absent on purpose. Its packets are raw samples whose layout belongs to
// the container (RIFF little-endian, AIFF big-endian, Matroska signed 8-bit), so
// WaxFlow's codecSurvives declines every PCM remux. Listing it here would only
// defer that failure to the engine. classifyEngineError now gives an engine-side
// refusal the same exit code this table's does, so the reason to keep PCM out is
// no longer the exit code but the message: remuxDeclined can say that a PCM
// re-encode is bit-exact, which the engine's own wording cannot.
func codecToFormat(id codec.ID) (string, bool) {
	switch id {
	case codec.Opus:
		return "opus", true
	case codec.AACLC:
		return "aac", true
	case codec.HEAAC:
		// The he-aac row exists so a copy keeps an HE source its identity
		// (WaxFlow's own format=aac requests redirect there too) instead of
		// declining into a lossy LC re-encode.
		return "he-aac", true
	case codec.FLAC:
		return "flac", true
	case codec.ALAC:
		return "alac", true
	case codec.MP3:
		return "mp3", true
	case codec.Vorbis:
		return "vorbis", true
	case codec.WavPack:
		return "wavpack", true
	case codec.APE:
		return "ape", true
	}
	// WMA stays out: WaxFlow decodes it but registers no output row, so there is
	// no format name a remux could run as; remuxDeclined says to transcode.
	return "", false
}
