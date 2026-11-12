package waxtap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
)

// The input-damage warning leads with a sentence; Result.InputLoss is the
// same two counts as numbers. A file cut short reports how far it ends short
// of the length it declares and no silence, a FLAC with a hole the reverse,
// and a whole file reports nothing at all.
func TestProcessCountsInputLoss(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	intact, truncated := damagedFixture(t, dir, "in.flac", FormatFLAC)
	_, holed := corruptedFixture(t, dir, "hole.flac", FormatFLAC, 512)

	samples := func(path string) int64 {
		t.Helper()
		a, _ := mustProbeFile(t, c, path).AudioStream()
		return a.Samples
	}
	toWAV := func(name, in string) *Result {
		t.Helper()
		res, err := c.Process(ctx, ProcessRequest{
			Input:       in,
			ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, name)), Transcode: &TranscodeSpec{Format: FormatWAV}},
		})
		if err != nil {
			t.Fatalf("Process %s: %v", filepath.Base(in), err)
		}
		return res
	}

	short := toWAV("short.wav", truncated)
	want := samples(intact) - samples(truncated)
	if short.InputLoss == nil {
		t.Fatalf("InputLoss = nil on a file cut short; warnings = %+v", short.Warnings)
	}
	if got := *short.InputLoss; got != (InputLoss{TruncatedSamples: want, SampleRate: 44100}) {
		t.Errorf("InputLoss = %+v, want %d truncated at 44100 Hz (the declared length less what the file holds) and none lost", got, want)
	}
	if got, wantDur := short.InputLoss.Truncated(), media.SampleTime(want, 44100); got != wantDur || short.InputLoss.Lost() != 0 {
		t.Errorf("Truncated, Lost = %v, %v, want %v and 0", got, short.InputLoss.Lost(), wantDur)
	}
	if !hasWarning(short, WarnInputDamage) {
		t.Error("the count came with no input-damage warning to word it")
	}

	hole := toWAV("hole.wav", holed)
	if hole.InputLoss == nil || hole.InputLoss.LostSamples <= 0 || hole.InputLoss.TruncatedSamples != 0 || hole.InputLoss.SampleRate != 44100 {
		t.Errorf("InputLoss = %+v on a FLAC with a hole, want its silence counted and nothing truncated", hole.InputLoss)
	}

	if whole := toWAV("whole.wav", intact); whole.InputLoss != nil {
		t.Errorf("InputLoss = %+v on a whole file, want nil", whole.InputLoss)
	}

	// A measurement delivers nothing, so it counts at the file's own rate, and
	// Measure hands the count on beside the warnings.
	m, err := c.Measure(ctx, truncated)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.InputLoss == nil || *m.InputLoss != *short.InputLoss {
		t.Errorf("Measure's InputLoss = %+v, want the %+v the write counted", m.InputLoss, *short.InputLoss)
	}
	if clean, err := c.Measure(ctx, intact); err != nil || clean.InputLoss != nil {
		t.Errorf("Measure of a whole file: InputLoss = %+v, err = %v", clean.InputLoss, err)
	}
}

// The counts are at the rate of the audio delivered. An encode that resamples
// states them at its output's rate, so they add up against the file the
// caller is holding, and the time they cover does not move.
func TestInputLossFollowsTheOutputRate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	_, truncated := damagedFixture(t, dir, "in.flac", FormatFLAC) // 44.1 kHz

	process := func(name string, f TranscodeFormat) *InputLoss {
		t.Helper()
		res, err := c.Process(ctx, ProcessRequest{
			Input:       truncated,
			ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, name)), Transcode: &TranscodeSpec{Format: f}},
		})
		if err != nil {
			t.Fatalf("Process to %s: %v", name, err)
		}
		if res.InputLoss == nil {
			t.Fatalf("%s: InputLoss = nil; warnings = %+v", name, res.Warnings)
		}
		return res.InputLoss
	}
	native := process("out.wav", FormatWAV)
	opus := process("out.opus", FormatOpus) // Opus runs at 48 kHz
	if native.SampleRate != 44100 || opus.SampleRate != 48000 {
		t.Fatalf("rates = %d and %d, want 44100 for the WAV and 48000 for the Opus", native.SampleRate, opus.SampleRate)
	}
	if opus.TruncatedSamples <= native.TruncatedSamples {
		t.Errorf("the Opus counts %d samples and the WAV %d; want the count rescaled to the higher rate", opus.TruncatedSamples, native.TruncatedSamples)
	}
	if d := (opus.Truncated() - native.Truncated()).Abs(); d > time.Millisecond {
		t.Errorf("the Opus is short by %v and the WAV by %v; want one length of missing audio at either rate", opus.Truncated(), native.Truncated())
	}
}

// A cut counts what its own spans lost, so the numbers follow the sentence:
// present for a cut that keeps the end of a file cut short, absent for one
// that removes the tail and so holds everything it asked for.
func TestCutInputLossOnlyThroughTheEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	_, truncated := damagedFixture(t, dir, "in.flac", FormatFLAC)

	cut := func(name string, r TimeRange) *Result {
		t.Helper()
		res, err := c.Process(ctx, ProcessRequest{Input: truncated, ProcessSpec: ProcessSpec{
			Output: ToFile(filepath.Join(dir, name)),
			Cut:    &CutSpec{Ranges: []TimeRange{r}},
		}})
		if err != nil {
			t.Fatalf("cut %s: %v", name, err)
		}
		return res
	}
	if res := cut("kept-end.flac", TimeRange{Start: 200 * time.Millisecond, End: 500 * time.Millisecond}); res.InputLoss == nil || res.InputLoss.TruncatedSamples <= 0 {
		t.Errorf("a cut that keeps the end: InputLoss = %+v, want what the end lost", res.InputLoss)
	}
	res := cut("dropped-end.flac", TimeRange{Start: time.Second, End: 3 * time.Second})
	if res.InputLoss != nil {
		t.Errorf("a cut that removes the tail: InputLoss = %+v, want nil", res.InputLoss)
	}
	if !hasWarning(res, WarnInputDamage) {
		t.Error("the file's own findings went missing with the count")
	}
}

// An album reports each track's count in track order, nil for a track that
// lost nothing, from a measurement and from a write alike. Only a loss under
// the half second an album refuses a track over can be there.
func TestAlbumCountsInputLossPerTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	intact, clipped := clippedFixture(t, dir, "track.flac", FormatFLAC)

	check := func(what string, got []*InputLoss) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("%s: %d entries, want one per track", what, len(got))
		}
		if got[0] != nil {
			t.Errorf("%s: the whole track counts %+v, want nil", what, got[0])
		}
		l := got[1]
		if l == nil || l.TruncatedSamples <= 0 || l.LostSamples != 0 || l.SampleRate != 44100 {
			t.Fatalf("%s: the clipped track counts %+v, want what it ends short by at its own 44100 Hz", what, l)
		}
		if d := l.Truncated(); d > 500*time.Millisecond {
			t.Errorf("%s: the clipped track is short by %v, past the half second an album refuses", what, d)
		}
	}
	measured, err := c.MeasureAlbum(ctx, []string{intact, clipped})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	check("MeasureAlbum", measured.InputLoss)

	// A write at the input's rate counts what the measurement did.
	same, err := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: intact, Output: filepath.Join(dir, "one.flac")},
		{Input: clipped, Output: filepath.Join(dir, "two.flac")},
	}, -14, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	check("ProcessAlbum", same.InputLoss)
	if *same.InputLoss[1] != *measured.InputLoss[1] {
		t.Errorf("a write counts %+v and a measurement %+v for the one track", *same.InputLoss[1], *measured.InputLoss[1])
	}

	// One that resamples counts at the rate of the file it wrote, as a single
	// file's does, and the warning beside it states the same number: the
	// sentence and the count are one read's.
	opus, err := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: intact, Output: filepath.Join(dir, "one.opus")},
		{Input: clipped, Output: filepath.Join(dir, "two.opus")},
	}, -14, TranscodeSpec{Format: FormatOpus})
	if err != nil {
		t.Fatalf("ProcessAlbum to Opus: %v", err)
	}
	if opus.InputLoss[0] != nil {
		t.Errorf("the whole track counts %+v, want nil", opus.InputLoss[0])
	}
	l := opus.InputLoss[1]
	if l == nil || l.SampleRate != 48000 || l.LostSamples != 0 {
		t.Fatalf("the clipped track's Opus counts %+v, want its truncation at the 48000 Hz it was written at", l)
	}
	if d := (l.Truncated() - measured.InputLoss[1].Truncated()).Abs(); d > time.Millisecond {
		t.Errorf("the Opus is short by %v and the measurement by %v; want one length of missing audio", l.Truncated(), measured.InputLoss[1].Truncated())
	}
	if detail, want := warningIn(opus.Warnings, WarnInputDamage), fmt.Sprintf("the audio ends %d samples", l.TruncatedSamples); !strings.Contains(detail, want) {
		t.Errorf("warning = %q, want it to state the count on the result (%q)", detail, want)
	}
}

// An album refuses a member that read short of its declared length, and
// credits the truncation the engine counted only where that length still
// includes it. A length the probe restated has it out already: crediting it
// again let through a track cut short by 0.45 s whose read then ended another
// 0.9 s early, 1.35 s of it missing from the album's gain.
func TestAlbumShortDecodeDoesNotCreditTruncationTwice(t *testing.T) {
	at := func(d time.Duration) media.Loss {
		return media.Loss{TruncatedSamples: int64(d.Seconds() * 44100), Rate: 44100}
	}
	const cut = 450 * time.Millisecond // under the half second refused on its own
	inputs := []string{"track.m4a"}
	for _, tc := range []struct {
		name     string
		declared time.Duration // the probe's length
		settled  media.Loss    // the truncation the probe already took out of it
		read     time.Duration // what the measurement covered
		refuse   bool
	}{
		{"restated length, read to it", 9550 * time.Millisecond, at(cut), 9550 * time.Millisecond, false},
		{"restated length, read ends 0.9 s early", 9550 * time.Millisecond, at(cut), 8650 * time.Millisecond, true},
		{"claimed length, the count explains the gap", 10 * time.Second, media.Loss{}, 9550 * time.Millisecond, false},
		{"claimed length, read ends 0.9 s past the count", 10 * time.Second, media.Loss{}, 8650 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := []albumProbe{{duration: tc.declared, settled: tc.settled}}
			perTrack := []loudness.Loudness{{Duration: tc.read, Loss: at(cut)}}
			err := albumShortDecode(inputs, probes, perTrack)
			if tc.refuse != (err != nil) {
				t.Fatalf("err = %v, want refused=%v", err, tc.refuse)
			}
			if err != nil && (!errors.Is(err, ErrUnsupportedInput) || !strings.Contains(err.Error(), "measurement covered")) {
				t.Errorf("err = %v, want the short read named as one", err)
			}
		})
	}
	// An advisory length holds a member to nothing, and the engine's own
	// count is still judged on its own.
	probes := []albumProbe{{duration: 10 * time.Second, advisory: true}}
	if err := albumShortDecode(inputs, probes, []loudness.Loudness{{Duration: 5 * time.Second, Loss: at(cut)}}); err != nil {
		t.Errorf("an advisory length refused a member: %v", err)
	}
	if err := albumShortDecode(inputs, probes, []loudness.Loudness{{Duration: 5 * time.Second, Loss: at(time.Second)}}); err == nil {
		t.Error("a second cut short passed because the length was advisory")
	}
}

// The row that leads a folded warning is the first whose detail counts what
// it lost, whichever fold: a split's pieces and an album's tracks read alike.
func TestFoldedDamageLeadsWithACountedRow(t *testing.T) {
	fold := func(rows ...[2]any) string {
		a := albumInputRemarks{code: WarnInputDamage}
		for _, r := range rows {
			a.observe(r[0].(string)+".flac", "detail of "+r[0].(string), r[1].(bool))
		}
		em := newEmitter(nil, "")
		a.warn(em)
		ws := em.collected()
		if len(ws) != 1 {
			t.Fatalf("warnings = %+v, want one", ws)
		}
		return ws[0].Detail
	}
	for name, tc := range map[string]struct {
		rows []([2]any)
		want string
	}{
		"none counted: the first leads":            {[][2]any{{"one", false}, {"two", false}}, "one.flac: detail of one (and 1 more track)"},
		"a counted row leads an earlier plain one": {[][2]any{{"one", false}, {"two", true}, {"three", false}}, "two.flac: detail of two (and 2 more tracks)"},
		"the first counted row keeps the lead":     {[][2]any{{"one", true}, {"two", true}}, "one.flac: detail of one (and 1 more track)"},
		"a later plain row never takes it":         {[][2]any{{"one", true}, {"two", false}}, "one.flac: detail of one (and 1 more track)"},
	} {
		if got := fold(tc.rows...); got != tc.want {
			t.Errorf("%s: detail = %q, want %q", name, got, tc.want)
		}
	}
}

// The count belongs to local processing, as the warning does. A YouTube
// delivery that came up short is a failure of the delivery, and nothing on the
// pipeline's result turns it into the caller's input to count: the facade
// sets InputLoss beside the warning, in Process alone.
func TestInputLossIsTheLocalPathsAlone(t *testing.T) {
	p := pipeline.Result{SourceLoss: media.Loss{LostSamples: 4096, TruncatedSamples: 8192, Rate: 48000}}
	if res := newProcessResult(SourceYouTube, p, Format{Codec: "opus"}, 0); res.InputLoss != nil {
		t.Errorf("a YouTube result carries InputLoss %+v", res.InputLoss)
	}
	got := inputLossOf(p.SourceLoss)
	if got == nil || *got != (InputLoss{LostSamples: 4096, TruncatedSamples: 8192, SampleRate: 48000}) {
		t.Errorf("inputLossOf = %+v, want the engine's two counts and their rate", got)
	}
	if got.Lost() != media.SampleTime(4096, 48000) || got.Truncated() != media.SampleTime(8192, 48000) {
		t.Errorf("Lost, Truncated = %v, %v, want the counts timed at 48000 Hz", got.Lost(), got.Truncated())
	}
	if inputLossOf(media.Loss{Rate: 48000}) != nil {
		t.Error("a source that lost nothing is not a nil InputLoss")
	}
	// No rate, no time: the counts stand, and nothing says how long they are.
	if bare := (InputLoss{LostSamples: 4096, TruncatedSamples: 8192}); bare.Lost() != 0 || bare.Truncated() != 0 {
		t.Errorf("counts with no rate timed at %v and %v", bare.Lost(), bare.Truncated())
	}
}
