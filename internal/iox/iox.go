// Package iox holds small io helpers shared across WaxTap.
package iox

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge reports that a capped read stopped because the source
// exceeded its byte limit. Callers classify it with errors.Is.
var ErrResponseTooLarge = errors.New("response body exceeds size cap")

// ReadAllCapped reads all of r, but returns an error wrapping ErrResponseTooLarge
// when the source would exceed limit bytes rather than silently truncating. It
// reads one byte past limit so an exactly-at-limit body is kept whole while an
// over-limit one is rejected. label names the resource in the error.
//
// A clipped body must never reach a parser, where truncation surfaces as a cryptic
// JSON/binary/JavaScript decode error downstream. Callers set limit well above any
// legitimate response, so the guard fires only on an anomaly.
//
// It buffers up to limit bytes in memory (io.ReadAll grows the slice until the
// cap), so limit is meant for whole-body reads with a known ceiling. Keep it a
// bounded constant; do not pass a multi-gigabyte or caller-controlled limit, which
// could exhaust memory before the cap is reached.
func ReadAllCapped(r io.Reader, limit int64, label string) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("%s would be truncated at the %d-byte cap: %w", label, limit, ErrResponseTooLarge)
	}
	return buf, nil
}
