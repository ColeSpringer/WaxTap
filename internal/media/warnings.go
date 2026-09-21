package media

import (
	"regexp"
	"slices"

	"github.com/colespringer/waxflow/format"
)

// memberPrefixRe matches the member index a concatenated timeline puts before
// each finding, "member 2: ", one or more deep. The shape is WaxFlow's
// documented contract for a timeline's Warnings (format.Media's doc and the
// merge job in its docs/api.md), not a private detail read off its source;
// TestCutWarningsCarryNoMemberPrefix holds the two to each other on a real
// timeline.
var memberPrefixRe = regexp.MustCompile(`^(member \d+: )+`)

// sourceWarnings turns an engine result's damage list into the source's own:
// every line once, without the member index a timeline prefixes. A cut of
// several spans opens the one source once per span, and the timeline reports
// each span's findings under its member number, so the same damage would
// otherwise arrive once per span under different names. Nothing else reaches
// here with a timeline: an album writes its tracks one at a time. Nil for a
// clean read.
func sourceWarnings(found []string) []string {
	var out []string
	for _, w := range found {
		w = memberPrefixRe.ReplaceAllString(w, "")
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// InputWarnings is the damage a read of med has found so far, in the source's
// own terms (see Result.InputWarnings): complete once the read has reached
// the end. A Media assembled from several spans of one file (a composed cut)
// reports each span's findings under a member index, which this folds away.
//
// An analysis carries its own list (waxflow.AnalyzeResult.InputWarnings,
// already folded by the same rule), so this serves a read that ends outside
// one: countFrames, which walks a file to its end for a frame count.
func InputWarnings(med format.Media) []string {
	return sourceWarnings(med.Info().Warnings)
}
