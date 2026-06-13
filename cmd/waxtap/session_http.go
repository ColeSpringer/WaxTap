package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/potoken"
)

// httpSessionProvider adopts a guest identity from a /session endpoint (the kind a
// PO-token minter exposes), returning the browser's exact visitorData and cookies
// so WaxTap streams under the same session the token was attested in.
//
// Like the bgutil token provider, it uses its own dedicated client and is never
// routed through --proxy/--insecure: the endpoint is typically a localhost
// sidecar, and full WEB validation requires the session host and the downloads to
// share an egress IP.
type httpSessionProvider struct {
	endpoint string
	http     *http.Client
}

// newHTTPSessionProvider builds a provider that GETs the session document from url.
func newHTTPSessionProvider(url string) *httpSessionProvider {
	return &httpSessionProvider{
		endpoint: url,
		http:     &http.Client{Timeout: 30 * time.Second},
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
	if err := sidecarJSON(ctx, p.http, http.MethodGet, p.endpoint, "session endpoint", nil, &doc); err != nil {
		return potoken.Session{}, err
	}
	vd := doc.visitorData()
	if vd == "" {
		return potoken.Session{}, &sidecarResponseError{label: "session endpoint", endpoint: p.endpoint, reason: "empty visitorData"}
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

// parseNetscapeCookies reads a Netscape/Mozilla cookies.txt file (the format
// yt-dlp and curl use) into http.Cookies for static --cookies adoption. Each data
// line is seven tab-separated fields: domain, include-subdomains flag, path,
// secure, expiry (unix seconds; 0 = session), name, value.
//
// The "#HttpOnly_" domain prefix is checked before comment skipping, since those
// lines are real cookies marked HttpOnly, not comments. Blank lines, ordinary
// "#" comments, and malformed (under-seven-field) lines are skipped.
func parseNetscapeCookies(path string) ([]*http.Cookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cookies %s: %w", path, err)
	}
	var cookies []*http.Cookie
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()

		httpOnly := false
		if rest, ok := strings.CutPrefix(line, "#HttpOnly_"); ok {
			httpOnly = true
			line = rest // the remainder is an ordinary 7-field record
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue // tolerate stray/malformed lines rather than failing the file
		}
		var expires time.Time
		if secs, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64); err == nil && secs > 0 {
			expires = time.Unix(secs, 0).UTC()
		}
		cookies = append(cookies, &http.Cookie{
			Domain:   fields[0],
			Path:     fields[2],
			Secure:   strings.EqualFold(strings.TrimSpace(fields[3]), "TRUE"),
			Expires:  expires,
			Name:     fields[5],
			Value:    fields[6],
			HttpOnly: httpOnly,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read cookies %s: %w", path, err)
	}
	return cookies, nil
}
