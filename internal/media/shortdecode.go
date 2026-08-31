package media

import (
	"fmt"
	"time"
)

// Short-decode detection: a decode (or measurement) that delivers materially
// less audio than the source declares, from a file the probe passed clean.
// Mid-file corruption has exactly this shape - the byte count and headers are
// intact, so probe warnings stay empty, and the decoder reaching the bad frame
// is the first and only evidence.
//
// The threshold is absolute on purpose. Every legitimate declared-vs-decoded
// drift is bounded in absolute terms, not proportional ones: encoder framing
// (priming, padding, final-frame rounding) stays within tens of milliseconds
// whatever the length, and even a container's advisory duration is
// millisecond-granular. A relative floor would only silence real losses on
// long audio, where five missing seconds of an audiobook are no less missing
// for being under two percent of it.
const shortDecodeMinShortfall = 500 * time.Millisecond

// shortDecode reports whether got falls materially short of declared. Unknown
// durations (either side zero or negative) never trip: no claim without both
// numbers.
func shortDecode(got, declared time.Duration) bool {
	if got <= 0 || declared <= 0 {
		return false
	}
	return declared-got > shortDecodeMinShortfall
}

// ShortDecodeNote returns a damage note when a rendered output holds materially
// less audio than the source declared, or "" when the lengths agree. It is the
// one wording every rendering path (pipeline output, album track) uses.
func ShortDecodeNote(got, declared time.Duration) string {
	if !shortDecode(got, declared) {
		return ""
	}
	return fmt.Sprintf("the decode ended at %s of a declared %s; the remainder of the file did not read",
		got.Round(10*time.Millisecond), declared.Round(10*time.Millisecond))
}

// ShortMeasureNote is ShortDecodeNote for a measure-only run, where the meter's
// frame count stands in for an output file: a measurement covering part of a
// track is reported as exactly that.
func ShortMeasureNote(got, declared time.Duration) string {
	if !shortDecode(got, declared) {
		return ""
	}
	return fmt.Sprintf("the measurement covered %s of a declared %s; the remainder of the file did not read",
		got.Round(10*time.Millisecond), declared.Round(10*time.Millisecond))
}
