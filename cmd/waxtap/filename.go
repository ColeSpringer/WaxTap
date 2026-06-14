package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// defaultTemplate includes the video ID to avoid collisions between repeated
// titles.
const defaultTemplate = "{title} [{id}].{ext}"

// maxStemBytes caps the filename stem before extension and collision suffixes.
const maxStemBytes = 200

// templateData holds the fields available to --output-template placeholders.
type templateData struct {
	Title  string
	ID     string
	Author string
	Ext    string // without a leading dot
	Itag   int
	Index  int // 1-based playlist position, 0 when not a playlist
}

// expandTemplate substitutes {title}, {id}, {author}, {itag}, {ext}, and {index}
// in tmpl. {index} expands to a zero-padded number for playlist items and to an
// empty string otherwise.
func expandTemplate(tmpl string, d templateData) string {
	index := ""
	if d.Index > 0 {
		index = fmt.Sprintf("%02d", d.Index)
	}
	return strings.NewReplacer(
		"{title}", d.Title,
		"{id}", d.ID,
		"{author}", d.Author,
		"{itag}", fmt.Sprintf("%d", d.Itag),
		"{ext}", d.Ext,
		"{index}", index,
	).Replace(tmpl)
}

// resolveOutputName expands a template and returns a sanitized, cross-platform
// filename. The extension (the template's trailing {ext}) is preserved through
// truncation.
func resolveOutputName(tmpl string, d templateData) string {
	raw := expandTemplate(tmpl, d)
	ext := filepath.Ext(raw)
	stem := strings.TrimSuffix(raw, ext)
	stem = truncateBytes(sanitizeStem(stem), maxStemBytes)
	return stem + sanitizeExt(ext)
}

// sanitizeStem makes a filename stem safe on Windows, macOS, and Linux: it drops
// reserved and control characters, collapses whitespace, trims trailing dots and
// spaces, avoids Windows reserved device names, and never returns empty.
func sanitizeStem(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ") // collapse runs of whitespace
	out = strings.Trim(out, " .")
	if out == "" {
		return "untitled"
	}
	if isWindowsReserved(out) {
		out = "_" + out
	}
	return out
}

// sanitizeExt keeps a leading dot plus alphanumerics, dropping anything odd.
func sanitizeExt(ext string) string {
	ext = strings.TrimPrefix(ext, ".")
	var b strings.Builder
	for _, r := range ext {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "." + b.String()
}

// windowsReserved is the set of reserved DOS device names. A file whose stem
// matches one (case-insensitively) is invalid on Windows even with an extension.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func isWindowsReserved(stem string) bool {
	// Reserved names apply to the part before the first dot.
	base := stem
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return windowsReserved[strings.ToLower(base)]
}

// truncateBytes shortens s to at most max bytes on a valid UTF-8 boundary, then
// trims any dangling dot or space left by the cut.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	t := s[:max]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	t = strings.TrimRight(t, " .")
	if t == "" {
		return "untitled"
	}
	return t
}

type collisionMode int

const (
	collisionFail collisionMode = iota
	collisionOverwrite
	collisionAutoNumber
	collisionSkip
)

func parseCollisionMode(s string) (collisionMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fail":
		return collisionFail, nil
	case "overwrite":
		return collisionOverwrite, nil
	case "auto-number", "autonumber", "number":
		return collisionAutoNumber, nil
	case "skip":
		return collisionSkip, nil
	default:
		return collisionFail, usagef("invalid --collision %q (want fail|overwrite|auto-number|skip)", s)
	}
}

// resolveCollision applies the collision mode to a candidate path. It returns the
// path to write, whether to skip, and an error (only for collisionFail on an
// existing file). For collisionAutoNumber it returns the first free " (n)"
// variant.
func resolveCollision(path string, mode collisionMode) (out string, skip bool, err error) {
	// One stat handles both the directory check and collision detection.
	fi, statErr := os.Stat(path)
	switch {
	case statErr != nil:
		return path, false, nil // let the eventual write report unrelated errors
	case fi.IsDir():
		return "", false, dirOutputError(path)
	}
	switch mode {
	case collisionOverwrite:
		return path, false, nil
	case collisionSkip:
		return path, true, nil
	case collisionAutoNumber:
		return nextAvailable(path), false, nil
	default: // collisionFail
		return "", false, usagef("output file already exists: %s (set --collision to auto-number, overwrite, or skip)", path)
	}
}

// nextAvailable returns the first non-existing "name (n).ext" variant of path.
func nextAvailable(path string) string {
	return nextAvailableFunc(path, pathExists)
}

// nextAvailableFunc returns the first "name (n).ext" variant of path for which
// taken reports false. The predicate can account for paths already claimed in
// memory as well as paths on disk.
func nextAvailableFunc(path string, taken func(string) bool) string {
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	for n := 1; ; n++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		if !taken(candidate) {
			return candidate
		}
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dirOutputError reports an output path that names an existing directory.
func dirOutputError(path string) error {
	return usagef("output path is an existing directory: %s (give a file path)", path)
}

// rejectDirOutput rejects an existing directory before collision handling can
// attempt to replace it with a staged file.
func rejectDirOutput(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return dirOutputError(path)
	}
	return nil
}

// rejectDirIsFile rejects a --dir value that names an existing non-directory.
func rejectDirIsFile(dir string) error {
	if dir == "" {
		return nil
	}
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return usagef("--dir is not a directory: %s", dir)
	}
	return nil
}
