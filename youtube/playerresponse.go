package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/waxerr"
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
		ExpiresInSeconds string      `json:"expiresInSeconds"`
		Formats          []rawFormat `json:"formats"`
		AdaptiveFormats  []rawFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`

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
	Itag             int    `json:"itag"`
	URL              string `json:"url"`
	SignatureCipher  string `json:"signatureCipher"`
	MimeType         string `json:"mimeType"`
	Bitrate          int    `json:"bitrate"`
	AverageBitrate   int    `json:"averageBitrate"`
	ContentLength    string `json:"contentLength"`
	AudioSampleRate  string `json:"audioSampleRate"`
	AudioChannels    int    `json:"audioChannels"`
	AudioQuality     string `json:"audioQuality"`
	ApproxDurationMs string `json:"approxDurationMs"`
	IsDrc            *bool  `json:"isDrc"`
	AudioTrack       *struct {
		ID             string `json:"id"`
		DisplayName    string `json:"displayName"`
		AudioIsDefault *bool  `json:"audioIsDefault"`
	} `json:"audioTrack"`
}

// parsePlayerResponse unmarshals a raw /player JSON body.
func parsePlayerResponse(body []byte) (*playerResponse, error) {
	var pr playerResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode player response: %w", err)
	}
	return &pr, nil
}

// parseWatchPage extracts ytInitialPlayerResponse from a watch-page HTML body
// using brace-aware scanning (robust to nested/string braces, unlike a regex).
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
func (pr *playerResponse) playabilityError() error {
	status := pr.PlayabilityStatus.Status
	reason := pr.PlayabilityStatus.Reason

	switch status {
	case "OK", "":
		// Live and upcoming streams are not handled by the download pipeline.
		// Completed livestream VODs report isLiveContent without isLiveNow or
		// isUpcoming, so they are allowed.
		if pr.isLiveNow() || pr.VideoDetails.IsUpcoming {
			return &waxerr.PlayabilityError{Status: status, Reason: "live or upcoming content", Sentinel: waxerr.ErrLiveContent}
		}
		return nil
	case "LOGIN_REQUIRED":
		// YouTube reuses this status for both private and age-gated videos.
		if pr.VideoDetails.IsPrivate || strings.Contains(strings.ToLower(reason), "private") {
			return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrVideoRestricted}
		}
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLoginRequired}
	case "AGE_CHECK_REQUIRED", "CONTENT_CHECK_REQUIRED":
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLoginRequired}
	case "LIVE_STREAM_OFFLINE":
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrLiveContent}
	default: // UNPLAYABLE, ERROR, and anything unknown
		return &waxerr.PlayabilityError{Status: status, Reason: reason, Sentinel: waxerr.ErrVideoUnavailable}
	}
}

// toVideo builds the public Video model and the matching raw audio formats used
// for stream resolution. It returns ErrNoAudioFormats when no audio
// rendition is present.
func (pr *playerResponse) toVideo(videoID string) (*Video, []rawFormat, error) {
	v := &Video{
		ID:          videoID,
		Title:       pr.VideoDetails.Title,
		Author:      pr.VideoDetails.Author,
		ChannelID:   pr.VideoDetails.ChannelID,
		Description: pr.VideoDetails.ShortDescription,
		IsLive:      pr.isLiveNow(),
		IsUpcoming:  pr.VideoDetails.IsUpcoming,
	}

	if d := parseSeconds(pr.VideoDetails.LengthSeconds); d > 0 {
		v.Duration = d
	} else if d := parseSeconds(pr.Microformat.PlayerMicroformatRenderer.LengthSeconds); d > 0 {
		v.Duration = d
	}
	v.PublishDate = parseDate(pr.Microformat.PlayerMicroformatRenderer.PublishDate)

	for _, t := range pr.VideoDetails.Thumbnail.Thumbnails {
		v.Thumbnails = append(v.Thumbnails, Thumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
	}

	raw := pr.audioFormats()
	v.Formats = mapFormats(raw)
	if len(v.Formats) == 0 {
		return nil, nil, waxerr.ErrNoAudioFormats
	}
	return v, raw, nil
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
		ContentLength:  atoi64(rf.ContentLength),
		Duration:       parseMillis(rf.ApproxDurationMs),
		IsDRC:          triFromPtr(rf.IsDrc),
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
	i := strings.Index(s, marker)
	if i < 0 {
		return nil, false
	}
	rel := strings.IndexByte(s[i:], '{')
	if rel < 0 {
		return nil, false
	}
	start := i + rel

	depth := 0
	inStr := false
	escaped := false
	for k := start; k < len(s); k++ {
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
				return []byte(s[start : k+1]), true
			}
		}
	}
	return nil, false
}
