package main

import (
	"bytes"
	"errors"
	"fmt"
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

// TestNoteDroppedPlaylist verifies the stderr note for watch URLs that also carry
// a playlist. Bare IDs, plain video URLs, and --quiet stay silent.
func TestNoteDroppedPlaylist(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		quiet    bool
		wantNote bool
	}{
		{"watch with list", "https://www.youtube.com/watch?v=dummyVideo0&list=PLtest123456789", false, true},
		{"plain video url", "https://www.youtube.com/watch?v=dummyVideo0", false, false},
		{"bare video id", "dummyVideo0", false, false},
		{"watch with list but quiet", "https://www.youtube.com/watch?v=dummyVideo0&list=PLtest123456789", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := &appEnv{out: io.Discard, errOut: &buf, cfg: &appConfig{quiet: tc.quiet}}
			noteDroppedPlaylist(env, tc.input, "download the playlist with --list")
			got := buf.String()
			switch {
			case tc.wantNote && !strings.Contains(got, "ignoring playlist PLtest123456789"):
				t.Errorf("note = %q, want it to name the dropped playlist", got)
			case !tc.wantNote && got != "":
				t.Errorf("note = %q, want none", got)
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

// TestNoteUseBothWebSourcesIfActionable covers the outcome-aware nudge: it fires
// only when a WEB cap, fallback, or failure makes a second source actionable, and
// stays silent on a clean WEB_CONTEXT delivery or a non-WEB error. The config gate
// is a single-source WEB path so webSourcesNote returns true.
func TestNoteUseBothWebSourcesIfActionable(t *testing.T) {
	cfg := &appConfig{playerContextURL: "p", potokenURL: "u"} // one WEB source, not both

	fires := func(res *waxtap.Result, err error) bool {
		var buf bytes.Buffer
		noteUseBothWebSourcesIfActionable(&appEnv{out: io.Discard, errOut: &buf, cfg: cfg}, res, err)
		return strings.Contains(buf.String(), "for WEB extraction")
	}
	withWarn := func(code waxtap.WarningCode) *waxtap.Result {
		return &waxtap.Result{Client: "ANDROID_VR", Warnings: []waxtap.Warning{{Code: code}}}
	}

	t.Run("nil res with incomplete-stream fires", func(t *testing.T) {
		if !fires(nil, fmt.Errorf("capped: %w", waxtap.ErrIncompleteStream)) {
			t.Error("a nil result with a WEB-relevant error should fire without panicking")
		}
	})
	t.Run("nil res with needs-po-token fires", func(t *testing.T) {
		if !fires(nil, fmt.Errorf("web: %w", waxtap.ErrNeedsPOToken)) {
			t.Error("ErrNeedsPOToken on a partial-WEB config should keep the supply-both pointer")
		}
	})
	t.Run("nil res with disk error stays silent", func(t *testing.T) {
		if fires(nil, errors.New("write /out/x: no space left on device")) {
			t.Error("a plain disk error must not trigger the WEB nudge")
		}
	})
	t.Run("clean WEB_CONTEXT success stays silent", func(t *testing.T) {
		if fires(&waxtap.Result{Client: "WEB_CONTEXT"}, nil) {
			t.Error("a clean delivery with no warnings must stay silent")
		}
	})
	t.Run("clean default-chain success stays silent", func(t *testing.T) {
		// Regression guard: ANDROID_VR is first-in-chain, not a WEB fallback, so a
		// clean default-chain download must not nudge on a partial-WEB config.
		if fires(&waxtap.Result{Client: "ANDROID_VR"}, nil) {
			t.Error("a clean default-chain (ANDROID_VR) success must stay silent")
		}
	})
	t.Run("context retry fires", func(t *testing.T) {
		if !fires(withWarn(waxtap.WarnWebContextRetry), nil) {
			t.Error("a WEB context cap/retry should fire the nudge")
		}
	})
	t.Run("context fallback fires", func(t *testing.T) {
		if !fires(withWarn(waxtap.WarnWebContextFallback), nil) {
			t.Error("a WEB context fallback to another client should fire the nudge")
		}
	})
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
