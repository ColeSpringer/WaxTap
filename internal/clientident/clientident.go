// Package clientident defines the built-in browser identity used by WaxTap's
// WEB-family YouTube clients. It centralizes values that need occasional updates:
// the emulated Chrome major and the InnerTube client versions.
package clientident

import "strconv"

// DefaultChromeMajor is the Chrome major used by the built-in WEB-family
// profiles. Keep it reasonably current; it does not need to track every release.
// Current stable versions are published at
// versionhistory.googleapis.com/v1/chrome/platforms/win/channels/stable/versions.
// Last updated in June 2026.
const DefaultChromeMajor = 149

const (
	// WebVersion is the built-in InnerTube version for the WEB client. It was
	// last verified against youtube.com configuration in June 2026.
	WebVersion = "2.20260603.05.00"
	// WebEmbeddedVersion is the built-in InnerTube version for the
	// WEB_EMBEDDED_PLAYER client. It is a known-working version from yt-dlp but
	// has not been verified against a live embed session.
	WebEmbeddedVersion = "1.20260115.01.00"
)

// ValidChromeMajor reports whether major is zero or between 1 and 999. Zero
// selects DefaultChromeMajor.
func ValidChromeMajor(major int) bool { return major >= 0 && major <= 999 }

// UserAgent returns a reduced desktop Chrome User-Agent. Zero and invalid values
// use DefaultChromeMajor. The top-level waxtap.New function rejects invalid
// values; lower-level packages use this fallback because their constructors do
// not return errors.
func UserAgent(major int) string {
	if !ValidChromeMajor(major) || major == 0 {
		major = DefaultChromeMajor
	}
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/" + strconv.Itoa(major) + ".0.0.0 Safari/537.36"
}
