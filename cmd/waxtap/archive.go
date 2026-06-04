package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
)

// downloadArchive records fetched video IDs across runs. It is separate from
// --skip-existing, which checks paths; the archive keys on video ID only.
//
// Appends are serialized with a process mutex and, on Unix, an advisory file
// lock. The archive is intended for one machine sharing one filesystem.
type downloadArchive struct {
	path string
	mu   sync.Mutex
	seen map[string]bool
}

// openArchive loads the archive file, creating an empty in-memory set when it
// does not yet exist (the file is created on first Add).
func openArchive(path string) (*downloadArchive, error) {
	a := &downloadArchive{path: path, seen: make(map[string]bool)}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return a, nil
		}
		return nil, fmt.Errorf("open download archive %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := archiveID(sc.Text()); id != "" {
			a.seen[id] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read download archive %s: %w", path, err)
	}
	return a, nil
}

// Has reports whether id is already recorded.
func (a *downloadArchive) Has(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[id]
}

// Add records id, appending it to the file under an OS lock. Recording an
// already-present id is a no-op.
func (a *downloadArchive) Add(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen[id] {
		return nil
	}

	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open download archive %s: %w", a.path, err)
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return fmt.Errorf("lock download archive %s: %w", a.path, err)
	}
	defer func() { _ = unlockFile(f) }()

	if _, err := fmt.Fprintln(f, id); err != nil {
		return fmt.Errorf("append to download archive %s: %w", a.path, err)
	}
	a.seen[id] = true
	return nil
}

// archiveID extracts the video ID from a line. It accepts a bare ID or a
// "youtube <id>" form (compatible with yt-dlp-style archives), ignoring blanks
// and comments.
func archiveID(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	if i := strings.LastIndexByte(line, ' '); i >= 0 {
		return line[i+1:]
	}
	return line
}
