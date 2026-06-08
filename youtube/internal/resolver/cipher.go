package resolver

import (
	"regexp"
	"strconv"
)

// This file reads the signature timestamp (sts) from base.js. The signature and
// n-parameter transforms themselves are no longer located here — solver.go runs
// the player's own descrambler instead of carving the functions out by regex.
// The sts is a plain integer field in the player config, so it remains a small,
// stable regex read and is kept separate: a player whose descrambler cannot be
// found must still surface its sts (the WEB /player request needs it).

// stsPatterns locate the signature timestamp in base.js. Current players use
// signatureTimestamp:<int>; older players may use sts:<int>. Only object-literal
// fields match. Assignments such as foo.signatureTimestamp=1 or foo.sts=1 are
// ignored because a false timestamp causes YouTube to reject the request.
var stsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`signatureTimestamp\s*:\s*(\d+)`),
	regexp.MustCompile(`[{,]\s*"?sts"?\s*:\s*(\d+)`),
}

// extractSignatureTimestamp returns the signature timestamp embedded in base.js.
// It reports false when no recognized pattern matches.
func extractSignatureTimestamp(js string) (int, bool) {
	for _, re := range stsPatterns {
		if m := re.FindStringSubmatch(js); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}
