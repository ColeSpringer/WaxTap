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

func TestWarnContainerExtMismatch(t *testing.T) {
	res := func(outPath, srcExt string) *waxtap.Result {
		return &waxtap.Result{OutputPath: outPath, SourceFormat: waxtap.Format{Extension: srcExt}}
	}
	cases := []struct {
		name string
		df   *downloadFlags
		res  *waxtap.Result
		want string // substring expected in the note; "" means no note
	}{
		{"keep-source m4a on webm", &downloadFlags{}, res("/tmp/out.m4a", "webm"), "output path uses .m4a, but the source container is .webm"},
		{"keep-source matching ext", &downloadFlags{}, res("/tmp/out.webm", "webm"), ""},
		{"keep-source case-insensitive match", &downloadFlags{}, res("/tmp/out.WEBM", "webm"), ""},
		// .m4a and .mp4 use the same container.
		{"mp4 output on m4a stream", &downloadFlags{}, res("/tmp/out.mp4", "m4a"), ""},
		{"m4a output on mp4 stream", &downloadFlags{}, res("/tmp/out.m4a", "mp4"), ""},
		// A .opus extension usually implies Ogg, not WebM.
		{"opus output on webm stream still warns", &downloadFlags{}, res("/tmp/out.opus", "webm"), "output path uses .opus, but the source container is .webm"},
		// Any ffmpeg edit remuxes into the named container.
		{"cut remux suppresses", &downloadFlags{}, &waxtap.Result{OutputPath: "/tmp/clip.mka", SourceFormat: waxtap.Format{Extension: "webm"}, CutApplied: true}, ""},
		{"sponsorblock remux suppresses", &downloadFlags{}, &waxtap.Result{OutputPath: "/tmp/clip.mka", SourceFormat: waxtap.Format{Extension: "webm"}, SponsorBlockApplied: true}, ""},
		{"downmix transcode suppresses", &downloadFlags{}, &waxtap.Result{OutputPath: "/tmp/out.ogg", SourceFormat: waxtap.Format{Extension: "webm"}, Transcoded: true}, ""},
		{"loudness apply suppresses", &downloadFlags{}, &waxtap.Result{OutputPath: "/tmp/out.mka", SourceFormat: waxtap.Format{Extension: "webm"}, LoudnessApplied: true}, ""},
		// A --format (copy or transcode) muxes to the named container via ffmpeg.
		{"format copy suppresses", &downloadFlags{format: "copy"}, res("/tmp/out.ogg", "webm"), ""},
		{"format transcode suppresses", &downloadFlags{format: "flac"}, res("/tmp/out.flac", "webm"), ""},
		{"no output path", &downloadFlags{}, res("", "webm"), ""},
		{"no source extension", &downloadFlags{}, res("/tmp/out.m4a", ""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnContainerExtMismatch(noteEnv(&buf), tc.df, tc.res)
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

func TestWarnBitrateIgnoredIfLossless(t *testing.T) {
	cases := []struct {
		name    string
		tf      waxtap.TranscodeFormat
		bitrate int
		want    bool
	}{
		{"flac with bitrate", waxtap.FormatFLAC, 320000, true},
		{"alac with bitrate", waxtap.FormatALAC, 320000, true},
		{"wav with bitrate", waxtap.FormatWAV, 320000, true},
		{"copy with bitrate", waxtap.FormatCopy, 320000, true},
		{"lossy mp3 with bitrate", waxtap.FormatMP3, 320000, false},
		{"lossy opus with bitrate", waxtap.FormatOpus, 320000, false},
		{"flac without bitrate", waxtap.FormatFLAC, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnBitrateIgnoredIfLossless(noteEnv(&buf), tc.tf, tc.bitrate)
			if got := strings.Contains(buf.String(), "--bitrate is ignored"); got != tc.want {
				t.Errorf("note emitted=%v (%q), want %v", got, buf.String(), tc.want)
			}
		})
	}
}

func TestNoteUseBothWebSources(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *appConfig
		wantFire   bool
		wantSetWeb bool // message includes "set --client web"
	}{
		{"token only default client", &appConfig{potokenURL: "u"}, true, true},
		{"token only client web", &appConfig{potokenURL: "u", client: "web"}, true, false},
		// A deliberately forced non-WEB client is not steered toward WEB.
		{"token only forced android", &appConfig{potokenURL: "u", client: "android_vr"}, false, false},
		{"context only default", &appConfig{potokenURL: "u", playerContextURL: "p"}, true, true},
		{"session only web", &appConfig{potokenURL: "u", sessionURL: "s", client: "web"}, true, false},
		{"both sources", &appConfig{potokenURL: "u", playerContextURL: "p", sessionURL: "s", client: "web"}, false, false},
		{"no web config", &appConfig{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			noteUseBothWebSources(&appEnv{out: io.Discard, errOut: &buf, cfg: tc.cfg})
			got := buf.String()
			if fired := strings.Contains(got, "for WEB extraction"); fired != tc.wantFire {
				t.Fatalf("fired=%v (%q), want %v", fired, got, tc.wantFire)
			}
			if tc.wantFire {
				if hasSetWeb := strings.Contains(got, "set --client web"); hasSetWeb != tc.wantSetWeb {
					t.Errorf("set-client-web=%v (%q), want %v", hasSetWeb, got, tc.wantSetWeb)
				}
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
