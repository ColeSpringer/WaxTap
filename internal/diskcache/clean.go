package diskcache

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// entryDirName is the directory the entry files live under, beneath the cache
// base: Options.Dir names it and New appends the schema version. A clean looks
// for it by name, since the base directory is the one the user gave and only
// this one is WaxTap's to remove.
const entryDirName = "players"

// Clean removes the cache under base: the players directory when it is
// WaxTap's (it carries WaxTap's own TagFile, or holds v<N> entry directories
// and nothing foreign, which is what a cache written before the tag holds),
// then base itself when that left it empty. A base that is not a directory is
// an error; one holding nothing of WaxTap's is left alone. It never removes
// anything else, so a mistyped cacheDir cannot cost a user their files.
//
// removed says WaxTap's entries were removed, not that base itself went: base
// stays whenever it holds anything else, and a caller reporting the result
// says what was cleaned rather than what was deleted.
func Clean(base string) (removed bool, err error) {
	info, err := os.Stat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, errors.New("not a directory")
	}
	entries := filepath.Join(base, entryDirName)
	if !isWaxTapCache(entries) {
		return false, nil
	}
	if err := os.RemoveAll(entries); err != nil {
		return false, err
	}
	// Only when that emptied it: a base the user also keeps other things in
	// stays, and os.Remove refuses a non-empty directory of its own accord.
	//
	// A base reached through a symlink is left alone entirely: os.Stat above
	// followed the link to judge it, but os.Remove would unlink the link
	// itself whatever the target holds, so a user who points --cache-dir at a
	// directory through a link would lose the link and keep the directory.
	if fi, lerr := os.Lstat(base); lerr == nil && fi.IsDir() {
		_ = os.Remove(base)
	}
	return true, nil
}

// Describe reports whether base exists, whether it is a directory, and
// whether it holds a WaxTap cache (the players directory Clean recognizes).
func Describe(base string) (exists, dir, populated bool) {
	info, err := os.Stat(base)
	if err != nil {
		return false, false, false
	}
	if !info.IsDir() {
		return true, false, false
	}
	return true, true, isWaxTapCache(filepath.Join(base, entryDirName))
}

// isWaxTapCache reports whether path is an entry directory WaxTap wrote: it
// carries WaxTap's own tag, or every name in it is a schema directory (v1,
// v2, ...), which is all a cache written before the tag holds. A directory
// that merely shares the name and holds anything else is not one to remove.
//
// The tag's contents are read, not just its name. CACHEDIR.TAG is a shared
// convention, so the file says only "something caches here"; what marks it as
// WaxTap's is the writer line under the specification's signature. Trusting
// the name alone would have `cache clean` remove another tool's cache that
// happened to sit under a directory called players.
func isWaxTapCache(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if waxtapTagged(filepath.Join(path, TagFile)) {
		return true
	}
	names, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	// At least one schema directory, and nothing that is not one, the tag, or
	// an OS dotfile. Requiring every entry to be a schema directory made a
	// single stray file (a .DS_Store, or a tag that is not ours) refuse to
	// clean WaxTap's own cache for good; requiring one to be present is what
	// keeps a directory that merely shares the name from being removed.
	found := false
	for _, e := range names {
		switch {
		case e.IsDir() && schemaDirName(e.Name()):
			found = true
		case e.Name() == TagFile, strings.HasPrefix(e.Name(), "."):
			// The tag is ours to remove whatever it says (a strict read
			// already ran above and this is the fallback), and a dotfile is
			// the filesystem's, not a sign of another tool's cache.
		default:
			return false
		}
	}
	return found
}

// waxtapTagged reports whether path is a cache tag WaxTap wrote: the
// specification's signature line, then the writer line Put records. It reads
// only the head of the file, which is all either line occupies.
func waxtapTagged(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(tagBody))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	body := string(head[:n])
	return strings.HasPrefix(body, TagSignature) && strings.Contains(body, tagWriterLine)
}

// schemaDirName reports whether name is "v" followed by digits, the shape New
// gives a schema-versioned entry directory.
func schemaDirName(name string) bool {
	rest, ok := strings.CutPrefix(name, "v")
	if !ok || rest == "" {
		return false
	}
	return strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
