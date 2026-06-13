package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/colespringer/waxtap/potoken"
)

// playerContextProvider is a potoken.PlayerContextProvider that fetches an
// attested WEB /player streaming context from a WaxSeal-style server. It posts a
// JSON body containing video_id to <baseURL>/player-context and maps the
// snake_case response onto a potoken.PlayerContext.
//
// Like the bgutil token provider, it uses its own dedicated HTTP client, never
// WaxTap's --proxy/--insecure client: the provider is typically a localhost
// sidecar that must not be proxied, and full WEB validation only holds when the
// context mint and the stream egress the same IP (the signed URL is IP-bound).
type playerContextProvider struct {
	endpoint string
	http     *http.Client
}

// newPlayerContextProvider builds a provider that talks to the server at baseURL
// (e.g. "http://127.0.0.1:4416"). The client carries no timeout of its own:
// every call arrives bounded by Timeouts.WebContext (applied in the library),
// and a second hidden cap here would silently override a user-raised budget.
func newPlayerContextProvider(baseURL string) *playerContextProvider {
	return &playerContextProvider{
		endpoint: strings.TrimRight(baseURL, "/") + "/player-context",
		http:     &http.Client{},
	}
}

type playerContextRequest struct {
	VideoID string `json:"video_id"`
}

// playerContextResponse mirrors the WaxSeal /player-context wire contract
// (snake_case). Metadata and the richer per-format fields may be absent on older
// servers; their zero values degrade gracefully (video-id filename, unknown
// duration, quality-blind selection).
type playerContextResponse struct {
	Status                       string                    `json:"status"`
	PlayerURL                    string                    `json:"player_url"`
	ServerAbrStreamingURL        string                    `json:"server_abr_streaming_url"`
	VideoPlaybackUstreamerConfig string                    `json:"video_playback_ustreamer_config"`
	VisitorData                  string                    `json:"visitor_data"`
	ClientVersion                string                    `json:"client_version"`
	Title                        string                    `json:"title"`
	Author                       string                    `json:"author"`
	LengthSeconds                int                       `json:"length_seconds"`
	AudioFormats                 []playerContextFormatJSON `json:"audio_formats"`
}

type playerContextFormatJSON struct {
	Itag             int    `json:"itag"`
	LMT              string `json:"lmt"`
	XTags            string `json:"xtags"`
	MimeType         string `json:"mime_type"`
	Bitrate          int    `json:"bitrate"`
	AudioQuality     string `json:"audio_quality"`
	AudioChannels    int    `json:"audio_channels"`
	AudioSampleRate  int    `json:"audio_sample_rate"`
	ContentLength    int64  `json:"content_length"`
	ApproxDurationMs int64  `json:"approx_duration_ms"`
	// IsDrc and AudioTrackID feed the SABR client_abr_state for DRC and
	// multi-audio renditions; absent means a plain default-track format.
	IsDrc        bool   `json:"is_drc"`
	AudioTrackID string `json:"audio_track_id"`
}

// ProvidePlayerContext requests an attested WEB context from the configured
// sidecar.
func (p *playerContextProvider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	var out playerContextResponse
	if err := sidecarJSON(ctx, p.http, http.MethodPost, p.endpoint, "player-context server",
		playerContextRequest{VideoID: videoID}, &out); err != nil {
		return potoken.PlayerContext{}, err
	}
	// Strict validation: reject a context that cannot stream so the caller falls
	// back to the default chain instantly rather than failing deeper in SABR
	// setup. The error names the wire keys (snake_case) so a provider author can
	// match them against their payload. video_playback_ustreamer_config is
	// validated again in the library, which covers non-CLI providers too.
	if out.Status != "" && !strings.EqualFold(out.Status, "OK") {
		return potoken.PlayerContext{}, &sidecarResponseError{label: "player-context server", endpoint: p.endpoint, reason: fmt.Sprintf("status %q", out.Status)}
	}
	if out.ServerAbrStreamingURL == "" || out.VisitorData == "" || out.VideoPlaybackUstreamerConfig == "" || len(out.AudioFormats) == 0 {
		return potoken.PlayerContext{}, &sidecarResponseError{label: "player-context server", endpoint: p.endpoint, reason: "missing server_abr_streaming_url, visitor_data, video_playback_ustreamer_config, or audio_formats"}
	}

	formats := make([]potoken.PlayerContextFormat, 0, len(out.AudioFormats))
	for _, f := range out.AudioFormats {
		formats = append(formats, potoken.PlayerContextFormat{
			Itag:             f.Itag,
			LMT:              f.LMT,
			XTags:            f.XTags,
			MimeType:         f.MimeType,
			Bitrate:          f.Bitrate,
			AudioQuality:     f.AudioQuality,
			AudioChannels:    f.AudioChannels,
			AudioSampleRate:  f.AudioSampleRate,
			ContentLength:    f.ContentLength,
			ApproxDurationMs: f.ApproxDurationMs,
			IsDrc:            f.IsDrc,
			AudioTrackID:     f.AudioTrackID,
		})
	}
	return potoken.PlayerContext{
		ServerAbrURL:    out.ServerAbrStreamingURL,
		PlayerURL:       out.PlayerURL,
		UstreamerConfig: out.VideoPlaybackUstreamerConfig,
		VisitorData:     out.VisitorData,
		ClientVersion:   out.ClientVersion,
		Title:           out.Title,
		Author:          out.Author,
		LengthSeconds:   out.LengthSeconds,
		AudioFormats:    formats,
	}, nil
}
