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
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/youtube"
)

// SidecarOption configures a sidecar provider built by NewSidecarPOTokenProvider,
// NewSidecarPlayerContextProvider, or NewSidecarSessionProvider.
type SidecarOption func(*sidecarConfig)

// sidecarConfig holds the settings a SidecarOption can adjust. Keeping them behind
// the option type lets a later setting be added without changing the constructor
// signatures.
type sidecarConfig struct {
	apiKey  string
	timeout time.Duration
}

// WithSidecarAPIKey sends key as the X-API-Key header on every sidecar request.
// An empty key (the default) sends no header. Use HTTPS for a remote sidecar.
func WithSidecarAPIKey(key string) SidecarOption {
	return func(c *sidecarConfig) { c.apiKey = key }
}

// WithSidecarTimeout bounds each HTTP request a sidecar provider makes, /report
// included. Zero or negative selects the default, 60 s, sized for WaxSeal's
// documented first call after a relaunch: launch, attestation, and a streaming
// proof (10 to 30 s), then the 12 s separation window. A wait the sidecar asks
// for through Retry-After is not counted against it; the caller's context is,
// and on the player-context provider Timeouts.WebContext bounds the whole call.
func WithSidecarTimeout(d time.Duration) SidecarOption {
	return func(c *sidecarConfig) { c.timeout = d }
}

func applySidecarOptions(opts []SidecarOption) sidecarConfig {
	var c sidecarConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.timeout <= 0 {
		c.timeout = defaultSidecarTimeout
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
// The provider uses a dedicated no-redirect client bounded by
// [WithSidecarTimeout] (60 s by default) that ignores [Options.HTTPClient]: a PO
// token is IP-bound, so the mint and the stream must share egress, and
// no-redirect pins credentials to the endpoint. A transient failure is retried
// once.
func NewSidecarPOTokenProvider(baseURL string, opts ...SidecarOption) (POTokenProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/get_pot")
	if err != nil {
		return nil, err
	}
	cfg := applySidecarOptions(opts)
	return newBgutilProvider(endpoint, cfg.apiKey, cfg.timeout), nil
}

// NewSidecarPlayerContextProvider returns a PlayerContextProvider that fetches an
// attested WEB /player streaming context from a WaxSeal-style endpoint, enabling
// the opt-in WEB SABR audio path. It POSTs video_id to <baseURL>/player-context
// (base or full endpoint accepted). A bad URL returns an error. Plug the result
// into [Options.PlayerContextProvider]; [New] requires a POTokenProvider alongside
// it because the WEB stream binds a GVS PO token to the context's visitorData.
//
// Each request is bounded by [WithSidecarTimeout] (60 s by default) and the call
// as a whole by [Timeouts.WebContext]. Like the token provider it ignores
// [Options.HTTPClient] and is never proxied. A transient failure is retried once,
// after the wait the sidecar states when it states one.
//
// The provider also implements [potoken.SessionInvalidator] against the /report
// sibling of that endpoint, so a session whose attested contexts deliver capped
// streams can be retired.
func NewSidecarPlayerContextProvider(baseURL string, opts ...SidecarOption) (PlayerContextProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/player-context")
	if err != nil {
		return nil, err
	}
	cfg := applySidecarOptions(opts)
	rep, err := newSidecarReporter(endpoint, "player-context report endpoint", cfg.apiKey, cfg.timeout)
	if err != nil {
		return nil, err
	}
	return newPlayerContextProvider(endpoint, cfg.apiKey, cfg.timeout, rep), nil
}

// NewSidecarSessionProvider returns a POTokenSessionProvider that adopts a guest
// identity ({visitor_data, cookies}) from a <baseURL>/session endpoint (base or
// full endpoint accepted), so WaxTap streams under the same session the token was
// attested in. A bad URL returns an error. Plug the result into
// [Options.SessionProvider].
//
// The provider uses a dedicated no-redirect client bounded by
// [WithSidecarTimeout] (60 s by default) that ignores [Options.HTTPClient]: full
// WEB validation requires the session host and the downloads to share an egress
// IP. A transient failure is retried once.
// The provider also implements [potoken.SessionInvalidator] against the /report
// sibling of that endpoint, so a session googlevideo has capped can be retired
// and replaced mid-download.
func NewSidecarSessionProvider(baseURL string, opts ...SidecarOption) (POTokenSessionProvider, error) {
	endpoint, err := buildSidecarURL(baseURL, "/session")
	if err != nil {
		return nil, err
	}
	cfg := applySidecarOptions(opts)
	rep, err := newSidecarReporter(endpoint, "session report endpoint", cfg.apiKey, cfg.timeout)
	if err != nil {
		return nil, err
	}
	return newHTTPSessionProvider(endpoint, cfg.apiKey, cfg.timeout, rep), nil
}

// siblingSidecarURL rewrites endpoint's last path segment to name, deriving one
// sidecar endpoint from another the caller configured. Cleaning first keeps a
// trailing slash on the configured endpoint from shifting the sibling down a
// level ("/api/session/" must still yield "/api/report").
func siblingSidecarURL(endpoint, name string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(path.Dir(path.Clean(u.Path)), name)
	return u.String(), nil
}

// sidecarReporter posts session-degradation reports to the POST /report sibling
// of a configured sidecar endpoint (WaxSeal's contract). The session and
// player-context providers both delegate their InvalidateSession to one.
type sidecarReporter struct {
	endpoint string
	label    string // identifies the owning provider in errors
	apiKey   string
	http     *http.Client
}

// newSidecarReporter derives the /report sibling of sourceEndpoint, which has
// already been validated by buildSidecarURL.
func newSidecarReporter(sourceEndpoint, label, apiKey string, timeout time.Duration) (sidecarReporter, error) {
	endpoint, err := siblingSidecarURL(sourceEndpoint, "report")
	if err != nil {
		return sidecarReporter{}, err
	}
	return sidecarReporter{
		endpoint: endpoint,
		label:    label,
		apiKey:   apiKey,
		http:     newSidecarClient(timeout),
	}, nil
}

// invalidate reports the session named by inv so the sidecar retires it.
//
// A rate-limited report is an error: the sidecar is asking for backoff, and
// acting as if the session were retired would re-adopt the same one. The
// accepted field is not consulted, because a report the sidecar rejects as
// stale means the session is already gone, which is the outcome asked for.
func (r sidecarReporter) invalidate(ctx context.Context, inv potoken.SessionInvalidation) error {
	// A missing generation is a local precondition failure, not an endpoint
	// response, so it is not a SidecarResponseError.
	if inv.Generation == 0 {
		return fmt.Errorf("%s: no session_generation to report (the sidecar's session or context response omitted it)", r.label)
	}
	body := map[string]any{"session_generation": inv.Generation}
	if inv.VideoID != "" {
		body["video_id"] = inv.VideoID
	}
	if inv.Reason != "" {
		body["reason"] = inv.Reason
	}
	var doc struct {
		RetryAfterSeconds int `json:"retry_after_seconds"`
	}
	if err := sidecarJSON(ctx, r.http, http.MethodPost, r.endpoint, r.label, r.apiKey, body, &doc); err != nil {
		return err
	}
	if doc.RetryAfterSeconds > 0 {
		return &SidecarResponseError{
			Label:      r.label,
			Endpoint:   r.endpoint,
			Reason:     fmt.Sprintf("session recycling is rate-limited; retry in %ds", doc.RetryAfterSeconds),
			RetryAfter: time.Duration(doc.RetryAfterSeconds) * time.Second,
		}
	}
	return nil
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
	// The reference sidecar clamps each text field of its error envelope at
	// 4 KiB and its own client reads 64 KiB, so a refusal whose reason and
	// details both carry a browser trace passes 8 KiB once JSON-escaped; a
	// limit under the envelope cuts the document and loses the code with it.
	sidecarErrorBodyLimit = 64 << 10 // 64 KiB
	sidecarReasonRunes    = 200      // cap on an extracted reason
	sidecarCodeRunes      = 64       // cap on an extracted code
)

// SidecarCodeVideoUnavailable is the refusal code WaxTap acts on: the sidecar's
// browser saw a non-OK playabilityStatus for the video (WaxSeal answers it with
// HTTP 422 and the status in details). The error unwraps to the same
// availability verdict a /player response with that status yields.
const SidecarCodeVideoUnavailable = "video-unavailable"

const (
	// defaultSidecarTimeout bounds one sidecar request when no option says
	// otherwise. WaxSeal's first call after a relaunch costs 10 to 30 s of launch,
	// attestation, and a streaming proof, then a 12 s separation window.
	defaultSidecarTimeout = 60 * time.Second
	// sidecarRetryMaxWait is the longest stated wait WaxTap sleeps through. Past
	// it the refusal is reported with the wait attached, so the caller (the
	// WEB-context cool-down, the CLI hint) decides what to do with the time.
	sidecarRetryMaxWait = 60 * time.Second
	// sidecarTransientWait is the poke a transient failure that stated no wait
	// earns before the single retry.
	sidecarTransientWait = 500 * time.Millisecond
)

// sidecarSleep is httpx.Sleep; tests swap it to record waits instead of sleeping.
var sidecarSleep = httpx.Sleep

// sidecarCall is sidecarJSON with one retry.
//
// SidecarRetryWait decides whether the failure earns it and how long to wait. A
// wait the sidecar stated is honoured up to sidecarRetryMaxWait; a transient
// failure that stated none earns sidecarTransientWait. Then PauseBlocked and
// PauseInterrupted apply the deadline policy Client.Do uses around the sleep.
// The second attempt's error is returned as it is.
//
// /report is deliberately not routed through here: a report is sent once.
func sidecarCall(ctx context.Context, client *http.Client, method, endpoint, label, apiKey string, in, out any) error {
	err := sidecarJSON(ctx, client, method, endpoint, label, apiKey, in, out)
	if err == nil {
		return nil
	}
	wait, retry := SidecarRetryWait(err)
	if !retry {
		return err
	}
	if berr := PauseBlocked(ctx, wait, err); berr != nil {
		return berr
	}
	if serr := sidecarSleep(ctx, wait); serr != nil {
		return PauseInterrupted(serr, err)
	}
	return sidecarJSON(ctx, client, method, endpoint, label, apiKey, in, out)
}

// SidecarRetryWait reports how long to wait before retrying err, and whether a
// retry is warranted at all. It reads [SidecarError] and [SidecarResponseError],
// so an adapter that translates another client's failures into those two types
// gets WaxTap's own rule rather than a second one that drifts from it. WaxTap
// allows one retry per sidecar request on this rule.
//
// A transport failure or a 408/5xx is transient: the sidecar may be launching a
// browser, mid-relaunch, or briefly wedged. A 429 earns a retry only when the
// sidecar states a wait: a bare 429 says back off, which is what the CLI's exit
// 5 tells the user, and a 500 ms poke would contradict it. Every other refusal
// (another 4xx, a malformed 200, a playability verdict) will answer the same way
// a moment later. A stated wait past 60 s earns none: that is a cool-down to
// report, not to sleep through.
func SidecarRetryWait(err error) (time.Duration, bool) {
	if _, ok := errors.AsType[*SidecarError](err); ok {
		return sidecarTransientWait, true
	}
	sre, ok := errors.AsType[*SidecarResponseError](err)
	if !ok {
		return 0, false
	}
	if sre.RetryAfter > 0 {
		if sre.RetryAfter > sidecarRetryMaxWait {
			return 0, false
		}
		return sre.RetryAfter, sre.StatusCode == http.StatusTooManyRequests || retryableSidecarStatus(sre.StatusCode)
	}
	if retryableSidecarStatus(sre.StatusCode) {
		return sidecarTransientWait, true
	}
	return 0, false
}

// PauseBlocked is the pause policy every WaxTap retry applies before it sleeps
// for a wait, exported beside SidecarRetryWait so an in-process adapter runs
// one rule rather than a copy of it. It returns the error to report instead of
// pausing for wait, or nil when the pause may proceed; pending is the typed
// failure the caller holds for what provoked the pause.
//
// A cancellation outranks pending: a context cancelled while a request was
// failing is the caller giving up, and reporting that as the refusal
// misclassifies it. A deadline that cannot fit wait plus a second of headroom
// returns pending now, since sleeping into a deadline buys a retry that cannot
// finish and a bare timeout that hides the cause; the comparison refuses at
// equality. A deadline already expired returns pending for the same reason.
//
// A nil pending is a caller with nothing to report, so a budget that cannot fit
// the wait returns nil and the pause proceeds. Pass the failure being retried.
func PauseBlocked(ctx context.Context, wait time.Duration, pending error) error {
	return httpx.PauseBlocked(ctx, wait, pending)
}

// PauseInterrupted is the other half of the policy, for a pause PauseBlocked
// allowed that the context then ended early: interrupted is the sleep's
// context error. A cancellation is the caller giving up and is returned as it
// is; a deadline expiring mid-pause is the case pending explains, so pending
// is returned instead of a bare timeout. A nil pending returns interrupted.
func PauseInterrupted(interrupted, pending error) error {
	return httpx.KeepCause(interrupted, pending)
}

// retryableSidecarStatus reports whether a status is the kind a second attempt
// can clear on its own. It is deliberately the same set internal/httpx retries
// on the YouTube path, so one status does not mean "transient" on a sidecar and
// "final" everywhere else. A 0 status is a malformed 200, which is a contract
// mismatch, not a transient one; a 501 or 505 names something the sidecar will
// not do, which a retry cannot change.
func retryableSidecarStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

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
	// Both readers below are bounded, so a larger body would leave bytes on the
	// wire and make Close discard the connection. sidecarCall retries, and a
	// discarded connection makes that retry pay a fresh dial and handshake.
	defer drainSidecarBody(resp)
	if resp.StatusCode != http.StatusOK {
		// Include a short reason from a known JSON field, but do not echo arbitrary
		// response bytes that might contain tokens or cookies.
		r := readSidecarRefusal(resp)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && r.Reason == "" {
			// A redirect is never followed (newSidecarClient), so where it
			// pointed is the one thing the answer says; the target is redacted
			// like the endpoint it was asked at.
			if loc := resp.Header.Get("Location"); loc != "" {
				r.Reason = capRunes("redirected to "+redactURL(loc), sidecarReasonRunes)
			}
		}
		return &SidecarResponseError{
			Label:      label,
			Endpoint:   endpoint,
			StatusCode: resp.StatusCode,
			Reason:     r.Reason,
			Code:       r.Code,
			Details:    r.Details,
			RetryAfter: r.RetryAfter,
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, sidecarSuccessBodyLimit)).Decode(out); err != nil {
		// A json decode error is a structured syntax/shape message (an offending
		// delimiter, or a field name and type), not a dump of raw response bytes, so
		// including it stays clear of tokens/cookies while making a custom sidecar
		// integration debuggable.
		return &SidecarResponseError{Label: label, Endpoint: endpoint, Reason: fmt.Sprintf("malformed JSON response: %v", err), Cause: err}
	}
	return nil
}

// sidecarRefusal is what a non-OK sidecar response says about itself: the short
// reason, the machine-readable code, the code's details, and the wait the
// sidecar asked for. Every field is optional; a body that carries none leaves
// the zero value.
type sidecarRefusal struct {
	Reason, Code, Details string
	RetryAfter            time.Duration
}

// readSidecarRefusal extracts the known fields of a refusal from a JSON response
// body and its headers, truncating the text ones. Other response bodies produce
// an empty refusal: raw bytes are never echoed, since they may carry tokens or
// cookies.
//
// A Retry-After header wins over the body's retry_after_seconds: the header is
// the HTTP-level statement, and a proxy or a later sidecar version can set it
// where the body cannot be changed.
func readSidecarRefusal(resp *http.Response) sidecarRefusal {
	var msg struct {
		Error             string `json:"error"`
		Message           string `json:"message"`
		Code              string `json:"code"`
		Details           string `json:"details"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	derr := json.NewDecoder(io.LimitReader(resp.Body, sidecarErrorBodyLimit)).Decode(&msg)
	reason := strings.TrimSpace(msg.Error)
	if reason == "" {
		reason = strings.TrimSpace(msg.Message)
	}
	// A decode leaves every field empty when it fails, so a document the limit
	// cut loses the code with it and the refusal would otherwise report a bare
	// status. Only a cut document is reported: io.ErrUnexpectedEOF is a JSON
	// body that ended mid-value, where a body that was never JSON (a proxy's
	// HTML error page) is a syntax error and stays unechoed, as does the
	// ordinary empty body behind a 3xx. The text names no body bytes.
	if reason == "" && errors.Is(derr, io.ErrUnexpectedEOF) {
		reason = fmt.Sprintf("refusal body past the %d-byte limit; its code and details were cut with it", sidecarErrorBodyLimit)
	}
	r := sidecarRefusal{
		Reason:  capRunes(reason, sidecarReasonRunes),
		Code:    capRunes(strings.TrimSpace(msg.Code), sidecarCodeRunes),
		Details: capRunes(strings.TrimSpace(msg.Details), sidecarReasonRunes),
	}
	if d, ok := httpx.ParseRetryAfter(resp.Header); ok {
		r.RetryAfter = d
	} else if msg.RetryAfterSeconds > 0 {
		r.RetryAfter = time.Duration(msg.RetryAfterSeconds) * time.Second
	}
	return r
}

// drainSidecarBody reads the bounded remainder of a response body and closes it,
// so the connection returns to the pool instead of being discarded.
func drainSidecarBody(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, sidecarSuccessBodyLimit)
	_ = resp.Body.Close()
}

// capRunes truncates s to at most n runes, appending an ellipsis when truncated.
// It counts runes so truncation never splits a multibyte character.
//
// A string of at most n bytes holds at most n runes, so the ordinary short
// field returns without decoding: the rune conversion is what a body read at
// sidecarErrorBodyLimit would otherwise pay on every refusal.
func capRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
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

// urlRun matches an http(s) URL up to the next space. Message text quotes and
// punctuates URLs (net/http renders `Get "https://host/path": ...`), so the match
// runs past the URL itself and redactURLsIn trims the tail back.
var urlRun = regexp.MustCompile(`https?://\S+`)

// redactURLsIn is [redactURL] for free text: it rewrites every URL it finds down
// to scheme and host. It exists for messages assembled from wrapped causes, where
// there is no single endpoint field to redact and a signed stream URL can arrive
// inside anything (a refresh failure, net/http's own *url.Error). Redacting where
// the text is built keeps it safe wherever it is later printed, which a redactor
// living in the CLI cannot do.
func redactURLsIn(s string) string {
	return urlRun.ReplaceAllStringFunc(s, func(m string) string {
		trimmed := strings.TrimRight(m, `"'.,;:!?)]}`)
		suffix := m[len(trimmed):]
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return "<url>" + suffix
		}
		return u.Scheme + "://" + u.Host + suffix
	})
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
	// Code is the body's machine-readable code ("code" in WaxSeal's envelope),
	// empty when the body carried none. It is reported verbatim; only
	// SidecarCodeVideoUnavailable changes classification.
	Code string
	// Details is the body's details field: the playabilityStatus behind a
	// video-unavailable refusal. Empty when absent.
	Details string
	// RetryAfter is the wait the sidecar asked for, from a Retry-After header or
	// a retry_after_seconds field (the header wins). Zero when it sent neither.
	RetryAfter time.Duration
	// Cause is the provider's own underlying error, for a caller that knows the
	// provider: an in-process adapter translating another client's failures into
	// this type carries that client's error here (WaxSeal's carries its
	// *client.APIError). It is neither unwrapped nor printed, so classification
	// (Unwrap stays the playability verdict) and redaction are unchanged, and a
	// caller reaches it by reading the field. WaxTap sets it in one place of its
	// own: a 200 whose body would not decode carries the decode error.
	Cause error
}

func (e *SidecarResponseError) Error() string {
	ep := redactURL(e.Endpoint)
	// A coded 200 is the verdict a provider raised from the response body, so the
	// code has to reach the message there too: it is what explains the exit code.
	what := "an unusable response"
	if e.StatusCode > 0 {
		what = fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	if e.Code != "" {
		what += fmt.Sprintf(" (%s)", e.Code)
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s at %s returned %s: %s", e.Label, ep, what, e.Reason)
	}
	return fmt.Sprintf("%s at %s returned %s", e.Label, ep, what)
}

// Unwrap returns the availability verdict a video-unavailable refusal maps to,
// so errors.Is sees ErrVideoUnavailable, ErrLoginRequired, and the rest through
// the sidecar error. Other refusals unwrap to nothing: their status describes
// the sidecar, not the video.
func (e *SidecarResponseError) Unwrap() error {
	if e.Code != SidecarCodeVideoUnavailable {
		return nil
	}
	// The reason carries the browser's own phrase, so "private", "members", and
	// "country" match here exactly as they do on a /player response.
	return youtube.ClassifyPlayability(e.Details, e.Reason)
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
func newBgutilProvider(endpoint, apiKey string, timeout time.Duration) *bgutilProvider {
	return &bgutilProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(timeout),
	}
}

// bgutilRequest and bgutilResponse mirror the bgutil /get_pot wire contract.
// Scope names what the binding is (player: a video ID; gvs: visitor data).
// WaxSeal namespaces its token cache by it; bgutil ignores it. It is always
// set: contentBinding refuses every scope it cannot name.
type bgutilRequest struct {
	ContentBinding string `json:"content_binding"`
	Scope          string `json:"scope"`
}

type bgutilResponse struct {
	POToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
	ExpiresAt      string `json:"expiresAt"`
}

// ProvidePOToken requests a scope-bound token from the configured sidecar.
func (p *bgutilProvider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	binding, scope, err := contentBinding(req)
	if err != nil {
		return potoken.Response{}, err
	}
	var out bgutilResponse
	if err := sidecarCall(ctx, p.http, http.MethodPost, p.endpoint, "bgutil PO-token server", p.apiKey,
		bgutilRequest{ContentBinding: binding, Scope: scope}, &out); err != nil {
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

// contentBinding selects the bgutil content_binding for the token scope, and the
// scope's own wire name: a player token binds to the video ID; a GVS (stream)
// token binds to the raw visitor-data string. Other scopes are unsupported by
// this provider.
//
// The wire name is potoken.Scope.String(), which is the vocabulary that package
// defines and ParseScope reads back; spelling it here again would be a second
// source of truth for someone else's names.
func contentBinding(req potoken.Request) (binding, scope string, err error) {
	switch req.Scope {
	case potoken.ScopePlayer:
		if req.VideoID == "" {
			return "", "", fmt.Errorf("bgutil: player PO token requested without a video ID")
		}
		return req.VideoID, req.Scope.String(), nil
	case potoken.ScopeGVS:
		if req.VisitorData == "" {
			return "", "", fmt.Errorf("bgutil: GVS PO token requested without visitor data")
		}
		return req.VisitorData, req.Scope.String(), nil
	default:
		return "", "", fmt.Errorf("bgutil: unsupported PO-token scope %q", req.Scope)
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
	reporter sidecarReporter
}

// newPlayerContextProvider builds a provider for a player-context endpoint that
// has already been validated by buildSidecarURL. Each request is bounded by
// timeout; the call as a whole still runs under the library's Timeouts.WebContext
// deadline.
func newPlayerContextProvider(endpoint, apiKey string, timeout time.Duration, rep sidecarReporter) *playerContextProvider {
	return &playerContextProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(timeout),
		reporter: rep,
	}
}

// InvalidateSession reports the session behind a capped context so the sidecar
// retires it and mints later contexts from a fresh one.
func (p *playerContextProvider) InvalidateSession(ctx context.Context, inv potoken.SessionInvalidation) error {
	return p.reporter.invalidate(ctx, inv)
}

type playerContextRequest struct {
	VideoID string `json:"video_id"`
}

// playerContextResponse mirrors the WaxSeal /player-context wire contract
// (snake_case). Metadata and the richer per-format fields may be absent on older
// servers; their zero values allow a video-ID filename, unknown duration, and
// selection without quality metadata.
type playerContextResponse struct {
	PlayabilityStatus            string                       `json:"playability_status"`
	PlayerURL                    string                       `json:"player_url"`
	ServerAbrStreamingURL        string                       `json:"server_abr_streaming_url"`
	VideoPlaybackUstreamerConfig string                       `json:"video_playback_ustreamer_config"`
	VisitorData                  string                       `json:"visitor_data"`
	UserAgent                    string                       `json:"user_agent"`
	ClientVersion                string                       `json:"client_version"`
	Title                        string                       `json:"title"`
	Author                       string                       `json:"author"`
	LengthSeconds                int                          `json:"length_seconds"`
	ChannelID                    string                       `json:"channel_id"`
	Description                  string                       `json:"description"`
	Thumbnails                   []playerContextThumbnailJSON `json:"thumbnails"`
	IsLiveContent                bool                         `json:"is_live_content"`
	IsLiveNow                    bool                         `json:"is_live_now"`
	IsUpcoming                   bool                         `json:"is_upcoming"`
	PublishDate                  string                       `json:"publish_date"`
	AudioFormats                 []playerContextFormatJSON    `json:"audio_formats"`
	// SessionGeneration names the daemon session for /report; absent leaves the
	// session unreportable.
	SessionGeneration uint64 `json:"session_generation"`
}

type playerContextThumbnailJSON struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
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
	if err := sidecarCall(ctx, p.http, http.MethodPost, p.endpoint, "player-context server", p.apiKey,
		playerContextRequest{VideoID: videoID}, &out); err != nil {
		return potoken.PlayerContext{}, err
	}
	// Reject unusable contexts before SABR setup so the caller can fall back. The
	// error names the snake_case wire keys for comparison with the response.
	// video_playback_ustreamer_config is also validated for non-CLI providers.
	if out.PlayabilityStatus != "" && !strings.EqualFold(out.PlayabilityStatus, "OK") {
		// A 200 that names a non-OK status is the same verdict a 422
		// video-unavailable carries; code it so both classify alike.
		return potoken.PlayerContext{}, &SidecarResponseError{
			Label:    "player-context server",
			Endpoint: p.endpoint,
			Code:     SidecarCodeVideoUnavailable,
			Details:  out.PlayabilityStatus,
			Reason:   fmt.Sprintf("playability_status %q", out.PlayabilityStatus),
		}
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
	// Allocate the ladder only when the body carried rungs, so an absent or empty
	// thumbnails key leaves Thumbnails nil.
	var thumbs []potoken.PlayerContextThumbnail
	for _, t := range out.Thumbnails {
		if t.URL == "" {
			continue
		}
		thumbs = append(thumbs, potoken.PlayerContextThumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
	}
	return potoken.PlayerContext{
		ServerAbrURL:    out.ServerAbrStreamingURL,
		PlayerURL:       out.PlayerURL,
		UstreamerConfig: out.VideoPlaybackUstreamerConfig,
		VisitorData:     out.VisitorData,
		UserAgent:       strings.TrimSpace(out.UserAgent),
		ClientVersion:   out.ClientVersion,
		Title:           out.Title,
		Author:          out.Author,
		LengthSeconds:   out.LengthSeconds,
		ChannelID:       out.ChannelID,
		Description:     out.Description,
		Thumbnails:      thumbs,
		IsLiveContent:   out.IsLiveContent,
		IsLiveNow:       out.IsLiveNow,
		IsUpcoming:      out.IsUpcoming,
		PublishDate:     out.PublishDate,
		AudioFormats:    formats,
		Generation:      out.SessionGeneration,
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
	reporter sidecarReporter
}

// newHTTPSessionProvider builds a provider for a session endpoint that has already
// been validated by buildSidecarURL.
func newHTTPSessionProvider(endpoint, apiKey string, timeout time.Duration, rep sidecarReporter) *httpSessionProvider {
	return &httpSessionProvider{
		endpoint: endpoint,
		apiKey:   apiKey,
		http:     newSidecarClient(timeout),
		reporter: rep,
	}
}

// sessionDoc mirrors the /session wire contract. snake_case keys are canonical
// (the reference minter, WaxSeal's :4416, uses them); camelCase variants are also
// accepted so the contract does not break on casing:
//
//	{"visitor_data":"<exact X-Goog-Visitor-Id literal>",
//	 "cookies":[{"name","value","domain","path","secure","http_only","expires"}],
//	 "user_agent":"<navigator.userAgent>", "client_version":"<INNERTUBE_CLIENT_VERSION>",
//	 "session_generation":<uint>}
//
// cookie_header and same_site are ignored: the cookies array carries the same
// information. user_agent and client_version are the attesting browser's
// identity, adopted onto WaxTap's WEB requests when present. expires is RFC3339
// or unix seconds; 0/absent means a session cookie. session_generation names the
// session for /report; absent leaves it unreportable.
type sessionDoc struct {
	VisitorData        string          `json:"visitor_data"`
	VisitorDataCamel   string          `json:"visitorData"`
	UserAgent          string          `json:"user_agent"`
	UserAgentCamel     string          `json:"userAgent"`
	ClientVersion      string          `json:"client_version"`
	ClientVersionCamel string          `json:"clientVersion"`
	Cookies            []sessionCookie `json:"cookies"`
	Generation         uint64          `json:"session_generation"`
	GenerationCamel    uint64          `json:"sessionGeneration"`
}

// userAgent returns the attesting browser's user agent, preferring snake_case.
func (d sessionDoc) userAgent() string {
	if ua := strings.TrimSpace(d.UserAgent); ua != "" {
		return ua
	}
	return strings.TrimSpace(d.UserAgentCamel)
}

// clientVersion returns the attesting player's client version, preferring
// snake_case.
func (d sessionDoc) clientVersion() string {
	if v := strings.TrimSpace(d.ClientVersion); v != "" {
		return v
	}
	return strings.TrimSpace(d.ClientVersionCamel)
}

// visitorData returns the supplied literal, preferring the canonical snake_case.
func (d sessionDoc) visitorData() string {
	if d.VisitorData != "" {
		return d.VisitorData
	}
	return d.VisitorDataCamel
}

// generation returns the supplied session generation, preferring snake_case.
func (d sessionDoc) generation() uint64 {
	if d.Generation != 0 {
		return d.Generation
	}
	return d.GenerationCamel
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

// ProvideSession fetches the session document. The visitorData is taken verbatim
// (no escaping or unescaping), so it stays byte-identical to the value the minter
// attests under. The one retry a transient failure earns is sidecarCall's.
func (p *httpSessionProvider) ProvideSession(ctx context.Context) (potoken.Session, error) {
	var doc sessionDoc
	if err := sidecarCall(ctx, p.http, http.MethodGet, p.endpoint, "session endpoint", p.apiKey, nil, &doc); err != nil {
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
	return potoken.Session{
		VisitorData:   vd,
		Cookies:       cookies,
		UserAgent:     doc.userAgent(),
		ClientVersion: doc.clientVersion(),
		Generation:    doc.generation(),
	}, nil
}

// InvalidateSession reports the adopted session unusable so the sidecar retires
// it and the next fetch adopts a replacement.
func (p *httpSessionProvider) InvalidateSession(ctx context.Context, inv potoken.SessionInvalidation) error {
	return p.reporter.invalidate(ctx, inv)
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
