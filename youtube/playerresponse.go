package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// playerResponse is the subset of the InnerTube /player response WaxTap needs.
type playerResponse struct {
	ResponseContext struct {
		VisitorData string `json:"visitorData"`
	} `json:"responseContext"`

	PlayabilityStatus struct {
		Status          string `json:"status"`
		Reason          string `json:"reason"`
		PlayableInEmbed bool   `json:"playableInEmbed"`
	} `json:"playabilityStatus"`

	StreamingData struct {
		ExpiresInSeconds string `json:"expiresInSeconds"`
		// ServerAbrStreamingURL is the POST endpoint for SABR-backed formats,
		// which do not include per-format URLs or signature ciphers.
		ServerAbrStreamingURL string      `json:"serverAbrStreamingUrl"`
		Formats               []rawFormat `json:"formats"`
		AdaptiveFormats       []rawFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`

	// PlayerConfig contains configuration required by SABR requests.
	PlayerConfig struct {
		MediaCommonConfig struct {
			MediaUstreamerRequestConfig struct {
				// VideoPlaybackUstreamerConfig is base64-encoded here and sent as
				// decoded bytes in each VideoPlaybackAbrRequest.
				VideoPlaybackUstreamerConfig string `json:"videoPlaybackUstreamerConfig"`
			} `json:"mediaUstreamerRequestConfig"`
		} `json:"mediaCommonConfig"`
	} `json:"playerConfig"`

	VideoDetails struct {
		VideoID          string `json:"videoId"`
		Title            string `json:"title"`
		LengthSeconds    string `json:"lengthSeconds"`
		ChannelID        string `json:"channelId"`
		Author           string `json:"author"`
		ShortDescription string `json:"shortDescription"`
		ViewCount        string `json:"viewCount"`
		IsPrivate        bool   `json:"isPrivate"`
		IsLiveContent    bool   `json:"isLiveContent"`
		IsUpcoming       bool   `json:"isUpcoming"`
		Thumbnail        struct {
			Thumbnails []rawThumbnail `json:"thumbnails"`
		} `json:"thumbnail"`
	} `json:"videoDetails"`

	Microformat struct {
		PlayerMicroformatRenderer struct {
			LengthSeconds        string `json:"lengthSeconds"`
			PublishDate          string `json:"publishDate"`
			UploadDate           string `json:"uploadDate"`
			OwnerChannelName     string `json:"ownerChannelName"`
			IsUnlisted           bool   `json:"isUnlisted"`
			LiveBroadcastDetails struct {
				IsLiveNow bool `json:"isLiveNow"`
			} `json:"liveBroadcastDetails"`
		} `json:"playerMicroformatRenderer"`
	} `json:"microformat"`
}

type rawThumbnail struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// rawFormat is a streaming format as encoded in the player response. Numeric
// fields YouTube sends as strings are decoded as strings and converted here.
type rawFormat struct {
	Itag            int    `json:"itag"`
	URL             string `json:"url"`
	SignatureCipher string `json:"signatureCipher"`
	MimeType        string `json:"mimeType"`
	Bitrate         int    `json:"bitrate"`
	AverageBitrate  int    `json:"averageBitrate"`
	ContentLength   string `json:"contentLength"`
	// LastModified and XTags distinguish encodings that share an itag. SABR
	// sends them back as part of FormatId.
	LastModified     string         `json:"lastModified"`
	XTags            string         `json:"xtags"`
	AudioSampleRate  string         `json:"audioSampleRate"`
	AudioChannels    int            `json:"audioChannels"`
	AudioQuality     string         `json:"audioQuality"`
	ApproxDurationMs string         `json:"approxDurationMs"`
	IsDrc            *bool          `json:"isDrc"`
	AudioTrack       *rawAudioTrack `json:"audioTrack"`
}

// rawAudioTrack identifies one audio track of a multi-audio video.
type rawAudioTrack struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	AudioIsDefault *bool  `json:"audioIsDefault"`
}

// parsePlayerResponse unmarshals a raw /player JSON body.
func parsePlayerResponse(body []byte) (*playerResponse, error) {
	var pr playerResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode player response: %w", err)
	}
	return &pr, nil
}

// parseWatchPage extracts ytInitialPlayerResponse from watch-page HTML. The
// scanner handles nested braces and braces inside strings.
func parseWatchPage(body []byte) (*playerResponse, error) {
	obj, ok := extractJSONObject(string(body), "ytInitialPlayerResponse")
	if !ok {
		return nil, errors.New("ytInitialPlayerResponse not found in watch page")
	}
	return parsePlayerResponse(obj)
}

func (pr *playerResponse) isLiveNow() bool {
	return pr.Microformat.PlayerMicroformatRenderer.LiveBroadcastDetails.IsLiveNow
}

// duration returns the video length, preferring videoDetails over the microformat
// (the latter is WEB-only). It is zero when neither carries a length.
func (pr *playerResponse) duration() time.Duration {
	if d := parseSeconds(pr.VideoDetails.LengthSeconds); d > 0 {
		return d
	}
	return parseSeconds(pr.Microformat.PlayerMicroformatRenderer.LengthSeconds)
}

// serverAbrURL returns the SABR streaming endpoint, if present.
func (pr *playerResponse) serverAbrURL() string {
	return pr.StreamingData.ServerAbrStreamingURL
}

// ustreamerConfig returns the base64-encoded SABR request configuration.
func (pr *playerResponse) ustreamerConfig() string {
	return pr.PlayerConfig.MediaCommonConfig.MediaUstreamerRequestConfig.VideoPlaybackUstreamerConfig
}

// expiresAt converts streamingData.expiresInSeconds to an absolute time. The
// signed stream URL's expire parameter is preferred later; this is the fallback
// when the URL does not carry one.
func (pr *playerResponse) expiresAt(now time.Time) time.Time {
	secs := atoi(pr.StreamingData.ExpiresInSeconds)
	if secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs) * time.Second)
}

// playabilityError classifies playabilityStatus into WaxTap's error taxonomy. A
// nil return means the response is usable by the download pipeline.
//
// Reason matching for members-only and geo-blocked videos is best-effort under
// the default en/US locale; a non-English HL may fall back to ErrVideoUnavailable.
func (pr *playerResponse) playabilityError() error {
	status := pr.PlayabilityStatus.Status
	reason := pr.PlayabilityStatus.Reason

	switch status {
	case "OK", "":
		// Upcoming and currently-live streams are not handled by the download
		// pipeline. A completed livestream VOD reports isLiveContent without isLiveNow
		// or isUpcoming, so it is allowed. Upcoming ranks first: a premiere can be both.
		switch {
		case pr.VideoDetails.IsUpcoming:
			return &waxerr.PlayabilityError{Status: status, Reason: "upcoming content", Sentinel: waxerr.ErrLiveNotStarted}
		case pr.isLiveNow():
			return &waxerr.PlayabilityError{Status: status, Reason: "live content", Sentinel: waxerr.ErrLiveContent}
		}
		return nil
	case "LOGIN_REQUIRED":
		// YouTube reuses this status for both private and age-gated videos.
		if pr.VideoDetails.IsPrivate || strings.Contains(strings.ToLower(reason), "private") {
			return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrVideoRestricted}
		}
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLoginRequired}
	case "AGE_CHECK_REQUIRED", "AGE_VERIFICATION_REQUIRED":
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrAgeRestricted}
	case "CONTENT_CHECK_REQUIRED":
		// A sensitive/graphic-content confirmation gate, distinct from age: an
		// interactive confirm the automated client cannot satisfy.
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLoginRequired}
	case "LIVE_STREAM_OFFLINE":
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLiveNotStarted}
	default: // UNPLAYABLE, ERROR, and anything unknown
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: classifyUnplayableReason(reason)}
	}
}

// classifyUnplayableReason maps a free-text UNPLAYABLE/ERROR reason to a specific
// availability sentinel. Matching is best-effort and English-oriented; an
// unrecognized reason is the generic ErrVideoUnavailable.
func classifyUnplayableReason(reason string) error {
	r := strings.ToLower(reason)
	switch {
	// "members" (plural, as YouTube phrases it) avoids matching "remember".
	case strings.Contains(r, "members"):
		return waxerr.ErrMembersOnly
	case strings.Contains(r, "country") || strings.Contains(r, "region"):
		return waxerr.ErrGeoBlocked
	default:
		return waxerr.ErrVideoUnavailable
	}
}

// toVideo builds the public Video model and matching raw formats for resolution.
// It returns ErrNoAudioFormats when no audio rendition is present.
func (pr *playerResponse) toVideo(videoID string) (*Video, []rawFormat, error) {
	v := &Video{
		ID:          videoID,
		URL:         "https://www.youtube.com/watch?v=" + videoID,
		Title:       pr.VideoDetails.Title,
		Author:      pr.VideoDetails.Author,
		ChannelID:   pr.VideoDetails.ChannelID,
		Description: pr.VideoDetails.ShortDescription,
		LiveStatus:  pr.liveStatus(),
	}

	v.Duration = pr.duration()
	// Non-WEB clients omit Microformat, leaving PublishDate at its zero value.
	v.PublishDate = parseDate(pr.Microformat.PlayerMicroformatRenderer.PublishDate)

	for _, t := range pr.VideoDetails.Thumbnail.Thumbnails {
		v.Thumbnails = append(v.Thumbnails, Thumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
	}
	// YouTube returns thumbnails smallest-first; deliver them largest-first with a
	// deterministic tie-break so callers can take Thumbnails[0] as the best.
	sortThumbnailsLargestFirst(v.Thumbnails)

	raw := pr.audioFormats()
	v.Formats = mapFormats(raw)
	if len(v.Formats) == 0 {
		return nil, nil, waxerr.ErrNoAudioFormats
	}
	return v, raw, nil
}

// liveStatus classifies the video's broadcast state from the player response.
// playabilityError rejects live/upcoming videos before a Video is built, so a
// returned Video is in practice LiveNone or LiveWasLive; the full mapping is kept
// so the classification is correct wherever it is derived.
func (pr *playerResponse) liveStatus() LiveStatus {
	switch {
	case pr.VideoDetails.IsUpcoming:
		return LiveUpcoming
	case pr.isLiveNow():
		return LiveNow
	case pr.VideoDetails.IsLiveContent:
		return LiveWasLive
	default:
		return LiveNone
	}
}

// sortThumbnailsLargestFirst orders thumbnails by descending pixel area, then a
// stable URL tie-break so equal-size candidates keep a deterministic order.
func sortThumbnailsLargestFirst(ts []Thumbnail) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Width != ts[j].Width {
			return ts[i].Width > ts[j].Width
		}
		if ts[i].Height != ts[j].Height {
			return ts[i].Height > ts[j].Height
		}
		return ts[i].URL < ts[j].URL
	})
}

// audioFormats returns the raw audio renditions (adaptive first). WaxTap is
// audio-only, so video and combined formats are skipped. The raw forms retain
// the url/signatureCipher needed for resolution.
func (pr *playerResponse) audioFormats() []rawFormat {
	var out []rawFormat
	for _, rf := range pr.StreamingData.AdaptiveFormats {
		if strings.HasPrefix(rf.MimeType, "audio/") {
			out = append(out, rf)
		}
	}
	for _, rf := range pr.StreamingData.Formats {
		if strings.HasPrefix(rf.MimeType, "audio/") {
			out = append(out, rf)
		}
	}
	return out
}

func mapFormats(raw []rawFormat) []format.Format {
	out := make([]format.Format, 0, len(raw))
	for _, rf := range raw {
		out = append(out, rf.toFormat())
	}
	return out
}

func (rf rawFormat) toFormat() format.Format {
	codec, ext := parseMIME(rf.MimeType)
	f := format.Format{
		Itag:           rf.Itag,
		MIMEType:       rf.MimeType,
		Codec:          codec,
		Extension:      ext,
		Bitrate:        rf.Bitrate,
		AverageBitrate: rf.AverageBitrate,
		SampleRate:     atoi(rf.AudioSampleRate),
		Channels:       rf.AudioChannels,
		AudioQuality:   parseAudioQualityTier(rf.AudioQuality),
		ContentLength:  atoi64(rf.ContentLength),
		Duration:       parseMillis(rf.ApproxDurationMs),
		IsDRC:          drcFromPtr(rf.IsDrc),
	}
	if rf.AudioTrack != nil {
		f.Language = rf.AudioTrack.ID
		f.IsOriginal = triFromPtr(rf.AudioTrack.AudioIsDefault)
		f.AudioTrack = &format.AudioTrack{
			ID:          rf.AudioTrack.ID,
			DisplayName: rf.AudioTrack.DisplayName,
			IsOriginal:  triFromPtr(rf.AudioTrack.AudioIsDefault),
		}
	}
	return f
}

// parseMIME splits a mimeType like `audio/webm; codecs="opus"` into a normalized
// codec id and a canonical file extension.
func parseMIME(mime string) (codec, ext string) {
	main, _, _ := strings.Cut(mime, ";")
	if _, sub, ok := strings.Cut(main, "/"); ok {
		ext = extForSubtype(strings.TrimSpace(sub))
	}
	if _, after, ok := strings.Cut(mime, "codecs="); ok {
		c := strings.Trim(strings.TrimSpace(after), `"`)
		if first, _, ok := strings.Cut(c, ","); ok {
			c = first // first (audio) codec
		}
		codec = strings.Trim(strings.TrimSpace(c), `"`)
	}
	return codec, ext
}

func extForSubtype(sub string) string {
	switch sub {
	case "mp4":
		return "m4a" // audio in an mp4 container
	default:
		return sub
	}
}

// parseAudioQualityTier maps YouTube's audioQuality value to the public tier
// model.
func parseAudioQualityTier(s string) format.AudioQualityTier {
	switch s {
	case "AUDIO_QUALITY_HIGH":
		return format.QualityHigh
	case "AUDIO_QUALITY_MEDIUM":
		return format.QualityMedium
	case "AUDIO_QUALITY_LOW":
		return format.QualityLow
	case "AUDIO_QUALITY_ULTRALOW":
		return format.QualityUltraLow
	default:
		return format.QualityUnknown
	}
}

func triFromPtr(b *bool) format.Tri {
	switch {
	case b == nil:
		return format.Unknown
	case *b:
		return format.Yes
	default:
		return format.No
	}
}

// drcFromPtr maps YouTube's presence-based isDrc flag. The field is true for DRC
// variants and omitted for non-DRC variants.
func drcFromPtr(b *bool) format.Tri {
	if b != nil && *b {
		return format.Yes
	}
	return format.No
}

func parseSeconds(s string) time.Duration {
	return time.Duration(atoi(s)) * time.Second
}

func parseMillis(s string) time.Duration {
	return time.Duration(atoi64(s)) * time.Millisecond
}

func parseDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// extractJSONObject finds marker in s and returns the balanced {...} object that
// follows it. It tracks string literals and escapes so braces inside strings do
// not end the object early.
func extractJSONObject(s, marker string) ([]byte, bool) {
	obj, _, ok := extractJSONObjectFrom(s, marker, 0)
	return obj, ok
}

// extractJSONObjectFrom is extractJSONObject with a starting offset. It returns
// the balanced object and the index just past its closing brace, so a caller can
// iterate successive matches of the same marker, for example to skip a decoy
// `var ytInitialData = {...}` assignment and find the real one.
func extractJSONObjectFrom(s, marker string, start int) (obj []byte, end int, ok bool) {
	if start < 0 {
		start = 0
	}
	rel := strings.Index(s[start:], marker)
	if rel < 0 {
		return nil, 0, false
	}
	i := start + rel
	brace := strings.IndexByte(s[i:], '{')
	if brace < 0 {
		return nil, 0, false
	}
	objStart := i + brace

	depth := 0
	inStr := false
	escaped := false
	for k := objStart; k < len(s); k++ {
		ch := s[k]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return []byte(s[objStart : k+1]), k + 1, true
			}
		}
	}
	return nil, 0, false
}
