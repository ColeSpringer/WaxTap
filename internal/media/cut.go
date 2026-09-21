package media

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Mode selects how a cut is rendered. The facade maps waxtap.CutMode onto these.
type Mode uint8

const (
	// ModeSmart copies (lossless cut-remux) when the cut keeps the source codec,
	// and re-encodes when a transcode, downmix, gain, or crossfade is involved.
	ModeSmart Mode = iota
	// ModeCopy forces a lossless cut-remux. It cannot transcode, downmix, or
	// crossfade, and it fails when WaxFlow cannot cut-remux the source codec.
	ModeCopy
	// ModeAccurate forces a re-encode.
	ModeAccurate
	// ModeCopyExact is ModeCopy with spliced interior joins; see
	// CutSpec.SpliceTrims.
	ModeCopyExact
)

func (m Mode) String() string {
	switch m {
	case ModeCopy:
		return "copy"
	case ModeAccurate:
		return "accurate"
	case ModeCopyExact:
		return "copy-exact"
	default:
		return "smart"
	}
}

// CutSpec describes a resolved cut. Keeps are the spans to retain, in order, on
// the source timeline; Total is the source duration. CopyCut asks for a lossless
// cut-remux (kept codec, byte-identical packets), which WaxTap tries first and
// falls back from to Encode when WaxFlow declines the source codec, or the
// destination or the cut's shape, which Declined names. Encode names the
// re-encode used for the re-encode path (or the CopyCut fallback).
type CutSpec struct {
	Keeps     []cutrange.Range
	Total     time.Duration
	Crossfade time.Duration
	CopyCut   bool
	// RequireCopyCutMode and RequireCopyFormat each fail the cut rather than
	// re-encoding when WaxFlow declines the lossless cut-remux: an explicit copy
	// request, which silently re-encoding would break.
	//
	// They are two fields rather than one bool so the refusal can name the flag
	// the caller actually passed. They are independent and may both be set.
	RequireCopyCutMode bool // --cut-mode copy
	RequireCopyFormat  bool // --format copy
	// SpliceTrims asks WaxFlow for exact interior splices (TranscodeOptions
	// .SpliceTrims): the pre-roll packets ahead of each interior head carry a
	// full-duration discard and each interior tail is exact. Only a Matroska
	// destination can state those trims, so the caller has already held the
	// output to .mka/.mkv/.webm; a declined plan fails the cut like every other
	// explicit copy request rather than re-encoding.
	SpliceTrims bool
	Encode      Spec
	// SourceSamples is the frame count a read of the source delivers, when the
	// caller measured it (Runner.MeasureLength) because the headers only claim a
	// length. The composed timeline holds every span to it, in both directions:
	// a span reaching the end asks for what the file has, and a file holding
	// more than its headers declare gives up the declaration, not the audio.
	// 0 means the caller did not measure, and Render measures the source itself
	// rather than plan a bounded span against a number nothing confirmed; a file
	// whose headers state an exact count is not measured either way.
	SourceSamples int64
}

// requireCopy reports whether either explicit copy request is in force.
func (s CutSpec) requireCopy() bool { return s.RequireCopyCutMode || s.RequireCopyFormat }

// copyFlags names the copy requests this spec carries, for advice that tells
// the caller to drop a flag they actually wrote.
func (s CutSpec) copyFlags() string {
	switch {
	case s.SpliceTrims:
		return "--cut-mode copy-exact"
	case s.RequireCopyCutMode && s.RequireCopyFormat:
		return "--format copy / --cut-mode copy"
	case s.RequireCopyCutMode:
		return "--cut-mode copy"
	default:
		return "--format copy"
	}
}

// decodeReason names what in this spec forces a decode, for the refusal above.
func decodeReason(s CutSpec) string {
	switch {
	case s.Crossfade > 0:
		return "a crossfade"
	case s.Encode.Channels != 0:
		return "a downmix"
	case s.Encode.GainDB != 0:
		return "a loudness gain"
	default:
		return "a re-encode"
	}
}

// CopyDecline says why a cut that would have copied packets decoded instead.
// Render reports it on CutResult.Declined; NotDeclined means the packets were
// copied, or no copy was in question (CutSpec.CopyCut false). WaxFlow's
// declines carry no reason, so the reasons are WaxTap's own: two are decided
// before any plan is asked for, one by asking for a second plan, and one is
// what the plan declined without saying.
type CopyDecline uint8

const (
	NotDeclined CopyDecline = iota
	// DeclinedCodec: the source codec cannot be cut in place at all (MP3's
	// bit reservoir, FLAC's frame numbering, Vorbis's overlap, ALAC, PCM,
	// and every codec WaxFlow only decodes); waxflow.Cuttable.
	DeclinedCodec
	// DeclinedStreamStart: an HE-AAC packet cut must keep the stream start,
	// and this cut's first span does not.
	DeclinedStreamStart
	// DeclinedContainer: the destination declined a cut another container
	// takes (raw ADTS, which states no trims): the same cut planned into
	// flat MP4 was accepted.
	DeclinedContainer
	// DeclinedCut: the codec can and the container could, and WaxFlow
	// declined this cut's shape (a span holding no whole packet, spans too
	// close together, a trim the walk could not place) or the packet walk
	// could not index the source.
	DeclinedCut
)

// String spells the decline, for a log line or a test failure that would
// otherwise print a bare integer.
func (d CopyDecline) String() string {
	switch d {
	case DeclinedCodec:
		return "codec"
	case DeclinedStreamStart:
		return "stream-start"
	case DeclinedContainer:
		return "container"
	case DeclinedCut:
		return "cut"
	default:
		return "none"
	}
}

// copyRefusal explains a declined packet cut to a caller who asked for one by
// name, in the terms of the reason that applied. Before Render could name the
// reason, both refusals had to list every reason at once, which read as a
// catalogue of things that might be wrong with a file rather than as what
// was.
func copyRefusal(why CopyDecline) string {
	switch why {
	case DeclinedStreamStart:
		return "an HE-AAC packet cut must keep the stream start, and this cut's first span does not"
	case DeclinedContainer:
		return "raw ADTS (.aac) cannot state the trims this cut needs, so name a container that can, .m4a or .mka"
	case DeclinedCut:
		return "the packet cut declined this cut's shape: a span must hold a whole packet, spans must not sit too close together, and the packet walk must index the source"
	default:
		return fmt.Sprintf("this source codec cannot be cut in place (%s can)", strings.Join(waxflow.CutFormats(), " and "))
	}
}

// CutFormats names the formats WaxFlow cuts without a decode, for prose in
// the facade, which never imports the engine.
func CutFormats() []string { return waxflow.CutFormats() }

// CutResult reports a completed cut.
type CutResult struct {
	Output  string
	Removed time.Duration
	Mode    Mode
	Applied bool
	// Declined says why a copy cut decoded instead of moving packets;
	// NotDeclined when the copy ran or none was in question.
	Declined CopyDecline
	// Levels is WaxFlow's level measurement of the cut re-encode; see
	// Result.Levels. It is always zero for a lossless cut-remux.
	Levels Levels
	// InputWarnings is the input damage the read found, complete as of the
	// end of the write; see Result.InputWarnings. A cut reads only the spans
	// it keeps, so damage inside a removed span stays unreported here.
	InputWarnings []string
	// Keeps are the spans the output really holds, on the source timeline. A
	// packet copy snaps each interior join inward to the packet grid and
	// reports the landed spans here; a re-encode keeps exactly what was asked
	// and reports the request. Removed, and every remap of source-timeline
	// metadata, follow Keeps rather than the request.
	Keeps []cutrange.Range
	// Snaps counts the interior joins the packet copy moved and SnapMax is
	// the largest single move. A join is a cut point, so the one range a
	// caller removed is one join however many of its two edges moved; both
	// are zero for a re-encode and for a copy whose edges already sat on the
	// grid.
	Snaps   int
	SnapMax time.Duration
}

// Render applies spec's cut to input and writes the result to output. Output is
// staged and atomically renamed on success. When CopyCut is set and no downmix,
// gain, or crossfade is requested, Render tries a lossless cut-remux first and
// re-encodes when the copy declines, saying why on Declined.
//
// input and output must name different files, for the reason Transcode gives.
func (r *Runner) Render(ctx context.Context, input, output string, spec CutSpec) (CutResult, error) {
	if len(spec.Keeps) == 0 {
		return CutResult{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
	}

	src, closeSrc, err := openSource(input)
	if err != nil {
		return CutResult{}, err
	}
	defer closeSrc()

	// The concurrency slot is taken by cutRemux and cutReencode, around the work
	// that reads or writes audio, rather than here: openComposed may have to
	// measure the source first, which takes a slot of its own, and a
	// concurrency-1 Runner held one here would deadlock against itself.
	hint := hintFor(input)
	outExt := hintFor(output)

	staged, err := tempfile.New(output)
	if err != nil {
		return CutResult{}, err
	}
	defer staged.Discard()

	// A lossless cut-remux applies when the caller wants the source codec kept and
	// nothing forces a decode (no downmix, gain, or crossfade).
	tryRemux := spec.CopyCut && spec.Crossfade == 0 && spec.Encode.Channels == 0 && spec.Encode.GainDB == 0
	mode := Mode(ModeAccurate)
	declined := NotDeclined
	var levels Levels
	var found []string
	keeps := spec.Keeps
	snaps, snapMax := 0, time.Duration(0)
	if tryRemux {
		out, rerr := r.cutRemux(ctx, src, hint, outExt, spec, staged)
		if rerr != nil {
			return CutResult{}, classifyEngineError(rerr, input, output)
		}
		if out.done {
			mode, found = ModeCopy, out.found
			if spec.SpliceTrims {
				mode = ModeCopyExact
			}
			keeps, snaps, snapMax = out.keeps, out.snaps, out.snapMax
		} else {
			// WaxFlow declined a lossless cut-remux of the source codec (e.g. FLAC),
			// or of the cut's shape (HE-AAC packet-cuts only from the stream start).
			declined = out.decline
			if spec.requireCopy() {
				return CutResult{}, fmt.Errorf("%w: cannot losslessly copy-cut this cut: %s; drop %s to re-encode, which stays lossless for a lossless source",
					waxerr.ErrIncompatibleSpec, copyRefusal(declined), spec.copyFlags())
			}
			// Fall through to a re-encode, which stays lossless for a lossless
			// source. A copy spec whose source has no same-family encoder (WMA,
			// Musepack) has no fallback to fall to; failing here names the escape, where
			// the engine would only say "no output format requested".
			if spec.Encode.Codec == CodecCopy {
				return CutResult{}, fmt.Errorf("%w: this source codec cannot be packet-cut and has no same-family encoder; pass an explicit format (e.g. flac) to render the cut", waxerr.ErrIncompatibleSpec)
			}
			if levels, found, err = r.cutReencode(ctx, input, src, hint, outExt, spec, staged); err != nil {
				return CutResult{}, classifyEngineError(err, input, output)
			}
		}
	} else {
		// An explicit copy request must not reach a decode. Nothing routes one
		// here today (the facade refuses a copy beside a transcode or a
		// crossfade, and the pipeline refuses one beside a fold), so this
		// guards the contract rather than a path: silently re-encoding and
		// reporting ModeAccurate is exactly what CutSpec.SpliceTrims and
		// RequireCopyCutMode exist to prevent.
		if spec.requireCopy() || spec.SpliceTrims {
			return CutResult{}, fmt.Errorf("%w: this cut needs a decode (%s), which %s forbids",
				waxerr.ErrIncompatibleSpec, decodeReason(spec), spec.copyFlags())
		}
		if levels, found, err = r.cutReencode(ctx, input, src, hint, outExt, spec, staged); err != nil {
			return CutResult{}, classifyEngineError(err, input, output)
		}
	}

	if err := staged.Commit(); err != nil {
		return CutResult{}, err
	}
	return CutResult{
		Output:        output,
		Removed:       spec.Total - cutrange.OutputDuration(keeps, spec.Crossfade),
		Mode:          mode,
		Applied:       true,
		Declined:      declined,
		Levels:        levels,
		InputWarnings: found,
		Keeps:         keeps,
		Snaps:         snaps,
		SnapMax:       snapMax,
	}, nil
}

// remuxOutcome is what a packet copy delivered: done false means WaxFlow
// declined and the caller re-encodes.
type remuxOutcome struct {
	done    bool
	decline CopyDecline      // why, when done is false
	keeps   []cutrange.Range // landed, on the source timeline
	snaps   int
	snapMax time.Duration
	found   []string // input damage the packet walk found
}

// cutRemux performs the lossless packet-level cut-remux. It reports done=false
// (and no error) when WaxFlow declines the copy, with the reason on the
// outcome, so the caller re-encodes instead, the spans the copy really landed
// on, and the damage the packet walk found when it ran.
//
// The cheap answers come first. Opening the demuxer settles the codec, and a
// codec no cut could ever serve declines there rather than after PacketGrid's
// read of every packet and the frame-index walk behind it. A Source is a
// positional reader, so a second open costs nothing and neither open disturbs
// the other.
func (r *Runner) cutRemux(ctx context.Context, src container.Source, hint, outExt string, spec CutSpec, dst *tempfile.File) (remuxOutcome, error) {
	keeps, total := spec.Keeps, spec.Total
	if err := r.acquire(ctx); err != nil {
		return remuxOutcome{}, err
	}
	defer r.release()
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return remuxOutcome{}, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return r.declineCopy(spec, DeclinedCodec) // unknown codec: let the re-encode path handle it
	}
	if !waxflow.Cuttable(track) {
		// The fast negative, on the codec alone: an MP3, FLAC, or Vorbis
		// source pays neither the packet read nor the walk to learn what this
		// answers for free.
		return r.declineCopy(spec, DeclinedCodec)
	}
	grid, err := r.engine.PacketGrid(src, hint)
	if err != nil {
		return remuxOutcome{}, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	// Walk a lazily walked demuxer before planning, now that the codec is one
	// this path can copy. validateCutSpans bounds every span by track.Samples
	// whether or not it is advisory, which a Matroska open now states only as
	// the Info Duration, and PlanCut places inner trims from the MidTrims a walk
	// recorded. An unwalked track has neither, so the copy plan would meet a
	// mid-stream refusal instead of declining. The cost is a frame-index pass,
	// warm in the page cache after PacketGrid's own read.
	//
	// A walk that fails declines rather than failing the cut: the re-encode
	// fallback reads the file itself and reports what it finds, which is a
	// better answer for a damaged source than refusing the copy outright.
	walked, walkErr, ctxErr := walkDefault(ctx, demux, info, track)
	if ctxErr != nil {
		return remuxOutcome{}, ctxErr
	}
	if walkErr != nil {
		r.log.DebugContext(ctx, "the packet walk failed; re-encoding the cut instead of copying it", "err", walkErr)
		return r.declineCopy(spec, DeclinedCut)
	}
	track = walked
	spans := toSpans(keeps, total, track.Fmt.Rate)
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: containerFor(outFormat, outExt), SpliceTrims: spec.SpliceTrims}

	plan, err := r.engine.PlanCut(track, opts, spans, grid)
	if err != nil {
		return remuxOutcome{}, err
	}
	if plan == nil {
		// The reason classifies WaxFlow's answer rather than pre-empting it,
		// so a rule it later lifts costs a generation here for nothing: the
		// plan it would return is taken, and only a decline is explained.
		why := DeclinedCut
		switch {
		case track.Codec == codec.HEAAC && spans[0].From > 0:
			// The one rule WaxFlow states in a Debug log and nowhere a
			// caller can read: an HE-AAC packet cut must keep the stream
			// start. Ahead of the container arm because it declines into
			// every container, so a re-plan would only confirm it.
			why = DeclinedStreamStart
		case containerDropsTrims(opts.Container):
			// PlanCut is arithmetic and table lookups, so asking again for
			// the flat MP4 muxer is free, and an answer there proves it was
			// the destination that declined: raw ADTS states no trims, and
			// a cut synthesizes them. The same container the copy's gapless
			// trim is dropped into, for the same reason.
			again := opts
			again.Container = ContainerProgressive
			if p, perr := r.engine.PlanCut(track, again, spans, grid); perr == nil && p != nil {
				why = DeclinedContainer
			}
		}
		return r.declineCopy(spec, why) // declined: fall back to a re-encode
	}
	cutTrack, landedSpans, err := waxflow.CutTrack(track, opts, spans, grid)
	if err != nil {
		return remuxOutcome{}, err
	}
	cutDemux, err := waxflow.Cut(demux, track, opts, spans, grid)
	if err != nil {
		return remuxOutcome{}, err
	}
	// NB: pass CutTrack's track, not plan.Track.
	tres, err := r.engine.RemuxDemuxer(ctx, cutDemux, cutTrack, dst, opts)
	if err != nil {
		return remuxOutcome{}, err
	}
	landed, snaps, snapMax := landedKeeps(keeps, spans, landedSpans, track.Fmt.Rate)
	return remuxOutcome{done: true, keeps: landed, snaps: snaps, snapMax: snapMax, found: sourceWarnings(tres.InputWarnings)}, nil
}

// declineCopy is the answer when WaxFlow will not packet-cut this cut, with
// why for the caller to report. A plain copy falls through to the re-encode;
// copy-exact asked for a packet cut by name, so it fails here rather than
// delivering a decode the request ruled out.
func (r *Runner) declineCopy(spec CutSpec, why CopyDecline) (remuxOutcome, error) {
	if spec.SpliceTrims {
		return remuxOutcome{}, fmt.Errorf("%w: copy-exact needs a packet-level cut, and this one declined: %s; use --cut-mode accurate",
			waxerr.ErrIncompatibleSpec, copyRefusal(why))
	}
	return remuxOutcome{decline: why}, nil
}

// landedKeeps maps the spans a packet cut delivered back onto the time
// ranges the caller asked for: one for one, an open-ended tail keeping the
// request's end.
//
// It also counts the joins the cut moved and the largest single move. A join
// is a cut point, the boundary between one kept span and the next, which is
// what the caller removed a range at and what every report calls it. Its two
// edges are the tail of the span before and the head of the span after, and
// either or both may move; the join counts once. Counting edges instead would
// report two joins for the one range a caller asked to remove.
//
// Heads move in both copy modes (no container states a front trim); tails
// move only in the plain copy.
func landedKeeps(keeps []cutrange.Range, asked, landed []waxflow.Span, rate int) ([]cutrange.Range, int, time.Duration) {
	if len(keeps) == 0 {
		return nil, 0, 0 // no span, no join; Render refuses this before here
	}
	out := make([]cutrange.Range, len(keeps))
	moved := make([]bool, len(keeps)) // per join, indexed by the span after it
	maxSnap := time.Duration(0)
	for i, k := range keeps {
		out[i] = k
		if i >= len(landed) {
			continue
		}
		if landed[i].From != asked[i].From {
			out[i].Start = durationOf(landed[i].From, rate)
			moved[i] = true
			maxSnap = max(maxSnap, durationOf(absDiff(landed[i].From, asked[i].From), rate))
		}
		if asked[i].To != waxflow.ToEnd && landed[i].To != asked[i].To {
			out[i].End = durationOf(landed[i].To, rate)
			if i+1 < len(moved) {
				moved[i+1] = true // the same join as the next span's head
			}
			maxSnap = max(maxSnap, durationOf(absDiff(landed[i].To, asked[i].To), rate))
		}
	}
	// One count per interior join: moved[i] for i > 0 is the join before span
	// i, set by either of its two edges. moved[0] is the first span's head,
	// which is exact by construction and never a join.
	joins := 0
	for _, m := range moved[1:] {
		if m {
			joins++
		}
	}
	return out, joins, maxSnap
}

// durationOf converts a sample count at rate to a duration, the inverse of
// samplesOf.
func durationOf(n int64, rate int) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(n) / float64(rate) * float64(time.Second)))
}

// absDiff is |a-b| on sample counts.
func absDiff(a, b int64) int64 {
	if a < b {
		return b - a
	}
	return a - b
}

// cutReencode renders the cut by decoding: it slices the kept spans, concatenates
// them (with an optional crossfade), and re-encodes with spec.Encode. It reports
// the encode's level measurement and the damage the read found alongside; see
// Result.Levels and Result.InputWarnings.
func (r *Runner) cutReencode(ctx context.Context, input string, src container.Source, hint, outExt string, spec CutSpec, dst *tempfile.File) (Levels, []string, error) {
	// Composing may measure the source, which takes a slot; the encode below
	// takes its own once the composition is built.
	med, err := r.openComposed(ctx, input, src, hint, spec.Keeps, spec.Total, spec.Crossfade, spec.SourceSamples)
	if err != nil {
		return Levels{}, nil, err
	}
	defer med.Close()

	if err := r.acquire(ctx); err != nil {
		return Levels{}, nil, err
	}
	defer r.release()
	opts := encodeOptions(spec.Encode)
	name, _ := codecFormat(spec.Encode.Codec)
	opts.Container = containerFor(name, outExt)
	tres, err := r.engine.TranscodeMedia(ctx, med, dst, opts)
	if err != nil {
		return Levels{}, nil, err
	}
	return levelsOf(tres), sourceWarnings(tres.InputWarnings), nil
}

// openComposed builds the WaxFlow Media for the kept spans: a single Slice for
// one span, or a Concat of per-span slices (with an optional crossfade) for
// several. The caller closes the returned Media.
//
// It probes the source's rate to convert the time-domain keeps to sample
// spans. sourceSamples is 0 or the count a caller's Runner.MeasureLength
// delivered; see CutSpec.SourceSamples. A source whose headers only claim a
// length and that the caller did not measure is measured here, so no bounded
// span is ever planned against a number nothing confirmed; input names the file
// that measurement reads. The measurement takes a concurrency slot, so a caller
// holding one must not reach here.
func (r *Runner) openComposed(ctx context.Context, input string, src container.Source, hint string, keeps []cutrange.Range, total time.Duration, crossfade time.Duration, sourceSamples int64) (format.Media, error) {
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	// measured, not sourceSamples > 0: a file that holds no frames measures as
	// zero, and reverting to the header's claim there would plan the one span
	// shape WaxFlow reports as a blind request rather than a damaged file.
	measured := sourceSamples > 0
	if !measured && lengthClaimed(demux, track) {
		// Nobody measured, and the headers only claim: measure now. The caller
		// asked for a cut of this file, not for a guess about its length.
		length, lerr := r.MeasureLength(ctx, input)
		if lerr != nil {
			return nil, lerr
		}
		sourceSamples, measured = length.Samples, true
	}
	// bound is what a bounded span is clamped under. The measurement is it,
	// alone: MeasureLength returns the count the run enforces, already settled
	// against the declaration (container.SettleLength clamps a capped track to
	// what its packets hold and takes the raw run for one that only claimed a
	// length), so re-clamping here could only subtract from an answer that is
	// already the truth.
	bound := track.Samples
	if measured {
		bound = sourceSamples
		track.Samples, track.SamplesAdvisory, track.SamplesExact = sourceSamples, false, true
	}
	// An open-ended final span inherits the track's own claim about its
	// length, and a Concat refuses one that is advisory (a WMA nobody
	// measured) or absent altogether (raw ADTS, MP3 in an AIFF-C, both
	// SamplesAdvisory false with no claim at all), so the open form is used
	// only where the count is trusted or measured.
	openEnded := !track.SamplesAdvisory && track.Samples >= 0
	rate := track.Fmt.Rate

	// measured wraps each opened media in the count this cut was planned
	// against, so the slice's own up-front check accepts every span the plan
	// holds and an overrun reports as the damaged file it is rather than as a
	// span built blind. It is a no-op when nothing was measured.
	hold := func(m format.Media) format.Media {
		if !measured {
			return m
		}
		return waxflow.MeasuredMedia(m, sourceSamples)
	}

	if len(keeps) == 1 {
		from, to := sampleBounds(keeps[0], total, rate, bound, openEnded)
		med, err := format.Open(src, hint, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
		}
		sl, err := waxflow.Slice(hold(med), from, to)
		if err != nil {
			med.Close()
			return nil, err
		}
		return sl, nil
	}

	members := make([]waxflow.ConcatSource, len(keeps))
	for i, k := range keeps {
		from, to := sampleBounds(k, total, rate, bound, openEnded)
		st, err := waxflow.SpanTrack(track, from, to)
		if err != nil {
			return nil, err
		}
		members[i] = waxflow.ConcatSource{Track: st, Open: func() (format.Media, error) {
			m, err := format.Open(src, hint, nil)
			if err != nil {
				return nil, err
			}
			return waxflow.Slice(hold(m), from, to)
		}}
	}
	xfade := int64(0)
	if crossfade > 0 {
		xfade = samplesOf(crossfade, rate)
	}
	return waxflow.Concat(members, waxflow.ConcatOptions{Crossfade: xfade})
}

// OpenComposed opens the cut-composed Media for measurement (loudness), without
// re-encoding. The caller closes it. sourceSamples is 0 or the count a
// caller's Runner.MeasureLength delivered; see CutSpec.SourceSamples; a source
// whose headers only claim a length is measured here when it is 0.
func (r *Runner) OpenComposed(ctx context.Context, input string, keeps []cutrange.Range, total, crossfade time.Duration, sourceSamples int64) (format.Media, func() error, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return nil, nil, err
	}
	med, err := r.openComposed(ctx, input, src, hintFor(input), keeps, total, crossfade, sourceSamples)
	if err != nil {
		_ = closeSrc()
		return nil, nil, err
	}
	closer := func() error {
		cerr := med.Close()
		if serr := closeSrc(); cerr == nil {
			cerr = serr
		}
		return cerr
	}
	return med, closer, nil
}

// toSpans converts kept time ranges to WaxFlow sample spans. The final span uses
// ToEnd when it reaches the source end so the trailer resolves the exact length.
func toSpans(keeps []cutrange.Range, total time.Duration, rate int) []waxflow.Span {
	spans := make([]waxflow.Span, len(keeps))
	for i, k := range keeps {
		from := samplesOf(k.Start, rate)
		to := samplesOf(k.End, rate)
		if k.End >= total {
			to = waxflow.ToEnd
		}
		spans[i] = waxflow.Span{From: from, To: to}
	}
	return spans
}

// sampleBounds returns the [from, to) sample bounds of a kept range. A range
// reaching total takes ToEnd when openEnded, as toSpans does: an open-ended
// slice runs to whatever the source holds and declares no limit a short
// source can fall foul of, where a bounded one is held to its declaration
// (Slice refuses a source that ends inside a declared span). An interior
// bound, and a final one on a track whose claim cannot be trusted open, is
// clamped to bound, the count the opened media and the timeline both
// refuse to be asked past.
func sampleBounds(k cutrange.Range, total time.Duration, rate int, bound int64, openEnded bool) (from, to int64) {
	from = samplesOf(k.Start, rate)
	if k.End >= total && openEnded {
		to = waxflow.ToEnd
	} else {
		to = samplesOf(min(k.End, total), rate)
		if bound > 0 && to > bound {
			to = bound
		}
	}
	if bound > 0 && from > bound {
		from = bound
	}
	return from, to
}

// samplesOf converts a duration to a sample count at rate.
func samplesOf(d time.Duration, rate int) int64 {
	return int64(math.Round(d.Seconds() * float64(rate)))
}

// ValidateCrossfade checks whether the retained spans can supply the requested
// overlap. A crossfade consumes d from both sides of each join, so an interior
// span must be at least 2*d. Rejecting short spans up front avoids an encode that
// emits no audio.
func ValidateCrossfade(keeps []cutrange.Range, d time.Duration) error {
	if d <= 0 || len(keeps) < 2 {
		return nil
	}
	for i, k := range keeps {
		required := d
		if i != 0 && i != len(keeps)-1 {
			required = 2 * d
		}
		if k.Duration() < required {
			return fmt.Errorf("%w: crossfade %v is too long for the %v span kept at %v (needs %v)",
				waxerr.ErrIncompatibleSpec, d, k.Duration(), k.Start, required)
		}
	}
	return nil
}
