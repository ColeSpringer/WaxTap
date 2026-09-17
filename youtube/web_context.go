package youtube

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube/internal/resolver"
)

// WebContextConfigured reports whether a WEB player-context provider is wired,
// enabling the opt-in WEB SABR audio path.
func (c *Client) WebContextConfigured() bool { return c.webContext != nil }

// ExtractWebContext builds an Extraction from an attested /player streaming
// context supplied by the configured PlayerContextProvider. The result uses the
// normal SABR resolution and download path: buildSABRConfig descrambles the
// URL's n parameter and mints a GVS PO token bound to the context's visitorData.
//
// videoID identifies the video to the provider and is stored on the returned
// Extraction so a mid-stream RELOAD can re-fetch a fresh context (see
// SABRStream.reextract).
//
// Config.WebContextTimeout bounds every provider call, including mid-stream
// refreshes. Provider failures return ProviderError so callers can fall back;
// cancellation of the caller's own context is propagated unwrapped.
func (c *Client) ExtractWebContext(ctx context.Context, videoID string) (*Extraction, error) {
	if c.webContext == nil {
		return nil, fmt.Errorf("%w: no player-context provider configured", waxerr.ErrExtractionFailed)
	}
	parent := ctx
	if c.webCtxTO > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.webCtxTO)
		defer cancel()
	}
	pc, err := c.webContext.ProvidePlayerContext(ctx, videoID)
	if err != nil {
		if perr := parent.Err(); perr != nil {
			return nil, perr // caller cancellation, not a provider failure
		}
		// Preserve the provider cause for transport and HTTP classification.
		return nil, &waxerr.ProviderError{Endpoint: "player-context", Cause: err}
	}
	// Live and upcoming broadcasts are refused as they are on /player: the
	// download pipeline does not handle them. These are verdicts about the video,
	// not provider failures, so they are not wrapped in a ProviderError and they
	// arm no provider cool-down. Upcoming ranks first: a premiere can be both.
	switch {
	case pc.IsUpcoming:
		return nil, &waxerr.PlayabilityError{Status: "OK", Reason: "upcoming content", Sentinel: waxerr.ErrLiveNotStarted}
	case pc.IsLiveNow:
		return nil, &waxerr.PlayabilityError{Status: "OK", Reason: "live content", Sentinel: waxerr.ErrLiveContent}
	}
	// UstreamerConfig is validated here with the other essentials: a SABR
	// session cannot stream without it, and rejecting the context now lets the
	// caller fall back instantly instead of failing in the SABR reload loop
	// after the download has started.
	if pc.ServerAbrURL == "" || pc.VisitorData == "" || pc.UstreamerConfig == "" {
		return nil, fmt.Errorf("%w: player context missing serverAbrStreamingUrl, visitorData, or videoPlaybackUstreamerConfig", waxerr.ErrExtractionFailed)
	}

	raw := webContextFormats(pc.AudioFormats)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: player context carried no audio formats", waxerr.ErrExtractionFailed)
	}

	video := &Video{
		ID:          videoID,
		URL:         watchURL(videoID),
		Title:       pc.Title,
		Author:      pc.Author,
		ChannelID:   pc.ChannelID,
		Description: pc.Description,
		Duration:    time.Duration(max(0, pc.LengthSeconds)) * time.Second,
		PublishDate: parseDate(pc.PublishDate),
		// Live and upcoming are refused above, so only LiveNone and LiveWasLive
		// can reach a delivered Video, as on every other path.
		LiveStatus: liveStatusFrom(false, false, pc.IsLiveContent),
		Formats:    mapFormats(raw),
	}
	for _, t := range pc.Thumbnails {
		video.Thumbnails = append(video.Thumbnails, Thumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
	}
	orderContextThumbnails(video.Thumbnails)
	if video.Duration == 0 {
		video.Duration = longestFormatDuration(video.Formats)
	}

	sess := newSession(c.gl)
	sess.adoptVisitorData(pc.VisitorData)

	return &Extraction{
		video:           video,
		profile:         c.webContextProfile(pc.ClientVersion),
		session:         sess,
		attempt:         AttemptWebContext,
		rawAudio:        raw,
		expiresAt:       expiresAtFromURL(pc.ServerAbrURL),
		serverAbrURL:    pc.ServerAbrURL, // raw, scrambled n; buildSABRConfig descrambles it
		ustreamerConfig: pc.UstreamerConfig,
		playerURL:       pc.PlayerURL, // pin the n-descramble to the context's base.js
		webContext:      true,
		identityGen:     c.resetSeq.Load(),
		contextGen:      pc.Generation,
	}, nil
}

// orderContextThumbnails delivers a context's ladder largest-first, so
// Thumbnails[0] is the best rung here as it is on every other path.
//
// A provider may send rungs with no width or height (the contract calls both
// optional). Sorting those by area would make every key zero and fall through to
// sortThumbnailsLargestFirst's URL tie-break, which orders the ladder
// alphabetically: "default" before "maxresdefault" before "mqdefault", the
// opposite of what the caller is promised. The wire order is documented as the
// player response's, smallest first, so reversing it is the only ordering the
// response actually supports.
func orderContextThumbnails(ts []Thumbnail) {
	for _, t := range ts {
		if t.Width > 0 || t.Height > 0 {
			sortThumbnailsLargestFirst(ts)
			return
		}
	}
	slices.Reverse(ts)
}

// ClientNameWebContext is the profile and [Extraction.ClientName] value reported
// for the attested WEB player-context streaming path. It names the magic string in
// one place; library callers can compare it against a Result's client to detect
// WEB-context delivery.
const ClientNameWebContext = "WEB_CONTEXT"

// webContextProfile builds the WEB_CONTEXT client profile: a WEB identity that
// requires only a GVS PO token (the player token is skipped because /player is
// not called here) and no signature timestamp. version comes from the attested
// context so the SABR client_info matches the session the URL was minted under,
// and the User-Agent comes from the client's web identity so a ChromeMajor
// override applies here exactly as on every other WEB-family path.
func (c *Client) webContextProfile(version string) ClientProfile {
	base := profileWeb
	base.Name = ClientNameWebContext
	base.UserAgent = c.webFallback.UserAgent
	if version != "" {
		base.Version = version
	}
	base.RequiresPOTokens = []potoken.Scope{potoken.ScopeGVS}
	base.NeedsSignatureTimestamp = false
	base.SupportsPlaylists = false
	return makeProfile(base)
}

// webContextFormats maps the provider's audio formats to rawFormat, preserving
// the (itag, lastModified, xtags) triple as a unit so SABR format selection is
// coherent with the signed URL. Codec/extension and the public Format are
// derived from MimeType by toFormat, so selection stays valid even when the
// sample-rate/channels/quality fields are absent. IsDrc and AudioTrackID feed
// the SABR client_abr_state (drc_enabled / audio_track_id) for DRC and
// multi-audio renditions.
func webContextFormats(formats []potoken.PlayerContextFormat) []rawFormat {
	out := make([]rawFormat, 0, len(formats))
	for _, f := range formats {
		// Ignore player-context entries WaxTap cannot request: missing itag,
		// non-audio MIME, or empty MIME. This mirrors the player-response audio
		// filter and normalizes whitespace/case because external contexts are less
		// predictable than YouTube's JSON. If every entry is dropped, the existing
		// empty-format guard returns ErrExtractionFailed for fallback.
		if f.Itag <= 0 || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.MimeType)), "audio/") {
			continue
		}
		rf := rawFormat{
			Itag:             f.Itag,
			MimeType:         f.MimeType,
			Bitrate:          f.Bitrate,
			ContentLength:    itoaNonZero(f.ContentLength),
			LastModified:     f.LMT,
			XTags:            f.XTags,
			AudioSampleRate:  itoaNonZero(int64(f.AudioSampleRate)),
			AudioChannels:    f.AudioChannels,
			AudioQuality:     f.AudioQuality,
			ApproxDurationMs: itoaNonZero(f.ApproxDurationMs),
		}
		if f.IsDrc {
			isDrc := true
			rf.IsDrc = &isDrc
		}
		if f.AudioTrackID != "" {
			rf.AudioTrack = &rawAudioTrack{ID: f.AudioTrackID}
		}
		out = append(out, rf)
	}
	return out
}

// itoaNonZero formats v as a decimal string, or "" when v is zero, matching the
// player response's habit of omitting absent numeric fields (rawFormat keeps
// them as strings).
func itoaNonZero(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// expiresAtFromURL reads the signed expiry from a googlevideo URL: the expire
// query parameter or the /expire/<unix>/ path form. It returns the zero time
// when absent or unparseable; callers treat that as unknown expiry.
func expiresAtFromURL(rawURL string) time.Time {
	u, err := url.Parse(rawURL)
	if err != nil {
		return time.Time{}
	}
	return resolver.ExpiryFromURL(u.Query(), u.Path)
}
