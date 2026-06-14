package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// buildSidecarURL validates a sidecar URL and appends defaultPath when the URL
// does not already contain an endpoint path. Existing query parameters are
// preserved.
func buildSidecarURL(base, defaultPath string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
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

// sidecarJSON exchanges JSON with the CLI's PO-token, session, and player-context
// providers. Connection failures return *sidecarError. Unusable responses
// return *sidecarResponseError without including raw response bodies. label
// identifies the provider in returned errors. A non-empty apiKey is sent in the
// X-API-Key header.
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
		// Preserve the provider name and endpoint so the CLI can distinguish a
		// connection failure from the sentinel that may wrap it.
		return &sidecarError{label: label, endpoint: endpoint, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Include a short reason from a known JSON field, but do not echo arbitrary
		// response bytes that might contain tokens or cookies.
		return &sidecarResponseError{
			label:      label,
			endpoint:   endpoint,
			statusCode: resp.StatusCode,
			reason:     sidecarReason(resp.Body),
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, sidecarSuccessBodyLimit)).Decode(out); err != nil {
		return &sidecarResponseError{label: label, endpoint: endpoint, reason: "malformed JSON response"}
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

// sidecarError reports a connection failure to a configured provider endpoint.
type sidecarError struct {
	label    string
	endpoint string
	err      error
}

func (e *sidecarError) Error() string {
	return fmt.Sprintf("%s unreachable at %s: %v", e.label, redactURL(e.endpoint), transportReason(e.err))
}

func (e *sidecarError) Unwrap() error { return e.err }

// transportReason removes the request URL from a transport error. sidecarError
// still unwraps to the original error for classification.
func transportReason(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil {
		return ue.Err
	}
	return err
}

// sidecarResponseError reports a non-OK status or an invalid response from a
// configured sidecar. statusCode is zero when an HTTP 200 response has invalid
// content. sidecarError is reserved for connection failures.
type sidecarResponseError struct {
	label      string
	endpoint   string
	statusCode int
	reason     string
}

func (e *sidecarResponseError) Error() string {
	ep := redactURL(e.endpoint)
	switch {
	case e.statusCode > 0 && e.reason != "":
		return fmt.Sprintf("%s at %s returned HTTP %d: %s", e.label, ep, e.statusCode, e.reason)
	case e.statusCode > 0:
		return fmt.Sprintf("%s at %s returned HTTP %d", e.label, ep, e.statusCode)
	case e.reason != "":
		return fmt.Sprintf("%s at %s returned an unusable response: %s", e.label, ep, e.reason)
	default:
		return fmt.Sprintf("%s at %s returned an unusable response", e.label, ep)
	}
}
