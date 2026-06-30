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

func TestResolveOutputNameSubdirectory(t *testing.T) {
	td := templateData{Title: "Song", ID: "abc123", Author: "Artist", Ext: "opus"}
	got := resolveOutputName("{author}/{id}.{ext}", td)
	if want := filepath.Join("Artist", "abc123.opus"); got != want {
		t.Errorf("template = %q, want %q", got, want)
	}
	// An empty leading component adds no stray directory.
	got = resolveOutputName("{author}/{id}.{ext}", templateData{ID: "abc123", Ext: "opus"})
	if got != "abc123.opus" {
		t.Errorf("empty author segment should be skipped, got %q", got)
	}
}

func TestResolveOutputNameSeparatorInValueStaysOneComponent(t *testing.T) {
	// A "/" inside a metadata value must not create a directory; only literal
	// template separators do.
	got := resolveOutputName("{title} [{id}].{ext}", templateData{Title: "AC/DC", ID: "abc123", Ext: "mp3"})
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("value separator leaked into a path: %q", got)
	}
	if got != "ACDC [abc123].mp3" {
		t.Errorf("got %q, want %q", got, "ACDC [abc123].mp3")
	}
}

func TestResolveOutputNameSingleDownloadIndexStrip(t *testing.T) {
	// {index} on a single download must not leave a dangling separator. A playlist
	// item keeps the zero-padded index.
	cases := []struct {
		name, tmpl string
		index      int
		want       string
	}{
		{"single download strips dash", "track-{index}.{ext}", 0, "track.webm"},
		{"playlist keeps index", "track-{index}.{ext}", 1, "track-01.webm"},
		{"lone index falls back to default stem", "{index}", 0, "untitled.webm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveOutputName(tc.tmpl, templateData{Ext: "webm", Index: tc.index})
			if got != tc.want {
				t.Errorf("resolveOutputName(%q, index=%d) = %q, want %q", tc.tmpl, tc.index, got, tc.want)
			}
		})
	}
}

func TestExpandTemplateIndexSeparators(t *testing.T) {
	// Each adjacent separator (but not ".") is stripped along with the empty {index}.
	for _, tc := range []struct{ tmpl, want string }{
		{"track-{index}.{ext}", "track.webm"},
		{"track_{index}.{ext}", "track.webm"},
		{"track {index}.{ext}", "track.webm"},
		{"{index}-track.{ext}", "track.webm"},
	} {
		if got := expandTemplate(tc.tmpl, templateData{Ext: "webm", Index: 0}); got != tc.want {
			t.Errorf("expandTemplate(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestResolveOutputNameTrailingEmptySegmentKeepsExtension(t *testing.T) {
	// An empty final component must not receive the extension or leave a directory.
	long := strings.Repeat("x", 300)
	got := resolveOutputName("{title}.{ext}/{index}", templateData{Title: long, Ext: "opus", Index: 0})
	if !strings.HasSuffix(got, ".opus") {
		t.Errorf("trailing empty segment dropped the extension: %q", got)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("empty final segment left a stray directory: %q", got)
	}
}

func TestResolveOutputNameNeutralizesTraversal(t *testing.T) {
	for _, tmpl := range []string{"/{id}.{ext}", "../{id}.{ext}", "{author}/../{id}.{ext}"} {
		got := resolveOutputName(tmpl, templateData{ID: "abc123", Ext: "mp3"})
		if filepath.IsAbs(got) {
			t.Errorf("%q -> %q is absolute", tmpl, got)
		}
		for seg := range strings.SplitSeq(got, string(filepath.Separator)) {
			if seg == ".." {
				t.Errorf("%q -> %q keeps a .. component", tmpl, got)
			}
		}
	}
}

func TestValidateOutputTemplate(t *testing.T) {
	valid := []string{
		defaultTemplate,
		"{title}.{ext}",
		"{index} - {author} - {title} [{id}].{ext}",
		"{author}/{id}.{ext}",
		"plain-name.{ext}",
		"{title}", // no {ext} is allowed; the real extension is added later
	}
	for _, tmpl := range valid {
		if err := validateOutputTemplate(tmpl); err != nil {
			t.Errorf("validateOutputTemplate(%q) = %v, want nil", tmpl, err)
		}
	}
	invalid := []string{
		"{artist}",       // unknown placeholder
		"{}",             // empty
		"{title, title}", // embedded junk
		"{title",         // unmatched open brace
		"title}",         // unmatched close brace
		"{title}.{xyz}",  // unknown second placeholder
	}
	for _, tmpl := range invalid {
		if err := validateOutputTemplate(tmpl); !isUsageError(err) {
			t.Errorf("validateOutputTemplate(%q) = %v, want a usage error", tmpl, err)
		}
	}
}

func TestResolveOutputNameAppendsExtension(t *testing.T) {
	cases := []struct {
		name, tmpl, want string
		data             templateData
	}{
		{"no ext in template gets real ext", "{title}", "Song.opus", templateData{Title: "Song", Ext: "opus"}},
		{"literal ext wins", "{title}.mp3", "Song.mp3", templateData{Title: "Song", Ext: "opus"}},
		{"all-empty falls back to untitled with ext", "{title}", "untitled.opus", templateData{Ext: "opus"}},
		{"all-empty unknown ext stays untitled", "{title}", "untitled", templateData{}},
		{"index zero expands empty without artifacts", "{index}{title}", "Song.flac", templateData{Title: "Song", Index: 0, Ext: "flac"}},
		// A dot inside a metadata value is not an extension: the real one is still
		// appended, and the value (including its space) is preserved.
		{"version number in title", "{title}", "Version 1.0.opus", templateData{Title: "Version 1.0", Ext: "opus"}},
		{"filename-like title", "{title}", "Song.mp3.opus", templateData{Title: "Song.mp3", Ext: "opus"}},
		{"abbreviation dot in title", "{title}", "Mr. Brightside.opus", templateData{Title: "Mr. Brightside", Ext: "opus"}},
		{"dot in title with literal template ext", "{title}.{ext}", "Version 1.0.opus", templateData{Title: "Version 1.0", Ext: "opus"}},
		// {ext} supplies the extension even without a literal dot, so the resolver
		// must not append the real extension again.
		{"ext placeholder without dot not doubled", "{id}{ext}", "abcopus", templateData{ID: "abc", Ext: "opus"}},
		{"ext placeholder with dash not doubled", "{id}-{ext}", "abc-opus", templateData{ID: "abc", Ext: "opus"}},
		// A literal dot in template text is not a trailing extension; the real one is
		// still appended.
		{"abbrev dot in template text", "Ep. {index} - {title}", "Ep. 01 - Song.opus", templateData{Title: "Song", Index: 1, Ext: "opus"}},
		{"url-like brackets in template", "{title} [www.example.com]", "Song [www.example.com].opus", templateData{Title: "Song", Ext: "opus"}},
		{"volume dot in template text", "Vol. 1 - {title}", "Vol. 1 - Song.flac", templateData{Title: "Song", Ext: "flac"}},
		{"real literal ext with internal dot", "Ep. {title}.mp3", "Ep. Song.mp3", templateData{Title: "Song", Ext: "opus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOutputName(tc.tmpl, tc.data); got != tc.want {
				t.Errorf("resolveOutputName(%q, %+v) = %q, want %q", tc.tmpl, tc.data, got, tc.want)
			}
		})
	}
}

func TestExtLooksReal(t *testing.T) {
	real := []string{".mp3", ".opus", ".FLAC", ".m4a", ".webm", ".ogg"}
	for _, s := range real {
		if !extLooksReal(s) {
			t.Errorf("extLooksReal(%q) = false, want true", s)
		}
	}
	// Empty strings, missing dots, and punctuation after the dot are not extensions.
	fake := []string{"", ".", "mp3", ". 01 - {title}", ".com]", ".tar-gz", ". 1 - x"}
	for _, s := range fake {
		if extLooksReal(s) {
			t.Errorf("extLooksReal(%q) = true, want false", s)
		}
	}
}

func TestTemplatePlaceholdersMatchExpander(t *testing.T) {
	// Every listed placeholder must be substituted by expandTemplate; validation
	// strips the same list.
	full := templateData{Title: "T", ID: "I", Author: "A", Ext: "E", Itag: 1, Index: 1}
	for _, name := range templatePlaceholders {
		tok := "{" + name + "}"
		if got := expandTemplate(tok, full); got == tok {
			t.Errorf("expandTemplate left %q unexpanded, but it is listed in templatePlaceholders", tok)
		}
	}
	// Unknown names pass through untouched so validation can reject them.
	if got := expandTemplate("{artist}", full); got != "{artist}" {
		t.Errorf("expandTemplate substituted unlisted {artist}: %q", got)
	}
}

func TestEnsureUnderDir(t *testing.T) {
	if err := ensureUnderDir("out", filepath.Join("out", "Artist", "id.mp3")); err != nil {
		t.Errorf("contained path rejected: %v", err)
	}
	if err := ensureUnderDir("out", filepath.Join("etc", "passwd")); err == nil {
		t.Error("a path outside the base dir should be rejected")
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
