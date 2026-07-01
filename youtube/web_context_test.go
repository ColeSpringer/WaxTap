package youtube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
)

func sampleContext() potoken.PlayerContext {
	return potoken.PlayerContext{
		ServerAbrURL:    "https://rr3.googlevideo.com/videoplayback?expire=1781138473&n=SCRAMBLED&sabr=1",
		PlayerURL:       "https://www.youtube.com/s/player/444511ca/player_es6.vflset/en_US/base.js",
		UstreamerConfig: "dXN0cmVhbWVy",
		VisitorData:     "CgtVQ19WSVNJVE9SXzEhqA",
		ClientVersion:   "2.20260606.02.00",
		Title:           "Big Buck Bunny",
		Author:          "Blender",
		LengthSeconds:   634,
		AudioFormats: []potoken.PlayerContextFormat{
			{Itag: 251, LMT: "1719185012384481", XTags: "", MimeType: `audio/webm; codecs="opus"`, Bitrate: 143452, AudioChannels: 2, AudioSampleRate: 48000, ContentLength: 9700000, ApproxDurationMs: 634624},
			{Itag: 140, LMT: "1719185037000000", XTags: "", MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 130992, AudioChannels: 2, AudioSampleRate: 44100, ContentLength: 10300000, ApproxDurationMs: 634590},
		},
	}
}

func webContextClient(pc potoken.PlayerContext, err error) *Client {
	return New(Config{
		GL: "US",
		PlayerContextProvider: potoken.PlayerContextProviderFunc(
			func(context.Context, string) (potoken.PlayerContext, error) { return pc, err },
		),
	})
}

func TestExtractWebContextMapping(t *testing.T) {
	c := webContextClient(sampleContext(), nil)
	if !c.WebContextConfigured() {
		t.Fatal("WebContextConfigured = false, want true")
	}
	ext, err := c.ExtractWebContext(context.Background(), "aqz-KE-bpKQ")
	if err != nil {
		t.Fatalf("ExtractWebContext: %v", err)
	}

	if ext.serverAbrURL != sampleContext().ServerAbrURL {
		t.Errorf("serverAbrURL = %q, want the raw (still-scrambled) context URL", ext.serverAbrURL)
	}
	if ext.ustreamerConfig != "dXN0cmVhbWVy" {
		t.Errorf("ustreamerConfig = %q", ext.ustreamerConfig)
	}
	if ext.playerURL != sampleContext().PlayerURL {
		t.Errorf("playerURL = %q, want the context's base.js (pins the n-descramble)", ext.playerURL)
	}
	if !ext.webContext {
		t.Error("webContext = false, want true (drives reextract via the context path)")
	}
	if ext.session.visitorData != sampleContext().VisitorData {
		t.Errorf("session.visitorData = %q, want the context visitorData", ext.session.visitorData)
	}
	if ext.session.source != visitorAdopted {
		t.Errorf("session.source = %v, want visitorAdopted (never overwritten)", ext.session.source)
	}

	// Profile: WEB_CONTEXT, GVS-only, no player token, no signature timestamp.
	p := ext.profile
	if p.Name != "WEB_CONTEXT" || p.InnerTubeID != 1 {
		t.Errorf("profile = %q/%d, want WEB_CONTEXT/1", p.Name, p.InnerTubeID)
	}
	if p.Version != "2.20260606.02.00" {
		t.Errorf("profile.Version = %q, want the context client_version", p.Version)
	}
	if !p.requiresPOToken(potoken.ScopeGVS) {
		t.Error("WEB_CONTEXT must require a GVS PO token")
	}
	if p.requiresPOToken(potoken.ScopePlayer) {
		t.Error("WEB_CONTEXT must NOT require a player PO token (no /player call)")
	}
	if p.NeedsSignatureTimestamp {
		t.Error("WEB_CONTEXT must not need a signature timestamp")
	}

	// Video metadata + formats parallel to rawAudio.
	v := ext.video
	if v.ID != "aqz-KE-bpKQ" || v.Title != "Big Buck Bunny" || v.Author != "Blender" {
		t.Errorf("video = %+v, want id/title/author populated", v)
	}
	if v.Duration != 634*time.Second {
		t.Errorf("video.Duration = %v, want 634s", v.Duration)
	}
	if len(ext.rawAudio) != 2 || len(v.Formats) != 2 {
		t.Fatalf("formats: rawAudio=%d public=%d, want 2/2 parallel", len(ext.rawAudio), len(v.Formats))
	}
	if got := ext.rawAudio[0]; got.Itag != 251 || got.LastModified != "1719185012384481" {
		t.Errorf("rawAudio[0] = itag %d lmt %q, want 251/1719185012384481 (triple preserved)", got.Itag, got.LastModified)
	}
	if v.Formats[0].Itag != 251 {
		t.Errorf("public Formats[0].Itag = %d, want 251 (parallel order)", v.Formats[0].Itag)
	}
}

func TestExtractWebContextErrors(t *testing.T) {
	t.Run("no provider", func(t *testing.T) {
		c := New(Config{GL: "US"})
		if c.WebContextConfigured() {
			t.Fatal("WebContextConfigured = true with no provider")
		}
		if _, err := c.ExtractWebContext(context.Background(), "v"); !errors.Is(err, waxerr.ErrExtractionFailed) {
			t.Errorf("err = %v, want ErrExtractionFailed", err)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		c := webContextClient(potoken.PlayerContext{}, errors.New("provider down"))
		_, err := c.ExtractWebContext(context.Background(), "v")
		pe, ok := errors.AsType[*waxerr.ProviderError](err)
		if !ok {
			t.Fatalf("err = %v, want *ProviderError (not flattened to ErrExtractionFailed)", err)
		}
		if pe.Endpoint != "player-context" {
			t.Errorf("ProviderError.Endpoint = %q, want player-context", pe.Endpoint)
		}
		// A provider failure is not an extraction failure.
		if errors.Is(err, waxerr.ErrExtractionFailed) {
			t.Errorf("err = %v, must not classify as ErrExtractionFailed", err)
		}
	})
	t.Run("missing url or visitor", func(t *testing.T) {
		pc := sampleContext()
		pc.ServerAbrURL = ""
		c := webContextClient(pc, nil)
		if _, err := c.ExtractWebContext(context.Background(), "v"); !errors.Is(err, waxerr.ErrExtractionFailed) {
			t.Errorf("err = %v, want ErrExtractionFailed for empty serverAbrURL", err)
		}
	})
	t.Run("no audio formats", func(t *testing.T) {
		pc := sampleContext()
		pc.AudioFormats = nil
		c := webContextClient(pc, nil)
		if _, err := c.ExtractWebContext(context.Background(), "v"); !errors.Is(err, waxerr.ErrExtractionFailed) {
			t.Errorf("err = %v, want ErrExtractionFailed for no audio formats", err)
		}
	})
	t.Run("missing ustreamer config", func(t *testing.T) {
		// A context without videoPlaybackUstreamerConfig cannot stream; it must
		// be rejected here (instant fallback) rather than deep in the SABR
		// reload loop after the download has started.
		pc := sampleContext()
		pc.UstreamerConfig = ""
		c := webContextClient(pc, nil)
		if _, err := c.ExtractWebContext(context.Background(), "v"); !errors.Is(err, waxerr.ErrExtractionFailed) {
			t.Errorf("err = %v, want ErrExtractionFailed for empty ustreamer config", err)
		}
	})
	t.Run("caller cancellation propagates unwrapped", func(t *testing.T) {
		c := webContextClient(potoken.PlayerContext{}, errors.New("aborted"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.ExtractWebContext(ctx, "v"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled (caller cancel is not a provider failure)", err)
		}
	})
	t.Run("provider timeout reads as provider failure", func(t *testing.T) {
		// The WebContextTimeout bound lives in the client, so it covers reextract
		// too. Its expiry must read as a fallback-able failure, not cancellation.
		c := New(Config{
			GL:                "US",
			WebContextTimeout: time.Millisecond,
			PlayerContextProvider: potoken.PlayerContextProviderFunc(
				func(ctx context.Context, _ string) (potoken.PlayerContext, error) {
					<-ctx.Done() // hung provider; honors ctx
					return potoken.PlayerContext{}, ctx.Err()
				},
			),
		})
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := c.ExtractWebContext(parent, "v")
		// An internal provider timeout remains eligible for fallback.
		if _, ok := errors.AsType[*waxerr.ProviderError](err); !ok {
			t.Errorf("err = %v, want *ProviderError (timeout must trigger fallback, not extractor breakage)", err)
		}
		// A provider-only timeout must not cancel the caller's context.
		if parent.Err() != nil {
			t.Errorf("caller context cancelled (%v); a provider-only timeout must not cancel it", parent.Err())
		}
	})
}

// TestWebContextProfileHonorsChromeMajor pins the WEB_CONTEXT identity to the
// same ChromeMajor treatment as every other built-in WEB profile: the path
// built for byte-level session coherence must not be the one path that ignores
// the override.
func TestWebContextProfileHonorsChromeMajor(t *testing.T) {
	c := New(Config{
		GL:          "US",
		ChromeMajor: 142,
		PlayerContextProvider: potoken.PlayerContextProviderFunc(
			func(context.Context, string) (potoken.PlayerContext, error) { return sampleContext(), nil },
		),
	})
	ext, err := c.ExtractWebContext(context.Background(), "v")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ext.profile.UserAgent, c.webFallback.UserAgent; got != want {
		t.Errorf("WEB_CONTEXT UserAgent = %q, want the client's web identity %q", got, want)
	}
	if !strings.Contains(ext.profile.UserAgent, "Chrome/142") {
		t.Errorf("WEB_CONTEXT UserAgent = %q, want it to carry Chrome/142", ext.profile.UserAgent)
	}
}

// TestWebContextFormatsCarryDrcAndTrack pins the DRC/multi-audio handoff: the
// provider's IsDrc and AudioTrackID must reach rawFormat, where buildSABRConfig
// reads them into client_abr_state.
func TestWebContextFormatsCarryDrcAndTrack(t *testing.T) {
	pc := sampleContext()
	pc.AudioFormats[0].IsDrc = true
	pc.AudioFormats[0].AudioTrackID = "en.4"
	c := webContextClient(pc, nil)
	ext, err := c.ExtractWebContext(context.Background(), "v")
	if err != nil {
		t.Fatal(err)
	}
	rf := ext.rawAudio[0]
	if rf.IsDrc == nil || !*rf.IsDrc {
		t.Error("rawAudio[0].IsDrc not set from the provider format")
	}
	if rf.AudioTrack == nil || rf.AudioTrack.ID != "en.4" {
		t.Errorf("rawAudio[0].AudioTrack = %+v, want ID en.4", rf.AudioTrack)
	}
	if rf2 := ext.rawAudio[1]; rf2.IsDrc != nil || rf2.AudioTrack != nil {
		t.Error("rawAudio[1] must stay unset (no DRC/track on the provider format)")
	}
}

func TestWebContextFormatsSkipDegenerate(t *testing.T) {
	// A malformed player context can include entries WaxTap cannot stream: a
	// non-positive itag, a non-audio MIME, or an empty MIME. Each is dropped before
	// SABR selection sees the renditions.
	pc := sampleContext()
	pc.AudioFormats = []potoken.PlayerContextFormat{
		{Itag: 251, MimeType: `audio/webm; codecs="opus"`, Bitrate: 143452, AudioChannels: 2, AudioSampleRate: 48000},
		{Itag: 0, MimeType: `audio/mp4; codecs="mp4a.40.2"`},     // non-positive itag
		{Itag: 137, MimeType: `video/mp4; codecs="avc1.640028"`}, // non-audio MIME
		{Itag: 250, MimeType: ""}, // empty MIME
	}
	c := webContextClient(pc, nil)
	ext, err := c.ExtractWebContext(context.Background(), "v")
	if err != nil {
		t.Fatalf("ExtractWebContext: %v", err)
	}
	if len(ext.rawAudio) != 1 || ext.rawAudio[0].Itag != 251 {
		t.Fatalf("rawAudio = %+v, want only the itag-251 audio entry", ext.rawAudio)
	}
}

func TestWebContextAllDegenerateFails(t *testing.T) {
	// When every entry is unusable, the context yields ErrExtractionFailed instead
	// of an empty rendition set.
	pc := sampleContext()
	pc.AudioFormats = []potoken.PlayerContextFormat{
		{Itag: 0, MimeType: `audio/webm; codecs="opus"`},
		{Itag: 137, MimeType: `video/mp4`},
		{Itag: 250, MimeType: ""},
	}
	c := webContextClient(pc, nil)
	if _, err := c.ExtractWebContext(context.Background(), "v"); !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Errorf("err = %v, want ErrExtractionFailed for an all-degenerate context", err)
	}
}

func TestExpiresAtFromURL(t *testing.T) {
	got := expiresAtFromURL("https://rr3.googlevideo.com/videoplayback?expire=1781138473&n=x")
	if want := time.Unix(1781138473, 0); !got.Equal(want) {
		t.Errorf("expiresAtFromURL = %v, want %v", got, want)
	}
	// The path-encoded form googlevideo also serves (shared with the resolver's
	// parser, which grew it from real URLs).
	got = expiresAtFromURL("https://rr3.googlevideo.com/videoplayback/expire/1781138473/ei/x/file/audio.webm")
	if want := time.Unix(1781138473, 0); !got.Equal(want) {
		t.Errorf("expiresAtFromURL(path form) = %v, want %v", got, want)
	}
	if got := expiresAtFromURL("https://rr3.googlevideo.com/videoplayback?n=x"); !got.IsZero() {
		t.Errorf("expiresAtFromURL(no expire) = %v, want zero", got)
	}
	if got := expiresAtFromURL("://bad"); !got.IsZero() {
		t.Errorf("expiresAtFromURL(bad) = %v, want zero", got)
	}
}
