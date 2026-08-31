//go:build !unix

package tempfile

// PreserveReplacedMode does nothing off unix. On Windows os.Chmod toggles the
// read-only attribute and os.Rename refuses to replace a read-only file, so
// preserving a 0444 destination would make the next overwrite fail where it
// succeeds today.
func PreserveReplacedMode(src, dst string) {}
