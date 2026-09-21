package waxtap

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ParseNetscapeCookies reads a Netscape/Mozilla cookies.txt file (the format
// yt-dlp and curl use) into http.Cookies. The returned slice matches
// [POTokenSession].Cookies, so a caller can adopt a static session from a
// browser-exported cookies.txt without reimplementing the format.
//
// Each data line is seven tab-separated fields: domain, include-subdomains flag,
// path, secure, expiry (unix seconds; 0 = session), name, value. Some exporters
// drop the trailing tab when the value is empty, leaving six fields; such a line
// is read with an empty value rather than skipped. The "#HttpOnly_" domain prefix
// is checked before comment skipping, since those lines are real cookies marked
// HttpOnly, not comments. Blank lines, ordinary "#" comments, and malformed
// (under-six-field) lines are skipped: those are usually a header or a stray
// note, not a cookie the caller meant to load.
//
// An empty expiry column is a session cookie, the way 0 is: some exporters
// leave it blank rather than writing a zero. A fractional one is read to the
// second, since a timestamp that carries milliseconds still names a moment.
// Anything else fails the file, naming the line: reading a value this format
// has no meaning for as "no expiry" would keep a credential the export had
// already retired alive for the rest of the run, with nothing to say so.
//
// The secure flag is the opposite case and is read leniently: TRUE or FALSE in
// any case, and anything else as FALSE. It decides only whether a cookie may
// travel over plain HTTP, and every request WaxTap makes with a cookie jar is
// HTTPS (sidecars use their own jar-less clients), so the flag cannot change
// what this library sends. Failing a whole file over a column with no effect
// would cost a working export for nothing.
func ParseNetscapeCookies(path string) ([]*http.Cookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cookies %s: %w", path, err)
	}
	var cookies []*http.Cookie
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		line := sc.Text()
		lineNo++

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
		if len(fields) < 6 {
			continue // tolerate stray/malformed lines rather than failing the file
		}
		secs, ok := parseCookieExpiry(fields[4])
		if !ok {
			return nil, fmt.Errorf("read cookies %s: line %d: expiry %q is not a unix timestamp", path, lineNo, fields[4])
		}
		var expires time.Time
		if secs > 0 {
			expires = time.Unix(secs, 0).UTC()
		}
		// A six-field line (trailing empty-value tab dropped) has no value column.
		value := ""
		if len(fields) >= 7 {
			value = fields[6]
		}
		cookies = append(cookies, &http.Cookie{
			Domain:   fields[0],
			Path:     fields[2],
			Secure:   strings.EqualFold(strings.TrimSpace(fields[3]), "TRUE"),
			Expires:  expires,
			Name:     fields[5],
			Value:    value,
			HttpOnly: httpOnly,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read cookies %s: %w", path, err)
	}
	return cookies, nil
}

// parseCookieExpiry reads the expiry column: unix seconds, a blank column for
// a session cookie, or a fractional value truncated to the second. It reports
// false for anything else, which is a value this format has no reading for.
func parseCookieExpiry(field string) (int64, bool) {
	t := strings.TrimSpace(field)
	if t == "" {
		return 0, true // session cookie, as an explicit 0 is
	}
	if secs, err := strconv.ParseInt(t, 10, 64); err == nil {
		return secs, true
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return int64(f), true
	}
	return 0, false
}
