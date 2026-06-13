package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

// noteEnv returns an environment that captures informational output.
func noteEnv(buf *bytes.Buffer) *appEnv {
	return &appEnv{out: io.Discard, errOut: buf, cfg: &appConfig{}}
}

func TestWarnChannelLayout(t *testing.T) {
	res := func(out, src int) *waxtap.Result {
		return &waxtap.Result{OutputFormat: waxtap.Format{Channels: out}, SourceFormat: waxtap.Format{Channels: src}}
	}
	cases := []struct {
		name string
		df   *downloadFlags
		res  *waxtap.Result
		want string // substring expected in the note; "" means no note
	}{
		{"explicit surround on stereo", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutSurround}, res(2, 2), "requested surround; delivered stereo (2ch)"},
		{"explicit stereo on mono", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutStereo}, res(1, 1), "requested stereo; delivered mono (1ch)"},
		{"default stereo satisfied", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutStereo}, res(2, 2), ""},
		{"not explicit", &downloadFlags{channelsExplicit: false, layout: waxtap.LayoutSurround}, res(2, 2), ""},
		{"itag ignores layout", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutSurround, itag: 251}, res(2, 2), ""},
		{"any never warns", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutAny}, res(2, 2), ""},
		{"unknown delivered count", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutSurround}, res(0, 0), ""},
		{"falls back to source channels", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutSurround}, res(0, 2), "delivered stereo (2ch)"},
		{"surround delivered names the count", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutStereo}, res(6, 6), "requested stereo; delivered 6ch"},
		// A downmix transcode can omit the output channel count. Report the requested
		// target instead of the source layout.
		{"downmix folds surround to stereo", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutStereo, downmix: true}, res(0, 6), ""},
		// Downmixing does not turn a mono source into stereo.
		{"downmix stereo on mono still warns", &downloadFlags{channelsExplicit: true, layout: waxtap.LayoutStereo, downmix: true}, res(0, 1), "delivered mono (1ch)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnChannelLayout(noteEnv(&buf), tc.df, tc.res)
			got := buf.String()
			switch {
			case tc.want == "" && got != "":
				t.Errorf("note = %q, want none", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("note = %q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestMeasureNote(t *testing.T) {
	cases := []struct {
		name string
		res  *waxtap.Result
		want bool
	}{
		{"measure only", &waxtap.Result{LoudnessMeasured: true, OutputPath: "/x.webm"}, true},
		{"measure + transcode", &waxtap.Result{LoudnessMeasured: true, Transcoded: true, OutputPath: "/x.flac"}, false},
		{"measure + cut", &waxtap.Result{LoudnessMeasured: true, CutApplied: true, OutputPath: "/x.webm"}, false},
		{"normalize not measure", &waxtap.Result{LoudnessApplied: true, OutputPath: "/x.flac"}, false},
		{"no output path", &waxtap.Result{LoudnessMeasured: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			measureNote(noteEnv(&buf), tc.res)
			if got := strings.Contains(buf.String(), "unaltered copy"); got != tc.want {
				t.Errorf("measureNote emitted=%v (%q), want %v", got, buf.String(), tc.want)
			}
		})
	}
}
