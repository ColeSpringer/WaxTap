package waxtap

import (
	"bufio"
	"bytes"
	"fmt"
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
// (under-six-field) lines are skipped.
func ParseNetscapeCookies(path string) ([]*http.Cookie, error) {
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
		if len(fields) < 6 {
			continue // tolerate stray/malformed lines rather than failing the file
		}
		var expires time.Time
		if secs, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64); err == nil && secs > 0 {
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
