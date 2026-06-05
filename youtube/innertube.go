package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/colespringer/waxtap/waxerr"
)

const (
	innerTubeBase  = "https://www.youtube.com/youtubei/v1"
	playerEndpoint = innerTubeBase + "/player"
	browseEndpoint = innerTubeBase + "/browse"

	// maxResponseBytes bounds how much of an InnerTube response we buffer.
	maxResponseBytes = 24 << 20 // 24 MiB
)

// innertubeRequest is the JSON body for an InnerTube call.
type innertubeRequest struct {
	VideoID         string           `json:"videoId,omitempty"`
	BrowseID        string           `json:"browseId,omitempty"`
	Continuation    string           `json:"continuation,omitempty"`
	Context         innertubeContext `json:"context"`
	PlaybackContext *playbackContext `json:"playbackContext,omitempty"`
	ContentCheckOK  bool             `json:"contentCheckOk,omitempty"`
	RacyCheckOK     bool             `json:"racyCheckOk,omitempty"`
	Params          string           `json:"params,omitempty"`
	// ServiceIntegrityDimensions carries the player-scope PO token in the
	// request body. Keep it as a pointer so omitempty drops the field entirely
	// for profiles that do not send a player token.
	ServiceIntegrityDimensions *serviceIntegrityDimensions `json:"serviceIntegrityDimensions,omitempty"`
}

// serviceIntegrityDimensions contains YouTube's player-request integrity fields.
// WaxTap currently sets only the PO token.
type serviceIntegrityDimensions struct {
	POToken string `json:"poToken,omitempty"`
}

type innertubeContext struct {
	Client innertubeClient `json:"client"`
}

// innertubeClient mirrors the profile into the request context. The headers are
// driven by the same profile (see makeProfile), so the wire identity is
// consistent across body and headers.
type innertubeClient struct {
	HL                string `json:"hl"`
	GL                string `json:"gl"`
	ClientName        string `json:"clientName"`
	ClientVersion     string `json:"clientVersion"`
	UserAgent         string `json:"userAgent,omitempty"`
	DeviceMake        string `json:"deviceMake,omitempty"`
	DeviceModel       string `json:"deviceModel,omitempty"`
	OSName            string `json:"osName,omitempty"`
	OSVersion         string `json:"osVersion,omitempty"`
	AndroidSDKVersion int    `json:"androidSdkVersion,omitempty"`
	TimeZone          string `json:"timeZone"`
	UTCOffset         int    `json:"utcOffsetMinutes"`
	VisitorData       string `json:"visitorData,omitempty"`
}

type playbackContext struct {
	ContentPlaybackContext contentPlaybackContext `json:"contentPlaybackContext"`
}

type contentPlaybackContext struct {
	HTML5Preference string `json:"html5Preference"`
}

func (c *Client) newInnertubeContext(p ClientProfile, s *session) innertubeContext {
	return innertubeContext{
		Client: innertubeClient{
			HL:                c.hl,
			GL:                c.gl,
			ClientName:        p.InnerTubeName,
			ClientVersion:     p.Version,
			UserAgent:         p.UserAgent,
			DeviceMake:        p.DeviceMake,
			DeviceModel:       p.DeviceModel,
			OSName:            p.OSName,
			OSVersion:         p.OSVersion,
			AndroidSDKVersion: p.AndroidSDKVersion,
			TimeZone:          "UTC",
			UTCOffset:         0,
			VisitorData:       s.visitorData,
		},
	}
}

func (c *Client) newPlayerRequest(p ClientProfile, s *session, videoID, poToken string) innertubeRequest {
	req := innertubeRequest{
		VideoID:        videoID,
		Context:        c.newInnertubeContext(p, s),
		ContentCheckOK: true,
		RacyCheckOK:    true,
		PlaybackContext: &playbackContext{
			ContentPlaybackContext: contentPlaybackContext{HTML5Preference: "HTML5_PREF_WANTS"},
		},
	}
	if poToken != "" {
		req.ServiceIntegrityDimensions = &serviceIntegrityDimensions{POToken: poToken}
	}
	return req
}

func (c *Client) newPlaylistRequest(p ClientProfile, s *session, playlistID, continuation string) innertubeRequest {
	req := innertubeRequest{
		Context:        c.newInnertubeContext(p, s),
		ContentCheckOK: true,
		RacyCheckOK:    true,
	}
	if continuation != "" {
		req.Continuation = continuation
	} else {
		req.BrowseID = "VL" + playlistID
	}
	return req
}

// acceptLanguage builds an Accept-Language header value from the configured host
// language.
func acceptLanguage(hl string) string {
	if hl == "" || hl == "en" {
		return "en-US,en;q=0.9"
	}
	return hl + ",en-US;q=0.8,en;q=0.6"
}

// addConsentCookie attaches the cookie-consent marker, but only when the client
// has no cookie jar. When a jar is present it already carries the consent cookie
// (seeded at construction) plus the bootstrapped session cookies, so adding one
// manually would duplicate it.
func (c *Client) addConsentCookie(req *http.Request, s *session) {
	if c.http.Jar() == nil {
		req.AddCookie(&http.Cookie{Name: "CONSENT", Value: s.consentCookieValue(), Path: "/", Domain: ".youtube.com"})
	}
}

// innertubePost marshals body and sends it to endpoint with the profile's static
// headers plus the session's visitor-id header and consent cookie, returning the
// response bytes. Retry/backoff and rate-limit handling live in the httpx client.
func (c *Client) innertubePost(ctx context.Context, p ClientProfile, s *session, endpoint string, body innertubeRequest) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpointURL := endpoint
	if p.APIKey != "" {
		endpointURL += "?key=" + p.APIKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range p.Headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Language", acceptLanguage(c.hl))
	if s.visitorData != "" {
		req.Header.Set("X-Goog-Visitor-Id", s.visitorData)
	}
	c.addConsentCookie(req, s)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: endpoint}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// httpGet fetches rawURL with the profile's user agent and the session's consent
// cookie. Used for the watch-page HTML fallback.
func (c *Client) httpGet(ctx context.Context, p ClientProfile, s *session, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept-Language", acceptLanguage(c.hl))
	c.addConsentCookie(req, s)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: rawURL}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}
