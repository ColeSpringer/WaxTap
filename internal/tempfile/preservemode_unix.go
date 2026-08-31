//go:build unix

package tempfile

import "os"

// PreserveReplacedMode copies dst's permission bits onto src before a replacing
// rename, so overwriting a file keeps the mode its owner chose. A missing or
// non-regular destination leaves src at the umask default it was staged with,
// and a chmod failure is not fatal: the publish still delivers the file, which
// is what the caller asked for.
//
// Lstat, not Stat: os.Rename over a symlink replaces the link itself and never
// touches its target, so a symlink destination has no replaced file to inherit
// from, and following it would copy permissions from a file this run does not
// modify. Preservation is deliberately not clamped by the umask: the point is
// to keep the replaced file's exact bits, wider or narrower than the umask
// would grant a fresh file.
//
// Call it only on a replacing publish. An exclusive publish never replaces
// anything, so there is nothing to inherit from and the umask decides.
func PreserveReplacedMode(src, dst string) {
	fi, err := os.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	_ = os.Chmod(src, fi.Mode().Perm())
}
