package resolver

import (
	"regexp"
	"strconv"
)

// This file reads the signature timestamp (sts) from base.js. The signature and
// n-parameter transforms themselves are no longer located here; solver.go runs
// the player's own descrambler instead of carving the functions out by regex.
// The sts is a plain integer field in the player config, so it remains a small,
// stable regex read and is kept separate: a player whose descrambler cannot be
// found must still surface its sts (the WEB /player request needs it).

// stsPatterns locate the signature timestamp in base.js. Current players use
// signatureTimestamp:<int>; older/embed players may use sts:<int>. Each pattern
// requires an object-field prefix ({ or ,) so member assignments such as
// foo.signatureTimestamp=1 or foo.sts=1 are ignored: a false timestamp causes
// YouTube to reject the /player request. The separator tolerates : or = and the
// key may be quoted, covering the player_es6, embed, and yt-dlp forms. The value
// is anchored to five or more digits so decoys like sts:0 or sts=1 cannot win
// while a real timestamp (currently ~20000, free to grow) still matches.
//
// signatureTimestamp is tried before sts so the modern field wins when both are
// present.
var stsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[{,]\s*"?signatureTimestamp"?\s*[:=]\s*(\d{5,})`),
	regexp.MustCompile(`[{,]\s*"?sts"?\s*[:=]\s*(\d{5,})`),
}

// extractSignatureTimestamp returns the signature timestamp embedded in base.js.
// It scans every match of each pattern and returns the first that parses to a
// positive integer, so a leading zero-valued decoy (e.g. signatureTimestamp:00000)
// does not shadow the real value later in the file. It reports false when no
// recognized pattern yields a positive value.
func extractSignatureTimestamp(js string) (int, bool) {
	for _, re := range stsPatterns {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}
