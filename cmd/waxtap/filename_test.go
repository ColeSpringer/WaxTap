package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSanitizeStem(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "Hello World", "Hello World"},
		{"illegal dropped", `Song A?`, "Song A"},
		{"slash and colon dropped", `AC/DC: Live`, "ACDC Live"},
		{"control chars dropped", "a\x00b\x1fc", "abc"},
		{"collapse whitespace", "a   b\t c", "a b c"},
		{"trim trailing dots/space", "name.  ", "name"},
		{"empty becomes untitled", `???`, "untitled"},
		{"reserved name prefixed", "CON", "_CON"},
		{"reserved with ext prefixed", "nul.txt", "_nul.txt"},
		{"reserved case-insensitive", "Com1", "_Com1"},
		{"non-reserved similar", "CONSOLE", "CONSOLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeStem(tt.in); got != tt.want {
				t.Errorf("sanitizeStem(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveOutputNamePreservesExtension(t *testing.T) {
	// A title long enough to force truncation must keep the extension intact.
	long := strings.Repeat("x", 300)
	got := resolveOutputName("{title}.{ext}", templateData{Title: long, Ext: "mp3"})
	if !strings.HasSuffix(got, ".mp3") {
		t.Fatalf("truncated name lost extension: %q", got)
	}
	if len(got) > maxStemBytes+len(".mp3") {
		t.Errorf("name too long: %d bytes", len(got))
	}
}

func TestResolveOutputNameTemplate(t *testing.T) {
	td := templateData{Title: "Song", ID: "abc123", Author: "Artist", Ext: "opus", Itag: 251, Index: 3}
	got := resolveOutputName("{index} - {author} - {title} [{id}].{ext}", td)
	want := "03 - Artist - Song [abc123].opus"
	if got != want {
		t.Errorf("template = %q, want %q", got, want)
	}
}

func TestResolveOutputNameIndexZeroOmitted(t *testing.T) {
	got := resolveOutputName("{index}{title}.{ext}", templateData{Title: "x", Ext: "mp3", Index: 0})
	if got != "x.mp3" {
		t.Errorf("index 0 should expand empty, got %q", got)
	}
}

func TestParseCollisionMode(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want collisionMode
		ok   bool
	}{
		{"fail", collisionFail, true},
		{"overwrite", collisionOverwrite, true},
		{"auto-number", collisionAutoNumber, true},
		{"autonumber", collisionAutoNumber, true},
		{"skip", collisionSkip, true},
		{"SKIP", collisionSkip, true},
		{"bogus", collisionFail, false},
	} {
		got, err := parseCollisionMode(tt.in)
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("parseCollisionMode(%q) = %v, %v", tt.in, got, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("parseCollisionMode(%q) expected error", tt.in)
		}
	}
}

func TestResolveCollision(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "song.mp3")
	if err := writeEmpty(existing); err != nil {
		t.Fatal(err)
	}

	t.Run("fail on existing", func(t *testing.T) {
		if _, _, err := resolveCollision(existing, collisionFail); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("skip on existing", func(t *testing.T) {
		_, skip, err := resolveCollision(existing, collisionSkip)
		if err != nil || !skip {
			t.Errorf("want skip, got skip=%v err=%v", skip, err)
		}
	})
	t.Run("overwrite returns same path", func(t *testing.T) {
		got, skip, err := resolveCollision(existing, collisionOverwrite)
		if err != nil || skip || got != existing {
			t.Errorf("overwrite = %q skip=%v err=%v", got, skip, err)
		}
	})
	t.Run("auto-number finds free name", func(t *testing.T) {
		got, _, err := resolveCollision(existing, collisionAutoNumber)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(dir, "song (1).mp3"); got != want {
			t.Errorf("auto-number = %q, want %q", got, want)
		}
	})
	t.Run("absent path unchanged", func(t *testing.T) {
		fresh := filepath.Join(dir, "new.mp3")
		got, skip, err := resolveCollision(fresh, collisionFail)
		if err != nil || skip || got != fresh {
			t.Errorf("absent = %q skip=%v err=%v", got, skip, err)
		}
	})
}

func TestResolveCollisionRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	// Every collision mode must reject a directory output before its mode logic,
	// so a staged file is never renamed onto a directory.
	for _, mode := range []collisionMode{collisionFail, collisionOverwrite, collisionSkip, collisionAutoNumber} {
		_, _, err := resolveCollision(dir, mode)
		if !isUsageError(err) || !strings.Contains(err.Error(), "existing directory") {
			t.Errorf("resolveCollision(dir, %v) = %v, want existing directory usage error", mode, err)
		}
	}
	// The playlist path reserver must reject directories too.
	r := newPathReserver()
	if _, _, err := r.reserveOr(dir, collisionOverwrite); !isUsageError(err) || !strings.Contains(err.Error(), "existing directory") {
		t.Errorf("reserveOr(dir) = %v, want existing directory usage error", err)
	}
}

func TestRejectDirIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := writeEmpty(file); err != nil {
		t.Fatal(err)
	}
	if err := rejectDirIsFile(file); !isUsageError(err) || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("rejectDirIsFile(file) = %v, want not a directory usage error", err)
	}
	if err := rejectDirIsFile(dir); err != nil {
		t.Errorf("rejectDirIsFile(dir) = %v, want nil", err)
	}
	if err := rejectDirIsFile(""); err != nil {
		t.Errorf("rejectDirIsFile(\"\") = %v, want nil", err)
	}
	if err := rejectDirIsFile(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("rejectDirIsFile(absent) = %v, want nil (created later)", err)
	}
}

func writeEmpty(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func TestPathReserverUniqueUnderContention(t *testing.T) {
	dir := t.TempDir()
	r := newPathReserver()
	target := filepath.Join(dir, "song.mp3")
	const n = 24
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, skip, err := r.reserveOr(target, collisionAutoNumber)
			if skip {
				err = errSkipped
			}
			results[i], errs[i] = p, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, p := range results {
		if errs[i] != nil {
			t.Fatalf("reserve %d: %v", i, errs[i])
		}
		if seen[p] {
			t.Errorf("duplicate reserved path %q", p)
		}
		seen[p] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct paths, want %d", len(seen), n)
	}
}

func TestPathReserverNilFallback(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "x.mp3")
	var r *pathReserver
	p, skip, err := r.reserveOr(fresh, collisionFail)
	if err != nil || skip || p != fresh {
		t.Errorf("nil reserver = %q skip=%v err=%v", p, skip, err)
	}
}

var errSkipped = errSkippedT{}

type errSkippedT struct{}

func (errSkippedT) Error() string { return "unexpected skip" }
