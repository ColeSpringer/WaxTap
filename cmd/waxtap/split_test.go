package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// The sheet every case below varies: a 6 s rip in three 2 s tracks.
const cliSplitSheet = `PERFORMER "Test Performer"
TITLE "Test Album"
FILE "rip.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:02:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:04:00
`

// splitFixture writes a rip and its sheet in a fresh temp dir.
func splitFixture(t *testing.T, sheet string) (dir, rip, cue string) {
	t.Helper()
	dir = t.TempDir()
	rip = filepath.Join(dir, "rip.wav")
	if err := os.WriteFile(rip, mediatest.SineWAV(6, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	cue = filepath.Join(dir, "rip.cue")
	if err := os.WriteFile(cue, []byte(sheet), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, rip, cue
}

func TestSplitCommandWritesNamedPieces(t *testing.T) {
	dir, rip, cue := splitFixture(t, cliSplitSheet)
	out := filepath.Join(dir, "out")

	stdout, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	for _, name := range []string{"01 - One.flac", "02 - Two.flac", "03 - Three.flac"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	for _, want := range []string{"#", "START", "TITLE", "OUTPUT"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("want the %s column in:\n%s", want, stdout)
		}
	}
}

// The sheet beside the rip is found on its own, and a lossless rip's own
// extension names the encoder.
func TestSplitCommandFindsTheSiblingSheet(t *testing.T) {
	dir, rip, _ := splitFixture(t, cliSplitSheet)
	out := filepath.Join(dir, "out")

	_, stderr, code := runMain(t, "split", rip, "-d", out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "01 - One.wav")); err != nil {
		t.Errorf("want wav pieces inferred from the rip: %v", err)
	}
}

func TestSplitCommandJSON(t *testing.T) {
	dir, rip, cue := splitFixture(t, cliSplitSheet)
	out := filepath.Join(dir, "out")

	stdout, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	if doc["schemaVersion"] != float64(schemaVersion) {
		t.Errorf("schemaVersion = %v", doc["schemaVersion"])
	}
	pieces, ok := doc["pieces"].([]any)
	if !ok || len(pieces) != 3 {
		t.Fatalf("pieces = %v", doc["pieces"])
	}
	second, _ := pieces[1].(map[string]any)
	if second["track"] != float64(2) {
		t.Errorf("pieces[1].track = %v, want 2", second["track"])
	}
	if s, _ := second["output"].(string); !strings.HasSuffix(s, "02 - Two.flac") {
		t.Errorf("pieces[1].output = %v", second["output"])
	}
	if second["start"] != float64(2) {
		t.Errorf("pieces[1].start = %v, want 2", second["start"])
	}
	album, _ := doc["album"].(map[string]any)
	if album["title"] != "Test Album" {
		t.Errorf("album.title = %v", album["title"])
	}
}

func TestSplitCommandRefusals(t *testing.T) {
	t.Run("no sheet", func(t *testing.T) {
		dir := t.TempDir()
		rip := filepath.Join(dir, "rip.wav")
		if err := os.WriteFile(rip, mediatest.SineWAV(6, 2), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runMain(t, "split", rip, "-f", "flac")
		if code != 2 || !strings.Contains(stderr, "--cue") {
			t.Errorf("exit %d, stderr %q; want exit 2 asking for --cue", code, stderr)
		}
	})

	t.Run("copy format", func(t *testing.T) {
		_, rip, cue := splitFixture(t, cliSplitSheet)
		_, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "copy")
		if code != 2 || !strings.Contains(stderr, "decodes the rip") {
			t.Errorf("exit %d, stderr %q; want exit 2", code, stderr)
		}
	})

	t.Run("lossy rip needs a format", func(t *testing.T) {
		dir, rip, cue := splitFixture(t, cliSplitSheet)
		mp3 := filepath.Join(dir, "rip.mp3")
		if _, _, code := runMain(t, "transcode", rip, "-f", "mp3", "-o", mp3); code != 0 {
			t.Fatal("fixture transcode failed")
		}
		_, stderr, code := runMain(t, "split", mp3, "--cue", cue)
		if code != 2 || !strings.Contains(stderr, "pass --format") {
			t.Errorf("exit %d, stderr %q; want exit 2 asking for --format", code, stderr)
		}
	})

	t.Run("url input", func(t *testing.T) {
		_, stderr, code := runMain(t, "split", "https://youtu.be/dummyVideo0", "-f", "flac")
		if code != 2 || !strings.Contains(stderr, "local files") {
			t.Errorf("exit %d, stderr %q; want exit 2", code, stderr)
		}
	})

	t.Run("collision fail", func(t *testing.T) {
		dir, rip, cue := splitFixture(t, cliSplitSheet)
		out := filepath.Join(dir, "out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		taken := filepath.Join(out, "02 - Two.flac")
		if err := os.WriteFile(taken, []byte("present"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out, "--collision", "fail")
		if code != 2 {
			t.Errorf("exit %d, stderr %q; want exit 2", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(out, "01 - One.flac")); err == nil {
			t.Error("a refused split wrote a piece before checking the rest")
		}
		if b, _ := os.ReadFile(taken); string(b) != "present" {
			t.Error("the existing file was overwritten")
		}
	})
}

// A sheet naming another file may still describe this rip, so the mismatch is a
// note rather than a refusal.
func TestSplitCommandNotesACueFileMismatch(t *testing.T) {
	dir, rip, _ := splitFixture(t, strings.Replace(cliSplitSheet, `FILE "rip.wav"`, `FILE "other.wav"`, 1))
	out := filepath.Join(dir, "out")

	stdout, stderr, code := runMain(t, "split", rip, "-d", out, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, string(noteCueFileMismatch)) || !strings.Contains(stdout, "other.wav") {
		t.Errorf("want the %s note in:\n%s", noteCueFileMismatch, stdout)
	}
}

// Audio before track 1 is a piece of its own, named for what it is.
func TestSplitCommandNamesTheLeadIn(t *testing.T) {
	sheet := strings.Replace(cliSplitSheet, `    TITLE "One"
    INDEX 01 00:00:00`, `    TITLE "One"
    INDEX 01 00:01:00`, 1)
	dir, rip, cue := splitFixture(t, sheet)
	out := filepath.Join(dir, "out")

	_, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "00 - Hidden Track.flac")); err != nil {
		t.Errorf("missing the lead-in piece: %v", err)
	}
}

// A mistyped --cue is a request error, not a failing disk.
func TestSplitCommandMissingCueIsUsage(t *testing.T) {
	dir, rip, _ := splitFixture(t, cliSplitSheet)
	_, stderr, code := runMain(t, "split", rip, "--cue", filepath.Join(dir, "typo.cue"), "-f", "flac")
	if code != 2 || !strings.Contains(stderr, "no cue sheet at") {
		t.Errorf("exit %d, stderr %q; want exit 2 naming the missing sheet", code, stderr)
	}
}

// A sheet is a document, not this platform's file system: a Windows-spelled
// FILE line still names its file on Unix.
func TestSheetFileBase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"rip.wav", "rip.wav"},
		{`C:\rips\rip.wav`, "rip.wav"},
		{"/home/a/rip.wav", "rip.wav"},
		{" rip.wav ", "rip.wav"},
		{"", ""},
		{`\`, ""},
	} {
		if got := sheetFileBase(tc.in); got != tc.want {
			t.Errorf("sheetFileBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// split refuses --collision skip, so the collision message must not offer it
// as the way out.
func TestSplitCollisionHintDoesNotOfferSkip(t *testing.T) {
	dir, rip, cue := splitFixture(t, cliSplitSheet)
	out := filepath.Join(dir, "out")
	if _, _, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out); code != 0 {
		t.Fatal("first split failed")
	}
	_, stderr, code := runMain(t, "split", rip, "--cue", cue, "-f", "flac", "-d", out)
	if code != 2 {
		t.Fatalf("exit %d, want 2 on the second run", code)
	}
	if strings.Contains(stderr, "or skip)") {
		t.Errorf("stderr = %q, want no bare skip suggestion: split refuses that mode", stderr)
	}
}
