package waxtap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v2/potoken"
)

// SidecarOption configures a sidecar provider built by NewSidecarPOTokenProvider,
// NewSidecarPlayerContextProvider, or NewSidecarSessionProvider.
type SidecarOption func(*sidecarConfig)

// sidecarConfig holds the settings a SidecarOption can adjust. Keeping them behind
// the option type lets a later setting be added without changing the constructor
// signatures.
type sidecarConfig struct {
	apiKey string
}

// WithSidecarAPIKey sends key as the X-API-Key header on every sidecar request.
// An empty key (the default) sends no header. Use HTTPS for a remote sidecar.
func WithSidecarAPIKey(key string) SidecarOption {
	return func(c *sidecarConfig) { c.apiKey = key }
}

func applySidecarOptions(opts []SidecarOption) sidecarConfig {
	var c sidecarConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	return c
}

// NewSidecarPOTokenProvider returns a POTokenProvider that mints PO tokens from a
// bgutil-wire endpoint (the protocol bgutil-ytdlp-pot-provider and WaxSeal's token
// server speak). It POSTs a content_binding to <baseURL>/get_pot; baseURL may be a
// base such as "http://127.0.0.1:4416" or a full endpoint, and the default path is
// appended only when absent. A bad URL returns an error. Plug the result into
// [Options.POTokenProvider].
//
// The provider uses a dedicated 30s-timeout, no-redirect client that ignores
// [Options.HTTPClient]: a PO token is IP-bound, so the mint and the stream must
// share egress, and no-redirect pins credentials to the endpoint.
func NewSidecarPOTokenProvider(baseURL string, opts ...SidecarOption) (POTokenProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/get_pot")
	if err != nil {
		return nil, err
	}
	return newBgutilProvider(endpoint, applySidecarOptions(opts).apiKey), nil
}

// NewSidecarPlayerContextProvider returns a PlayerContextProvider that fetches an
// attested WEB /player streaming context from a WaxSeal-style endpoint, enabling
// the opt-in WEB SABR audio path. It POSTs video_id to <baseURL>/player-context
// (base or full endpoint accepted). A bad URL returns an error. Plug the result
// into [Options.PlayerContextProvider]; [New] requires a POTokenProvider alongside
// it because the WEB stream binds a GVS PO token to the context's visitorData.
//
// The client imposes no timeout of its own; calls rely on [Timeouts.WebContext].
// Like the token provider it ignores [Options.HTTPClient] and is never proxied.
func NewSidecarPlayerContextProvider(baseURL string, opts ...SidecarOption) (PlayerContextProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/player-context")
	if err != nil {
		return nil, err
	}
	return newPlayerContextProvider(endpoint, applySidecarOptions(opts).apiKey), nil
}

// NewSidecarSessionProvider returns a POTokenSessionProvider that adopts a guest
// identity ({visitor_data, cookies}) from a <baseURL>/session endpoint (base or
// full endpoint accepted), so WaxTap streams under the same session the token was
// attested in. A bad URL returns an error. Plug the result into
// [Options.SessionProvider].
//
// The provider uses a dedicated 30s-timeout, no-redirect client that ignores
// [Options.HTTPClient]: full WEB validation requires the session host and the
// downloads to share an egress IP.
func NewSidecarSessionProvider(baseURL string, opts ...SidecarOption) (POTokenSessionProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/session")
	if err != nil {
		return nil, err
	}
	return newHTTPSessionProvider(endpoint, applySidecarOptions(opts).apiKey), nil
}

// validateHTTPBaseURL parses base and requires an http or https scheme and a
// host, the same check sidecar URLs use. It returns the parsed URL so callers
// can build on it without reparsing.
func validateHTTPBaseURL(base string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host")
	}
	return u, nil
}

// buildSidecarURL validates a sidecar URL and appends defaultPath when the URL
// does not already contain an endpoint path. Existing query parameters are
// preserved.
func buildSidecarURL(base, defaultPath string) (string, error) {
	u, err := validateHTTPBaseURL(base)
	if err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = defaultPath
	}
	return u.String(), nil
}

// newSidecarClient returns a client that does not follow redirects, keeping
// credentials and request bodies bound to the configured endpoint.
func newSidecarClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Limit sidecar response bodies so a misconfigured endpoint cannot consume
// unbounded memory or keep a request open indefinitely. Error responses use a
// lower limit because only a short reason is retained.
const (
	sidecarSuccessBodyLimit = 1 << 20 // 1 MiB
	sidecarErrorBodyLimit   = 8 << 10 // 8 KiB
	sidecarReasonRunes      = 200     // cap on an extracted reason
)

// sidecarJSON exchanges JSON with the PO-token, session, and player-context
// providers. Connection failures return *SidecarError. Unusable responses return
// *SidecarResponseError without including raw response bodies. label identifies
// the provider in returned errors. A non-empty apiKey is sent in the X-API-Key
// header.
func sidecarJSON(ctx context.Context, client *http.Client, method, endpoint, label, apiKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Preserve the provider name and endpoint so the caller can distinguish a
		// connection failure from the sentinel that may wrap it.
		return &SidecarError{Label: label, Endpoint: endpoint, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Include a short reason from a known JSON field, but do not echo arbitrary
		// response bytes that might contain tokens or cookies.
		return &SidecarResponseError{
			Label:      label,
			Endpoint:   endpoint,
			StatusCode: resp.StatusCode,
			Reason:     sidecarReason(resp.Body),
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, sidecarSuccessBodyLimit)).Decode(out); err != nil {
		// A json decode error is a structured syntax/shape message (an offending
		// delimiter, or a field name and type), not a dump of raw response bytes, so
		// including it stays clear of tokens/cookies while making a custom sidecar
		// integration debuggable.
		return &SidecarResponseError{Label: label, Endpoint: endpoint, Reason: fmt.Sprintf("malformed JSON response: %v", err)}
	}
	return nil
}

// sidecarReason extracts and truncates the error or message field from a JSON
// response. Other response bodies produce an empty reason.
func sidecarReason(body io.Reader) string {
	var msg struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(body, sidecarErrorBodyLimit)).Decode(&msg)
	reason := strings.TrimSpace(msg.Error)
	if reason == "" {
		reason = strings.TrimSpace(msg.Message)
	}
	return capRunes(reason, sidecarReasonRunes)
}

// capRunes truncates s to at most n runes, appending an ellipsis when truncated.
// It counts runes so truncation never splits a multibyte character.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// redactURL removes credentials, query parameters, and fragments before a URL is
// included in an error. Invalid URLs are replaced with a static placeholder.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// SidecarError reports a connection failure to a configured sidecar endpoint. Its
// Error string self-redacts the endpoint; SidecarResponseError is the counterpart
// for a reachable endpoint that returned a non-OK status or unusable response.
type SidecarError struct {
	Label    string // provider name, such as "bgutil PO-token server"
	Endpoint string // configured endpoint (redacted in the Error string)
	Err      error  // underlying transport error; Unwrap returns it
}

func (e *SidecarError) Error() string {
	return fmt.Sprintf("%s unreachable at %s: %v", e.Label, redactURL(e.Endpoint), transportReason(e.Err))
}

func (e *SidecarError) Unwrap() error { return e.Err }

// transportReason removes the request URL from a transport error. SidecarError
// still unwraps to the original error for classification.
func transportReason(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil {
		return ue.Err
	}
	return err
}

// SidecarResponseError reports a non-OK status or an invalid response from a
// configured sidecar. StatusCode is zero when an HTTP 200 response had invalid
// content. SidecarError is reserved for connection failures. Its Error string
// self-redacts the endpoint.
type SidecarResponseError struct {
	Label      string // provider name, such as "session endpoint"
	Endpoint   string // configured endpoint (redacted in the Error string)
	StatusCode int    // HTTP status, or 0 when a 200 carried invalid content
	Reason     string // short, sanitized reason; never raw response bytes
}

func (e *SidecarResponseError) Error() string {
	ep := redactURL(e.Endpoint)
	switch {
	case e.StatusCode > 0 && e.Reason != "":
		return fmt.Sprintf("%s at %s returned HTTP %d: %s", e.Label, ep, e.StatusCode, e.Reason)
	case e.StatusCode > 0:
		return fmt.Sprintf("%s at %s returned HTTP %d", e.Label, ep, e.StatusCode)
	case e.Reason != "":
		return fmt.Sprintf("%s at %s returned an unusable response: %s", e.Label, ep, e.Reason)
	default:
		return fmt.Sprintf("%s at %s returned an unusable response", e.Label, ep)
	}
}

// bgutilProvider is a potoken.Provider that mints PO tokens from a bgutil-wire
// HTTP server. It posts a JSON body containing content_binding to the endpoint and
// maps the returned poToken/expiresAt onto a potoken.Response.
//
// The content binding is scope-specific: a player-scope token binds to the video
// ID; a GVS (stream) token binds to the session's visitor-data string.
//
// Token traffic uses a dedicated HTTP client, never WaxTap's shared client: the
// provider is typically a localhost sidecar that must not be proxied, and full
// byte-level WEB validation only works when the token request and the stream
// egress the same IP (the token is bound to the minting host).
type bgutilProvider struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// newBgutilProvider builds a provider for a bgutil endpoint that has already been
// validated by buildSidecarURL.
func newBgutilProvider(endpoint, apiKey string) *bgutilProvider {
	return &bgutilProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(30 * time.Second),
	}
}

// bgutilRequest and bgutilResponse mirror the bgutil /get_pot wire contract.
type bgutilRequest struct {
	ContentBinding string `json:"content_binding"`
}

type bgutilResponse struct {
	POToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
	ExpiresAt      string `json:"expiresAt"`
}

// ProvidePOToken requests a scope-bound token from the configured sidecar.
func (p *bgutilProvider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	binding, err := contentBinding(req)
	if err != nil {
		return potoken.Response{}, err
	}
	var out bgutilResponse
	if err := sidecarJSON(ctx, p.http, http.MethodPost, p.endpoint, "bgutil PO-token server", p.apiKey,
		bgutilRequest{ContentBinding: binding}, &out); err != nil {
		return potoken.Response{}, err
	}
	if out.POToken == "" {
		return potoken.Response{}, &SidecarResponseError{Label: "bgutil PO-token server", Endpoint: p.endpoint, Reason: "empty token"}
	}
	return potoken.Response{
		Token:     out.POToken,
		ExpiresAt: parseBgutilExpiry(out.ExpiresAt),
	}, nil
}

// contentBinding selects the bgutil content_binding for the token scope: a player
// token binds to the video ID; a GVS (stream) token binds to the raw visitor-data
// string. Other scopes are unsupported by this provider.
func contentBinding(req potoken.Request) (string, error) {
	switch req.Scope {
	case potoken.ScopePlayer:
		if req.VideoID == "" {
			return "", fmt.Errorf("bgutil: player PO token requested without a video ID")
		}
		return req.VideoID, nil
	case potoken.ScopeGVS:
		if req.VisitorData == "" {
			return "", fmt.Errorf("bgutil: GVS PO token requested without visitor data")
		}
		return req.VisitorData, nil
	default:
		return "", fmt.Errorf("bgutil: unsupported PO-token scope %q", req.Scope)
	}
}

// parseBgutilExpiry reads the bgutil expiresAt field, normally RFC3339
// ("2026-06-09T07:25:25Z") but tolerated as Unix seconds. An empty or unparseable
// value yields the zero time, which the caller treats as unknown.
func parseBgutilExpiry(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

// playerContextProvider is a potoken.PlayerContextProvider that fetches an
// attested WEB /player streaming context from a WaxSeal-style server. It posts a
// JSON body containing video_id and maps the snake_case response onto a
// potoken.PlayerContext.
//
// Like the bgutil token provider, it uses its own dedicated HTTP client, never
// WaxTap's shared client: the provider is typically a localhost sidecar that must
// not be proxied, and full WEB validation only holds when the context mint and the
// stream egress the same IP (the signed URL is IP-bound).
type playerContextProvider struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// newPlayerContextProvider builds a provider for a player-context endpoint that
// has already been validated by buildSidecarURL. Calls rely on the library's
// Timeouts.WebContext deadline, so the HTTP client does not impose another one.
func newPlayerContextProvider(endpoint, apiKey string) *playerContextProvider {
	return &playerContextProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(0),
	}
}

type playerContextRequest struct {
	VideoID string `json:"video_id"`
}

// playerContextResponse mirrors the WaxSeal /player-context wire contract
// (snake_case). Metadata and the richer per-format fields may be absent on older
// servers; their zero values allow a video-ID filename, unknown duration, and
// selection without quality metadata.
type playerContextResponse struct {
	PlayabilityStatus            string                    `json:"playability_status"`
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
	// IsDrc and AudioTrackID feed the SABR client_abr_state for DRC and multi-audio
	// renditions; absent means a plain default-track format.
	IsDrc        bool   `json:"is_drc"`
	AudioTrackID string `json:"audio_track_id"`
}

// ProvidePlayerContext requests an attested WEB context from the configured
// sidecar.
func (p *playerContextProvider) ProvidePlayerContext(ctx context.Context, videoID string) (potoken.PlayerContext, error) {
	var out playerContextResponse
	if err := sidecarJSON(ctx, p.http, http.MethodPost, p.endpoint, "player-context server", p.apiKey,
		playerContextRequest{VideoID: videoID}, &out); err != nil {
		return potoken.PlayerContext{}, err
	}
	// Reject unusable contexts before SABR setup so the caller can fall back. The
	// error names the snake_case wire keys for comparison with the response.
	// video_playback_ustreamer_config is also validated for non-CLI providers.
	if out.PlayabilityStatus != "" && !strings.EqualFold(out.PlayabilityStatus, "OK") {
		return potoken.PlayerContext{}, &SidecarResponseError{Label: "player-context server", Endpoint: p.endpoint, Reason: fmt.Sprintf("playability_status %q", out.PlayabilityStatus)}
	}
	if out.ServerAbrStreamingURL == "" || out.VisitorData == "" || out.VideoPlaybackUstreamerConfig == "" || len(out.AudioFormats) == 0 {
		return potoken.PlayerContext{}, &SidecarResponseError{Label: "player-context server", Endpoint: p.endpoint, Reason: "missing server_abr_streaming_url, visitor_data, video_playback_ustreamer_config, or audio_formats"}
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

// httpSessionProvider adopts a guest identity from a /session endpoint (the kind a
// PO-token minter exposes), returning the browser's exact visitorData and cookies
// so WaxTap streams under the same session the token was attested in.
//
// Like the bgutil token provider, it uses its own dedicated client and is never
// routed through WaxTap's shared client: the endpoint is typically a localhost
// sidecar, and full WEB validation requires the session host and the downloads to
// share an egress IP.
type httpSessionProvider struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// newHTTPSessionProvider builds a provider for a session endpoint that has already
// been validated by buildSidecarURL.
func newHTTPSessionProvider(endpoint, apiKey string) *httpSessionProvider {
	return &httpSessionProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(30 * time.Second),
	}
}

// sessionDoc mirrors the /session wire contract. snake_case keys are canonical
// (the reference minter, WaxSeal's :4417, uses them); camelCase variants are also
// accepted so the contract does not break on casing:
//
//	{"visitor_data":"<exact X-Goog-Visitor-Id literal>",
//	 "cookies":[{"name","value","domain","path","secure","http_only","expires"}]}
//
// Extra keys (user_agent, client_version, cookie_header) are ignored. expires is
// RFC3339 or unix seconds; 0/absent means a session cookie.
type sessionDoc struct {
	VisitorData      string          `json:"visitor_data"`
	VisitorDataCamel string          `json:"visitorData"`
	Cookies          []sessionCookie `json:"cookies"`
}

// visitorData returns the supplied literal, preferring the canonical snake_case.
func (d sessionDoc) visitorData() string {
	if d.VisitorData != "" {
		return d.VisitorData
	}
	return d.VisitorDataCamel
}

type sessionCookie struct {
	Name          string          `json:"name"`
	Value         string          `json:"value"`
	Domain        string          `json:"domain"`
	Path          string          `json:"path"`
	Secure        bool            `json:"secure"`
	HTTPOnly      bool            `json:"http_only"`
	HTTPOnlyCamel bool            `json:"httpOnly"`
	Expires       json.RawMessage `json:"expires"`
}

func (c sessionCookie) httpOnly() bool { return c.HTTPOnly || c.HTTPOnlyCamel }

// ProvideSession fetches the session document, retrying once on a transient
// failure. The visitorData is taken verbatim (no escaping or unescaping), so it
// stays byte-identical to the value the minter attests under.
func (p *httpSessionProvider) ProvideSession(ctx context.Context) (potoken.Session, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return potoken.Session{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		sess, err := p.fetch(ctx)
		if err == nil {
			return sess, nil
		}
		if ctx.Err() != nil {
			return potoken.Session{}, ctx.Err()
		}
		lastErr = err
	}
	return potoken.Session{}, lastErr
}

func (p *httpSessionProvider) fetch(ctx context.Context) (potoken.Session, error) {
	var doc sessionDoc
	if err := sidecarJSON(ctx, p.http, http.MethodGet, p.endpoint, "session endpoint", p.apiKey, nil, &doc); err != nil {
		return potoken.Session{}, err
	}
	vd := doc.visitorData()
	if vd == "" {
		return potoken.Session{}, &SidecarResponseError{Label: "session endpoint", Endpoint: p.endpoint, Reason: "empty visitorData"}
	}

	cookies := make([]*http.Cookie, 0, len(doc.Cookies))
	for _, c := range doc.Cookies {
		cookies = append(cookies, &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HttpOnly: c.httpOnly(),
			Expires:  parseSessionExpiry(c.Expires),
		})
	}
	return potoken.Session{VisitorData: vd, Cookies: cookies}, nil
}

// parseSessionExpiry accepts a unix-seconds number or an RFC3339 string. An empty,
// null, zero, or unparseable value yields the zero time (a session cookie).
func parseSessionExpiry(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		if secs <= 0 {
			return time.Time{}
		}
		return time.Unix(secs, 0).UTC()
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			return t
		}
	}
	return time.Time{}
}
