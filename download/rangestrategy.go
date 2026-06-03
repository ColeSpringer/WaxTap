package download

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/colespringer/waxtap/waxerr"
)

// RangeStrategy defines how a byte range is requested and how the response is
// validated. Standard HTTP servers use a Range header and answer 206; some media
// origins expect a &range= query parameter and answer 200 with the requested
// bytes.
//
// Ranges are inclusive: [start, end] selects end-start+1 bytes. An end < 0 means
// "to the end of the resource" (open-ended), used by the streaming sinks when
// resuming from an offset.
type RangeStrategy interface {
	// Apply sets the range on req. It is only called when a range is requested
	// (start > 0 or end >= 0).
	Apply(req *http.Request, start, end int64)
	// Validate checks that resp is a correct answer for the requested range.
	// ranged reports whether Apply was called. It returns an error when the
	// status is unexpected, the server ignored the range, or the declared length
	// disagrees with the request.
	Validate(resp *http.Response, start, end int64, ranged bool) error
}

// HeaderRange requests ranges with the standard Range header and expects a 206
// Partial Content response. It is the default strategy.
type HeaderRange struct{}

// Apply sets "Range: bytes=start-end" (open-ended when end < 0).
func (HeaderRange) Apply(req *http.Request, start, end int64) {
	if end < 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
}

// Validate accepts 206 for a ranged request and 200 for an un-ranged one. A 200
// answer to a ranged request means the server ignored the Range header and
// returned the whole resource, which would corrupt offset-based writes, so it is
// rejected.
func (HeaderRange) Validate(resp *http.Response, start, end int64, ranged bool) error {
	if !ranged {
		if resp.StatusCode != http.StatusOK {
			return statusError(resp)
		}
		return nil
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return checkRangedLength(resp, start, end)
	case http.StatusOK:
		return fmt.Errorf("download: server ignored Range header (got 200 for bytes=%d-%d)", start, end)
	default:
		return statusError(resp)
	}
}

// QueryRange requests ranges with a &range=start-end query parameter and expects
// a 200 response carrying the requested bytes.
type QueryRange struct{}

// Apply sets the range query parameter (open-ended when end < 0).
func (QueryRange) Apply(req *http.Request, start, end int64) {
	q := req.URL.Query()
	if end < 0 {
		q.Set("range", fmt.Sprintf("%d-", start))
	} else {
		q.Set("range", fmt.Sprintf("%d-%d", start, end))
	}
	req.URL.RawQuery = q.Encode()
}

// Validate accepts 200 and checks the declared length for a bounded range.
func (QueryRange) Validate(resp *http.Response, start, end int64, ranged bool) error {
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	if ranged {
		return checkRangedLength(resp, start, end)
	}
	return nil
}

// checkRangedLength validates that a ranged response covers the requested span.
// If Content-Range is present, its start must match start and, for bounded
// ranges, its end must match end. A response with the right byte count but the
// wrong offset is unsafe for offset writes.
//
// For bounded ranges, Content-Length is checked when provided. The copy path
// still verifies the actual byte count.
func checkRangedLength(resp *http.Response, start, end int64) error {
	if crStart, crEnd, _, ok := parseContentRange(resp); ok {
		if crStart != start {
			return fmt.Errorf("download: response Content-Range starts at %d, requested %d", crStart, start)
		}
		if end >= 0 && crEnd != end {
			return fmt.Errorf("download: response Content-Range ends at %d, requested %d", crEnd, end)
		}
	}
	if end < 0 {
		return nil // open-ended; only the start offset (checked above) is meaningful
	}
	want := end - start + 1
	if cl := resp.ContentLength; cl >= 0 && cl != want {
		return fmt.Errorf("download: range bytes=%d-%d returned %d bytes, want %d", start, end, cl, want)
	}
	return nil
}

func statusError(resp *http.Response) error {
	e := &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status}
	if resp.Request != nil && resp.Request.URL != nil {
		e.URL = resp.Request.URL.String()
	}
	return e
}

// parseContentRange parses a "bytes start-end/total" header. ok is false when the
// header is absent, malformed, or unsatisfiable (e.g. "bytes */1234"). total is 0
// when the resource length is given as "*".
func parseContentRange(resp *http.Response) (start, end, total int64, ok bool) {
	cr := strings.TrimSpace(resp.Header.Get("Content-Range"))
	if cr == "" {
		return 0, 0, 0, false
	}
	cr = strings.TrimPrefix(cr, "bytes ")
	slash := strings.LastIndexByte(cr, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}
	rangePart, totalPart := strings.TrimSpace(cr[:slash]), strings.TrimSpace(cr[slash+1:])
	if totalPart != "*" {
		t, err := strconv.ParseInt(totalPart, 10, 64)
		if err != nil || t < 0 {
			return 0, 0, 0, false
		}
		total = t
	}
	dash := strings.IndexByte(rangePart, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	s, err1 := strconv.ParseInt(strings.TrimSpace(rangePart[:dash]), 10, 64)
	e, err2 := strconv.ParseInt(strings.TrimSpace(rangePart[dash+1:]), 10, 64)
	if err1 != nil || err2 != nil || s < 0 || e < s {
		return 0, 0, 0, false
	}
	return s, e, total, true
}

// contentRangeTotal returns the resource's total size from a Content-Range
// header, or 0 when absent/unknown. It lets the streaming sinks learn the total
// length from a 206 response when the Source did not carry one.
func contentRangeTotal(resp *http.Response) int64 {
	if _, _, total, ok := parseContentRange(resp); ok {
		return total
	}
	return 0
}
