package youtube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
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
		ChannelID:       "UCdummy",
		Description:     "desc",
		Thumbnails: []potoken.PlayerContextThumbnail{
			{URL: "https://i.ytimg.com/vi/dummyVideo0/default.jpg", Width: 168, Height: 94},
			{URL: "https://i.ytimg.com/vi/dummyVideo0/mqdefault.jpg", Width: 336, Height: 188},
			{URL: "https://i.ytimg.com/vi/dummyVideo0/maxresdefault.jpg", Width: 1280, Height: 720},
		},
		IsLiveContent: true,
		PublishDate:   "2015-04-10T00:00:00-07:00",
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
	if v.URL != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" {
		t.Errorf("video.URL = %q, want the canonical watch URL every extraction path sets", v.URL)
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

	if v.ChannelID != "UCdummy" || v.Description != "desc" {
		t.Errorf("channel/description = %q/%q, want the context's", v.ChannelID, v.Description)
	}
	if len(v.Thumbnails) != 3 || v.Thumbnails[0].Width != 1280 || v.Thumbnails[2].Width != 168 {
		t.Errorf("thumbnails = %+v, want the ladder sorted largest first", v.Thumbnails)
	}
	if want := time.Date(2015, 4, 10, 7, 0, 0, 0, time.UTC); !v.PublishDate.Equal(want) {
		t.Errorf("publishDate = %v, want %v (RFC 3339 with offset, in UTC)", v.PublishDate, want)
	}
	if v.LiveStatus != LiveWasLive {
		t.Errorf("liveStatus = %v, want was_live from is_live_content", v.LiveStatus)
	}
}

func TestExtractWebContextRefusesLiveAndUpcoming(t *testing.T) {
	for _, tc := range []struct {
		name           string
		live, upcoming bool
		want           error
	}{
		{"live", true, false, waxerr.ErrLiveContent},
		{"upcoming", false, true, waxerr.ErrLiveNotStarted},
		{"both, upcoming wins", true, true, waxerr.ErrLiveNotStarted},
	} {
		pc := sampleContext()
		pc.IsLiveNow, pc.IsUpcoming = tc.live, tc.upcoming
		_, err := webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if _, isProvider := errors.AsType[*waxerr.ProviderError](err); isProvider {
			t.Errorf("%s: a verdict about the video is not a provider failure", tc.name)
		}
	}
}

// TestExtractWebContextMetadataOptional covers a provider that sends none of the
// metadata keys (an older WaxSeal, or a non-WaxSeal sidecar).
func TestExtractWebContextMetadataOptional(t *testing.T) {
	pc := sampleContext()
	pc.ChannelID, pc.Description, pc.PublishDate = "", "", ""
	pc.Thumbnails = nil
	pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming = false, false, false
	ext, err := webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("ExtractWebContext: %v", err)
	}
	v := ext.video
	if v.ChannelID != "" || v.Description != "" || !v.PublishDate.IsZero() || v.Thumbnails != nil {
		t.Errorf("video = %+v, want the metadata fields left zero", v)
	}
	if v.LiveStatus != LiveNone {
		t.Errorf("liveStatus = %v, want LiveNone", v.LiveStatus)
	}
}

func TestExtractWebContextBareDate(t *testing.T) {
	pc := sampleContext()
	pc.PublishDate = "2015-04-10"
	ext, err := webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2015, 4, 10, 0, 0, 0, 0, time.UTC); !ext.video.PublishDate.Equal(want) {
		t.Errorf("publishDate = %v, want %v", ext.video.PublishDate, want)
	}

	pc.PublishDate = "soon"
	ext, err = webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("an unparseable date is not an error: %v", err)
	}
	if !ext.video.PublishDate.IsZero() {
		t.Errorf("publishDate = %v, want zero", ext.video.PublishDate)
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

// A context that reports no video length still describes renditions that do,
// so the longest rendition stands in, in whole seconds, as it does for a player
// response; a negative length counts as none.
func TestExtractWebContextBackfillsDurationFromFormats(t *testing.T) {
	for name, length := range map[string]int{"zero": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			pc := sampleContext()
			pc.LengthSeconds = length
			ext, err := webContextClient(pc, nil).ExtractWebContext(context.Background(), "aqz-KE-bpKQ")
			if err != nil {
				t.Fatal(err)
			}
			if ext.video.Duration != 634*time.Second {
				t.Errorf("video.Duration = %v, want 634s from the longest rendition's approxDurationMs", ext.video.Duration)
			}
		})
	}
}

// TestExtractWebContextDimensionlessLadder covers a provider that sends urls
// with no width or height (both are optional): sorting by area would make every
// key zero and fall through to the URL tie-break, ordering the ladder
// alphabetically, which is the opposite of largest-first.
func TestExtractWebContextDimensionlessLadder(t *testing.T) {
	pc := sampleContext()
	pc.Thumbnails = []potoken.PlayerContextThumbnail{
		{URL: "https://i.ytimg.com/vi/dummyVideo0/default.jpg"},
		{URL: "https://i.ytimg.com/vi/dummyVideo0/mqdefault.jpg"},
		{URL: "https://i.ytimg.com/vi/dummyVideo0/maxresdefault.jpg"},
	}
	ext, err := webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatal(err)
	}
	got := ext.video.Thumbnails
	if len(got) != 3 {
		t.Fatalf("thumbnails = %+v, want 3", got)
	}
	// The wire order is the player response's, smallest first, so the largest is
	// the last rung the provider sent.
	if !strings.HasSuffix(got[0].URL, "maxresdefault.jpg") || !strings.HasSuffix(got[2].URL, "default.jpg") {
		t.Errorf("thumbnails = %+v, want the wire order reversed, not sorted by URL", got)
	}

	// One rung with dimensions is enough to sort by area again.
	pc.Thumbnails[0].Width, pc.Thumbnails[0].Height = 1280, 720
	ext, err = webContextClient(pc, nil).ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ext.video.Thumbnails[0].URL, "default.jpg") {
		t.Errorf("thumbnails = %+v, want the declared 1280x720 rung first", ext.video.Thumbnails)
	}
}
