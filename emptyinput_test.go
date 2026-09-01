package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// emptyWAV is a container that parses and declares a codec, rate and layout but
// carries no frames at all. It is the one input whose zero duration is a fact
// rather than a gap in the header, which is the distinction every assertion in
// this file rests on.
func emptyWAV() []byte { return mediatest.ToneWAVMs(440, 0, 2, 44100) }

// A file with no audio frames converts, and says so. The alternative shapes are
// both worse: failing takes a batch down over one file, and succeeding in
// silence hands back an empty output that looks like a WaxTap defect.
func TestProcessWarnsEmptyInput(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		spec ProcessSpec
	}{
		{"transcode", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}}},
		{"normalize", ProcessSpec{
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Loudness:  &LoudnessSpec{Mode: LoudnessApply, Target: -14},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := wavFrom(t, dir, "empty.wav", emptyWAV())
			spec := tc.spec
			spec.Output = ToFile(filepath.Join(dir, "out.flac"))

			res, err := newOfflineClient(t).Process(ctx, ProcessRequest{Input: in, ProcessSpec: spec})
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			detail := warningDetail(res, WarnEmptyInput)
			if detail == "" {
				t.Fatalf("no %s warning; warnings = %+v", WarnEmptyInput, res.Warnings)
			}
			if !strings.Contains(detail, "no audio frames") {
				t.Errorf("detail = %q, want it to name the cause", detail)
			}
			if _, serr := os.Stat(res.OutputPath); serr != nil {
				t.Errorf("no output delivered: %v", serr)
			}
		})
	}
}

// The measurement's own explanation must name the emptiness too. A zero-frame
// file has a -Inf sample peak, so without the frame count the cause fell through
// to "digital silence", which describes samples the file does not have.
func TestEmptyInputIsNotCalledDigitalSilence(t *testing.T) {
	dir := t.TempDir()
	in := wavFrom(t, dir, "empty.wav", emptyWAV())

	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly, Target: -14}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	detail := warningDetail(res, WarnLoudnessUnmeasurable)
	if detail == "" {
		t.Fatalf("no %s warning; warnings = %+v", WarnLoudnessUnmeasurable, res.Warnings)
	}
	if strings.Contains(detail, "digital silence") {
		t.Errorf("detail = %q, want the frame count named rather than the signal", detail)
	}
	if !strings.Contains(detail, "no audio frames") {
		t.Errorf("detail = %q, want it to say the track has no frames", detail)
	}
}

// A cut is the one request an empty input cannot satisfy, and the refusal must
// say why: "unknown duration" sent the user looking for a header problem that
// is not there.
func TestCutRejectsEmptyInputWithTheRealReason(t *testing.T) {
	dir := t.TempDir()
	in := wavFrom(t, dir, "empty.wav", emptyWAV())

	_, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "out.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}},
		},
	})
	if err == nil {
		t.Fatal("cutting an empty input succeeded")
	}
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Errorf("err = %v, want ErrUnsupportedInput", err)
	}
	if !strings.Contains(err.Error(), "no audio frames") {
		t.Errorf("err = %q, want it to name the empty track", err)
	}
	if strings.Contains(err.Error(), "unknown duration") {
		t.Errorf("err = %q, still claims the duration is merely unknown", err)
	}
}

// An ordinary file must not trip any of this: a positive frame count is the
// common case and the warning has to stay off it.
func TestOrdinaryInputDoesNotWarnEmpty(t *testing.T) {
	dir := t.TempDir()
	in := wavFrom(t, dir, "tone.wav", mediatest.SineWAV(3, 2))

	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "out.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if d := warningDetail(res, WarnEmptyInput); d != "" {
		t.Errorf("an ordinary tone warned: %q", d)
	}
}
