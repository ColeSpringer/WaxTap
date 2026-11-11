package media

import (
	"fmt"
	"regexp"
	"strconv"
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

// lostSamplesRe matches the count the engine puts on a finding that took
// samples out of the stream: "1291 bytes are not a valid frame, dropped: 4096
// samples lost", "the frames skip from sample 8192 to 12288: 4096 samples
// lost". A FLAC numbers its frames, so the engine knows the size of a hole
// and delivers that much silence in its place.
//
// keep in sync with waxflow container/flacn and container/ogg (warn "...
// samples lost"). The engine states the count nowhere but in this text (see
// docs/upstream-requests.md), and the coupling fails quiet: a reworded
// warning reads as nothing lost, and the damage is still reported in the
// engine's own words. TestLostSamplesReadsTheEnginesCount holds the two
// together on a real hole.
var lostSamplesRe = regexp.MustCompile(`\b(\d+) samples lost\b`)

// LostSamples totals the samples a read's damage findings say were lost and
// delivered as silence. warnings is the read's own list (Result.InputWarnings),
// which names each hole once; the engine caps that list, so a file damaged in
// more places than it lists is undercounted.
func LostSamples(warnings []string) int64 {
	var n int64
	for _, w := range warnings {
		if m := lostSamplesRe.FindStringSubmatch(w); m != nil {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				n += v
			}
		}
	}
	return n
}

// LostAudioNote returns a damage note when a read delivered materially less
// audio than its length says because damage took it and silence stands in its
// place, or "" below the threshold a short decode is held to. The length holds
// there, so nothing else would say how much of the read was the file's own.
func LostAudioNote(lost time.Duration) string {
	if lost <= shortDecodeMinShortfall {
		return ""
	}
	return fmt.Sprintf("damage took %s of the audio, which reads as silence", lost.Round(10*time.Millisecond))
}
