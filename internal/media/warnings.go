package media

import (
	"regexp"
	"slices"
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
