package waxtap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// wavFrom writes bytes as a WAV fixture in dir and returns its path.
func wavFrom(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A loudness figure that comes back unusable is reported with the reason it is
// unusable, because "n/a" and a null in the JSON look identical whether the
// clip was too short, silent, or merely very quiet, and the three call for
// different things from the user.
func TestProcessWarnsLoudnessUnmeasurable(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		data []byte
		want string // "" means no warning at all
	}{
		{"sub-gate clip", mediatest.ToneWAVMs(440, 300, 2, 44100), "shorter than the 400 ms"},
		{"digital silence", mediatest.SilenceWAV(5, 2), "digital silence"},
		{"ordinary tone", mediatest.SineWAV(3, 2), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := wavFrom(t, dir, "in.wav", tc.data)

			// Measure-only: the input side alone, which is finding 12's shape,
			// a silent track reporting nulls with nothing to explain them.
			res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
				Input:       in,
				ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly, Target: -14}},
			})
			if err != nil {
				t.Fatalf("measure-only Process: %v", err)
			}
			detail := warningDetail(res, WarnLoudnessUnmeasurable)
			switch {
			case tc.want == "" && detail != "":
				t.Fatalf("a measurable input warned: %q", detail)
			case tc.want == "":
				return
			case detail == "":
				t.Fatalf("no %s warning; warnings = %+v", WarnLoudnessUnmeasurable, res.Warnings)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to name the cause (%q)", detail, tc.want)
			}
			if !strings.HasPrefix(detail, "input ") {
				t.Errorf("detail = %q, want it to say which side could not be measured", detail)
			}
		})
	}
}

// Applying normalization to silence leaves both sides unmeasurable, and both
// are reported: the output line is the one a reader checks to see the run
// landed, so it must not be the one left unexplained.
func TestProcessWarnsLoudnessUnmeasurableBothSides(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := wavFrom(t, dir, "silent.wav", mediatest.SilenceWAV(5, 2))

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "out.wav")),
			Transcode: &TranscodeSpec{Format: FormatWAV},
			Loudness:  &LoudnessSpec{Mode: LoudnessApply, Target: -14},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	var sides []string
	for _, w := range res.Warnings {
		if w.Code == WarnLoudnessUnmeasurable {
			sides = append(sides, strings.SplitN(w.Detail, " ", 2)[0])
		}
	}
	if len(sides) != 2 || sides[0] != "input" || sides[1] != "output" {
		t.Fatalf("warned sides = %v, want [input output]; warnings = %+v", sides, res.Warnings)
	}
}
